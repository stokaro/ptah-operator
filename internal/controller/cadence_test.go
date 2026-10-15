package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The 0.2.0 soak measured a 2m interval refreshing every ~3.2 minutes: the
// next refresh was set an interval after the previous cycle ended, so the four
// Jobs of a cycle stretched every period. These rows hold the cadence to the
// start of the cycle and hold every fallback to the old wait.
func TestRefreshCadenceCountsFromTheStartOfTheCycle(t *testing.T) {
	t.Parallel()
	interval := 2 * time.Minute
	// Claimed on the resource's own slot, so the cadence is the plain interval.
	aligned := 37 * time.Second
	start := time.Unix(0, 0).Add(aligned).Add(time.Duration(29610000) * interval).UTC()
	if next := claimDeadline(nil, true, false, start.Add(13*time.Second), interval, 0, false); !next.Time.Equal(start.Add(13*time.Second + interval)) {
		t.Fatalf("Resolve claim without a slot set %v, want exactly one interval later", next)
	}
	carried := metav1.NewTime(start.Add(interval))

	// A Resolve starts the cycle and sets the next one from its own start,
	// whatever deadline was there before.
	stale := metav1.NewTime(start.Add(-time.Hour))
	for _, current := range []*metav1.Time{nil, &stale, &carried} {
		next := claimDeadline(current, true, false, start, interval, aligned, true)
		if next == nil || !next.Time.Equal(start.Add(interval)) {
			t.Fatalf("Resolve claim over %v set %v, want %v", current, next, start.Add(interval))
		}
	}
	// The read-only steps after it carry the deadline.
	later := start.Add(40 * time.Second)
	if next := claimDeadline(&carried, false, false, later, interval, 0, false); next == nil || !next.Time.Equal(carried.Time) {
		t.Fatalf("read-only claim inside the cycle set %v, want the carried %v", next, carried.Time)
	}
	// A mutating claim, or a read-only one with no later deadline, clears it.
	if next := claimDeadline(&carried, false, true, later, interval, 0, false); next != nil {
		t.Fatalf("mutating claim kept deadline %v", next)
	}
	if next := claimDeadline(&stale, false, false, later, interval, 0, false); next != nil {
		t.Fatalf("read-only claim kept a passed deadline %v", next)
	}
	if next := claimDeadline(nil, false, false, later, interval, 0, false); next != nil {
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
// its claim owes: a Resolve the next refresh one interval after it started, or
// on its slot up to half an interval later when it shares the interval with
// others; any other claim none.
func schemaClaimDeadlineHeld(schema *operatorv1alpha1.PtahSchema) bool {
	next, active := schema.Status.NextReconciliationTime, schema.Status.ActiveOperation
	if active != nil && active.Type == operatorv1alpha1.OperationResolve {
		plain := active.StartedAt.Add(interval(schema))
		return next != nil && !next.Time.Before(plain) && !next.Time.After(plain.Add(interval(schema)/2))
	}
	return next == nil
}

// A fleet created in one burst stops refreshing in one burst. Twenty resources
// claim their first Resolve within a second of each other and then follow
// their deadlines with up to two seconds of dispatch latency. From the third
// cycle on, no 60-second window holds more than half the fleet's claims plus
// the drift a slot tolerates, no period is shorter than the interval, and none
// is longer than one and a half intervals plus the latency. The same fleet
// without slots, the cadence before them, keeps all twenty in one window.
func TestRefreshSlotsSpreadAFleetCreatedTogether(t *testing.T) {
	t.Parallel()
	interval := 2 * time.Minute
	start := time.Date(2026, time.October, 15, 3, 15, 0, 0, time.UTC)
	simulate := func(slotted bool) (worst int, periods []time.Duration) {
		claims := make([][]time.Time, 20)
		for i := range claims {
			claimedAt := start.Add(time.Duration(i) * 50 * time.Millisecond)
			for cycle := 0; cycle < 12; cycle++ {
				claims[i] = append(claims[i], claimedAt)
				latency := time.Duration((i*37+cycle*101)%2000) * time.Millisecond
				next := claimDeadline(nil, true, false, claimedAt, interval, evenSlot(i, 20, interval), slotted)
				claimedAt = next.Time.Add(latency)
			}
		}
		var late []time.Time
		for _, resource := range claims {
			late = append(late, resource[3:]...)
			for k := 1; k < len(resource); k++ {
				periods = append(periods, resource[k].Sub(resource[k-1]))
			}
		}
		slices.SortFunc(late, time.Time.Compare)
		for first := range late {
			count := 0
			for _, claim := range late[first:] {
				if claim.Sub(late[first]) >= time.Minute {
					break
				}
				count++
			}
			worst = max(worst, count)
		}
		return worst, periods
	}
	worst, periods := simulate(true)
	if worst > 12 {
		t.Fatalf("slotted fleet put %d claims in one 60-second window, want at most 12 of 20", worst)
	}
	for _, period := range periods {
		if period < interval || period > interval+interval/2+2*time.Second {
			t.Fatalf("slotted period %s outside [%s, %s]", period, interval, interval+interval/2+2*time.Second)
		}
	}
	if unslotted, _ := simulate(false); unslotted <= 12 {
		t.Fatalf("the cadence before slots put only %d claims in one window; the check would not catch a herd", unslotted)
	}
}

// resolveDeadline keeps the plain interval near the slot and otherwise moves
// later onto it, never by more than half an interval and never earlier.
func TestResolveDeadlineMovesOnlyLaterOntoTheSlot(t *testing.T) {
	t.Parallel()
	interval := 2 * time.Minute
	slot := 37 * time.Second
	onSlot := time.Unix(0, 0).Add(slot).Add(29610000 * interval).UTC()
	for _, row := range []struct {
		name    string
		claimed time.Time
		want    time.Time
	}{
		{"on the slot", onSlot, onSlot.Add(interval)},
		{"within a tenth past it", onSlot.Add(12 * time.Second), onSlot.Add(12*time.Second + interval)},
		{"well past it", onSlot.Add(80 * time.Second), onSlot.Add(2 * interval)},
		{"just past the tenth", onSlot.Add(13 * time.Second), onSlot.Add(13*time.Second + interval + interval/2)},
	} {
		got := resolveDeadline(row.claimed, interval, slot)
		if !got.Equal(row.want) {
			t.Fatalf("%s: resolveDeadline = %s after the claim, want %s", row.name, got.Sub(row.claimed), row.want.Sub(row.claimed))
		}
		if got.Sub(row.claimed) < interval || got.Sub(row.claimed) > interval+interval/2 {
			t.Fatalf("%s: deadline %s after the claim, outside [interval, 1.5 intervals]", row.name, got.Sub(row.claimed))
		}
	}
	if got := resolveDeadline(onSlot, 0, slot); !got.Equal(onSlot) {
		t.Fatalf("zero interval moved the deadline to %s", got)
	}
}

// Slots come from the fleet in the manager's cache: the schemas and migrations
// that share an interval, in UID order, evenly spaced. Another interval is not
// part of the fleet, and a resource alone in its interval has no slot.
func TestRefreshSlotsAreRanksInTheFleetSharingAnInterval(t *testing.T) {
	t.Parallel()
	every := 2 * time.Minute
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	schema := func(uid string, period time.Duration) *operatorv1alpha1.PtahSchema {
		return &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "s-" + uid, Namespace: "a", UID: types.UID(uid)},
			Spec: operatorv1alpha1.PtahSchemaSpec{Interval: metav1.Duration{Duration: period}}}
	}
	migration := func(uid string, period time.Duration) *operatorv1alpha1.PtahMigration {
		return &operatorv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: "m-" + uid, Namespace: "b", UID: types.UID(uid)},
			Spec: operatorv1alpha1.PtahMigrationSpec{Interval: metav1.Duration{Duration: period}}}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		schema("u3", every), schema("u1", every), migration("u4", every), migration("u0", every),
		schema("u9", time.Hour)).Build()
	for uid, want := range map[string]time.Duration{"u0": 0, "u1": 30 * time.Second, "u3": time.Minute, "u4": 90 * time.Second} {
		slot, ok := refreshSlot(context.Background(), reader, types.UID(uid), every)
		if !ok || slot != want {
			t.Fatalf("%s slot = %s, %v; want %s", uid, slot, ok, want)
		}
	}
	if _, ok := refreshSlot(context.Background(), reader, "u9", time.Hour); ok {
		t.Fatal("a resource alone in its interval got a slot")
	}
	if _, ok := refreshSlot(context.Background(), nil, "u1", every); ok {
		t.Fatal("a slot without a cache to read the fleet from")
	}
}
