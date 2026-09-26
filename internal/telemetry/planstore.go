package telemetry

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The plan store gauges answer whether retained plans are consuming the budget
// an installation set for them. The operator prunes nothing on its own -- how
// much history to keep follows from audit requirements and an etcd budget, and
// neither is this project's to choose -- so the store only grows, and the
// deployment that owns the budget needs to see it grow.
//
// A schema plan's bytes -- its SQL and the document around it -- live in
// immutable chunk ConfigMaps, and spec.size is the exact byte count the chunks
// hold, so the bytes are read from the plans rather than by listing ConfigMaps.
// A migration plan names migrations the artifact carries and stores no chunk of
// its own, so it is counted and adds no bytes.
//
// Like the state gauges, these are rebuilt on every scrape from the cached
// view and published only while that view is synchronized.

// PlanStoreView lists the plans the store gauges are built from.
type PlanStoreView interface {
	Synced() bool
	ListSchemaPlans(ctx context.Context) ([]operatorv1alpha1.PtahSchemaPlan, error)
	ListMigrationPlans(ctx context.Context) ([]operatorv1alpha1.PtahMigrationPlan, error)
}

// PlanStoreCollector publishes how many plans each family retains and how many
// bytes the schema plans hold in their chunks.
type PlanStoreCollector struct {
	view  PlanStoreView
	mutex sync.Mutex

	plans      *prometheus.Desc
	chunkBytes *prometheus.Desc
}

// NewPlanStoreCollector registers the plan store gauges on one registry.
func NewPlanStoreCollector(registerer prometheus.Registerer, view PlanStoreView) *PlanStoreCollector {
	collector := &PlanStoreCollector{
		view: view,
		plans: prometheus.NewDesc(
			"ptah_operator_stored_plans",
			"Plans retained in the cluster, by family. Absent while the view is not synchronized.",
			[]string{"family"}, nil),
		chunkBytes: prometheus.NewDesc(
			"ptah_operator_stored_plan_bytes",
			"Bytes the retained schema plans hold in their chunk ConfigMaps, from each plan's spec.size. Absent while the view is not synchronized.",
			nil, nil),
	}
	if registerer != nil {
		registerer.MustRegister(collector)
	}
	return collector
}

func (c *PlanStoreCollector) Describe(into chan<- *prometheus.Desc) {
	into <- c.plans
	into <- c.chunkBytes
}

// Collect rebuilds the gauges. A view that has not synchronized, or a read that
// fails, publishes nothing rather than a store of zero.
func (c *PlanStoreCollector) Collect(into chan<- prometheus.Metric) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.view == nil || !c.view.Synced() {
		return
	}
	ctx := context.Background()
	schemaPlans, schemaErr := c.view.ListSchemaPlans(ctx)
	migrationPlans, migrationErr := c.view.ListMigrationPlans(ctx)
	if schemaErr != nil || migrationErr != nil {
		return
	}
	var chunkBytes int64
	for index := range schemaPlans {
		chunkBytes += schemaPlans[index].Spec.Size
	}
	into <- prometheus.MustNewConstMetric(c.plans, prometheus.GaugeValue, float64(len(schemaPlans)), string(FamilySchema))
	into <- prometheus.MustNewConstMetric(c.plans, prometheus.GaugeValue, float64(len(migrationPlans)), string(FamilyMigration))
	into <- prometheus.MustNewConstMetric(c.chunkBytes, prometheus.GaugeValue, float64(chunkBytes))
}
