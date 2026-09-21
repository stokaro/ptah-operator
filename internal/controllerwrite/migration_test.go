package controllerwrite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestValidationHandlerAllowsExactMigrationJobCreate(t *testing.T) {
	t.Parallel()

	migration, job := migrationJobFixture(t, operatorv1alpha1.MigrationOperationHistory)
	handler := migrationHandlerFixture(t, migration)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, job))
	if !response.Allowed {
		t.Fatalf("migration Job create was denied: %s", responseMessage(response))
	}
}

func TestValidationHandlerRefusesMigrationJobsOutsideTheirClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*operatorv1alpha1.PtahMigration, *batchv1.Job)
		message string
	}{
		{
			name: "no claim at all",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				migration.Status.ActiveOperation = nil
			},
			message: "not-yet-created migration operation",
		},
		{
			name: "claim already has its Job",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				migration.Status.ActiveOperation.JobUID = types.UID("another-job")
			},
			message: "not-yet-created migration operation",
		},
		{
			name: "Job name is not the claimed name",
			mutate: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.Name = "ptah-m-history-orders-0000000000000000"
			},
			message: "not-yet-created migration operation",
		},
		{
			name: "owner UID is not the current migration",
			mutate: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.OwnerReferences[0].UID = types.UID("a-deleted-migration")
			},
			message: "current PtahMigration UID",
		},
		{
			name: "owner is neither subject",
			mutate: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.OwnerReferences[0].Kind = "Deployment"
			},
			message: "exact operator controller owner",
		},
		{
			name: "Pod template drifts from the claim's snapshot",
			mutate: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.Spec.Template.Spec.Containers[0].Args = append(
					job.Spec.Template.Spec.Containers[0].Args, "--extra",
				)
			},
			message: "outside the migration operation intent",
		},
		{
			name: "labels drift from the reconstructed intent",
			mutate: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.Labels[workload.LabelOperation] = "apply"
			},
			message: "outside the migration operation intent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, job := migrationJobFixture(t, operatorv1alpha1.MigrationOperationHistory)
			test.mutate(migration, job)
			handler := migrationHandlerFixture(t, migration)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, job))
			if response.Allowed {
				t.Fatal("migration Job create was admitted outside its claim")
			}
			if !strings.Contains(responseMessage(response), test.message) {
				t.Fatalf("denial = %q, want one mentioning %q", responseMessage(response), test.message)
			}
		})
	}
}

func TestValidationHandlerAllowsOnlyTheMigrationCleanupTTL(t *testing.T) {
	t.Parallel()

	migration, job := migrationJobFixture(t, operatorv1alpha1.MigrationOperationHistory)
	terminal := withGeneratedJobIdentity(job)
	migration.Status.ActiveOperation.JobUID = terminal.UID
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	handler := migrationHandlerFixture(t, migration, terminal)

	cleaned := terminal.DeepCopy()
	ttl := int32(300)
	cleaned.Spec.TTLSecondsAfterFinished = &ttl
	response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
	if !response.Allowed {
		t.Fatalf("migration Job cleanup was denied: %s", responseMessage(response))
	}

	running := terminal.DeepCopy()
	running.Status.Conditions = nil
	handler = migrationHandlerFixture(t, migration, running)
	runningCleaned := running.DeepCopy()
	runningCleaned.Spec.TTLSecondsAfterFinished = &ttl
	response = handler.Handle(context.Background(), migrationUpdateRequest(t, running, runningCleaned))
	if response.Allowed {
		t.Fatal("cleanup TTL was admitted before the migration Job reached a terminal status")
	}

	handler = migrationHandlerFixture(t, migration, terminal)
	rewritten := terminal.DeepCopy()
	rewritten.Spec.TTLSecondsAfterFinished = &ttl
	rewritten.Spec.Template.Spec.Containers[0].Image = "example.test/other@" + digest('9')
	response = handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, rewritten))
	if response.Allowed {
		t.Fatal("a Pod template rewrite rode along with the cleanup TTL")
	}
}

// responseMessage is the denial text an admission response carries.
func responseMessage(response cradmission.Response) string {
	if response.Result == nil {
		return ""
	}
	return response.Result.Message
}

func migrationUpdateRequest(t *testing.T, oldJob, job *batchv1.Job) cradmission.Request {
	t.Helper()

	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)
	return request
}

func migrationHandlerFixture(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	objects ...client.Object,
) *controllerwrite.ValidationHandler {
	t.Helper()

	scheme := controllerWriteScheme(t)
	stored := append([]client.Object{migration, migrationServiceAccount()}, objects...)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored...).Build()
	return handlerWithReader(migrationJobBuilder(), reader)
}

func migrationJobBuilder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          "example.test/executor@" + digest('2'),
		RunnerImage:            "example.test/runner@" + digest('3'),
		PtahVersion:            "v0.3.0",
		ControllerImage:        "example.test/controller@" + digest('1'),
		ControllerRevision:     "test-revision",
		ControllerStateVersion: 1,
	}
}

func migrationServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant-a", Name: "ptah-orders", UID: "service-account-uid", ResourceVersion: "1",
	}}
}

// migrationJobFixture returns a migration whose claim is persisted and the Job
// that claim authorizes, built by the same builder admission rebuilds with.
func migrationJobFixture(
	t *testing.T,
	operationType operatorv1alpha1.MigrationOperationType,
) (*operatorv1alpha1.PtahMigration, *batchv1.Job) {
	t.Helper()

	migration := &operatorv1alpha1.PtahMigration{
		TypeMeta: metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "orders", UID: "migration-uid",
		},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/orders-migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Execution: operatorv1alpha1.ExecutionSpec{ServiceAccountName: "ptah-orders"},
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
		},
	}
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 digest('8'),
		InputFingerprint:   digest('a'),
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
		Attempt:            1,
		StartedAt:          metav1.Now(),
		Source: &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: "oci://registry.test/acme/orders-migrations@" + digest('4'),
			Digest:            digest('4'),
		},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
			URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
		},
		CoordinationDigest: digest('6'),
	}
	migration.Status.ActiveOperation = operation

	builder := migrationJobBuilder()
	name, err := workload.NameForMigration(migration, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	job, err := builder.BuildMigration(migration, *operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
			Object: operatorv1alpha1.AdmissionObjectBinding{
				Name: "ptah-orders", UID: "service-account-uid", ResourceVersion: "1",
			},
		},
	}
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = snapshotDigest
	operation.AdmissionSnapshot = snapshot
	job, err = builder.BuildMigration(migration, *operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	job.TypeMeta = metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
	return migration, job
}

// An Apply Job carries the approved plan's bindings, so admission has to read
// the same plan to rebuild it. A Job whose plan is not the one the claim named
// cannot be reconstructed, and is refused before it reaches the API.
func TestValidationHandlerBindsMigrationApplyJobsToTheirPlan(t *testing.T) {
	t.Parallel()

	migration, plan, job := migrationApplyJobFixture(t)
	handler := migrationHandlerFixture(t, migration, plan)
	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, job))
	if !response.Allowed {
		t.Fatalf("migration Apply Job create was denied: %s", responseMessage(response))
	}

	for _, test := range []struct {
		name    string
		mutate  func(*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan)
		message string
	}{
		{
			name: "the claim names no plan",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.ActiveOperation.PlanRef = nil
			},
			message: "names no immutable plan",
		},
		{
			name: "the plan was replaced after the claim",
			mutate: func(migration *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
				migration.Status.ActiveOperation.PlanRef.UID = types.UID("a-newer-plan")
			},
			message: "plan UID does not match the operation claim",
		},
		{
			name: "the plan approved another realm",
			mutate: func(_ *operatorv1alpha1.PtahMigration, plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Spec.CoordinationDigest = digest('9')
			},
			message: "cannot reconstruct the submitted Job",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan, job := migrationApplyJobFixture(t)
			test.mutate(migration, plan)
			handler := migrationHandlerFixture(t, migration, plan)
			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, job))
			if response.Allowed {
				t.Fatal("a migration Apply Job was admitted without the plan that authorized it")
			}
			if !strings.Contains(responseMessage(response), test.message) {
				t.Fatalf("denial = %q, want one mentioning %q", responseMessage(response), test.message)
			}
		})
	}
}

// migrationApplyJobFixture is an Apply claim, the immutable plan it names, and
// the Job that claim authorizes.
func migrationApplyJobFixture(t *testing.T) (
	*operatorv1alpha1.PtahMigration,
	*operatorv1alpha1.PtahMigrationPlan,
	*batchv1.Job,
) {
	t.Helper()

	migration, _ := migrationJobFixture(t, operatorv1alpha1.MigrationOperationHistory)
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: "ptah-mplan-orders-1", UID: types.UID("migration-plan-uid"),
		},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			ContractVersion:      1,
			MigrationRef:         operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
			Fingerprint:          digest('b'),
			HistoryFingerprint:   digest('5'),
			CurrentVersion:       2,
			ArtifactDigest:       digest('4'),
			CoordinationDigest:   digest('6'),
			TargetIdentityDigest: digest('7'),
			Migrations:           []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "h1:orders-0003"}},
		},
	}

	operation := migration.Status.ActiveOperation
	operation.Type = operatorv1alpha1.MigrationOperationApply
	operation.AdmissionSnapshot = nil
	operation.PlanRef = &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
	// Second precision, because that is what the API server stores and what
	// admission therefore rebuilds the Job from.
	operation.StartedAt = operation.StartedAt.Rfc3339Copy()
	dispatchNotAfter := metav1.NewTime(operation.StartedAt.Add(600 * time.Second)).Rfc3339Copy()
	operation.DispatchNotAfter = &dispatchNotAfter
	operation.ExecutionNotAfter = dispatchNotAfter.DeepCopy()

	builder := migrationJobBuilder()
	name, err := workload.NameForMigration(migration, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	job, err := builder.BuildMigration(migration, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
			Object: operatorv1alpha1.AdmissionObjectBinding{
				Name: "ptah-orders", UID: "service-account-uid", ResourceVersion: "1",
			},
		},
	}
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = snapshotDigest
	operation.AdmissionSnapshot = snapshot
	job, err = builder.BuildMigration(migration, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	job.TypeMeta = metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
	return migration, plan, job
}
