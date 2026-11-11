package mutationlifecycle_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// The instant every row starts at. Each row moves away from it, so a wait is
// always measured between two distinct readings.
var sightedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

var (
	orders   = types.NamespacedName{Namespace: "team-a", Name: "orders"}
	invoices = types.NamespacedName{Namespace: "team-a", Name: "invoices"}
)

// markedWithNoJob is what a manager reads between another manager's mark and
// that manager's create: the mark, no recorded UID, and no Job.
var markedWithNoJob = mutationlifecycle.JobClaim{Mutating: true, DispatchStarted: true}

type verdictAt struct {
	verdict mutationlifecycle.JobVerdict
	cause   mutationlifecycle.JobCause
	wait    time.Duration
}

func judge(jobs *mutationlifecycle.MissingJobs, resource types.NamespacedName, operationID string,
	claim mutationlifecycle.JobClaim, at time.Duration,
) verdictAt {
	verdict, cause, wait := jobs.Verdict(resource, operationID, claim, sightedAt.Add(at))
	return verdictAt{verdict, cause, wait}
}

func awaiting(wait time.Duration) verdictAt {
	return verdictAt{mutationlifecycle.VerdictAwait, mutationlifecycle.CauseMissing, wait}
}

var lost = verdictAt{mutationlifecycle.VerdictUnaccounted, mutationlifecycle.CauseMissing, 0}

// The grace is the dispatching manager's own bound plus a margin, and no
// shorter: a reader that waited less could still retire a claim whose create
// is on its way.
func TestTheGraceOutlastsTheDispatchCriticalSection(t *testing.T) {
	t.Parallel()
	if mutationlifecycle.MissingJobGrace <= mutationlifecycle.DispatchCriticalSection {
		t.Fatalf("MissingJobGrace = %s, want longer than DispatchCriticalSection %s",
			mutationlifecycle.MissingJobGrace, mutationlifecycle.DispatchCriticalSection)
	}
}

// A marked claim with no Job waits from this process's first sighting, for
// the whole grace and no longer, and the wait it reports is what is left.
func TestAMarkedClaimWithNoJobWaitsTheGraceBeforeItIsLost(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	grace := mutationlifecycle.MissingJobGrace
	for _, step := range []struct {
		at   time.Duration
		want verdictAt
	}{
		{0, awaiting(grace)},
		{grace / 2, awaiting(grace - grace/2)},
		{grace - time.Second, awaiting(time.Second)},
		{grace, lost},
		{grace + time.Minute, lost},
	} {
		if got := judge(jobs, orders, "apply-1", markedWithNoJob, step.at); got != step.want {
			t.Fatalf("Verdict() %s after the first sighting = %+v, want %+v", step.at, got, step.want)
		}
	}
}

// A recorded UID was written after the create returned, so the Job certainly
// landed and a missing one is lost on the first pass.
func TestAClaimThatRecordedItsJobIsLostAtOnce(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	for name, claim := range map[string]mutationlifecycle.JobClaim{
		"marked and recorded": {Mutating: true, DispatchStarted: true, RecordedJobUID: "job"},
		"recorded only":       {Mutating: true, RecordedJobUID: "job"},
	} {
		if got := judge(jobs, orders, "apply-1", claim, 0); got != lost {
			t.Errorf("%s: Verdict() = %+v, want %+v", name, got, lost)
		}
	}
}

// Every claim that is not the marked one with no Job gets VerdictFor's own
// verdict, at once.
func TestOnlyAMarkedMutatingClaimWithNoJobWaits(t *testing.T) {
	t.Parallel()

	for name, claim := range map[string]mutationlifecycle.JobClaim{
		"nothing dispatched":              {Mutating: true},
		"a read-only claim marked":        {DispatchStarted: true},
		"a read-only claim recorded":      {DispatchStarted: true, RecordedJobUID: "job"},
		"a Job created before its record": {Mutating: true, DispatchStarted: true, Found: true, FoundJobUID: "job", OwnedExactly: true},
		"a supervised Job": {
			Mutating: true, DispatchStarted: true, RecordedJobUID: "job",
			Found: true, FoundJobUID: "job", OwnedExactly: true,
		},
		"a replaced Job": {
			Mutating: true, DispatchStarted: true, RecordedJobUID: "job",
			Found: true, FoundJobUID: "other", OwnedExactly: true,
		},
		"a disowned Job": {Mutating: true, DispatchStarted: true, Found: true, FoundJobUID: "job"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			verdict, cause := mutationlifecycle.VerdictFor(claim)
			want := verdictAt{verdict, cause, 0}
			if got := judge(&mutationlifecycle.MissingJobs{}, orders, "apply-1", claim, 0); got != want {
				t.Fatalf("Verdict() = %+v, want VerdictFor's %+v", got, want)
			}
		})
	}
}

// A sighting belongs to one claim. A new claim on the same resource waits from
// its own first sighting rather than inheriting the old claim's.
func TestANewClaimWaitsFromItsOwnFirstSighting(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	grace := mutationlifecycle.MissingJobGrace
	judge(jobs, orders, "apply-1", markedWithNoJob, 0)
	if got := judge(jobs, orders, "apply-2", markedWithNoJob, grace); got != awaiting(grace) {
		t.Fatalf("a new claim seen once the old one's grace ended = %+v, want %+v", got, awaiting(grace))
	}
}

// Any other verdict on the resource drops its sighting. A claim whose Job
// appeared and then went missing again before anyone recorded it waits from
// its new sighting.
func TestAnyOtherVerdictForgetsTheSighting(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	found := mutationlifecycle.JobClaim{Mutating: true, DispatchStarted: true, Found: true, FoundJobUID: "job", OwnedExactly: true}
	withdrawn := mutationlifecycle.JobClaim{Mutating: true}
	for name, between := range map[string]mutationlifecycle.JobClaim{
		"the Job appeared":       found,
		"the mark was withdrawn": withdrawn,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jobs := &mutationlifecycle.MissingJobs{}
			judge(jobs, orders, "apply-1", markedWithNoJob, 0)
			judge(jobs, orders, "apply-1", between, time.Second)
			if got := judge(jobs, orders, "apply-1", markedWithNoJob, grace); got != awaiting(grace) {
				t.Fatalf("the claim seen again after %s = %+v, want a new wait %+v", name, got, awaiting(grace))
			}
		})
	}
}

// The lost verdict keeps the sighting, so a pass that has to retire the claim
// again does not wait the grace a second time.
func TestALostVerdictKeepsItsSighting(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	grace := mutationlifecycle.MissingJobGrace
	judge(jobs, orders, "apply-1", markedWithNoJob, 0)
	judge(jobs, orders, "apply-1", markedWithNoJob, grace)
	if got := judge(jobs, orders, "apply-1", markedWithNoJob, grace+time.Second); got != lost {
		t.Fatalf("the pass after the lost verdict = %+v, want %+v", got, lost)
	}
}

// A process that starts again remembers nothing and waits the whole grace,
// however long the claim had been waiting before.
func TestARestartedProcessWaitsAgain(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	judge(&mutationlifecycle.MissingJobs{}, orders, "apply-1", markedWithNoJob, 0)
	restarted := &mutationlifecycle.MissingJobs{}
	if got := judge(restarted, orders, "apply-1", markedWithNoJob, grace/2); got != awaiting(grace) {
		t.Fatalf("a restarted process's first pass = %+v, want %+v", got, awaiting(grace))
	}
}

// A sighting from long ago, of a resource no pass reaches any more, is
// dropped when another one is recorded; a sighting inside its grace is not.
func TestAnAbandonedSightingIsDroppedByTheNextOne(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	for _, row := range []struct {
		name      string
		other     time.Duration
		want      verdictAt
		wantFresh bool
	}{
		{name: "the other resource is sighted while orders waits", other: time.Second, want: lost},
		{name: "the other resource is sighted a day later", other: 24 * time.Hour, wantFresh: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			jobs := &mutationlifecycle.MissingJobs{}
			judge(jobs, orders, "apply-1", markedWithNoJob, 0)
			judge(jobs, invoices, "apply-9", markedWithNoJob, row.other)
			at := row.other
			if at < grace {
				at = grace
			}
			want := row.want
			if row.wantFresh {
				want = awaiting(grace)
			}
			if got := judge(jobs, orders, "apply-1", markedWithNoJob, at); got != want {
				t.Fatalf("orders seen again at %s = %+v, want %+v", at, got, want)
			}
		})
	}
}

// A clock that went back is not a reason to call a Job lost early; the wait
// starts again from the earlier reading.
func TestAClockThatWentBackStartsTheWaitAgain(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	grace := mutationlifecycle.MissingJobGrace
	judge(jobs, orders, "apply-1", markedWithNoJob, time.Hour)
	if got := judge(jobs, orders, "apply-1", markedWithNoJob, 0); got != awaiting(grace) {
		t.Fatalf("a reading before the first sighting = %+v, want %+v", got, awaiting(grace))
	}
}

// A reconciler shares one MissingJobs across its workers. Run with -race.
func TestMissingJobsIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	jobs := &mutationlifecycle.MissingJobs{}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			resource := types.NamespacedName{Namespace: "team-a", Name: fmt.Sprintf("resource-%d", worker%3)}
			for pass := range 200 {
				at := time.Duration(pass) * time.Second
				judge(jobs, resource, "apply-1", markedWithNoJob, at)
				if pass%7 == 0 {
					judge(jobs, resource, "apply-1", mutationlifecycle.JobClaim{Mutating: true}, at)
				}
			}
		})
	}
	wg.Wait()
}
