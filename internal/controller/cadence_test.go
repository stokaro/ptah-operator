package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The 0.2.0 soak measured a 2m interval refreshing every ~3.2 minutes: the
// next refresh was set an interval after the previous cycle ended, so the four
// Jobs of a cycle stretched every period. These rows hold the cadence to the
// start of the cycle and hold every fallback to the old wait.
func TestRefreshCadenceCountsFromTheStartOfTheCycle(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.October, 11, 7, 0, 0, 0, time.UTC)
	interval := 2 * time.Minute
	carried := metav1.NewTime(start.Add(interval))

	// A Resolve starts the cycle and sets the next one from its own start,
	// whatever deadline was there before.
	stale := metav1.NewTime(start.Add(-time.Hour))
	for _, current := range []*metav1.Time{nil, &stale, &carried} {
		next := claimDeadline(current, true, false, start, interval)
		if next == nil || !next.Time.Equal(start.Add(interval)) {
			t.Fatalf("Resolve claim over %v set %v, want %v", current, next, start.Add(interval))
		}
	}
	// The read-only steps after it carry the deadline.
	later := start.Add(40 * time.Second)
	if next := claimDeadline(&carried, false, false, later, interval); next == nil || !next.Time.Equal(carried.Time) {
		t.Fatalf("read-only claim inside the cycle set %v, want the carried %v", next, carried.Time)
	}
	// A mutating claim, or a read-only one with no later deadline, clears it.
	if next := claimDeadline(&carried, false, true, later, interval); next != nil {
		t.Fatalf("mutating claim kept deadline %v", next)
	}
	if next := claimDeadline(&stale, false, false, later, interval); next != nil {
		t.Fatalf("read-only claim kept a passed deadline %v", next)
	}
	if next := claimDeadline(nil, false, false, later, interval); next != nil {
		t.Fatalf("read-only claim invented deadline %v", next)
	}

	// A handoff inside the cycle keeps the carried deadline, and marks the
	// next step due now when there is none or it has passed.
	now := metav1.NewTime(later)
	if next := handoffDeadline(&carried, now); !next.Time.Equal(carried.Time) {
		t.Fatalf("handoff replaced the carried deadline with %v", next)
	}
	for _, current := range []*metav1.Time{nil, &stale} {
		if next := handoffDeadline(current, now); !next.Time.Equal(later) {
			t.Fatalf("handoff over %v set %v, want now", current, next)
		}
	}

	// The cycle ends 70 seconds in: the next refresh is due at start+interval,
	// 50 seconds later, not a full interval after the end.
	end := start.Add(70 * time.Second)
	if next := cycleDeadline(&carried, end, interval); !next.Time.Equal(start.Add(interval)) {
		t.Fatalf("cycle end set %v, want %v", next.Time, start.Add(interval))
	}
	// A cycle that outran its interval, or work no Resolve started, waits one
	// interval from its end rather than refreshing back to back.
	overran := start.Add(3 * time.Minute)
	if next := cycleDeadline(&carried, overran, interval); !next.Time.Equal(overran.Add(interval)) {
		t.Fatalf("overrun cycle set %v, want %v", next.Time, overran.Add(interval))
	}
	if next := cycleDeadline(nil, end, interval); !next.Time.Equal(end.Add(interval)) {
		t.Fatalf("cycle without a carried deadline set %v, want %v", next.Time, end.Add(interval))
	}
}

// schemaClaimDeadlineHeld says whether a claimed schema persists the deadline
// its claim owes: a Resolve the next refresh one interval after it started,
// any other claim none.
func schemaClaimDeadlineHeld(schema *operatorv1alpha1.PtahSchema) bool {
	next, active := schema.Status.NextReconciliationTime, schema.Status.ActiveOperation
	if active != nil && active.Type == operatorv1alpha1.OperationResolve {
		return next != nil && next.Time.Equal(active.StartedAt.Add(interval(schema)))
	}
	return next == nil
}
