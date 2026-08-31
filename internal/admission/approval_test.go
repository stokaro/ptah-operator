package admission

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

type fixedClock struct{ value time.Time }

func (c fixedClock) Now() time.Time { return c.value }

func TestApprovalCreateStampsAuthenticatedIdentity(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{
		Username: "alice@example.com",
		UID:      "idp-123",
		Groups:   []string{"developers", "approvers", "developers"},
	}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied valid approval: %#v", response.Result)
	}
	if len(response.Patches) == 0 {
		t.Fatal("Handle() returned no identity patch")
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice@example.com", "idp-123", "approvers", "admission-uid"} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("identity patch %s does not contain %q", patchJSON, want)
		}
	}
}

func TestApprovalValidationAcceptsTheDistinctMutatingRequestUID(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, false)
	user := authenticationv1.UserInfo{
		Username: "alice@example.com",
		UID:      "idp-123",
		Groups:   []string{"approvers", "developers"},
	}
	approval.Spec.Approver = operatorv1alpha1.ApprovalIdentity{
		Username: user.Username,
		UID:      user.UID,
		Groups:   normalizedGroups(user.Groups),
	}
	approval.Spec.ApprovedAt = metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	approval.Spec.MutationRequestUID = "mutating-review-uid"
	request := requestFor(t, approval, admissionv1.Create)
	request.UID = "validating-review-uid"
	request.UserInfo = user

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied identity stamped by the earlier mutating review: %#v", response.Result)
	}
}

func TestApprovalValidationRequiresMutatingIdentityStamp(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, false)
	approval.Spec.Approver = operatorv1alpha1.ApprovalIdentity{Username: "alice", Groups: normalizedGroups(nil)}
	approval.Spec.ApprovedAt = metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "mutating admission webhook") {
		t.Fatalf("Handle() accepted an unstamped identity: %#v", response.Result)
	}
}

func TestApprovalCreateRejectsStalePlan(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, false, true)
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted a plan without committed storage")
	}
}

func TestApprovalCreateHydratesDerivedPlanBindings(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.ArtifactDigest = ""
	approval.Spec.CoordinationDigest = ""
	approval.Spec.TargetIdentityDigest = ""
	approval.Spec.ActualStateFingerprint = ""
	approval.Spec.DesiredStateFingerprint = ""
	approval.Spec.PolicyFingerprint = ""
	approval.Spec.VerificationPolicyUID = ""
	approval.Spec.VerificationPolicyDigest = ""
	approval.Spec.PtahVersion = ""
	approval.Spec.ExecutorImage = ""
	approval.Spec.RunnerImage = ""
	approval.Spec.RunnerProtocolVersion = 0
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied minimal exact-plan approval: %#v", response.Result)
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sha256:artifact", coordinationDigest, "sha256:target", "sha256:actual", "sha256:desired",
		"sha256:policy", "policy-v1-uid", "v0.3.0", "example.invalid/ptah@sha256:executor",
		"example.invalid/operator@sha256:runner", "runnerProtocolVersion",
	} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("hydration patch %s does not contain %q", patchJSON, want)
		}
	}
}

func TestApprovalCreateRejectsConflictingDerivedBinding(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.ArtifactDigest = "sha256:different"
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() silently replaced a conflicting approval binding")
	}
}

func TestApprovalCreateRequiresExplicitPlanFingerprint(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.PlanFingerprint = ""
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted an approval without an explicit plan fingerprint")
	}
}

func TestApprovalCreateDoesNotResolvePlanAcrossNamespaces(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	apiClient, ok := handler.Reader.(client.Client)
	if !ok {
		t.Fatalf("fixture reader type %T does not implement client.Client", handler.Reader)
	}
	ctx := context.Background()
	foreignPlan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := apiClient.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "app-plan"}, foreignPlan); err != nil {
		t.Fatal(err)
	}
	foreignPlan.ResourceVersion = ""
	foreignPlan.Namespace = "team-b"
	foreignPlan.Name = "foreign-plan"
	foreignPlan.UID = "foreign-plan-uid"
	if err := apiClient.Create(ctx, foreignPlan); err != nil {
		t.Fatal(err)
	}
	approval.Spec.PlanRef = operatorv1alpha1.ImmutableObjectReference{
		Name: foreignPlan.Name,
		UID:  foreignPlan.UID,
	}
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(ctx, request)
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusNotFound {
		t.Fatalf("Handle() response = %#v, want namespace-local plan NotFound", response.Result)
	}
	if !strings.Contains(response.Result.Message, foreignPlan.Name) {
		t.Fatalf("Handle() response = %#v, want missing local plan name", response.Result)
	}
	retained := &operatorv1alpha1.PtahSchemaPlan{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(foreignPlan), retained); err != nil {
		t.Fatalf("foreign plan was not retained: %v", err)
	}
	if retained.UID != foreignPlan.UID {
		t.Fatalf("foreign plan UID = %q, want %q", retained.UID, foreignPlan.UID)
	}
}

func TestApprovalCreateRequiresCurrentApprovalBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		want   string
		mutate func(*operatorv1alpha1.PtahSchemaStatus)
	}{
		{
			name: "schema is not awaiting approval",
			want: "referenced schema is not awaiting approval",
			mutate: func(status *operatorv1alpha1.PtahSchemaStatus) {
				status.Phase = operatorv1alpha1.PhaseReadyToApply
			},
		},
		{
			name: "approval is not required",
			want: "referenced schema does not currently require approval",
			mutate: func(status *operatorv1alpha1.PtahSchemaStatus) {
				status.Conditions = []metav1.Condition{{
					Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse,
				}}
			},
		},
		{
			name: "operation is active",
			want: "referenced schema has an active operation",
			mutate: func(status *operatorv1alpha1.PtahSchemaStatus) {
				status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationApply, ID: "apply-operation",
				}
			},
		},
		{
			name: "plan already has an approval",
			want: "referenced plan already has a recorded approval",
			mutate: func(status *operatorv1alpha1.PtahSchemaStatus) {
				status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
					Name: "previous-approval", UID: "previous-approval-uid",
				}
			},
		},
		{
			name: "coordination binding changed",
			want: "schema source or target changed after the plan was generated",
			mutate: func(status *operatorv1alpha1.PtahSchemaStatus) {
				status.Target.CoordinationDigest = "sha256:" + strings.Repeat("8", 64)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler, approval := readyFixture(t, true, true, test.mutate)
			request := requestFor(t, approval, admissionv1.Create)
			request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

			response := handler.Handle(context.Background(), request)
			if response.Allowed {
				t.Fatal("Handle() accepted an approval outside the current approval boundary")
			}
			if response.Result == nil || !strings.Contains(response.Result.Message, test.want) {
				t.Fatalf("Handle() denial = %#v, want message containing %q", response.Result, test.want)
			}
		})
	}
}

func TestApprovalUpdateRejectsSpecMutation(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, false)
	old := approval.DeepCopy()
	approval.Spec.PlanFingerprint = "sha256:changed"
	request := requestFor(t, approval, admissionv1.Update)
	oldRaw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	request.OldObject = runtime.RawExtension{Raw: oldRaw}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted an approval spec mutation")
	}
}

func readyFixture(
	t *testing.T,
	ready, mutate bool,
	statusMutators ...func(*operatorv1alpha1.PtahSchemaStatus),
) (*ApprovalHandler, *operatorv1alpha1.PtahSchemaApproval) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	policyBytes := []byte("version: 1\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	policyUID := types.UID("policy-v1-uid")
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid"},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "prod/team-a/app",
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml"},
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			Phase: operatorv1alpha1.PhaseAwaitingApproval,
			Source: operatorv1alpha1.SchemaSourceStatus{
				Digest: "sha256:artifact", VerificationPolicyUID: policyUID, VerificationPolicyDigest: policyDigest,
			},
			Target: operatorv1alpha1.TargetStatus{
				CoordinationDigest: coordinationDigest,
				IdentityDigest:     "sha256:target",
			},
			Plan: &operatorv1alpha1.CurrentPlanStatus{UID: "plan-uid", Fingerprint: "sha256:plan"},
			Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue,
				Reason: "Waiting", LastTransitionTime: now,
			}},
		},
	}
	for _, mutateStatus := range statusMutators {
		mutateStatus(&schema.Status)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-plan", UID: "plan-uid", Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: "app", UID: "schema-uid"},
			Fingerprint:              "sha256:plan",
			ArtifactDigest:           "sha256:artifact",
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     "sha256:target",
			ActualStateFingerprint:   "sha256:actual",
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        "sha256:policy",
			VerificationPolicyUID:    policyUID,
			VerificationPolicyDigest: policyDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@sha256:executor",
			RunnerImage:              "example.invalid/operator@sha256:runner",
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		},
		Status: operatorv1alpha1.PtahSchemaPlanStatus{ObservedGeneration: 1},
	}
	if ready {
		plan.Status.Conditions = []metav1.Condition{{
			Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue,
			Reason: "Published", LastTransitionTime: now,
		}}
	}
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "approve-app"},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: "app", UID: "schema-uid"},
			PlanRef:                  operatorv1alpha1.ImmutableObjectReference{Name: "app-plan", UID: "plan-uid"},
			PlanFingerprint:          "sha256:plan",
			ArtifactDigest:           "sha256:artifact",
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     "sha256:target",
			ActualStateFingerprint:   "sha256:actual",
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        "sha256:policy",
			VerificationPolicyUID:    policyUID,
			VerificationPolicyDigest: policyDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@sha256:executor",
			RunnerImage:              "example.invalid/operator@sha256:runner",
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		},
	}
	immutable := true
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy", UID: policyUID},
		Immutable:  &immutable, BinaryData: map[string][]byte{"policy.yaml": policyBytes},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(schema, plan, policyConfigMap).Build()
	return &ApprovalHandler{
		Reader: reader, Decoder: cradmission.NewDecoder(scheme),
		Clock: fixedClock{value: now.Time}, Mutate: mutate,
	}, approval
}

func requestFor(t *testing.T, approval *operatorv1alpha1.PtahSchemaApproval, operation admissionv1.Operation) cradmission.Request {
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

func containsJSON(data []byte, value string) bool {
	encoded, _ := json.Marshal(value)
	return strings.Contains(string(data), strings.Trim(string(encoded), `"`))
}
