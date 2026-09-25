package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The state gauges answer what the event counters cannot: how many resources
// need attention now, which ones have stopped being looked at, and which
// operation has been running for longer than anyone expected. Like the
// unresolved gauges they are rebuilt from durable status on every scrape, read
// through the same cached view, and published only while that view is
// synchronized -- so ptah_operator_unresolved_view_synced is the guard an
// alert on these requires as well, and an empty scrape is never a fleet of
// zero.
//
// Every label is a bounded set: the two families, the phases each API
// declares, and the operation types each API declares. No resource name, UID
// or error text is a label.

// StateView lists the resources the state gauges are built from.
type StateView interface {
	Synced() bool
	ListSchemas(ctx context.Context) ([]operatorv1alpha1.PtahSchema, error)
	ListMigrations(ctx context.Context) ([]operatorv1alpha1.PtahMigration, error)
}

// StateCollector publishes the current population of resources by phase, the
// ones overdue for their own next reconciliation, the operations in flight
// and their age, and the lock releases still owed.
type StateCollector struct {
	view  StateView
	now   func() time.Time
	mutex sync.Mutex

	resources      *prometheus.Desc
	overdue        *prometheus.Desc
	overdueSeconds *prometheus.Desc
	active         *prometheus.Desc
	activeSeconds  *prometheus.Desc
	lockReleases   *prometheus.Desc
}

// NewStateCollector registers the state gauges on one registry.
func NewStateCollector(registerer prometheus.Registerer, view StateView, now func() time.Time) *StateCollector {
	if now == nil {
		now = time.Now
	}
	collector := &StateCollector{
		view: view,
		now:  now,
		resources: prometheus.NewDesc(
			"ptah_operator_resources",
			"Resources by family and by the phase their status reports. Absent while the view is not synchronized.",
			[]string{"family", "phase"}, nil),
		overdue: prometheus.NewDesc(
			"ptah_operator_overdue_resources",
			"Resources, not suspended, whose status.nextReconciliationTime has passed, by family.",
			[]string{"family"}, nil),
		overdueSeconds: prometheus.NewDesc(
			"ptah_operator_overdue_seconds",
			"Seconds the most overdue resource of a family is past its own status.nextReconciliationTime. Absent where none is overdue.",
			[]string{"family"}, nil),
		active: prometheus.NewDesc(
			"ptah_operator_active_operations",
			"Resources with an operation in flight, by family and operation type.",
			[]string{"family", "operation"}, nil),
		activeSeconds: prometheus.NewDesc(
			"ptah_operator_active_operation_seconds",
			"Seconds since the oldest operation of a type started, by family and operation type.",
			[]string{"family", "operation"}, nil),
		lockReleases: prometheus.NewDesc(
			"ptah_operator_pending_lock_releases",
			"Resources owing the release of a database-realm Lease they claimed, by family.",
			[]string{"family"}, nil),
	}
	if registerer != nil {
		registerer.MustRegister(collector)
	}
	return collector
}

func (c *StateCollector) Describe(into chan<- *prometheus.Desc) {
	into <- c.resources
	into <- c.overdue
	into <- c.overdueSeconds
	into <- c.active
	into <- c.activeSeconds
	into <- c.lockReleases
}

// resourceState is what the gauges need from one resource of either family.
type resourceState struct {
	phase            string
	suspended        bool
	nextReconcile    *time.Time
	operation        string
	operationStarted time.Time
	owesLockRelease  bool
}

// Collect rebuilds the gauges. A view that has not synchronized, or a read
// that fails, publishes nothing: the unresolved collector reports that state
// and counts the failure, and the two read the same view.
func (c *StateCollector) Collect(into chan<- prometheus.Metric) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.view == nil || !c.view.Synced() {
		return
	}
	ctx := context.Background()
	schemas, schemaErr := c.view.ListSchemas(ctx)
	migrations, migrationErr := c.view.ListMigrations(ctx)
	if schemaErr != nil || migrationErr != nil {
		return
	}
	now := c.now()
	c.emit(into, now, FamilySchema, schemaStates(schemas))
	c.emit(into, now, FamilyMigration, migrationStates(migrations))
}

func (c *StateCollector) emit(into chan<- prometheus.Metric, now time.Time, family ResourceFamily, states []resourceState) {
	phases := map[string]int{}
	operations := map[string]int{}
	oldestOperation := map[string]time.Time{}
	overdue, lockReleases := 0, 0
	var mostOverdue time.Duration
	for _, state := range states {
		phases[state.phase]++
		if state.owesLockRelease {
			lockReleases++
		}
		if state.operation != "" {
			operations[state.operation]++
			if oldest, seen := oldestOperation[state.operation]; !seen || state.operationStarted.Before(oldest) {
				oldestOperation[state.operation] = state.operationStarted
			}
		}
		if !state.suspended && state.nextReconcile != nil && now.After(*state.nextReconcile) {
			overdue++
			mostOverdue = max(mostOverdue, now.Sub(*state.nextReconcile))
		}
	}
	label := string(family)
	for phase, count := range phases {
		into <- prometheus.MustNewConstMetric(c.resources, prometheus.GaugeValue, float64(count), label, phase)
	}
	into <- prometheus.MustNewConstMetric(c.overdue, prometheus.GaugeValue, float64(overdue), label)
	if overdue > 0 {
		into <- prometheus.MustNewConstMetric(c.overdueSeconds, prometheus.GaugeValue, mostOverdue.Seconds(), label)
	}
	for operation, count := range operations {
		into <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(count), label, operation)
		age := now.Sub(oldestOperation[operation]).Seconds()
		into <- prometheus.MustNewConstMetric(c.activeSeconds, prometheus.GaugeValue, max(age, 0), label, operation)
	}
	into <- prometheus.MustNewConstMetric(c.lockReleases, prometheus.GaugeValue, float64(lockReleases), label)
}

// phaseLabel names a resource no reconciliation has written a phase for yet,
// so the label set stays the API's phases plus one.
func phaseLabel(phase string) string {
	if phase == "" {
		return "Unset"
	}
	return phase
}

// schemaStates reduces schemas to what the state gauges read.
func schemaStates(schemas []operatorv1alpha1.PtahSchema) []resourceState {
	states := make([]resourceState, 0, len(schemas))
	for _, schema := range schemas {
		state := resourceState{
			phase:           phaseLabel(string(schema.Status.Phase)),
			suspended:       schema.Spec.Suspend,
			owesLockRelease: schema.Status.PendingLockRelease != nil,
		}
		if next := schema.Status.NextReconciliationTime; next != nil {
			at := next.Time
			state.nextReconcile = &at
		}
		if active := schema.Status.ActiveOperation; active != nil {
			state.operation, state.operationStarted = string(active.Type), active.StartedAt.Time
		}
		states = append(states, state)
	}
	return states
}

// migrationStates reduces migrations to what the state gauges read.
func migrationStates(migrations []operatorv1alpha1.PtahMigration) []resourceState {
	states := make([]resourceState, 0, len(migrations))
	for _, migration := range migrations {
		state := resourceState{
			phase:           phaseLabel(string(migration.Status.Phase)),
			suspended:       migration.Spec.Suspend,
			owesLockRelease: migration.Status.PendingLockRelease != nil,
		}
		if next := migration.Status.NextReconciliationTime; next != nil {
			at := next.Time
			state.nextReconcile = &at
		}
		if active := migration.Status.ActiveOperation; active != nil {
			state.operation, state.operationStarted = string(active.Type), active.StartedAt.Time
		}
		states = append(states, state)
	}
	return states
}
