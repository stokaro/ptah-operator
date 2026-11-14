package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// appliedRunWithLog finishes the claimed Apply of version 3 with a frame whose
// run carries log, and returns the migration as the pass left it. previous, when
// set, is the RunLogged an earlier run left.
func appliedRunWithLog(
	t *testing.T,
	log *dataplane.MigrationLogReport,
	previous *metav1.Condition,
) *operatorv1alpha1.PtahMigration {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	if previous != nil {
		meta.SetStatusCondition(&migration.Status.Conditions, *previous)
	}
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	report := reportOf(dataplane.MigrationOutcomeApplied, []int64{3}, []int64{3})
	report.MigrationLog = log
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, MutationStarted: true,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun:         report,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
		t.Fatalf("LastRun = %#v, want the applied run recorded", run)
	}
	return actual
}

// lostEntry is one entry of version 3 refused as MySQL refuses a value longer
// than its column, as the runner forwards it: without Ptah's message.
func lostEntry(state string) dataplane.MigrationLogLoss {
	return dataplane.MigrationLogLoss{Operation: "up", Version: 3, VersionKey: "3", State: state, SQLState: "22001", ErrorNumber: 1406}
}

// earlierRunLogged is what a run before this one left on RunLogged.
func earlierRunLogged(status metav1.ConditionStatus, reason operatorv1alpha1.ConditionReason) *metav1.Condition {
	return &metav1.Condition{
		Type: operatorv1alpha1.ConditionMigrationRunLogged, Status: status, Reason: string(reason),
		Message: "an earlier run's record", LastTransitionTime: metav1.Now(),
	}
}

// A run whose log entries the database refused says so on lastRun, on its
// journal entry and on RunLogged, by count and by the code the database
// answered with, and its outcome stays Applied.
func TestARunWithLostLogEntriesSaysItsRecordIsIncomplete(t *testing.T) {
	t.Parallel()

	actual := appliedRunWithLog(t, &dataplane.MigrationLogReport{
		Written: 1, Lost: []dataplane.MigrationLogLoss{lostEntry(dataplane.MigrationLogApplied)},
	}, earlierRunLogged(metav1.ConditionTrue, operatorv1alpha1.ReasonLogWritten))

	want := &operatorv1alpha1.MigrationLogStatus{Written: 1, Lost: 1, Losses: []operatorv1alpha1.MigrationLogLossStatus{{
		Version: 3, State: operatorv1alpha1.MigrationLogEntryApplied, SQLState: "22001", ErrorNumber: 1406,
	}}}
	if got := actual.Status.LastRun.MigrationLog; !reflect.DeepEqual(got, want) {
		t.Fatalf("LastRun.MigrationLog = %+v, want %+v", got, want)
	}
	end := oneRunEnd(t, actual)
	if !reflect.DeepEqual(end.MigrationLog, want) || end.Outcome != operatorv1alpha1.JournalSucceeded {
		t.Fatalf("RunFinished = %#v, want a succeeded run with log %+v", end, want)
	}
	logged := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationRunLogged)
	if logged == nil || logged.Status != metav1.ConditionFalse || logged.Reason != string(operatorv1alpha1.ReasonLogEntriesLost) ||
		logged.ObservedGeneration != actual.Generation {
		t.Fatalf("RunLogged = %#v, want False %s at generation %d", logged, operatorv1alpha1.ReasonLogEntriesLost, actual.Generation)
	}
	wantMessage := fmt.Sprintf("The database refused 1 of the 2 entries the run of operation %s wrote to schema_migrations_log "+
		"with SQLSTATE 22001 (error 1406), so its own record of the run is incomplete. The run's outcome is unchanged; "+
		"status.lastRun.migrationLog and the run's journal entry name the entries", actual.Status.LastRun.OperationID)
	if logged.Message != wantMessage {
		t.Fatalf("RunLogged message = %q, want %q", logged.Message, wantMessage)
	}
}

// RunLogged reads only a run that wrote or lost an entry. A run with no report
// and an up-to-date run that wrote nothing leave what the last run that did
// said, so a lost record stays reported until a run writes its own in full.
func TestRunLoggedChangesOnlyOnARunThatWroteItsLog(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		log        *dataplane.MigrationLogReport
		previous   *metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason operatorv1alpha1.ConditionReason
		wantKept   bool
	}{
		{
			name:       "every entry written after a lost one",
			log:        &dataplane.MigrationLogReport{Written: 2},
			previous:   earlierRunLogged(metav1.ConditionFalse, operatorv1alpha1.ReasonLogEntriesLost),
			wantStatus: metav1.ConditionTrue, wantReason: operatorv1alpha1.ReasonLogWritten,
		},
		{
			name:       "every entry written on the first run",
			log:        &dataplane.MigrationLogReport{Written: 2},
			wantStatus: metav1.ConditionTrue, wantReason: operatorv1alpha1.ReasonLogWritten,
		},
		{
			name:       "no report after a lost entry",
			previous:   earlierRunLogged(metav1.ConditionFalse, operatorv1alpha1.ReasonLogEntriesLost),
			wantStatus: metav1.ConditionFalse, wantReason: operatorv1alpha1.ReasonLogEntriesLost, wantKept: true,
		},
		{
			name:       "nothing written after a lost entry",
			log:        &dataplane.MigrationLogReport{},
			previous:   earlierRunLogged(metav1.ConditionFalse, operatorv1alpha1.ReasonLogEntriesLost),
			wantStatus: metav1.ConditionFalse, wantReason: operatorv1alpha1.ReasonLogEntriesLost, wantKept: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			actual := appliedRunWithLog(t, test.log, test.previous)
			logged := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationRunLogged)
			if logged == nil || logged.Status != test.wantStatus || logged.Reason != string(test.wantReason) {
				t.Fatalf("RunLogged = %#v, want %s %s", logged, test.wantStatus, test.wantReason)
			}
			if kept := test.previous != nil && logged.Message == test.previous.Message; test.wantKept != kept {
				t.Fatalf("RunLogged message = %q, kept the earlier run's: %t, want %t", logged.Message, kept, test.wantKept)
			}
			if !test.wantKept && !strings.Contains(logged.Message, "accepted all 2 entries the run of operation "+actual.Status.LastRun.OperationID) {
				t.Fatalf("RunLogged message = %q, want it to count the entries this run wrote", logged.Message)
			}
		})
	}
}

// A run with no report on its first execution leaves no RunLogged at all: no
// run has said anything about the record yet.
func TestRunLoggedIsAbsentUntilARunReportsItsLog(t *testing.T) {
	t.Parallel()

	actual := appliedRunWithLog(t, nil, nil)
	if logged := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationRunLogged); logged != nil {
		t.Fatalf("RunLogged = %#v, want none", logged)
	}
	if actual.Status.LastRun.MigrationLog != nil || oneRunEnd(t, actual).MigrationLog != nil {
		t.Fatalf("a run with no report recorded a log: %#v", actual.Status.LastRun)
	}
}

// A termination summary carries no log report, so a run read from it claims
// nothing about the record.
func TestATerminationSummaryClaimsNothingAboutTheLog(t *testing.T) {
	t.Parallel()
	run := summaryMigrationApply(runner.Summary{
		Migration: &runner.MigrationSummary{Outcome: dataplane.MigrationOutcomeApplied},
	}).run
	if run == nil || run.migrationLog != nil {
		t.Fatalf("the summary's run = %+v, want no log", run)
	}
}

// Status keeps the first entries the database refused and counts them all, and
// a state status has no word for leaves no record rather than a guessed one.
func TestMigrationLogStatusBoundsWhatItKeeps(t *testing.T) {
	t.Parallel()

	var losses []dataplane.MigrationLogLoss
	for version := range int64(maxMigrationLogLosses/2 + 2) {
		losses = append(losses, lostEntry(dataplane.MigrationLogStarted), lostEntry(dataplane.MigrationLogFailed))
		losses[len(losses)-2].Version, losses[len(losses)-1].Version = version+1, version+1
	}
	status := migrationLogStatus(&dataplane.MigrationLogReport{Written: 3, Lost: losses})
	if status == nil || status.Written != 3 || int(status.Lost) != len(losses) || len(status.Losses) != maxMigrationLogLosses {
		t.Fatalf("migrationLogStatus() = %+v, want %d lost and the first %d kept", status, len(losses), maxMigrationLogLosses)
	}
	if last := status.Losses[maxMigrationLogLosses-1]; last.Version != losses[maxMigrationLogLosses-1].Version ||
		last.State != operatorv1alpha1.MigrationLogEntryFailed {
		t.Fatalf("the last kept entry = %+v, want %+v", last, losses[maxMigrationLogLosses-1])
	}
	if status := migrationLogStatus(nil); status != nil {
		t.Fatalf("migrationLogStatus(nil) = %+v, want nil", status)
	}
	unknown := lostEntry("rolled_back")
	if status := migrationLogStatus(&dataplane.MigrationLogReport{Lost: []dataplane.MigrationLogLoss{unknown}}); status != nil {
		t.Fatalf("migrationLogStatus() of an unknown state = %+v, want nil", status)
	}
}
