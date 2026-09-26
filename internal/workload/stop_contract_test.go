package workload

import (
	"strconv"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// builtJob is one operation Job and whether it may change the database.
type builtJob struct {
	name     string
	job      *batchv1.Job
	mutating bool
}

// everyOperationJob builds one Job for every operation of both families.
func everyOperationJob(t *testing.T) []builtJob {
	t.Helper()
	builder := builderFixture()
	schema := schemaFixture()
	plan := planFixture(schema, builder)
	var jobs []builtJob
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify, operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan, operatorv1alpha1.OperationApply,
	} {
		var operationPlan *operatorv1alpha1.PtahSchemaPlan
		if operation == operatorv1alpha1.OperationApply {
			operationPlan = plan
		}
		job, err := builder.Build(schema.DeepCopy(), operationFixture(operation), operationPlan)
		if err != nil {
			t.Fatalf("Build(%s) error = %v", operation, err)
		}
		jobs = append(jobs, builtJob{name: "schema " + string(operation), job: job, mutating: operation == operatorv1alpha1.OperationApply})
	}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply,
	} {
		var migrationPlan *operatorv1alpha1.PtahMigrationPlan
		if operation == operatorv1alpha1.MigrationOperationApply {
			migrationPlan = migrationPlanFixture()
		}
		job, err := builder.BuildMigration(migrationFixture(), migrationOperationFixture(operation), migrationPlan)
		if err != nil {
			t.Fatalf("BuildMigration(%s) error = %v", operation, err)
		}
		jobs = append(jobs, builtJob{name: "migration " + string(operation), job: job, mutating: operation == operatorv1alpha1.MigrationOperationApply})
	}
	return jobs
}

// A mutating Pod asks the cluster autoscaler not to remove its node from under
// it, and nothing else does. The Job's annotations are the template's, because
// the controller-write guard requires them to be equal.
func TestOnlyMutatingPodsRefuseAutoscalerEviction(t *testing.T) {
	t.Parallel()

	mutating := 0
	for _, built := range everyOperationJob(t) {
		for where, annotations := range map[string]map[string]string{
			"Job":          built.job.Annotations,
			"Pod template": built.job.Spec.Template.Annotations,
		} {
			value, present := annotations[AnnotationSafeToEvict]
			switch {
			case built.mutating && (!present || value != "false"):
				t.Errorf("%s %s annotation %s = %q (present %t), want \"false\"", built.name, where, AnnotationSafeToEvict, value, present)
			case !built.mutating && present:
				t.Errorf("%s %s carries %s=%q; only a mutating Pod refuses eviction", built.name, where, AnnotationSafeToEvict, value)
			}
		}
		if built.mutating {
			mutating++
		}
	}
	if mutating != 2 {
		t.Fatalf("checked %d mutating Jobs, want the schema Apply and the migration Apply", mutating)
	}
}

// The runner container names the file the runner writes its summary to, and
// the kubelet reads it as a file.
func TestTheRunnerContainerNamesItsTerminationMessage(t *testing.T) {
	t.Parallel()

	if runner.TerminationMessagePath != corev1.TerminationMessagePathDefault {
		t.Fatalf("the runner writes %q and Kubernetes mounts %q", runner.TerminationMessagePath, corev1.TerminationMessagePathDefault)
	}
	jobs := everyOperationJob(t)
	for _, built := range jobs {
		container := requireContainer(t, built.job.Spec.Template.Spec.Containers, mainContainerName)
		if container.TerminationMessagePath != runner.TerminationMessagePath ||
			container.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
			t.Errorf("%s runner container termination message = %q %q, want %q File",
				built.name, container.TerminationMessagePath, container.TerminationMessagePolicy, runner.TerminationMessagePath)
		}
	}
	if len(jobs) != 9 {
		t.Fatalf("checked %d Jobs, want every operation of both families", len(jobs))
	}
}

// A mutating Pod is told the grace it was given, as the same number the Pod
// spec carries, so the runner never sizes its stop delay against a grace the
// Pod does not have. A read-only Pod gets the default and is not told.
func TestMutatingPodsCarryTheirGraceToTheRunner(t *testing.T) {
	t.Parallel()

	for _, built := range everyOperationJob(t) {
		spec := built.job.Spec.Template.Spec
		if spec.TerminationGracePeriodSeconds == nil {
			t.Fatalf("%s Pod has no termination grace", built.name)
		}
		grace := *spec.TerminationGracePeriodSeconds
		variable, told := envMap(built.job)[runner.EnvTerminationGracePeriod]
		switch {
		case built.mutating && (!told || variable.Value != strconv.FormatInt(grace, 10)):
			t.Errorf("%s runner is told %q (present %t), and its Pod has %d seconds", built.name, variable.Value, told, grace)
		case !built.mutating && told:
			t.Errorf("%s read-only runner is told a grace", built.name)
		case !built.mutating && grace != defaultTerminationGracePeriodSeconds:
			t.Errorf("%s Pod has %d seconds, want the default %d", built.name, grace, defaultTerminationGracePeriodSeconds)
		}
	}
}

// A schema Apply takes the grace its claim recorded, because the controller
// dates the end of the Apply's execution horizon by that record. The runner is
// told the recorded value, not the default.
func TestASchemaApplyCarriesTheGraceItsClaimRecorded(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	schema := schemaFixture()
	operation := operationFixture(operatorv1alpha1.OperationApply)
	operation.TerminationGracePeriodSeconds = 45
	job, err := builder.Build(schema, operation, planFixture(schema, builder))
	if err != nil {
		t.Fatal(err)
	}
	if got := *job.Spec.Template.Spec.TerminationGracePeriodSeconds; got != 45 {
		t.Fatalf("Apply Pod grace = %d, want the recorded 45", got)
	}
	if got := requireEnv(t, job, runner.EnvTerminationGracePeriod).Value; got != "45" {
		t.Fatalf("Apply runner is told %q, want the recorded 45", got)
	}
}
