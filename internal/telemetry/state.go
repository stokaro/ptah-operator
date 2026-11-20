package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/blockedsince"
)

// The state gauges answer what the event counters cannot: how many resources
// need attention now, which ones have stopped being looked at, and which
// operation has been running for longer than anyone expected. Like the
// unresolved gauges they are rebuilt from durable status on every scrape, read
// through the same API reading, and published only while that view is
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
// ones overdue for their own next reconciliation, the age of the oldest
// block, the operations in flight and their age, and the lock releases still
// owed.
type StateCollector struct {
	view  StateView
	now   func() time.Time
	mutex sync.Mutex

	resources      *prometheus.Desc
	overdue        *prometheus.Desc
	overdueSeconds *prometheus.Desc
	blockedSeconds *prometheus.Desc
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
		blockedSeconds: prometheus.NewDesc(
			"ptah_operator_blocked_seconds",
			"Seconds the longest-blocked resource of a family, not suspended, has been blocked since its status.blockedSince. Absent where none is blocked.",
			[]string{"family"}, nil),
		active: prometheus.NewDesc(
			"ptah_operator_active_operations",
			"Resources with an operation in flight, by family and operation type. A failed attempt waiting for its retry is not in flight.",
			[]string{"family", "operation"}, nil),
		activeSeconds: prometheus.NewDesc(
			"ptah_operator_active_operation_seconds",
			"Seconds since the oldest operation of a type became eligible to run, excluding a declared retry delay, by family and operation type.",
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
	into <- c.blockedSeconds
	into <- c.active
	into <- c.activeSeconds
	into <- c.lockReleases
}

// resourceState is what the gauges need from one resource of either family.
type resourceState struct {
	phase string
	// failed is a resource whose last attempt failed. A schema keeps that
	// attempt's claim in status.activeOperation until its retry, so the claim
	// alone would count an ended operation as one still running and page as
	// stalled for the whole failureRetryInterval.
	failed        bool
	suspended     bool
	nextReconcile *time.Time
	// blockedSince is status.blockedSince: when the resource stopped at
	// Blocked for a person, kept through the reads it repeats meanwhile. It
	// is nil where the status classifies as settled.
	blockedSince       *time.Time
	operation          string
	operationStarted   time.Time
	operationNotBefore *time.Time
	owesLockRelease    bool
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
	var oldestBlock *time.Time
	for _, state := range states {
		phases[state.phase]++
		if state.owesLockRelease {
			lockReleases++
		}
		if state.operation != "" && !state.failed && (state.operationNotBefore == nil || !now.Before(*state.operationNotBefore)) {
			started := state.operationStarted
			if state.operationNotBefore != nil && state.operationNotBefore.After(started) {
				started = *state.operationNotBefore
			}
			operations[state.operation]++
			if oldest, seen := oldestOperation[state.operation]; !seen || started.Before(oldest) {
				oldestOperation[state.operation] = started
			}
		}
		if !state.suspended && state.nextReconcile != nil && now.After(*state.nextReconcile) {
			overdue++
			mostOverdue = max(mostOverdue, now.Sub(*state.nextReconcile))
		}
		if !state.suspended && state.blockedSince != nil && (oldestBlock == nil || state.blockedSince.Before(*oldestBlock)) {
			oldestBlock = state.blockedSince
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
	if oldestBlock != nil {
		into <- prometheus.MustNewConstMetric(c.blockedSeconds, prometheus.GaugeValue, max(now.Sub(*oldestBlock).Seconds(), 0), label)
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
			phase: phaseLabel(string(schema.Status.Phase)),
			// A proof whose target moved keeps its claim while it waits for a
			// person, as a failed read does until its retry, and fails each attempt.
			failed: schema.Status.Phase == operatorv1alpha1.PhaseFailed ||
				(schema.Status.Phase == operatorv1alpha1.PhaseBlocked && schema.Status.PendingObservation != nil),
			suspended:       schema.Spec.Suspend,
			owesLockRelease: schema.Status.PendingLockRelease != nil,
		}
		if next := schema.Status.NextReconciliationTime; next != nil {
			at := next.Time
			state.nextReconcile = &at
		}
		// A date counts only while the status still explains it: a manager
		// that does not know the field leaves it on a resource it moves on.
		if since := schema.Status.BlockedSince; since != nil && blockedsince.Schema(&schema).Counted() {
			at := since.Time
			state.blockedSince = &at
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
			failed:          migration.Status.Phase == operatorv1alpha1.MigrationPhaseFailed,
			suspended:       migration.Spec.Suspend,
			owesLockRelease: migration.Status.PendingLockRelease != nil,
		}
		if next := migration.Status.NextReconciliationTime; next != nil {
			at := next.Time
			state.nextReconcile = &at
		}
		if since := migration.Status.BlockedSince; since != nil && blockedsince.Migration(&migration).Counted() {
			at := since.Time
			state.blockedSince = &at
		}
		if active := migration.Status.ActiveOperation; active != nil {
			state.operation, state.operationStarted = string(active.Type), active.StartedAt.Time
			// Read-only retries retain their phase and claim during backoff.
			// They become eligible at RetryNotBefore; neither the wait nor
			// its elapsed time is an operation stalled in flight.
			if active.Type != operatorv1alpha1.MigrationOperationApply && active.RetryNotBefore != nil {
				at := active.RetryNotBefore.Time
				state.operationNotBefore = &at
			}
		}
		states = append(states, state)
	}
	return states
}
