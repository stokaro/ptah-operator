package workload

import (
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestEmptyNodeSelectorsSurviveTheJobWireFormat(t *testing.T) {
	t.Parallel()

	builder := builderFixture()
	schema, migration := schemaFixture(), migrationFixture()
	schema.Spec.Execution.NodeSelector = map[string]string{}
	migration.Spec.Execution.NodeSelector = map[string]string{}
	for _, row := range []struct {
		name  string
		build func() (*batchv1.Job, error)
	}{
		{"schema", func() (*batchv1.Job, error) {
			return builder.Build(schema, operationFixture(operatorv1alpha1.OperationResolve), nil)
		}},
		{"migration", func() (*batchv1.Job, error) {
			return builder.BuildMigration(migration, migrationOperationFixture(operatorv1alpha1.MigrationOperationResolve), nil)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			job, err := row.build()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			var submitted batchv1.Job
			if err := json.Unmarshal(encoded, &submitted); err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqualWithNilDifferentFromEmpty(job.Spec, submitted.Spec) {
				t.Fatal("the built Job changes under the wire encoding used by admission")
			}
		})
	}
}
