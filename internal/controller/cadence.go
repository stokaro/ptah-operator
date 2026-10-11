package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// claimDeadline is the deadline a claim persists. A Resolve starts a cycle and
// sets the next one an interval after it starts. Another read-only step keeps a
// later deadline its cycle carries; a mutating claim clears it.
func claimDeadline(current *metav1.Time, resolve, mutating bool, claimedAt time.Time, interval time.Duration) *metav1.Time {
	switch {
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
