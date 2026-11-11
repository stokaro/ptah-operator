package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

var provenanceStamp = metav1.NewTime(time.Date(2026, 8, 30, 11, 50, 0, 0, time.UTC))

// approvedClaim gives the claim what a claim of this release copies from the plan
// and the approval it was written from.
func approvedClaim(operation *operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) {
	operation.PlanFingerprint = plan.Spec.Fingerprint
	operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "orders-approval", UID: "approval-uid"}
	operation.ApprovedBy = &operatorv1alpha1.ApprovalIdentity{Username: "alice@example.test"}
	stamped := provenanceStamp
	operation.ApprovedAt = &stamped
}

// finishedMigrationRun dispatches one Apply under the claim prepare leaves, and
// lets it end with report: the status holds what the controller recorded. Only the
// migration, the claim and the workload exist: no approval, no plan the run names
// by anything but its reference, as after both were removed.
func finishedMigrationRun(
	t *testing.T,
	report *dataplane.MigrationRunReport,
	prepare func(*operatorv1alpha1.MigrationOperationStatus, *operatorv1alpha1.PtahMigrationPlan),
) (*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan) {
	t.Helper()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	if prepare != nil {
		prepare(operation, plan)
	}
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	var logs []byte
	if report != nil {
		logs = migrationFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
			OperationID: operation.ID, ChildExitCode: 0,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
			MigrationRun:         report,
		})
	}
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return readMigration(t, api, migration), plan
}

func migrationRunReport(outcome string, applied ...int64) *dataplane.MigrationRunReport {
	return &dataplane.MigrationRunReport{
		ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
		Outcome: outcome, Planned: []int64{3}, Applied: applied,
	}
}

// journalEntriesOf is the entries of one type.
func journalEntriesOf(migration *operatorv1alpha1.PtahMigration, kind operatorv1alpha1.JournalEntryType) []operatorv1alpha1.JournalEntry {
	var entries []operatorv1alpha1.JournalEntry
	if migration.Status.Journal != nil {
		for _, entry := range migration.Status.Journal.Entries {
			if entry.Type == kind {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// A claim is written before the Job and outlives the plan and the approval, so it
// copies what the record of the run will have to name.
func TestAClaimCopiesWhatItsRunWillName(t *testing.T) {
	t.Parallel()

	t.Run("under an approval", func(t *testing.T) {
		t.Parallel()

		migration, plan := awaitingApprovalFixture(t)
		approval := migrationApprovalFor(migration, plan)
		approval.Spec.Approver = operatorv1alpha1.ApprovalIdentity{Username: "alice@example.test", UID: "alice-uid", Groups: []string{"dba"}}
		approval.Spec.ApprovedAt = provenanceStamp
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := readMigration(t, api, migration)
		operation := actual.Status.ActiveOperation
		if operation == nil || operation.PlanFingerprint != plan.Spec.Fingerprint {
			t.Fatalf("the claim does not carry the plan's fingerprint: %#v", operation)
		}
		if operation.ApprovedBy == nil || operation.ApprovedBy.Username != "alice@example.test" || operation.ApprovedBy.UID != "alice-uid" ||
			len(operation.ApprovedBy.Groups) != 1 {
			t.Fatalf("the claim does not carry the identity admission stamped: %#v", operation.ApprovedBy)
		}
		if operation.ApprovedAt == nil || !operation.ApprovedAt.Equal(&provenanceStamp) {
			t.Fatalf("the claim does not carry the stamp's time: %#v", operation.ApprovedAt)
		}
		accepted := journalEntriesOf(actual, operatorv1alpha1.JournalApprovalAccepted)
		if len(accepted) != 1 || accepted[0].Approver != "alice@example.test" ||
			accepted[0].ApprovalUID != approval.UID || accepted[0].PlanUID != plan.UID || accepted[0].PlanFingerprint != plan.Spec.Fingerprint {
			t.Fatalf("the journal does not join the approval to the Apply it allowed: %#v", accepted)
		}
	})

	t.Run("under Always", func(t *testing.T) {
		t.Parallel()

		migration, plan := awaitingApprovalFixture(t)
		migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
		migration.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
		repolicyPlan(t, migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		operation := readMigration(t, api, migration).Status.ActiveOperation
		if operation == nil || operation.PlanFingerprint != plan.Spec.Fingerprint {
			t.Fatalf("the claim does not carry the plan's fingerprint: %#v", operation)
		}
		if operation.ApprovalRef != nil || operation.ApprovedBy != nil || operation.ApprovedAt != nil {
			t.Fatalf("the claim names a decision nobody made: %#v", operation)
		}
	})
}

// Every ending of a run keeps what the claim knew, with no Job, approval or plan
// read for it. What the claim did not know stays absent: a claim a release without
// the copies wrote names its approval and not who made it.
func TestARunRecordsWhatItsClaimKnew(t *testing.T) {
	t.Parallel()

	approved := func(operation *operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) {
		approvedClaim(operation, plan)
	}
	for _, row := range []struct {
		name     string
		report   *dataplane.MigrationRunReport
		prepare  func(*operatorv1alpha1.MigrationOperationStatus, *operatorv1alpha1.PtahMigrationPlan)
		outcome  operatorv1alpha1.MigrationRunOutcome
		journal  operatorv1alpha1.JournalOutcome
		approved bool
		stamped  bool
		copied   bool
	}{
		{
			name: "applied under an approval", report: migrationRunReport(dataplane.MigrationOutcomeApplied, 3), prepare: approved,
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, journal: operatorv1alpha1.JournalSucceeded,
			approved: true, stamped: true, copied: true,
		},
		{
			name: "failed under an approval", report: migrationRunReport(dataplane.MigrationOutcomeFailed), prepare: approved,
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, journal: operatorv1alpha1.JournalFailed,
			approved: true, stamped: true, copied: true,
		},
		{
			name: "partial under an approval", report: migrationRunReport(dataplane.MigrationOutcomePartial), prepare: approved,
			outcome: operatorv1alpha1.MigrationRunOutcomePartial, journal: operatorv1alpha1.JournalPartial,
			approved: true, stamped: true, copied: true,
		},
		{
			name: "with no readable result, under an approval", prepare: approved,
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, journal: operatorv1alpha1.JournalUnknown,
			approved: true, stamped: true, copied: true,
		},
		{
			name: "applied under Always", report: migrationRunReport(dataplane.MigrationOutcomeApplied, 3),
			prepare: func(operation *operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) {
				operation.PlanFingerprint = plan.Spec.Fingerprint
			},
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, journal: operatorv1alpha1.JournalSucceeded,
			copied: true,
		},
		{
			name: "a claim that names its approval and copied nothing", report: migrationRunReport(dataplane.MigrationOutcomeApplied, 3),
			prepare: func(operation *operatorv1alpha1.MigrationOperationStatus, _ *operatorv1alpha1.PtahMigrationPlan) {
				operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "orders-approval", UID: "approval-uid"}
			},
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, journal: operatorv1alpha1.JournalSucceeded,
			approved: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := finishedMigrationRun(t, row.report, row.prepare)
			run := migration.Status.LastRun
			if run == nil || run.Outcome != row.outcome {
				t.Fatalf("last run = %#v, want outcome %q", run, row.outcome)
			}
			if run.OperationID != testDigest {
				t.Fatalf("operation = %q, want the claim's %q", run.OperationID, testDigest)
			}
			if run.PlanRef == nil || run.PlanRef.UID != plan.UID || run.PlanRef.Name != plan.Name {
				t.Fatalf("plan = %#v, want the claim's", run.PlanRef)
			}
			fingerprint := ""
			if row.copied {
				fingerprint = plan.Spec.Fingerprint
			}
			if run.PlanFingerprint != fingerprint {
				t.Fatalf("fingerprint = %q, want %q", run.PlanFingerprint, fingerprint)
			}
			if run.ExecutionBindingID != migration.Status.ExecutionBinding.Epoch {
				t.Fatalf("execution binding = %q, want %q", run.ExecutionBindingID, migration.Status.ExecutionBinding.Epoch)
			}
			// Only a result frame says which database a run went to. With none, the
			// record names none.
			target := migration.Status.History.TargetIdentityDigest
			if row.report == nil {
				target = ""
			}
			if run.TargetIdentityDigest != target {
				t.Fatalf("target = %q, want %q", run.TargetIdentityDigest, target)
			}
			if run.ArtifactDigest != migration.Status.Artifact.Digest || run.Source != nil {
				t.Fatalf("source = %q, %#v, want the OCI digest %q", run.ArtifactDigest, run.Source, migration.Status.Artifact.Digest)
			}
			authorization := run.Authorization
			if authorization == nil {
				t.Fatal("the run records nothing about what allowed it")
			}
			switch {
			case !row.approved:
				if authorization.Kind != operatorv1alpha1.MigrationRunAuthorizedByPolicy ||
					authorization.ApprovalRef != nil || authorization.Approver != nil || authorization.ApprovedAt != nil {
					t.Fatalf("authorization = %#v, want the policy and no decision", authorization)
				}
			case authorization.Kind != operatorv1alpha1.MigrationRunAuthorizedByApproval ||
				authorization.ApprovalRef == nil || authorization.ApprovalRef.UID != "approval-uid":
				t.Fatalf("authorization = %#v, want the approval the claim names", authorization)
			}
			if row.stamped && (authorization.Approver == nil || authorization.Approver.Username != "alice@example.test" ||
				authorization.ApprovedAt == nil || !authorization.ApprovedAt.Equal(&provenanceStamp)) {
				t.Fatalf("authorization = %#v, want the stamp the claim copied", authorization)
			}
			if !row.stamped && (authorization.Approver != nil || authorization.ApprovedAt != nil) {
				t.Fatalf("authorization = %#v, an identity nobody recorded", authorization)
			}

			end := oneRunEnd(t, migration)
			if end.Outcome != row.journal {
				t.Fatalf("the journal's end reads %s, want %s: %#v", end.Outcome, row.journal, end)
			}
			if end.OperationID != run.OperationID || end.PlanUID != run.PlanRef.UID || end.Plan != run.PlanRef.Name ||
				end.PlanFingerprint != run.PlanFingerprint || end.JobUID != run.JobUID || end.JobName != run.JobName ||
				end.Authorization != authorization.Kind {
				t.Fatalf("the journal's end does not say which run this was: %#v, run %#v", end, run)
			}
			if row.approved && (end.ApprovalUID != "approval-uid" || end.Approval != "orders-approval") {
				t.Fatalf("the journal's end does not name the approval: %#v", end)
			}
			if row.stamped && end.Approver != "alice@example.test" {
				t.Fatalf("the journal's end does not name the approver: %#v", end)
			}
			if !row.stamped && end.Approver != "" {
				t.Fatalf("the journal's end names an approver nobody recorded: %#v", end)
			}
		})
	}
}

// An uncertain retirement with no Job left is the case the record exists for: only
// the claim knows what ran, and nothing about the Job is made up.
func TestAnUncertainRetirementWithNoJobKeepsWhatItsClaimKnew(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	approvedClaim(operation, plan)
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	held := readMigration(t, api, migration)
	run := held.Status.LastRun
	if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want Unknown", run)
	}
	if run.JobUID != "" || run.JobName != "" {
		t.Fatalf("the run names a Job nothing confirmed: %q/%q", run.JobName, run.JobUID)
	}
	if run.OperationID != operation.ID || run.PlanRef == nil || run.PlanRef.UID != plan.UID || run.PlanFingerprint != plan.Spec.Fingerprint ||
		run.Authorization == nil || run.Authorization.ApprovalRef == nil || run.Authorization.ApprovalRef.UID != "approval-uid" {
		t.Fatalf("the run does not keep what its claim knew: %#v", run)
	}
	if run.TargetIdentityDigest != "" {
		t.Fatalf("target = %q, and nothing ran that could have named one", run.TargetIdentityDigest)
	}
	// The hold names the only database the run can have changed.
	if hold := held.Status.UnresolvedRun; hold == nil || hold.TargetIdentityDigest != migration.Status.History.TargetIdentityDigest {
		t.Fatalf("hold = %#v, want the database its plan was computed against", hold)
	}
}

// migrationHistoryInSync is a reading with nothing pending.
func migrationHistoryInSync() dataplane.MigrationStatusReport {
	return dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  3, TotalMigrations: 2, PendingMigrations: []int64{},
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
		},
	}
}

// A run that named no database may have run nowhere, or against what its Secret
// held by then. A clean reading of the planned database settles the hold, by the rule
// that has always settled it, and says nothing about the run.
func TestAReadingDoesNotVerifyARunThatNamedNoDatabase(t *testing.T) {
	t.Parallel()

	retiredWithNoJob := func(t *testing.T) *operatorv1alpha1.PtahMigration {
		t.Helper()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		approvedClaim(operation, plan)
		reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan)
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		held := readMigration(t, api, migration)
		// The binding change is how the fixture gets a claim retired with no Job. A
		// reading runs under the manager's own binding.
		held.Status.ExecutionBinding = migrationExecutionBinding()
		return held
	}
	retiredWithNoResult := func(t *testing.T) *operatorv1alpha1.PtahMigration {
		t.Helper()
		migration, _ := finishedMigrationRun(t, nil, func(operation *operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) {
			approvedClaim(operation, plan)
		})
		return migration
	}
	for name, retired := range map[string]func(*testing.T) *operatorv1alpha1.PtahMigration{
		"no Job":    retiredWithNoJob,
		"no result": retiredWithNoResult,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			held := retired(t)
			if held.Status.UnresolvedRun == nil || held.Status.LastRun == nil || held.Status.LastRun.TargetIdentityDigest != "" {
				t.Fatalf("the fixture is not a held run that named no database: %#v", held.Status.LastRun)
			}
			read, _ := readMigrationHistory(t, held, migrationHistoryInSync())
			if read.Status.UnresolvedRun != nil {
				t.Fatalf("a clean reading of that database did not settle the hold: %#v", read.Status.UnresolvedRun)
			}
			if read.Status.LastRun.Verification != nil {
				t.Fatalf("the reading verified a run that never said which database it reached: %#v", read.Status.LastRun.Verification)
			}
			if entries := journalEntriesOf(read, operatorv1alpha1.JournalVerified); len(entries) != 0 {
				t.Fatalf("the journal records a reading after a run that never said which database it reached: %#v", entries)
			}
		})
	}
}

// The first reading after a run is what a wait for that run rests on, so it is
// recorded with the reading, and the journal keeps it past a later run.
func TestTheReadingAfterARunIsRecordedOnIt(t *testing.T) {
	t.Parallel()

	inSync := migrationHistoryInSync()
	dirty := pendingMigrationHistory()
	dirty.DirtyRevision = &dataplane.MigrationDirty{Version: 3}
	for _, row := range []struct {
		name   string
		report dataplane.MigrationStatusReport
		result operatorv1alpha1.MigrationRunVerificationResult
		kept   operatorv1alpha1.JournalOutcome
	}{
		{"a database with nothing pending", inSync, operatorv1alpha1.MigrationRunVerifiedInSync, operatorv1alpha1.JournalInSync},
		{"a database the source has more for", pendingMigrationHistory(), operatorv1alpha1.MigrationRunVerifiedPending, operatorv1alpha1.JournalPending},
		{"a history nothing may run against", dirty, operatorv1alpha1.MigrationRunVerifiedBlocked, operatorv1alpha1.JournalBlocked},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			finished, _ := finishedMigrationRun(t, migrationRunReport(dataplane.MigrationOutcomeApplied, 3),
				func(operation *operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) {
					approvedClaim(operation, plan)
				})
			if finished.Status.LastRun.Verification != nil {
				t.Fatalf("a run is verified before any reading: %#v", finished.Status.LastRun.Verification)
			}
			read, _ := readMigrationHistory(t, finished, row.report)
			verification := read.Status.LastRun.Verification
			if verification == nil {
				t.Fatalf("the reading after the run was not recorded on it: %#v", read.Status.LastRun)
			}
			history := read.Status.History
			if verification.Result != row.result || !verification.ObservedAt.Equal(&history.ObservedAt) ||
				verification.HistoryFingerprint != history.Fingerprint || verification.CurrentVersion != history.CurrentVersion {
				t.Fatalf("verification = %#v, want %s from reading %#v", verification, row.result, history)
			}
			// A reading that publishes a plan writes that entry by the wall clock, a
			// month from the fixture's, and the age bound takes what precedes it. The
			// table in internal/lifecyclejournal holds that row's entry.
			checkJournal := read.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval
			entries := journalEntriesOf(read, operatorv1alpha1.JournalVerified)
			if checkJournal && (len(entries) != 1 || entries[0].OperationID != read.Status.LastRun.OperationID || entries[0].Outcome != row.kept ||
				entries[0].PlanUID != read.Status.LastRun.PlanRef.UID || !entries[0].At.Equal(&history.ObservedAt)) {
				t.Fatalf("the journal does not keep the reading under the run it followed: %#v", entries)
			}

			// A later reading at its interval is no longer the one after the run. The
			// fixture's clock is fixed, so the first reading is dated apart from it.
			earlier := metav1.NewTime(history.ObservedAt.Add(-time.Hour))
			read.Status.LastRun.Verification.ObservedAt = earlier
			again, _ := readMigrationHistory(t, read, row.report)
			if !again.Status.LastRun.Verification.ObservedAt.Equal(&earlier) {
				t.Fatalf("a later reading rewrote the verification: %#v, was from %s", again.Status.LastRun.Verification, earlier)
			}
			if checkJournal && len(journalEntriesOf(again, operatorv1alpha1.JournalVerified)) != 1 {
				t.Fatalf("a later reading wrote another Verified entry: %#v", again.Status.Journal)
			}
		})
	}

	t.Run("a reading of another database verifies nothing", func(t *testing.T) {
		t.Parallel()

		finished, _ := finishedMigrationRun(t, migrationRunReport(dataplane.MigrationOutcomeApplied, 3), nil)
		finished.Status.LastRun.TargetIdentityDigest = "sha256:" + strings.Repeat("7", 64)
		read, _ := readMigrationHistory(t, finished, inSync)
		if read.Status.LastRun.Verification != nil {
			t.Fatalf("a reading of another database verified the run: %#v", read.Status.LastRun.Verification)
		}
		if entries := journalEntriesOf(read, operatorv1alpha1.JournalVerified); len(entries) != 0 {
			t.Fatalf("the journal records a reading of another database: %#v", entries)
		}
	})

	t.Run("a run a release before these fields recorded stays as it was", func(t *testing.T) {
		t.Parallel()

		finished, _ := finishedMigrationRun(t, migrationRunReport(dataplane.MigrationOutcomeApplied, 3), nil)
		finished.Status.LastRun.OperationID = ""
		read, _ := readMigrationHistory(t, finished, inSync)
		if read.Status.LastRun.Verification != nil {
			t.Fatalf("the reading verified a run that names nothing: %#v", read.Status.LastRun.Verification)
		}
	})
}

// An acknowledgment says which run it settled, by the record it settled.
func TestAnAcknowledgmentNamesTheRunInTheJournal(t *testing.T) {
	t.Parallel()

	migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomeUnknown)
	hold := migration.Status.UnresolvedRun
	acknowledgment := runAcknowledgment(migration, "orders-run-acknowledged", hold.OperationID, time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC))
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, acknowledgment, verificationPolicyConfigMap())
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	entries := journalEntriesOf(readMigration(t, api, migration), operatorv1alpha1.JournalRunAcknowledged)
	if len(entries) != 1 {
		t.Fatalf("the journal holds %d acknowledgments, want one: %#v", len(entries), entries)
	}
	entry := entries[0]
	if entry.OperationID != hold.OperationID || entry.Plan != hold.PlanRef.Name || entry.PlanUID != hold.PlanRef.UID ||
		entry.JobName != hold.JobName || entry.JobUID != hold.JobUID || entry.Outcome != operatorv1alpha1.JournalUnknown {
		t.Fatalf("the acknowledgment does not name the run it settled: %#v, hold %#v", entry, hold)
	}
}

// The Secret moved to another database before the Pod started, and the runner refused
// before its child started, naming the digest of what the Secret holds. The record
// keeps what the result named, as the hold always has, so the two agree: the record is
// about which database the run named, which the run did not touch. A reading of that
// database verifies it, and the run still reads Unknown, which is what a wait stops on.
func TestARunThatRefusedBeforeStartingNamesWhatItsSecretHeld(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	planned := migration.Status.History.TargetIdentityDigest
	moved := "sha256:" + strings.Repeat("b", 64)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	logs := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: moved,
		Error:                &runner.ResultError{Code: "target_binding_mismatch", Message: "database target identity changed after planning"},
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	held := readMigration(t, api, migration)
	run := held.Status.LastRun
	if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || run.TargetIdentityDigest != moved {
		t.Fatalf("last run = %#v, want Unknown naming %q", run, moved)
	}
	if hold := held.Status.UnresolvedRun; hold == nil || hold.TargetIdentityDigest != moved {
		t.Fatalf("hold = %#v, want it naming %q", hold, moved)
	}

	// The history status still holds the planned database, so this reads that one.
	if held.Status.History.TargetIdentityDigest != planned {
		t.Fatalf("the reading before the run names %q, want %q", held.Status.History.TargetIdentityDigest, planned)
	}
	other, _ := readMigrationHistory(t, held, migrationHistoryInSync())
	if other.Status.LastRun.Verification != nil {
		t.Fatalf("a reading of the planned database verified a run that named another: %#v", other.Status.LastRun.Verification)
	}

	named := held.DeepCopy()
	named.Status.History.TargetIdentityDigest = moved
	read, _ := readMigrationHistory(t, named, migrationHistoryInSync())
	if read.Status.LastRun.Verification == nil || read.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("a reading of the database the run named: %#v", read.Status.LastRun)
	}
}

// A run that reported nothing names no database, and its hold names the one its
// plan was computed against. That is the only database the run can have changed:
// the runner refuses an Apply whose URL names another target before Ptah starts
// (target_binding_mismatch). So a reading of the planned database settles the
// hold, and a reading of another one, after the Secret moved, does not.
func TestAHoldWhoseRunNamedNoDatabaseSettlesOnlyOnThePlannedOne(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	applyClaimFor(t, migration, plan)
	planned := migration.Status.History.TargetIdentityDigest
	moved := "sha256:" + strings.Repeat("b", 64)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	held := readMigration(t, api, migration)
	if run := held.Status.LastRun; run == nil || run.TargetIdentityDigest != "" {
		t.Fatalf("last run = %#v, want one that names no database", run)
	}
	if hold := held.Status.UnresolvedRun; hold == nil || hold.TargetIdentityDigest != planned {
		t.Fatalf("hold = %#v, want it naming the planned database %q", hold, planned)
	}

	elsewhere := held.DeepCopy()
	elsewhere.Status.History.TargetIdentityDigest = moved
	other, _ := readMigrationHistory(t, elsewhere, migrationHistoryInSync())
	if other.Status.UnresolvedRun == nil {
		t.Fatal("a reading of another database settled the hold")
	}

	read, _ := readMigrationHistory(t, held, migrationHistoryInSync())
	if read.Status.UnresolvedRun != nil {
		t.Fatalf("a reading of the planned database left the hold: %#v", read.Status.UnresolvedRun)
	}
}

// A run's record names the database by a digest the schema holds it to, so what a
// frame says stands only when it is one: a status write the API server refused
// would leave the claim standing over a run that ended. With nothing to go on the
// record names none, and a hold names the planned database as somewhere to start.
func TestARunNamesTheDatabaseItReportedWhenThatIsADigest(t *testing.T) {
	t.Parallel()

	reported := "sha256:" + strings.Repeat("7", 64)
	planned := "sha256:" + strings.Repeat("8", 64)
	withHistory := &operatorv1alpha1.PtahMigration{}
	withHistory.Status.History = &operatorv1alpha1.MigrationHistoryStatus{TargetIdentityDigest: planned}
	for _, row := range []struct {
		name      string
		migration *operatorv1alpha1.PtahMigration
		reported  string
		wantRun   string
		wantHold  string
	}{
		{"a digest", withHistory, reported, reported, reported},
		{"nothing", withHistory, "", "", planned},
		{"something else", withHistory, "postgres://somewhere", "", planned},
		{"nothing, with no history", &operatorv1alpha1.PtahMigration{}, "", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run := &operatorv1alpha1.MigrationRunStatus{}
			recordMigrationRunProvenance(run, row.migration, &operatorv1alpha1.MigrationOperationStatus{}, row.reported)
			if run.TargetIdentityDigest != row.wantRun {
				t.Fatalf("the run names %q, want %q", run.TargetIdentityDigest, row.wantRun)
			}
			if got := migrationRunTarget(row.migration, row.reported); got != row.wantHold {
				t.Fatalf("a hold names %q, want %q", got, row.wantHold)
			}
		})
	}
}

// A run names the source it read. An OCI artifact is a digest; a ConfigMap or a Git
// commit is a binding, and it comes from the claim with the revision and the policy
// status holds, which is what the plan bound.
func TestARunNamesTheSourceItRead(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("a", 64)
	content := "sha256:" + strings.Repeat("b", 64)
	policy := &operatorv1alpha1.SourcePolicyBinding{UID: "policy-uid", Digest: "sha256:" + strings.Repeat("c", 64)}
	revision := &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-migrations", UID: "configmap-uid", ResourceVersion: "3"}
	git := &operatorv1alpha1.GitSourceBinding{URL: "https://git.example/orders.git", Path: "db/migrations", ContentDigest: content}
	gitRevision := sourcebinding.GitRevision(git.URL, git.Path)
	for _, row := range []struct {
		name         string
		claim        *operatorv1alpha1.SourceAccessBinding
		revision     *operatorv1alpha1.ConfigMapSourceRevision
		policy       *operatorv1alpha1.SourcePolicyBinding
		wantArtifact string
		wantSource   *operatorv1alpha1.PlanSourceBinding
	}{
		{name: "an OCI artifact", claim: &operatorv1alpha1.SourceAccessBinding{Digest: digest}, wantArtifact: digest},
		{
			name:       "a Git commit",
			claim:      &operatorv1alpha1.SourceAccessBinding{Git: git},
			wantSource: &operatorv1alpha1.PlanSourceBinding{Kind: operatorv1alpha1.SourceKindGit, Revision: gitRevision, ContentDigest: content},
		},
		{
			name:   "a Git commit under a source policy",
			claim:  &operatorv1alpha1.SourceAccessBinding{Git: git},
			policy: policy,
			wantSource: &operatorv1alpha1.PlanSourceBinding{
				Kind: operatorv1alpha1.SourceKindGit, Revision: gitRevision, ContentDigest: content, Policy: policy,
			},
		},
		{
			name:       "a ConfigMap",
			claim:      &operatorv1alpha1.SourceAccessBinding{Snapshot: &operatorv1alpha1.SourceSnapshotBinding{ContentDigest: content}},
			revision:   revision,
			wantSource: &operatorv1alpha1.PlanSourceBinding{Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "configmap-uid", ContentDigest: content},
		},
		{
			name:  "a ConfigMap whose revision status does not hold, named by neither",
			claim: &operatorv1alpha1.SourceAccessBinding{Snapshot: &operatorv1alpha1.SourceSnapshotBinding{ContentDigest: content}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration := &operatorv1alpha1.PtahMigration{}
			migration.Status.ConfigMap, migration.Status.SourcePolicy = row.revision, row.policy
			// What status resolves next is not what the claim ran.
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Digest: "sha256:" + strings.Repeat("d", 64)}
			run := &operatorv1alpha1.MigrationRunStatus{}
			recordMigrationRunProvenance(run, migration, &operatorv1alpha1.MigrationOperationStatus{Source: row.claim}, "")
			if run.ArtifactDigest != row.wantArtifact {
				t.Fatalf("artifact = %q, want %q", run.ArtifactDigest, row.wantArtifact)
			}
			if !equality.Semantic.DeepEqual(run.Source, row.wantSource) {
				t.Fatalf("source = %#v, want %#v", run.Source, row.wantSource)
			}
		})
	}
}
