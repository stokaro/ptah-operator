package controllerwrite_test

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestValidationHandlerAllowsAReproducibleMigrationPlan(t *testing.T) {
	t.Parallel()

	migration, plan := migrationPlanFixture(t)
	handler := migrationPlanHandler(t, migration)

	response := handler.Handle(context.Background(), migrationPlanRequest(t, plan))
	if !response.Allowed {
		t.Fatalf("a reproducible migration plan was denied: %s", responseMessage(response))
	}
}

func TestValidationHandlerRefusesMigrationPlansItCannotReproduce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan)
		message string
	}{
		{
			name: "the history moved after the plan was made",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.History.Fingerprint = digest('9')
			},
			message: "history fingerprint does not match",
		},
		{
			name: "the plan names another database",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.History.TargetIdentityDigest = digest('9')
			},
			message: "target identity does not match",
		},
		{
			name: "the plan names another artifact",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.Artifact.Digest = digest('9')
			},
			message: "artifact digest does not match",
		},
		{
			name: "a migration was added to the sequence after it was fingerprinted",
			mutate: func(_ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Spec.Migrations = append(plan.Spec.Migrations, operatorv1alpha1.PlannedMigration{
					Version: 9, Checksum: "checksum-9",
				})
			},
			message: "sequence length does not match",
		},
		{
			name: "a checksum changed after the plan was fingerprinted",
			mutate: func(_ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Spec.Migrations[0].Checksum = "another-checksum"
			},
			message: "fingerprint does not follow from the bindings",
		},
		{
			name: "the name is not derived from the fingerprint",
			mutate: func(_ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Name = "ptah-mplan-000000000000000000000000"
			},
			message: "name is not derived from its fingerprint",
		},
		{
			name: "the plan was published under a retired execution binding",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.ExecutionBinding.ExecutorImage = "example.test/executor@" + digest('9')
			},
			message: "execution binding is not the migration's current one",
		},
		{
			name: "status was injected",
			mutate: func(_ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Status.ObservedGeneration = 1
			},
			message: "must not inject status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := migrationPlanFixture(t)
			test.mutate(migration, plan)
			handler := migrationPlanHandler(t, migration)

			response := handler.Handle(context.Background(), migrationPlanRequest(t, plan))
			if response.Allowed {
				t.Fatal("a migration plan outside its migration's evidence was admitted")
			}
			if !strings.Contains(responseMessage(response), test.message) {
				t.Fatalf("denial = %q, want one mentioning %q", responseMessage(response), test.message)
			}
		})
	}
}

func migrationPlanRequest(t *testing.T, plan *operatorv1alpha1.PtahMigrationPlan) cradmission.Request {
	t.Helper()

	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "request-uid",
		Name:      plan.Name,
		Namespace: plan.Namespace,
		Operation: admissionv1.Create,
		UserInfo:  authenticationv1.UserInfo{Username: managerUsername},
		Object:    rawObject(t, plan),
		Resource: metav1.GroupVersionResource{
			Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "ptahmigrationplans",
		},
		Kind: metav1.GroupVersionKind{
			Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Kind: "PtahMigrationPlan",
		},
	}}
}

func migrationPlanHandler(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
) *controllerwrite.ValidationHandler {
	t.Helper()

	scheme := controllerWriteScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).Build()
	return handlerWithReader(migrationJobBuilder(), reader)
}

// migrationPlanFixture returns a migration whose status carries the evidence a
// plan is decided from, and the plan that follows from it.
func migrationPlanFixture(t *testing.T) (*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan) {
	t.Helper()

	migration := &operatorv1alpha1.PtahMigration{
		TypeMeta: metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "orders", UID: types.UID("migration-uid"),
		},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "tenant-a/orders",
				URLFrom:         corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/orders-migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-11111111111111111111111111111111",
				ControllerImage:        "example.test/controller@" + digest('1'),
				ControllerRevision:     "test-revision",
				ControllerStateVersion: 1,
				PtahVersion:            "v0.3.0",
				ExecutorImage:          "example.test/executor@" + digest('2'),
				RunnerImage:            "example.test/runner@" + digest('3'),
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
			Artifact: &operatorv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.test/acme/orders-migrations@" + digest('4'),
				Digest:            digest('4'),
			},
			History: &operatorv1alpha1.MigrationHistoryStatus{
				ObservedAt:           metav1.Now(),
				ContractVersion:      1,
				CurrentVersion:       2,
				Fingerprint:          digest('5'),
				TargetIdentityDigest: digest('6'),
				AppliedCount:         1,
				PendingCount:         1,
			},
		},
	}
	planned := []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}}
	sequenceDigest, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		t.Fatal(err)
	}
	binding := migration.Status.ExecutionBinding
	planBinding := migrationplan.Binding{
		MigrationUID:             migration.UID,
		HistoryFingerprint:       migration.Status.History.Fingerprint,
		SequenceDigest:           sequenceDigest,
		ArtifactDigest:           migration.Status.Artifact.Digest,
		CoordinationDigest:       digest('7'),
		TargetIdentityDigest:     migration.Status.History.TargetIdentityDigest,
		PolicyFingerprint:        digest('8'),
		VerificationPolicyUID:    types.UID("verification-policy-uid"),
		VerificationPolicyDigest: digest('a'),
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
	}
	planFingerprint, err := planBinding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrationplan.Desired(migration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:          migrationplan.ContractVersion,
		MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
		Fingerprint:              planFingerprint,
		Migrations:               planned,
		HistoryFingerprint:       planBinding.HistoryFingerprint,
		CurrentVersion:           migration.Status.History.CurrentVersion,
		ArtifactDigest:           planBinding.ArtifactDigest,
		CoordinationDigest:       planBinding.CoordinationDigest,
		TargetIdentityDigest:     planBinding.TargetIdentityDigest,
		PolicyFingerprint:        planBinding.PolicyFingerprint,
		VerificationPolicyUID:    planBinding.VerificationPolicyUID,
		VerificationPolicyDigest: planBinding.VerificationPolicyDigest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		CreatedAt:                metav1.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.TypeMeta = metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigrationPlan"}
	return migration, plan
}

var _ client.Object = &operatorv1alpha1.PtahMigrationPlan{}
