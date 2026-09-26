package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A migration Apply whose log is gone is decided from the runner's termination
// summary where, and only where, the summary may stand in for the frame; and
// what it decides is what the frame would have. Each refused row is a run that
// stays unknown, which is exactly what it was before summaries existed.
func TestAMigrationApplyWithNoReadableFrameFallsBackToItsTerminationSummary(t *testing.T) {
	t.Parallel()

	const (
		lost    = "lost"    // the log holds nothing
		cut     = "cut"     // the log ends inside the frame the summary is bound to
		whole   = "whole"   // the log holds the frame
		foreign = "foreign" // the log holds a frame for another attempt
		other   = "other"   // the log ends inside a different frame
	)
	stopped := func(operation *operatorv1alpha1.MigrationOperationStatus, migration *operatorv1alpha1.PtahMigration) runner.Result {
		return runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
			OperationID: operation.ID, ChildExitCode: -1,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
			MutationStarted:      true,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
				Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{3, 4}, Applied: []int64{3},
			},
			Error: &runner.ResultError{Code: "execution_error",
				Message: "the ptah migration-apply process was stopped because the Pod is terminating"},
		}
	}
	partial := func(result *runner.Result) {
		result.MigrationRun.Outcome = dataplane.MigrationOutcomePartial
		result.Uncertain = true
	}
	for _, row := range []struct {
		name string
		// summarized is the result the summary was written from, and framed
		// the one the log's frame, when it has one, was written from.
		summarized, framed func(*runner.Result)
		log                string
		message            func(summary string) string
		wantOutcome        operatorv1alpha1.MigrationRunOutcome
		wantPhase          operatorv1alpha1.MigrationPhase
		wantMessage        string
		wantApplied        []int64
		wantRefusalEvent   bool
	}{
		{
			name: "the log is gone and the summary reports a run stopped between files", log: lost,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeFailed, wantPhase: operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantMessage: "termination message", wantApplied: []int64{3},
		},
		{
			name: "the log ends inside the frame the summary is bound to", log: cut,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeFailed, wantPhase: operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantMessage: "version 3 recorded applied", wantApplied: []int64{3},
		},
		{
			name: "the summary reports a partial run", log: lost, summarized: partial,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomePartial, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "outcome partial", wantApplied: []int64{3},
		},
		{
			name: "the summary reports a refusal before the child started", log: lost,
			summarized: func(result *runner.Result) {
				result.MigrationRun, result.MutationStarted, result.ChildExitCode = nil, false, -1
				result.Error = &runner.ResultError{Code: "dispatch_deadline_expired", Message: "the dispatch deadline expired"}
			},
			// A frame saying the same is recorded as unknown too: a Job may
			// run more than one Pod, and one Pod's refusal is not an account.
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "dispatch_deadline_expired",
		},
		{
			name: "the summary reports another database realm", log: lost,
			summarized: func(result *runner.Result) { result.CoordinationDigest = "sha256:" + strings.Repeat("e", 64) },
			// The same check a frame is held to.
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "a database other than the one it was planned for",
		},
		{
			name: "the container wrote no summary", log: lost, message: func(string) string { return "" },
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result",
		},
		{
			name: "the termination message is the runtime's alone", log: lost, message: func(string) string { return "OOMKilled" },
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result",
		},
		{
			name: "the summary answers another attempt", log: lost,
			summarized:  func(result *runner.Result) { result.OperationID = "sha256:" + strings.Repeat("f", 64) },
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result", wantRefusalEvent: true,
		},
		{
			name: "the summary was cut short", log: lost,
			message:     func(summary string) string { return summary[:len(summary)/2] },
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result", wantRefusalEvent: true,
		},
		{
			// The frame is there and refused, and a summary cannot overrule it.
			name: "the log holds a frame for another attempt", log: foreign,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result", wantRefusalEvent: true,
		},
		{
			name: "the log ends inside a frame the summary is not bound to", log: other, framed: partial,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "read the Apply result", wantRefusalEvent: true,
		},
		{
			// A readable frame is the account, whatever the summary says.
			name: "the log holds a readable frame that disagrees with the summary", log: whole, framed: partial,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomePartial, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "committed some of its statements", wantApplied: []int64{3},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)

			summarized := stopped(operation, migration)
			if row.summarized != nil {
				row.summarized(&summarized)
			}
			encoded, err := runner.EncodeResult(summarized)
			if err != nil || encoded.SummaryErr != nil {
				t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
			}
			message := string(encoded.Summary)
			if row.message != nil {
				message = row.message(message)
			}
			pod.Status.ContainerStatuses[0].State.Terminated.Message = message

			framed := stopped(operation, migration)
			if row.framed != nil {
				row.framed(&framed)
			}
			var logs []byte
			switch row.log {
			case cut:
				logs = encoded.Frame[:len(encoded.Frame)-len("\nPTAH_RUNNER_RESULT_END_V1\n")-4]
			case whole:
				logs = migrationFrame(t, framed)
			case foreign:
				framed.OperationID = "sha256:" + strings.Repeat("0", 64)
				logs = migrationFrame(t, framed)
			case other:
				frame := migrationFrame(t, framed)
				logs = frame[:len(frame)-len("\nPTAH_RUNNER_RESULT_END_V1\n")-4]
			}

			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder
			holdMigrationApplyLease(t, reconciler, api, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			run := actual.Status.LastRun
			if run == nil {
				t.Fatalf("the Apply left no record: phase=%q", actual.Status.Phase)
			}
			if run.Outcome != row.wantOutcome || actual.Status.Phase != row.wantPhase {
				t.Fatalf("outcome = %q in phase %q, want %q in %q: %s",
					run.Outcome, actual.Status.Phase, row.wantOutcome, row.wantPhase, run.Message)
			}
			if !strings.Contains(run.Message, row.wantMessage) {
				t.Fatalf("run message = %q, want it to say %q", run.Message, row.wantMessage)
			}
			if !equalVersions(run.AppliedVersions, row.wantApplied) {
				t.Fatalf("applied versions = %v, want %v", run.AppliedVersions, row.wantApplied)
			}
			unresolved := row.wantOutcome == operatorv1alpha1.MigrationRunOutcomeUnknown ||
				row.wantOutcome == operatorv1alpha1.MigrationRunOutcomePartial
			if unresolved != (actual.Status.UnresolvedRun != nil) {
				t.Fatalf("unresolved run = %#v, want one recorded=%t", actual.Status.UnresolvedRun, unresolved)
			}
			refused := false
			for len(recorder.Events) > 0 {
				if strings.Contains(<-recorder.Events, "TerminationSummaryRefused") {
					refused = true
				}
			}
			if refused != row.wantRefusalEvent {
				t.Fatalf("TerminationSummaryRefused event = %t, want %t", refused, row.wantRefusalEvent)
			}
		})
	}
}

// A schema Apply is never decided from a summary. Its frame is taken only
// whole, and the one thing a summary could add -- that no mutation started --
// is not something the schema family accepts even from a frame, so a lost log
// leaves the Apply exactly as uncertain as it was, even beside a summary that
// reports it succeeded.
func TestASchemaApplyWithALostLogStaysUncertainWhateverItsSummarySays(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	succeeded := runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply,
		OperationID: schema.Status.ActiveOperation.ID, ChildExitCode: 0, MutationStarted: true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	}
	encoded, err := runner.EncodeResult(succeeded)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Message = string(encoded.Summary)
	if _, err := runner.ParseSummaryFor(pod.Status.ContainerStatuses[0].State.Terminated.Message,
		runner.OperationApply, succeeded.OperationID); err != nil {
		t.Fatalf("the fixture's summary is not one the runner would accept: %v", err)
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("pending observation = %#v, want an Apply whose outcome is unknown", pending)
	}
}

func equalVersions(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
