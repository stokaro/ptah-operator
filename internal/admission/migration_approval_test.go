package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestMigrationApprovalCreateStampsIdentityAndHydratesBindings(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, true, nil)
	// The approver names only the decision: which migration, which plan, and
	// the fingerprint that plan carries.
	request := migrationApprovalRequest(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{
		Username: "operator@example.test", UID: "user-uid", Groups: []string{"platform", "platform"},
	}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("an exact migration approval was denied: %#v", response.Result)
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"operator@example.test", "user-uid", "platform", "mutationRequestUID", "approvedAt",
		"historyFingerprint", "targetIdentityDigest", "executionBindingID", testControllerImage,
	} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("the stamped approval %s does not carry %q", patchJSON, want)
		}
	}
	if strings.Count(string(patchJSON), `"platform"`) != 1 {
		t.Fatalf("duplicate groups were not normalized away: %s", patchJSON)
	}
}

func TestMigrationApprovalRefusesWhatTheEvidenceNoLongerSupports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*operatorv1alpha1.PtahMigration)
		message string
	}{
		{
			name: "the history moved after the plan was generated",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.History.Fingerprint = "sha256:" + strings.Repeat("9", 64)
			},
			message: "history moved after the plan was generated",
		},
		{
			name: "the database identity changed",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.History.TargetIdentityDigest = "sha256:" + strings.Repeat("9", 64)
			},
			message: "database identity changed",
		},
		{
			name: "the artifact was re-resolved",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.Artifact.Digest = "sha256:" + strings.Repeat("9", 64)
			},
			message: "resolved artifact changed",
		},
		{
			name: "an execution component rolled out",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@sha256:other"
			},
			message: "execution component changed",
		},
		{
			name: "the migration is not waiting for an approval",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.Phase = operatorv1alpha1.MigrationPhaseInSync
			},
			message: "not awaiting approval",
		},
		{
			name: "an operation is already in flight",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
					Type: operatorv1alpha1.MigrationOperationApply, JobName: "ptah-m-apply-app",
				}
			},
			message: "has an active operation",
		},
		{
			name: "the plan is no longer the migration's current one",
			mutate: func(migration *operatorv1alpha1.PtahMigration) {
				migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{
					Name: "ptah-mplan-000000000000000000000000", UID: types.UID("another-plan"),
				}
			},
			message: "no longer the migration's current plan",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, approval := migrationApprovalFixture(t, false, test.mutate)
			request := migrationApprovalRequest(t, approval, admissionv1.Create)
			request.UserInfo = authenticationv1.UserInfo{Username: approval.Spec.Approver.Username, UID: approval.Spec.Approver.UID}
			response := handler.Handle(context.Background(), request)
			if response.Allowed {
				t.Fatal("an approval the evidence no longer supports was admitted")
			}
			if !strings.Contains(response.Result.Message, test.message) {
				t.Fatalf("denial = %q, want one mentioning %q", response.Result.Message, test.message)
			}
		})
	}
}

func TestMigrationApprovalUpdateRejectsSpecMutation(t *testing.T) {
	t.Parallel()

	handler, approval := migrationApprovalFixture(t, false, nil)
	previous := approval.DeepCopy()
	approval.Spec.PlanFingerprint = "sha256:" + strings.Repeat("7", 64)
	request := migrationApprovalRequest(t, approval, admissionv1.Update)
	previousRaw, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	request.OldObject = runtime.RawExtension{Raw: previousRaw}

	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("an approval's decision was rewritten in place")
	}
	if !strings.Contains(response.Result.Message, "immutable") {
		t.Fatalf("denial = %q", response.Result.Message)
	}
}

func migrationApprovalRequest(
	t *testing.T,
	approval *operatorv1alpha1.PtahMigrationApproval,
	operation admissionv1.Operation,
) cradmission.Request {
	t.Helper()

	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "admission-uid", Namespace: approval.Namespace, Name: approval.Name,
		Operation: operation, Object: runtime.RawExtension{Raw: raw},
	}}
}

// migrationApprovalFixture returns a handler whose reader holds a migration
// awaiting approval, its published plan and its verification policy, plus the
// approval an approver would submit.
func migrationApprovalFixture(
	t *testing.T,
	mutate bool,
	mutateMigration func(*operatorv1alpha1.PtahMigration),
) (*MigrationApprovalHandler, *operatorv1alpha1.PtahMigrationApproval) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, operatorv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	policyBytes := []byte("version: 1\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	policyUID := types.UID("policy-v1-uid")
	immutable := true
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy", UID: policyUID},
		Immutable:  &immutable,
		Data:       map[string]string{"policy.yaml": string(policyBytes)},
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	historyFingerprint := "sha256:" + strings.Repeat("5", 64)
	targetIdentityDigest := "sha256:" + strings.Repeat("6", 64)
	artifactDigest := "sha256:" + strings.Repeat("4", 64)
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "migration-uid"},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "prod/team-a/app",
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml",
				},
			},
			Policy: operatorv1alpha1.MigrationPolicy{Apply: operatorv1alpha1.ApplyPolicyOnApproval},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch: "v1-33333333333333333333333333333333", ControllerImage: testControllerImage,
				ControllerRevision:     "controller-test-revision",
				ControllerStateVersion: 1, PtahVersion: "v0.3.0",
				ExecutorImage:         "example.invalid/ptah@sha256:executor",
				RunnerImage:           "example.invalid/operator@sha256:runner",
				RunnerProtocolVersion: int32(runner.ProtocolVersion),
			},
			Artifact: &operatorv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.test/acme/migrations@" + artifactDigest,
				Digest:            artifactDigest,
			},
			History: &operatorv1alpha1.MigrationHistoryStatus{
				ObservedAt:      metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
				ContractVersion: 1, CurrentVersion: 2,
				Fingerprint: historyFingerprint, TargetIdentityDigest: targetIdentityDigest,
				AppliedCount: 1, PendingCount: 1,
			},
			Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionTrue,
				Reason: string(operatorv1alpha1.ReasonAwaitingApproval), Message: "waiting",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	binding := migration.Status.ExecutionBinding
	planned := []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}}
	sequenceDigest, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		t.Fatal(err)
	}
	policyFingerprint, err := fingerprint.DigestCanonicalJSON(map[string]string{
		"apply": string(operatorv1alpha1.ApplyPolicyOnApproval), "lock_timeout": "0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	planBinding := migrationplan.Binding{
		MigrationUID: migration.UID, HistoryFingerprint: historyFingerprint, SequenceDigest: sequenceDigest,
		ArtifactDigest: artifactDigest, CoordinationDigest: coordinationDigest,
		TargetIdentityDigest: targetIdentityDigest, PolicyFingerprint: policyFingerprint,
		VerificationPolicyUID: policyUID, VerificationPolicyDigest: policyDigest,
		ExecutionBindingID: binding.Epoch, ControllerImage: binding.ControllerImage,
		ControllerRevision: binding.ControllerRevision, ControllerStateVersion: binding.ControllerStateVersion,
		PtahVersion: binding.PtahVersion, ExecutorImage: binding.ExecutorImage,
		RunnerImage: binding.RunnerImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
	}
	planFingerprint, err := planBinding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrationplan.Desired(migration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion: migrationplan.ContractVersion,
		MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
		Fingerprint:     planFingerprint, Migrations: planned,
		HistoryFingerprint: historyFingerprint, CurrentVersion: 2,
		ArtifactDigest: artifactDigest, CoordinationDigest: coordinationDigest,
		TargetIdentityDigest: targetIdentityDigest, PolicyFingerprint: policyFingerprint,
		VerificationPolicyUID: policyUID, VerificationPolicyDigest: policyDigest,
		ExecutionBindingID: binding.Epoch, ControllerImage: binding.ControllerImage,
		ControllerRevision: binding.ControllerRevision, ControllerStateVersion: binding.ControllerStateVersion,
		PtahVersion: binding.PtahVersion, ExecutorImage: binding.ExecutorImage,
		RunnerImage: binding.RunnerImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
		CreatedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.UID = types.UID("migration-plan-uid")
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
	if mutateMigration != nil {
		mutateMigration(migration)
	}

	approval := &operatorv1alpha1.PtahMigrationApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "approve-3"},
		Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
			MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint: planFingerprint,
		},
	}
	if !mutate {
		// The validating path sees what the mutating path already stamped.
		approval.Spec.HistoryFingerprint = historyFingerprint
		approval.Spec.ArtifactDigest = artifactDigest
		approval.Spec.CoordinationDigest = coordinationDigest
		approval.Spec.TargetIdentityDigest = targetIdentityDigest
		approval.Spec.PolicyFingerprint = policyFingerprint
		approval.Spec.VerificationPolicyUID = policyUID
		approval.Spec.VerificationPolicyDigest = policyDigest
		approval.Spec.ExecutionBindingID = binding.Epoch
		approval.Spec.ControllerImage = binding.ControllerImage
		approval.Spec.ControllerRevision = binding.ControllerRevision
		approval.Spec.ControllerStateVersion = binding.ControllerStateVersion
		approval.Spec.PtahVersion = binding.PtahVersion
		approval.Spec.ExecutorImage = binding.ExecutorImage
		approval.Spec.RunnerImage = binding.RunnerImage
		approval.Spec.RunnerProtocolVersion = binding.RunnerProtocolVersion
		approval.Spec.Approver = operatorv1alpha1.ApprovalIdentity{Username: "operator@example.test", UID: "user-uid"}
		approval.Spec.ApprovedAt = metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
		approval.Spec.MutationRequestUID = "admission-uid"
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(migration, plan, policyConfigMap).Build()
	return &MigrationApprovalHandler{
		Reader: reader, Decoder: cradmission.NewDecoder(scheme), Mutate: mutate,
		Clock:           fixedApprovalClock{now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)},
		ControllerImage: testControllerImage, ControllerRevision: "controller-test-revision",
		ControllerStateVersion: 1,
	}, approval
}

type fixedApprovalClock struct{ now time.Time }

func (c fixedApprovalClock) Now() time.Time { return c.now }
