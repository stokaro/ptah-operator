package mutationlifecycle

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// A mutating claim is marked before its one create, and the create lands a
// moment later. A manager that reads the claim between the two finds the mark,
// no recorded Job UID and no Job, which is also the state a create that never
// happened leaves. While one manager acts, the state can only mean the second.
// After a node pause the old leader keeps reconciling until its lease runs
// out, so for a while two managers act, and one of them read the other's mark
// before that create landed (#869): it retired the claim as an Apply nobody
// accounted for, and the approval was spent.
//
// The mark is a bool. A time written beside it by one manager and compared on
// another's clock would carry the skew between the two clocks into the
// verdict. So the manager that finds the Job missing measures on its own
// clock: it waits from the first time it saw that state for that claim, for as
// long as the dispatching manager can still be between its mark and its
// create. After that the Job is lost.
//
// A claim with a recorded Job UID is not this case. The UID is written only
// after the create returned, so its Job certainly landed, and a missing one is
// lost at once.

// MissingJobGrace is how long this process watches a marked mutating claim
// with no recorded Job UID find its Job missing before it calls the Job lost.
//
// What keeps a create from landing after the grace is below, and a change to
// any of it has to answer for this bound:
//
//   - The dispatching manager runs its create on the context Driver.Dispatch
//     ends at DispatchCriticalSection. That outlasts the chart's Job-write
//     webhook timeout, so the API server normally answers first. A client
//     that gives up there anyway cancels the request on the API server.
//   - A refused create withdraws the mark on a context of its own, which ends
//     by DispatchStopBound. The grace runs from there, so a reader does not
//     retire a claim whose mark is about to be taken back, which would spend
//     an approval no Job used.
//   - createLandingMargin covers a cancellation arriving and a storage write
//     already in flight.
//   - The Job-write webhook in internal/controllerwrite reads the claim
//     uncached and refuses a create once the claim is retired or its owner is
//     deleting. A create whose admission runs after the claim was retired is
//     refused there.
//
// A dispatcher that cannot cancel, because it is frozen or partitioned,
// leaves the API server to finish the request on its own bounds. That case
// fails closed: a Job stored under a retired claim runs no Pod, because Pod
// admission (internal/podintent) refuses a Pod whose claim is no longer
// active.
const MissingJobGrace = DispatchStopBound + createLandingMargin

// createLandingMargin is the time the API server may still take to act on the
// cancellation of a request abandoned at its bound, the create at
// DispatchCriticalSection or the withdrawal at DispatchStopBound, or to finish
// a storage write that was already under way when it arrived. That takes
// milliseconds when the cluster is healthy. Five seconds is long next to it,
// and short next to a spent approval, which costs a person the time to approve
// again.
const createLandingMargin = 5 * time.Second

// missingJobSightingHorizon is how old a sighting may be before a later
// sighting of another resource drops it. A claim that is still waiting reaches
// its verdict after MissingJobGrace, so a sighting this old belongs to a claim
// that was retired or a resource that was deleted. A claim still waiting that
// long behind something else waits the grace again, which only delays a
// verdict that fails closed.
const missingJobSightingHorizon = 10 * MissingJobGrace

// MissingJobs remembers, per resource, the marked mutating claim this process
// found with no Job, and when it first did. It is kept in memory on purpose: a
// manager that restarts forgets and waits the whole grace again, which only
// delays a verdict that fails closed, and nothing another manager wrote is
// read as a time.
//
// It holds one sighting per resource. A new claim replaces the old claim's
// sighting, and a sighting older than missingJobSightingHorizon is dropped by
// the next one recorded. A pass that reaches any verdict other than waiting or
// lost drops it too, but only a pass that happens: a state no pass of this
// process read leaves the sighting where it was. The lost verdict keeps it, so
// a pass that has to retire the claim again, after a conflict or behind the
// realm, does not wait again.
//
// That leaves one window open. A create the API server refused before storing
// it makes the dispatching manager withdraw its mark and mark again under the
// same operation ID. If no pass of this process read the claim unmarked
// between the two, the old sighting stands for the new mark, and a second
// create still in flight can be called lost before its own grace has passed,
// which spends the approval as #869 did. The key stays the operation ID. A new
// attempt per mark would rename the Job, and a key on the resourceVersion
// would need proof that no pass writes the resource before its verdict.
//
// The zero value is ready to use. It is safe for concurrent use.
type MissingJobs struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]missingJobSighting
}

type missingJobSighting struct {
	operationID string
	first       time.Time
}

// Verdict is VerdictFor for a claim on the resource, with the wait described
// above. A claim VerdictFor finds unaccounted because it is marked, names no
// Job and finds none is VerdictAwait until MissingJobGrace has passed since
// this process first saw that claim so, and the duration is how much of the
// grace is left. Every other verdict is VerdictFor's, with no duration.
//
// now has to come from a clock that keeps its monotonic reading, so a step of
// the wall clock cannot shorten the wait.
func (m *MissingJobs) Verdict(
	resource types.NamespacedName,
	operationID string,
	claim JobClaim,
	now time.Time,
) (JobVerdict, JobCause, time.Duration) {
	verdict, cause := VerdictFor(claim)
	if verdict != VerdictUnaccounted || cause != CauseMissing || claim.RecordedJobUID != "" {
		m.forget(resource)
		return verdict, cause, 0
	}
	if wait := m.await(resource, operationID, now); wait > 0 {
		return VerdictAwait, cause, wait
	}
	return verdict, cause, 0
}

// await records a sighting and reports how much of the grace is left.
func (m *MissingJobs) await(resource types.NamespacedName, operationID string, now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[types.NamespacedName]missingJobSighting{}
	}
	for key, sighting := range m.seen {
		if key != resource && now.Sub(sighting.first) > missingJobSightingHorizon {
			delete(m.seen, key)
		}
	}
	sighting, seen := m.seen[resource]
	// A withdrawn and renewed mark keeps its operation ID, so it reuses the
	// sighting unless a pass saw it withdrawn; MissingJobs says what that
	// leaves.
	if !seen || sighting.operationID != operationID || now.Before(sighting.first) {
		m.seen[resource] = missingJobSighting{operationID: operationID, first: now}
		return MissingJobGrace
	}
	if elapsed := now.Sub(sighting.first); elapsed < MissingJobGrace {
		return MissingJobGrace - elapsed
	}
	return 0
}

func (m *MissingJobs) forget(resource types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, resource)
}
