package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// An event counter cannot answer the question an operator actually has after a
// mutation went unaccounted for: is one still standing, and for how long. A
// counter is incremented once, by a process that may since have restarted, and
// says nothing about the population that exists now.
//
// So these are gauges, and they are rebuilt from durable status on every
// scrape rather than kept in memory. That is what makes them survive a restart
// and survive an unrelated refusal rewriting the resource's conditions: they
// read the field the record lives in, which is the same field the controller
// refuses on.
//
// The empty view is the trap. A cache that has not synchronized lists nothing,
// and nothing reads exactly like "no mutation is unresolved" -- the most
// dangerous possible false negative for this signal. So the gauges are not
// emitted at all until the view reports itself synchronized, and a separate
// gauge says which of those two states the scrape is in. An alert on the
// unresolved count has to require the view gauge as well, and the
// documentation says so.

// UnresolvedView lists the durable records the gauges are built from. It is an
// interface so the manager can pass its cached client and a test can pass a
// fixture, and so the synchronized answer comes from whoever actually knows.
type UnresolvedView interface {
	// Synced reports whether the view has caught up. A false answer means the
	// counts below are not evidence of anything.
	Synced() bool
	// UnresolvedSchemas returns, for each PtahSchema owing proof of an Apply
	// nobody accounted for, the instant the operator could first have settled
	// it. A zero time means the resource is counted with no age.
	UnresolvedSchemas(ctx context.Context) ([]time.Time, error)
	// UnresolvedMigrations returns the same for each PtahMigration carrying a
	// run nobody accounted for.
	UnresolvedMigrations(ctx context.Context) ([]time.Time, error)
}

// UnresolvedCollector publishes the current population and age of unresolved
// mutating work.
type UnresolvedCollector struct {
	view UnresolvedView
	now  func() time.Time

	// collectFailures counts scrapes that could not read the view, so a
	// silently failing collector is visible rather than looking like zero.
	collectFailures prometheus.Counter
	mutex           sync.Mutex

	synced    *prometheus.Desc
	count     *prometheus.Desc
	oldestAge *prometheus.Desc
}

// NewUnresolvedCollector registers the gauges on one registry.
func NewUnresolvedCollector(registerer prometheus.Registerer, view UnresolvedView, now func() time.Time) *UnresolvedCollector {
	if now == nil {
		now = time.Now
	}
	collector := &UnresolvedCollector{
		view: view,
		now:  now,
		collectFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "unresolved_view_read_failures_total",
			Help:      "Scrapes that could not read the durable state the unresolved gauges are built from.",
		}),
		synced: prometheus.NewDesc(
			"ptah_operator_unresolved_view_synced",
			"1 when the view backing the unresolved gauges has synchronized. While 0, the gauges are absent and their absence is not evidence.",
			nil, nil),
		count: prometheus.NewDesc(
			"ptah_operator_unresolved_attempts",
			"Resources carrying a durable record of a mutation nobody accounted for, by family.",
			[]string{"family"}, nil),
		oldestAge: prometheus.NewDesc(
			"ptah_operator_unresolved_owed_seconds",
			"Seconds since the operator could first have settled the oldest unresolved record, by family. Absent where a family has none, and absent for a record with no such instant recorded.",
			[]string{"family"}, nil),
	}
	if registerer != nil {
		registerer.MustRegister(collector)
	}
	return collector
}

func (c *UnresolvedCollector) Describe(into chan<- *prometheus.Desc) {
	c.collectFailures.Describe(into)
	into <- c.synced
	into <- c.count
	into <- c.oldestAge
}

// Collect rebuilds the gauges from durable state. The failure counter is
// emitted from here, after the read, so a scrape that failed reports its own
// failure rather than the count from before it.
func (c *UnresolvedCollector) Collect(into chan<- prometheus.Metric) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	defer func() { into <- c.collectFailures }()

	if c.view == nil || !c.view.Synced() {
		into <- prometheus.MustNewConstMetric(c.synced, prometheus.GaugeValue, 0)
		return
	}

	ctx := context.Background()
	schemas, schemaErr := c.view.UnresolvedSchemas(ctx)
	migrations, migrationErr := c.view.UnresolvedMigrations(ctx)
	if schemaErr != nil || migrationErr != nil {
		// A read that failed is not a population of zero. Report the view as
		// unsynchronized for this scrape and count the failure.
		c.collectFailures.Inc()
		into <- prometheus.MustNewConstMetric(c.synced, prometheus.GaugeValue, 0)
		return
	}

	into <- prometheus.MustNewConstMetric(c.synced, prometheus.GaugeValue, 1)
	c.emit(into, FamilySchema, schemas)
	c.emit(into, FamilyMigration, migrations)
}

// emit publishes one family's count, and its oldest age where it has one.
func (c *UnresolvedCollector) emit(into chan<- prometheus.Metric, family ResourceFamily, recorded []time.Time) {
	into <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(len(recorded)), string(family))
	oldest, found := oldestRecord(recorded)
	if !found {
		// No age rather than an age of zero: a zero would read as "one was
		// recorded this instant", which is the opposite of none.
		return
	}
	age := c.now().Sub(oldest).Seconds()
	if age < 0 {
		age = 0
	}
	into <- prometheus.MustNewConstMetric(c.oldestAge, prometheus.GaugeValue, age, string(family))
}

func oldestRecord(recorded []time.Time) (time.Time, bool) {
	var oldest time.Time
	found := false
	for _, at := range recorded {
		if at.IsZero() {
			continue
		}
		if !found || at.Before(oldest) {
			oldest, found = at, true
		}
	}
	return oldest, found
}

// UnresolvedSchemaRecords returns, per schema owing proof, the instant the
// operator could first have settled it: a pending observation is settleable
// once its immutable horizon has passed, because until then a mutating Pod may
// still be running and no reading would be evidence. A migration record is
// settleable as soon as it exists, so the two families report the same
// quantity from different fields, which is why this lives here and not in the
// caller.
func UnresolvedSchemaRecords(schemas []operatorv1alpha1.PtahSchema) []time.Time {
	var recorded []time.Time
	for _, schema := range schemas {
		pending := schema.Status.PendingObservation
		if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
			continue
		}
		if pending.ObserveAfter == nil {
			// Counted, with no age: a record whose horizon was never written
			// is still a record, and inventing an instant for it would report
			// an age nothing measured.
			recorded = append(recorded, time.Time{})
			continue
		}
		recorded = append(recorded, pending.ObserveAfter.Time)
	}
	return recorded
}

// UnresolvedMigrationRecords returns when each migration recorded a run nobody
// accounted for.
func UnresolvedMigrationRecords(migrations []operatorv1alpha1.PtahMigration) []time.Time {
	var recorded []time.Time
	for _, migration := range migrations {
		unresolved := migration.Status.UnresolvedRun
		if unresolved == nil {
			continue
		}
		recorded = append(recorded, unresolved.RecordedAt.Time)
	}
	return recorded
}
