package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
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
			if run.TargetIdentityDigest != migration.Status.History.TargetIdentityDigest {
				t.Fatalf("target = %q, want %q", run.TargetIdentityDigest, migration.Status.History.TargetIdentityDigest)
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
	run := readMigration(t, api, migration).Status.LastRun
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
	if run.TargetIdentityDigest != migration.Status.History.TargetIdentityDigest {
		t.Fatalf("target = %q, want the database it was dispatched against", run.TargetIdentityDigest)
	}
}

// The first reading after a run is what a wait for that run rests on, so it is
// recorded with the reading, and the journal keeps it past a later run.
func TestTheReadingAfterARunIsRecordedOnIt(t *testing.T) {
	t.Parallel()

	inSync := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  3, TotalMigrations: 2, PendingMigrations: []int64{},
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
		},
	}
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

// A run's record names the database by a digest the schema holds it to, so what a
// frame says stands only when it is one: a status write the API server refused
// would leave the claim standing over a run that ended.
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
		want      string
	}{
		{"a digest", withHistory, reported, reported},
		{"nothing", withHistory, "", planned},
		{"something else", withHistory, "postgres://somewhere", planned},
		{"nothing, with no history", &operatorv1alpha1.PtahMigration{}, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := migrationRunTarget(row.migration, row.reported); got != row.want {
				t.Fatalf("target = %q, want %q", got, row.want)
			}
		})
	}
}
