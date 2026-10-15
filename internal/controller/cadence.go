package controller

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// spec.interval is the cadence of refresh cycles: the time from the start of
// one cycle to the start of the next. A cycle starts with a Resolve claim, and
// that claim persists the next cycle's deadline. The read-only steps after it
// carry that deadline and the end of the cycle keeps it, so the four Jobs of a
// schema cycle or the three of a migration cycle no longer stretch the period.
//
// Steps within a cycle are driven by the phase, not by this deadline. Work
// that did not start with a Resolve, such as the proof after an Apply, and a
// cycle that ran longer than its interval wait one interval from their end,
// as before; neither turns into back-to-back refreshes.
//
// Resources created together would otherwise refresh together for as long as
// they live. The 0.2.3 soak on MySQL measured it: the minutes alternated
// between about 5,800 and 2,700 manager requests, the whole fleet refreshing
// in the first, and a change batch that landed on one of them passed the API
// budget. So the schemas and migrations that share an interval take evenly
// spaced slots in it, in the order of their UIDs, and a Resolve that started
// well past its slot moves the next one later onto it. The next refresh is
// never earlier than one interval after the claim, and it moves by at most half
// an interval, so no cycle gets shorter and the oldest observation stays
// bounded. A hashed slot per resource would not do: twenty random offsets put
// thirteen or more in some half interval as often as not.

// refreshSlot is the offset within its interval that this resource's refresh
// starts at, from the manager's cache. A resource alone in its interval, or one
// whose fleet the cache cannot list, keeps the plain cadence (ok false).
func refreshSlot(ctx context.Context, reader client.Reader, uid types.UID, period time.Duration) (time.Duration, bool) {
	if reader == nil || period <= 0 || uid == "" {
		return 0, false
	}
	schemas := &operatorv1alpha1.PtahSchemaList{}
	migrations := &operatorv1alpha1.PtahMigrationList{}
	if reader.List(ctx, schemas) != nil || reader.List(ctx, migrations) != nil {
		return 0, false
	}
	fleet := []types.UID{uid}
	for i := range schemas.Items {
		if schema := &schemas.Items[i]; schema.UID != uid && interval(schema) == period {
			fleet = append(fleet, schema.UID)
		}
	}
	for i := range migrations.Items {
		if migration := &migrations.Items[i]; migration.UID != uid && migrationInterval(migration) == period {
			fleet = append(fleet, migration.UID)
		}
	}
	if len(fleet) < 2 {
		return 0, false
	}
	slices.Sort(fleet)
	return evenSlot(slices.Index(fleet, uid), len(fleet), period), true
}

// evenSlot is the start of the rank-th of n equal parts of the interval.
func evenSlot(rank, n int, interval time.Duration) time.Duration {
	return time.Duration(int64(interval) / int64(n) * int64(rank))
}

// resolveDeadline is the next refresh after a Resolve claimed at claimedAt.
// Dispatch latency starts every claim a little after its deadline, so a claim
// up to a tenth of the interval past its slot keeps the plain interval rather
// than lengthening every period. One further past moves later onto the slot,
// half an interval at most; a second cycle finishes a longer move.
func resolveDeadline(claimedAt time.Time, interval, slot time.Duration) time.Time {
	next := claimedAt.Add(interval)
	if interval <= 0 {
		return next
	}
	past := time.Duration((next.UnixNano() - int64(slot)) % int64(interval))
	if past < 0 {
		past += interval
	}
	if past <= interval/10 {
		return next
	}
	return next.Add(min(interval-past, interval/2))
}

// claimDeadline is the deadline a claim persists. A Resolve starts a cycle and
// sets the next one at least an interval after it starts: on the resource's
// slot when it has one (resolveDeadline), exactly an interval later when not.
// Another read-only step keeps a later deadline its cycle carries; a mutating
// claim clears it.
func claimDeadline(current *metav1.Time, resolve, mutating bool, claimedAt time.Time, interval, slot time.Duration, slotted bool) *metav1.Time {
	switch {
	case resolve && slotted:
		next := metav1.NewTime(resolveDeadline(claimedAt, interval, slot))
		return &next
	case resolve:
		next := metav1.NewTime(claimedAt.Add(interval))
		return &next
	case !mutating && current != nil && current.After(claimedAt):
		return current
	default:
		return nil
	}
}

// handoffDeadline marks the next step of a cycle due now, unless the cycle
// carries a later deadline for the next refresh.
func handoffDeadline(current *metav1.Time, now metav1.Time) *metav1.Time {
	if current != nil && current.After(now.Time) {
		return current
	}
	return &now
}

// cycleDeadline is when the next refresh is due once a cycle ends: the deadline
// its Resolve set, or one interval from now when that has passed or none was set.
func cycleDeadline(current *metav1.Time, now time.Time, interval time.Duration) metav1.Time {
	if current != nil && current.After(now) {
		return *current
	}
	return metav1.NewTime(now.Add(interval))
}
