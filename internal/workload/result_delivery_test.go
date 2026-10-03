package workload

import (
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
)

func TestEveryOperationBuilderProjectsDurableCredentials(t *testing.T) {
	builder := builderFixture()
	builder.ResultEndpoint = "https://receiver.operator.svc:9444"
	for _, kind := range []api.OperationType{api.OperationResolve, api.OperationVerify, api.OperationObserve, api.OperationPlan, api.OperationApply} {
		t.Run("schema/"+string(kind), func(t *testing.T) {
			schema := schemaFixture()
			schema.Generation = 7
			operation := operationFixture(kind)
			var plan *api.PtahSchemaPlan
			if kind == api.OperationApply {
				plan = planFixture(schema, builder)
			}
			job, err := builder.Build(schema, operation, plan)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := jobconfig.Read(job, schema.UID, operation.ID)
			if err != nil || bound.Generation != 7 || bound.Endpoint != builder.ResultEndpoint {
				t.Fatalf("projection=%#v, %v", bound, err)
			}
		})
	}
	for _, kind := range []api.MigrationOperationType{api.MigrationOperationResolve, api.MigrationOperationVerify, api.MigrationOperationHistory, api.MigrationOperationApply} {
		t.Run("migration/"+string(kind), func(t *testing.T) {
			migration := migrationFixture()
			migration.Generation = 9
			operation := migrationOperationFixture(kind)
			job, err := builder.BuildMigration(migration, operation, migrationPlanFixture())
			if err != nil {
				t.Fatal(err)
			}
			bound, err := jobconfig.Read(job, migration.UID, operation.ID)
			if err != nil || bound.Generation != 9 || bound.Endpoint != builder.ResultEndpoint {
				t.Fatalf("projection=%#v, %v", bound, err)
			}
		})
	}
}
