package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// planView is a plan store whose contents a test controls.
type planView struct {
	synced     bool
	schemas    []operatorv1alpha1.PtahSchemaPlan
	migrations []operatorv1alpha1.PtahMigrationPlan
	err        error
}

func (v planView) Synced() bool { return v.synced }

func (v planView) ListSchemaPlans(context.Context) ([]operatorv1alpha1.PtahSchemaPlan, error) {
	return v.schemas, v.err
}

func (v planView) ListMigrationPlans(context.Context) ([]operatorv1alpha1.PtahMigrationPlan, error) {
	return v.migrations, v.err
}

func gatherPlans(t *testing.T, view telemetry.PlanStoreView) string {
	t.Helper()
	return gatherRegistry(t, func(registry *prometheus.Registry) {
		telemetry.NewPlanStoreCollector(registry, view)
	})
}

// A store read before the view caught up, or through a failed read, is not a
// store of zero plans.
func TestThePlanStoreGaugesAreAbsentUntilTheViewCanBeRead(t *testing.T) {
	t.Parallel()
	for name, view := range map[string]planView{
		"unsynchronized": {synced: false, schemas: []operatorv1alpha1.PtahSchemaPlan{{}}},
		"read failed":    {synced: true, err: errors.New("boom")},
	} {
		if out := gatherPlans(t, view); out != "" {
			t.Errorf("%s: the scrape published the store:\n%s", name, out)
		}
	}
}

// Schema plans hold their bytes in chunks, and spec.size is those bytes; a
// migration plan holds none and is counted without them.
func TestThePlanStoreGaugesCountEveryPlanAndTheSchemaPlanBytes(t *testing.T) {
	t.Parallel()
	out := gatherPlans(t, planView{
		synced: true,
		schemas: []operatorv1alpha1.PtahSchemaPlan{
			{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Size: 512 << 10}},
			{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Size: 3}},
		},
		migrations: []operatorv1alpha1.PtahMigrationPlan{{}, {}, {}},
	})
	for _, want := range []string{
		"ptah_operator_stored_plans{family=schema} 2",
		"ptah_operator_stored_plans{family=migration} 3",
		"ptah_operator_stored_plan_bytes 524291",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("the scrape does not carry %q:\n%s", want, out)
		}
	}
}

// An empty store is reported as empty, because the view could be read.
func TestAnEmptyPlanStoreReadsZero(t *testing.T) {
	t.Parallel()
	out := gatherPlans(t, planView{synced: true})
	for _, want := range []string{
		"ptah_operator_stored_plans{family=schema} 0",
		"ptah_operator_stored_plans{family=migration} 0",
		"ptah_operator_stored_plan_bytes 0",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("the scrape does not carry %q:\n%s", want, out)
		}
	}
}
