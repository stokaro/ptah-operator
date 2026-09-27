package workload

import (
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// buildsPerOperation is how many times each operation is built. Go randomizes
// map iteration on every range, so a Job whose shape came from iterating a
// map differs between builds with high probability; several builds make a
// pass by luck vanishingly unlikely.
const buildsPerOperation = 8

// TestEveryOperationBuildsTheSameJobEveryTime holds both builders to what an
// admission snapshot refresh relies on: the same claim builds the same Job.
// A controller that finds the rebuilt Pod template differing from the one its
// snapshot recorded resolves the snapshot again once, which is what a
// manager release looks like, and would never stop if the builder itself
// moved.
//
// The whole Job is compared rather than podintent.DigestTemplate of its
// template. The digest is a function of the template, so equal Jobs have
// equal digests, and podintent imports this package, so the digest cannot be
// taken from a test inside it.
func TestEveryOperationBuildsTheSameJobEveryTime(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
		operatorv1alpha1.OperationApply,
	} {
		t.Run("schema "+string(operation), func(t *testing.T) {
			t.Parallel()
			requireSameJob(t, func() (*batchv1.Job, error) {
				// A fresh schema each time: planFixture records its plan
				// on the schema it is handed.
				schema := schemaFixture()
				var plan *operatorv1alpha1.PtahSchemaPlan
				if operation == operatorv1alpha1.OperationApply {
					plan = planFixture(schema, builder)
				}
				return builder.Build(schema, operationFixture(operation), plan)
			})
		})
	}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve,
		operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory,
		operatorv1alpha1.MigrationOperationApply,
	} {
		t.Run("migration "+string(operation), func(t *testing.T) {
			t.Parallel()
			requireSameJob(t, func() (*batchv1.Job, error) {
				return builder.BuildMigration(migrationFixture(), migrationOperationFixture(operation), migrationPlanFixture())
			})
		})
	}
}

func requireSameJob(t *testing.T, build func() (*batchv1.Job, error)) {
	t.Helper()

	first, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for attempt := 1; attempt < buildsPerOperation; attempt++ {
		again, err := build()
		if err != nil {
			t.Fatalf("build %d: %v", attempt, err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("build %d differs from the first:\nfirst %#v\nagain %#v", attempt, first.Spec.Template, again.Spec.Template)
		}
	}
}
