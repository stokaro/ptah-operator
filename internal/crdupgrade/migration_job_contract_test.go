package crdupgrade

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// TestControllerJobWriteGuardAdmitsTheJobsTheBuilderProduces runs the Jobs the
// operator actually builds through the sealed CEL contract that guards the
// controller's write permission. A migration Job is a new shape under an old
// guard, and the guard fails closed: if the contract does not know the shape,
// the controller cannot dispatch at all.
func TestControllerJobWriteGuardAdmitsTheJobsTheBuilderProduces(t *testing.T) {
	t.Parallel()

	controllerImage := "example.test/controller@sha256:" + strings.Repeat("1", 64)
	builder := workload.Builder{
		ExecutorImage:          "example.test/executor@sha256:" + strings.Repeat("2", 64),
		RunnerImage:            "example.test/runner@sha256:" + strings.Repeat("3", 64),
		PtahVersion:            "v0.3.0",
		ControllerImage:        controllerImage,
		ControllerRevision:     "test-revision",
		ControllerStateVersion: ourStateVersion,
	}

	tests := []struct {
		name      string
		operation operatorv1alpha1.MigrationOperationType
	}{
		{name: "resolve reaches the registry", operation: operatorv1alpha1.MigrationOperationResolve},
		{name: "verify reaches the registry", operation: operatorv1alpha1.MigrationOperationVerify},
		{name: "history reads the database", operation: operatorv1alpha1.MigrationOperationHistory},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, operation := contractMigrationFixture(t, builder, test.operation)
			job, err := builder.BuildMigration(migration, operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			object := celObject(t, job)
			for index, validation := range controllerJobWriteValidations("refused") {
				if !evaluateJobContract(t, validation.Expression, object, controllerImage) {
					t.Fatalf("validation %d refused the migration Job the builder produced:\n%s", index, validation.Expression)
				}
			}
		})
	}

	t.Run("apply carries its bounds", func(t *testing.T) {
		t.Parallel()
		migration, operation := contractMigrationFixture(t, builder, operatorv1alpha1.MigrationOperationApply)
		operation.PlanRef = &operatorv1alpha1.ImmutableObjectReference{Name: "orders-plan-1", UID: "plan-uid"}
		dispatchNotAfter := metav1.NewTime(operation.StartedAt.Add(120 * time.Second))
		executionNotAfter := metav1.NewTime(operation.StartedAt.Add(300 * time.Second))
		operation.DispatchNotAfter = &dispatchNotAfter
		operation.ExecutionNotAfter = &executionNotAfter
		name, err := workload.NameForMigration(migration, operation)
		if err != nil {
			t.Fatal(err)
		}
		operation.JobName = name
		plan := &operatorv1alpha1.PtahMigrationPlan{
			ObjectMeta: metav1.ObjectMeta{Namespace: migration.Namespace, Name: "orders-plan-1", UID: "plan-uid"},
			Spec: operatorv1alpha1.PtahMigrationPlanSpec{
				ContractVersion:      1,
				MigrationRef:         operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
				HistoryFingerprint:   "sha256:" + strings.Repeat("5", 64),
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: "sha256:" + strings.Repeat("7", 64),
				Migrations:           []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "h1:orders-0003"}},
			},
		}
		job, err := builder.BuildMigration(migration, operation, plan)
		if err != nil {
			t.Fatal(err)
		}
		object := celObject(t, job)
		for index, validation := range controllerJobWriteValidations("refused") {
			if !evaluateJobContract(t, validation.Expression, object, controllerImage) {
				t.Fatalf("validation %d refused the migration Apply Job the builder produced:\n%s", index, validation.Expression)
			}
		}
	})

	t.Run("the schema Job the same contract has always admitted", func(t *testing.T) {
		t.Parallel()
		schema, operation := contractSchemaFixture(t, builder)
		job, err := builder.Build(schema, operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		object := celObject(t, job)
		for index, validation := range controllerJobWriteValidations("refused") {
			if !evaluateJobContract(t, validation.Expression, object, controllerImage) {
				t.Fatalf("validation %d refused the schema Job the builder produced:\n%s", index, validation.Expression)
			}
		}
	})

	t.Run("a migration Job wearing the schema subject label is refused", func(t *testing.T) {
		t.Parallel()
		migration, operation := contractMigrationFixture(t, builder, operatorv1alpha1.MigrationOperationHistory)
		job, err := builder.BuildMigration(migration, operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		job.Labels[workload.LabelSchema] = job.Labels[workload.LabelMigration]
		delete(job.Labels, workload.LabelMigration)
		object := celObject(t, job)
		refused := false
		for _, validation := range controllerJobWriteValidations("refused") {
			if !evaluateJobContract(t, validation.Expression, object, controllerImage) {
				refused = true
				break
			}
		}
		if !refused {
			t.Fatal("the contract admitted a Job whose labels do not name any subject it owns")
		}
	})
}

func contractMigrationFixture(
	t *testing.T,
	builder workload.Builder,
	operationType operatorv1alpha1.MigrationOperationType,
) (*operatorv1alpha1.PtahMigration, operatorv1alpha1.MigrationOperationStatus) {
	t.Helper()

	sha := func(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "orders", UID: "migration-uid"},
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
			Execution: operatorv1alpha1.ExecutionSpec{ServiceAccountName: "ptah-execution", ActiveDeadlineSeconds: 900},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-11111111111111111111111111111111",
				ControllerImage:        builder.ControllerImage,
				ControllerRevision:     builder.ControllerRevision,
				ControllerStateVersion: builder.ControllerStateVersion,
				PtahVersion:            builder.PtahVersion,
				ExecutorImage:          builder.ExecutorImage,
				RunnerImage:            builder.RunnerImage,
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
		},
	}
	operation := operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 sha('8'),
		InputFingerprint:   sha('a'),
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
		Attempt:            1,
		StartedAt:          metav1.Now(),
		CoordinationDigest: sha('6'),
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: sha('c'), TemplateDigest: sha('d')},
		Source: &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: "oci://registry.test/acme/orders-migrations@" + sha('4'),
			Digest:            sha('4'),
		},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
			URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
		},
	}
	name, err := workload.NameForMigration(migration, operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	return migration, operation
}

func contractSchemaFixture(
	t *testing.T,
	builder workload.Builder,
) (*operatorv1alpha1.PtahSchema, operatorv1alpha1.ActiveOperationStatus) {
	t.Helper()

	sha := func(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "orders", UID: "schema-uid"},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "tenant-a/orders-primary",
				URLFrom:         corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/orders:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Execution: operatorv1alpha1.ExecutionSpec{ServiceAccountName: "ptah-execution", ActiveDeadlineSeconds: 900},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-11111111111111111111111111111111",
				ControllerImage:        builder.ControllerImage,
				ControllerRevision:     builder.ControllerRevision,
				ControllerStateVersion: builder.ControllerStateVersion,
				PtahVersion:            builder.PtahVersion,
				ExecutorImage:          builder.ExecutorImage,
				RunnerImage:            builder.RunnerImage,
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
		},
	}
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 sha('8'),
		InputFingerprint:   sha('a'),
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
		Attempt:            1,
		StartedAt:          metav1.Now(),
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: sha('c'), TemplateDigest: sha('d')},
	}
	name, err := workload.NameFor(schema, operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	return schema, operation
}

func celObject(t *testing.T, object any) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func evaluateJobContract(t *testing.T, expression string, object map[string]any, controllerImage string) bool {
	t.Helper()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("params", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile Job contract: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build Job contract: %v", err)
	}
	result, _, err := program.Eval(map[string]any{
		"object":    object,
		"oldObject": nil,
		"request":   map[string]any{"operation": "CREATE"},
		"params":    map[string]any{},
		"variables": map[string]any{
			"activeRelease":                  int64(2),
			"previousRelease":                int64(1),
			"activeControllerImage":          controllerImage,
			"activeControllerState":          int64(ourStateVersion),
			"activeControllerStateString":    ourStateVersionString(),
			"isAnyAdmissionConvergenceProbe": false,
		},
	})
	if err != nil {
		t.Fatalf("evaluate Job contract: %v", err)
	}
	admitted, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("Job contract result = %T(%v), want bool", result.Value(), result.Value())
	}
	return admitted
}
