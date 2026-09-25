package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// CachedUnresolvedView answers the unresolved gauges from the manager's own
// cache.
//
// It reports itself unsynchronized until something tells it otherwise, because
// a cache that has not caught up lists nothing and nothing would be published
// as a population of zero. The manager marks it synchronized by starting it as
// a Runnable, which controller-runtime does only after the cache has synced --
// so the honest answer comes from the machinery that actually knows, rather
// than from a timer or an assumption here.
type CachedUnresolvedView struct {
	reader client.Reader
	synced atomic.Bool
}

// NewCachedUnresolvedView reads through the cache the manager already fills.
func NewCachedUnresolvedView(reader client.Reader) *CachedUnresolvedView {
	return &CachedUnresolvedView{reader: reader}
}

// Start marks the view usable and holds until the manager stops. It satisfies
// controller-runtime's Runnable, which is started after the cache has synced.
func (v *CachedUnresolvedView) Start(ctx context.Context) error {
	v.synced.Store(true)
	<-ctx.Done()
	return nil
}

func (v *CachedUnresolvedView) Synced() bool { return v.synced.Load() }

func (v *CachedUnresolvedView) UnresolvedSchemas(ctx context.Context) ([]time.Time, error) {
	schemas := &operatorv1alpha1.PtahSchemaList{}
	if err := v.reader.List(ctx, schemas); err != nil {
		return nil, err
	}
	return UnresolvedSchemaRecords(schemas.Items), nil
}

func (v *CachedUnresolvedView) UnresolvedMigrations(ctx context.Context) ([]time.Time, error) {
	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := v.reader.List(ctx, migrations); err != nil {
		return nil, err
	}
	return UnresolvedMigrationRecords(migrations.Items), nil
}

// ListSchemas returns every PtahSchema the cache holds, for the state gauges.
func (v *CachedUnresolvedView) ListSchemas(ctx context.Context) ([]operatorv1alpha1.PtahSchema, error) {
	schemas := &operatorv1alpha1.PtahSchemaList{}
	if err := v.reader.List(ctx, schemas); err != nil {
		return nil, err
	}
	return schemas.Items, nil
}

// ListMigrations returns every PtahMigration the cache holds, for the state
// gauges.
func (v *CachedUnresolvedView) ListMigrations(ctx context.Context) ([]operatorv1alpha1.PtahMigration, error) {
	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := v.reader.List(ctx, migrations); err != nil {
		return nil, err
	}
	return migrations.Items, nil
}

// ListSchemaPlans lists every retained schema plan through the cache.
func (v *CachedUnresolvedView) ListSchemaPlans(ctx context.Context) ([]operatorv1alpha1.PtahSchemaPlan, error) {
	plans := &operatorv1alpha1.PtahSchemaPlanList{}
	if err := v.reader.List(ctx, plans); err != nil {
		return nil, err
	}
	return plans.Items, nil
}

// ListMigrationPlans lists every retained migration plan through the cache.
func (v *CachedUnresolvedView) ListMigrationPlans(ctx context.Context) ([]operatorv1alpha1.PtahMigrationPlan, error) {
	plans := &operatorv1alpha1.PtahMigrationPlanList{}
	if err := v.reader.List(ctx, plans); err != nil {
		return nil, err
	}
	return plans.Items, nil
}
