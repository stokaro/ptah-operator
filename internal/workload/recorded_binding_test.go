package workload

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// executorMoved is builder after a release that moved the executor image and
// the Ptah version and nothing else.
func executorMoved(builder Builder) Builder {
	builder.ExecutorImage = "example.invalid/ptah@" + digest('9')
	builder.PtahVersion = "v0.4.0"
	return builder
}

// A release that moved the executor refuses an Apply claim made under the
// binding its predecessor recorded, and builds, under that binding, the Job
// its predecessor dispatched for the claim.
func TestABuilderRebuildsAnApplyUnderTheBindingItWasDispatchedUnder(t *testing.T) {
	t.Parallel()

	t.Run("schema", func(t *testing.T) {
		t.Parallel()

		schema := schemaFixture()
		previous := builderFixture()
		plan := planFixture(schema, previous)
		operation := operationFixture(operatorv1alpha1.OperationApply)
		dispatched, err := previous.Build(schema, operation, plan)
		if err != nil {
			t.Fatal(err)
		}
		next := executorMoved(previous)
		if _, err := next.Build(schema, operation, plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("Build() error = %v, want the claim refused as stale for this release", err)
		}
		rebuilt, err := next.BuildUnderBinding(schema.Status.ExecutionBinding, schema, operation, plan)
		if err != nil {
			t.Fatalf("BuildUnderBinding() error = %v", err)
		}
		if !apiequality.Semantic.DeepEqual(rebuilt, dispatched) {
			t.Fatalf("the rebuilt Job differs from the one dispatched:\nrebuilt=%#v\ndispatched=%#v", rebuilt.Spec, dispatched.Spec)
		}
	})
	t.Run("migration", func(t *testing.T) {
		t.Parallel()

		migration := migrationFixture()
		previous := builderFixture()
		operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
		plan := migrationPlanFixture()
		dispatched, err := previous.BuildMigration(migration, operation, plan)
		if err != nil {
			t.Fatal(err)
		}
		next := executorMoved(previous)
		if _, err := next.BuildMigration(migration, operation, plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("BuildMigration() error = %v, want the claim refused as stale for this release", err)
		}
		rebuilt, err := next.BuildMigrationUnderBinding(migration.Status.ExecutionBinding, migration, operation, plan)
		if err != nil {
			t.Fatalf("BuildMigrationUnderBinding() error = %v", err)
		}
		if !apiequality.Semantic.DeepEqual(rebuilt, dispatched) {
			t.Fatalf("the rebuilt Job differs from the one dispatched:\nrebuilt=%#v\ndispatched=%#v", rebuilt.Spec, dispatched.Spec)
		}
	})
	// A move across a Ptah release that adds a version-dependent argument:
	// the predecessor's Job lacks it, and so does the rebuild, while a Job
	// this manager dispatches under its own binding carries it.
	t.Run("migration across v0.34.0", func(t *testing.T) {
		t.Parallel()

		previous, migration := migrationUnderPtah("v0.33.1", 'b')
		next, moved := migrationUnderPtah("v0.34.0", 'c')
		operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
		plan := migrationPlanFixture()
		dispatched, err := previous.BuildMigration(migration, operation, plan)
		if err != nil {
			t.Fatal(err)
		}
		own, err := next.BuildMigration(moved, operation, plan)
		if err != nil {
			t.Fatal(err)
		}
		if apiequality.Semantic.DeepEqual(own, dispatched) || !slices.Contains(fetchArgs(t, own), "--skip-validation") ||
			slices.Contains(fetchArgs(t, dispatched), "--skip-validation") {
			t.Fatalf("the versions do not straddle --skip-validation: dispatched %q, own %q", fetchArgs(t, dispatched), fetchArgs(t, own))
		}
		rebuilt, err := next.BuildMigrationUnderBinding(migration.Status.ExecutionBinding, migration, operation, plan)
		if err != nil {
			t.Fatalf("BuildMigrationUnderBinding() error = %v", err)
		}
		if !apiequality.Semantic.DeepEqual(rebuilt, dispatched) {
			t.Fatalf("the rebuilt Job differs from the one dispatched:\nrebuilt=%#v\ndispatched=%#v", rebuilt.Spec, dispatched.Spec)
		}
	})
}

// fetchArgs are the arguments of the init container that pulls a migration
// artifact.
func fetchArgs(t *testing.T, job *batchv1.Job) []string {
	t.Helper()
	return requireContainer(t, job.Spec.Template.Spec.InitContainers, migrationFetchContainerName).Args
}

// A builder rebuilds only under a binding it can read as recorded: one of its
// own controller-state version and runner protocol, naming an executor it
// would accept from its own configuration.
func TestABuilderRefusesABindingItCannotBuildUnder(t *testing.T) {
	t.Parallel()

	recorded := func() *operatorv1alpha1.ExecutionBindingStatus {
		return schemaFixture().Status.ExecutionBinding.DeepCopy()
	}
	for _, row := range []struct {
		name    string
		binding func() *operatorv1alpha1.ExecutionBindingStatus
		says    string
	}{
		{name: "no binding", binding: func() *operatorv1alpha1.ExecutionBindingStatus { return nil }, says: "execution binding is required"},
		{
			name: "another controller-state version",
			binding: func() *operatorv1alpha1.ExecutionBindingStatus {
				binding := recorded()
				binding.ControllerStateVersion++
				return binding
			},
			says: "controller state version",
		},
		{
			name: "another runner protocol",
			binding: func() *operatorv1alpha1.ExecutionBindingStatus {
				binding := recorded()
				binding.RunnerProtocolVersion = int32(runner.ProtocolVersion) + 1
				return binding
			},
			says: "runner protocol",
		},
		{
			name: "an executor not pinned by digest",
			binding: func() *operatorv1alpha1.ExecutionBindingStatus {
				binding := recorded()
				binding.ExecutorImage = "example.invalid/ptah:v0.3.0"
				return binding
			},
			says: "executor image",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			previous := builderFixture()
			plan := planFixture(schema, previous)
			operation := operationFixture(operatorv1alpha1.OperationApply)
			// The control: the fixture's own binding builds.
			if _, err := executorMoved(previous).BuildUnderBinding(recorded(), schema, operation, plan); err != nil {
				t.Fatalf("BuildUnderBinding() under the recorded binding error = %v", err)
			}
			job, err := executorMoved(previous).BuildUnderBinding(row.binding(), schema, operation, plan)
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("BuildUnderBinding() = %v, %v; want a refusal naming %q", job != nil, err, row.says)
			}
		})
	}
}
