package telemetry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// snapshotReadTimeout leaves room in the qualification profile's four-second
// scrape timeout for encoding and transport. All four lists share this budget.
const snapshotReadTimeout = 3 * time.Second

type viewLifetime struct{ ctx context.Context }

// SnapshotCollector reads current API state once per leader scrape and shares
// it across the unresolved, resource-state and plan-store collectors. A warm
// informer cache can keep answering after its API access has failed; it cannot
// establish whether the state behind these alerts is still readable.
//
// The reader must be the manager's APIReader, not its cached client. Lists do
// not request resourceVersion=0: an arbitrarily stale API cache is not enough.
// The four kinds are not an atomic cross-kind snapshot. Any failed list rejects
// the entire reading, so no other gauge presents a partial census as complete.
// Plan chunks and ConfigMap payloads are never listed.
type SnapshotCollector struct {
	reader   client.Reader
	lifetime atomic.Pointer[viewLifetime]
	mutex    sync.Mutex
	view     stateSnapshot
	children []prometheus.Collector
}

// NewSnapshotCollector registers the state metrics as one collection. Add the
// returned collector to the manager as a leader-elected Runnable as well.
func NewSnapshotCollector(registerer prometheus.Registerer, reader client.Reader, now func() time.Time) *SnapshotCollector {
	c := &SnapshotCollector{reader: reader}
	c.children = []prometheus.Collector{
		NewUnresolvedCollector(nil, &c.view, now),
		NewStateCollector(nil, &c.view, now),
		NewPlanStoreCollector(nil, &c.view),
	}
	if registerer != nil {
		registerer.MustRegister(c)
	}
	return c
}

// NeedLeaderElection keeps a healthy follower from hiding a lost leader scrape.
func (*SnapshotCollector) NeedLeaderElection() bool { return true }

// Start permits reads only for this leadership lifetime. Cancellation also
// cancels an in-progress API read and prevents an old leader publishing counts.
func (c *SnapshotCollector) Start(ctx context.Context) error {
	lifetime := &viewLifetime{ctx: ctx}
	if !c.lifetime.CompareAndSwap(nil, lifetime) {
		return errors.New("the telemetry view is already running")
	}
	defer c.lifetime.CompareAndSwap(lifetime, nil)
	<-ctx.Done()
	return nil
}

func (c *SnapshotCollector) Describe(into chan<- *prometheus.Desc) {
	for _, child := range c.children {
		child.Describe(into)
	}
}

func (c *SnapshotCollector) Collect(into chan<- prometheus.Metric) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.view = stateSnapshot{}
	lifetime := c.lifetime.Load()
	if lifetime != nil && lifetime.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(lifetime.ctx, snapshotReadTimeout)
		c.view.active = true
		for _, list := range []client.ObjectList{&c.view.schemas, &c.view.migrations, &c.view.schemaPlans, &c.view.migrationPlans} {
			if c.view.err = c.reader.List(ctx, list); c.view.err != nil {
				break
			}
		}
		cancel()
		c.view.active = lifetime.ctx.Err() == nil
	}
	for _, child := range c.children {
		child.Collect(into)
	}
	// Do not retain full resources between scrapes.
	c.view = stateSnapshot{}
}

type stateSnapshot struct {
	active         bool
	err            error
	schemas        operatorv1alpha1.PtahSchemaList
	migrations     operatorv1alpha1.PtahMigrationList
	schemaPlans    operatorv1alpha1.PtahSchemaPlanList
	migrationPlans operatorv1alpha1.PtahMigrationPlanList
}

func (v *stateSnapshot) Synced() bool { return v.active }

func (v *stateSnapshot) UnresolvedSchemas(context.Context) ([]time.Time, error) {
	return UnresolvedSchemaRecords(v.schemas.Items), v.err
}

func (v *stateSnapshot) UnresolvedMigrations(context.Context) ([]time.Time, error) {
	return UnresolvedMigrationRecords(v.migrations.Items), v.err
}

func (v *stateSnapshot) ListSchemas(context.Context) ([]operatorv1alpha1.PtahSchema, error) {
	return v.schemas.Items, v.err
}

func (v *stateSnapshot) ListMigrations(context.Context) ([]operatorv1alpha1.PtahMigration, error) {
	return v.migrations.Items, v.err
}

func (v *stateSnapshot) ListSchemaPlans(context.Context) ([]operatorv1alpha1.PtahSchemaPlan, error) {
	return v.schemaPlans.Items, v.err
}

func (v *stateSnapshot) ListMigrationPlans(context.Context) ([]operatorv1alpha1.PtahMigrationPlan, error) {
	return v.migrationPlans.Items, v.err
}
