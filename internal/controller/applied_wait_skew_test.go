package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The waits below read what the reconciler stored for a migration run that applied,
// when the steps around the run were harvested by two managers whose clocks
// disagree. A failover is the fixture's clock moved back a minute between two
// harvests: the manager that takes over reads its own clock, behind the other's.
// The run's end, the reading before it and the reading after it are each stamped
// by the manager that harvested them, so no order of those instants says which
// came first. What the run's record holds does.

// failOverBehind hands the resource to a manager whose clock is a minute behind.
func failOverBehind(clock *fixedClock) {
	clock.now = clock.now.Add(-time.Minute)
}

// appliedVersionOne is the result of an Apply that ran version 1, against the
// database the history was read from.
func appliedVersionOne(harness *migrationLifecycle) runner.Result {
	return runner.Result{
		Operation: runner.OperationMigrationApply, ChildExitCode: 0,
		CoordinationDigest:   harness.coordinationDigest(),
		TargetIdentityDigest: testDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
			Planned: []int64{1}, Applied: []int64{1},
		},
	}
}

// appliedWait is one of the waits the plugin makes for a run: approve --wait, wait
// --for applied, and the same narrowed to the plan.
type appliedWait struct {
	name    string
	options operate.WaitOptions
}

// approvedApplyClaimed reads the fixture to a plan waiting for a person, approves
// it, and has the reconciler claim the Apply. It returns the waits the plugin
// makes from the resource at the gate.
func approvedApplyClaimed(t *testing.T) (*migrationLifecycle, *fixedClock, []appliedWait) {
	t.Helper()
	harness, clock, plan := waitingForAnApproval(t)
	waiting := harness.migration()
	approval := migrationApprovalFor(waiting, plan)
	if err := harness.api.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	harness.claimed(operatorv1alpha1.MigrationOperationApply)
	return harness, clock, []appliedWait{
		{"approve --wait", operate.WaitOptions{
			Target: operate.TargetApplied, Since: waiting,
			Decision: &operate.Decision{PlanName: plan.Name, PlanUID: plan.UID, ApprovalName: approval.Name, ApprovalUID: approval.UID},
		}},
		{"wait --for applied", operate.WaitOptions{Target: operate.TargetApplied, Since: waiting}},
		{"wait --for applied --plan", operate.WaitOptions{Target: operate.TargetApplied, Since: waiting, Plan: plan.Name}},
	}
}

func migrationRef(migration *operatorv1alpha1.PtahMigration) operate.Ref {
	return operate.Ref{Family: operate.FamilyMigration, Namespace: migration.Namespace, Name: migration.Name}
}

// A run's read back is harvested by a manager a minute behind the one that
// harvested the run, so the reading is dated before the run's end. The reading
// records itself on the run, and every wait is done at that document.
func TestAppliedWaitsTakeAReadBackDatedBeforeTheRun(t *testing.T) {
	t.Parallel()
	harness, clock, waits := approvedApplyClaimed(t)
	harness.answer(appliedVersionOne(harness))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	failOverBehind(clock)
	harness.answer(historyAnswer(harness, inSyncReport()))

	read := harness.migration()
	run, history := read.Status.LastRun, read.Status.History
	if read.Status.Phase != operatorv1alpha1.MigrationPhaseInSync || read.Status.ActiveOperation != nil || run == nil ||
		run.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied || run.Verification == nil || history == nil ||
		!history.ObservedAt.Before(run.FinishedAt) {
		t.Fatalf("phase %s, run %#v, history %#v; want a run read back in sync by a reading dated before its end",
			read.Status.Phase, run, history)
	}

	for _, wait := range waits {
		t.Run(wait.name, func(t *testing.T) {
			result, err := waitOn(harness.api, migrationRef(read), wait.options)
			if err != nil {
				t.Fatalf("wait = %v, want the run's read back to end it", err)
			}
			if result.Reads != 1 {
				t.Fatalf("done at read %d, want the first read", result.Reads)
			}
		})
	}
}

// The Secret moved after the run, so the read back is of another database, and the
// manager that read it is a minute behind the one that harvested the run. That
// reading cannot verify the run, and every wait says so, by the database the
// history names rather than by its date.
func TestAppliedWaitsStopOnAReadingOfAnotherDatabase(t *testing.T) {
	t.Parallel()
	harness, clock, waits := approvedApplyClaimed(t)
	harness.answer(appliedVersionOne(harness))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	failOverBehind(clock)
	elsewhere := historyAnswer(harness, inSyncReport())
	elsewhere.TargetIdentityDigest = "sha256:" + strings.Repeat("7", 64)
	harness.answer(elsewhere)

	read := harness.migration()
	run, history := read.Status.LastRun, read.Status.History
	if read.Status.ActiveOperation != nil || run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied ||
		run.Verification != nil || history == nil || history.TargetIdentityDigest == run.TargetIdentityDigest ||
		!history.ObservedAt.Before(run.FinishedAt) {
		t.Fatalf("operation %#v, run %#v, history %#v; want a run with no reading on it, beside a reading of another "+
			"database dated before its end", read.Status.ActiveOperation, run, history)
	}

	for _, wait := range waits {
		t.Run(wait.name, func(t *testing.T) {
			_, err := waitOn(harness.api, migrationRef(read), wait.options)
			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopNotProved ||
				!strings.Contains(stop.Message, "run "+run.JobName+" recorded its migrations applied, and a later reading did not verify it") {
				t.Fatalf("wait = %v, want a %s stop that says a later reading did not verify run %s", err, operate.StopNotProved, run.JobName)
			}
		})
	}
}

// A run whose read back has not happened: the reconciler rests the resource first,
// on the reading from before the run. That reading was harvested by a manager a
// minute ahead of the one that harvested the run, so it is dated after the run's
// end. No reading followed the run, so no wait may say one did: none ends, and
// none stops on a reading it did not see.
func TestAppliedWaitsKeepRunningWhenTheResourceRestsBeforeTheReadBack(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		// before is what happens while the Apply runs.
		before func(*testing.T, *migrationLifecycle)
		phase  operatorv1alpha1.MigrationPhase
	}{
		{
			// The operator observes an Apply to its end and claims nothing after it.
			name: "suspended",
			before: func(t *testing.T, harness *migrationLifecycle) {
				t.Helper()
				suspended := harness.migration()
				suspended.Spec.Suspend = true
				if err := harness.api.Update(context.Background(), suspended); err != nil {
					t.Fatal(err)
				}
			},
			phase: operatorv1alpha1.MigrationPhaseSuspended,
		},
		{
			// Another resource claims the database realm, which the census before the
			// next claim refuses.
			name: "blocked by its realm",
			before: func(t *testing.T, harness *migrationLifecycle) {
				t.Helper()
				migration := harness.migration()
				competitor := realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
				if err := harness.api.Create(context.Background(), competitor); err != nil {
					t.Fatal(err)
				}
			},
			phase: operatorv1alpha1.MigrationPhaseBlocked,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			harness, clock, waits := approvedApplyClaimed(t)
			row.before(t, harness)
			failOverBehind(clock)
			harness.answer(appliedVersionOne(harness))

			rested := harness.migration()
			run, history := rested.Status.LastRun, rested.Status.History
			if rested.Status.Phase != row.phase || rested.Status.ActiveOperation != nil || run == nil ||
				run.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied || run.Verification != nil || history == nil ||
				!run.FinishedAt.Before(&history.ObservedAt) {
				t.Fatalf("phase %s, operation %#v, run %#v, history %#v; want %s at rest after a run with no reading after it, "+
					"beside the reading from before it dated after its end",
					rested.Status.Phase, rested.Status.ActiveOperation, run, history, row.phase)
			}
			if row.phase == operatorv1alpha1.MigrationPhaseBlocked {
				blocked := meta.FindStatusCondition(rested.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
				if blocked == nil || blocked.Reason != string(operatorv1alpha1.ReasonRealmConflict) {
					t.Fatalf("Blocked = %#v, want the realm's refusal", blocked)
				}
			}

			for _, wait := range waits {
				t.Run(wait.name, func(t *testing.T) {
					_, err := waitOn(harness.api, migrationRef(rested), wait.options)
					assertWaitKeepsRunning(t, err)
					var timedOut *operate.TimeoutError
					if errors.As(err, &timedOut) && !strings.Contains(timedOut.Waiting, "the history read back after run "+run.JobName) {
						t.Fatalf("timeout = %v, want it still waiting for the history read back after run %s", err, run.JobName)
					}
				})
			}
		})
	}
}
