package e2e

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	mpOperation   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mpHistory     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mpTarget      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	mpFingerprint = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	mpArtifact    = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	mpBinding     = "v1-ffffffffffffffffffffffffffffffff"
)

func mpTime(minute int) metav1.Time {
	return metav1.NewTime(time.Date(2026, 10, 24, 7, minute, 0, 0, time.UTC))
}

func mpPlan() *ptahv1alpha1.PtahMigrationPlan {
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	plan.Name, plan.UID = "plan-a", "plan-a-uid"
	plan.Spec.Fingerprint, plan.Spec.ArtifactDigest = mpFingerprint, mpArtifact
	plan.Spec.TargetIdentityDigest, plan.Spec.ExecutionBindingID = mpTarget, mpBinding
	return plan
}

func mpApproval() *ptahv1alpha1.PtahMigrationApproval {
	approval := &ptahv1alpha1.PtahMigrationApproval{}
	approval.Name, approval.UID = "approve-a", "approve-a-uid"
	approval.Spec.Approver.Username = "alice@example.test"
	approval.Spec.ApprovedAt = mpTime(1)
	return approval
}

// mpRecorded is the status of one approved run the operator recorded and read back.
func mpRecorded() ptahv1alpha1.PtahMigrationStatus {
	finished, stamped := mpTime(3), mpTime(1)
	return ptahv1alpha1.PtahMigrationStatus{
		LastRun: &ptahv1alpha1.MigrationRunStatus{
			Outcome: ptahv1alpha1.MigrationRunOutcomeApplied, JobName: "job-a", JobUID: "job-a-uid",
			StartedAt: mpTime(2), FinishedAt: &finished, AppliedVersions: []int64{2, 3},
			OperationID: mpOperation, PlanRef: &ptahv1alpha1.ImmutableObjectReference{Name: "plan-a", UID: "plan-a-uid"},
			PlanFingerprint: mpFingerprint, ArtifactDigest: mpArtifact, TargetIdentityDigest: mpTarget, TargetIdentityReported: true, ExecutionBindingID: mpBinding,
			Authorization: &ptahv1alpha1.MigrationRunAuthorizationStatus{
				Kind:        ptahv1alpha1.MigrationRunAuthorizedByApproval,
				ApprovalRef: &ptahv1alpha1.ImmutableObjectReference{Name: "approve-a", UID: "approve-a-uid"},
				Approver:    &ptahv1alpha1.ApprovalIdentity{Username: "alice@example.test"},
				ApprovedAt:  &stamped,
			},
			Verification: &ptahv1alpha1.MigrationRunVerificationStatus{
				ObservedAt: mpTime(4), HistoryFingerprint: mpHistory, CurrentVersion: 3, Result: ptahv1alpha1.MigrationRunVerifiedInSync,
			},
		},
		Journal: &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{
			{Type: ptahv1alpha1.JournalPlanPublished, At: mpTime(0), Plan: "plan-a"},
			{Type: ptahv1alpha1.JournalApprovalAccepted, At: mpTime(2), ApprovalUID: "approve-a-uid", Plan: "plan-a", PlanUID: "plan-a-uid"},
			{Type: ptahv1alpha1.JournalRunFinished, At: mpTime(3), OperationID: mpOperation, Plan: "plan-a", PlanUID: "plan-a-uid", JobUID: "job-a-uid"},
			{Type: ptahv1alpha1.JournalVerified, At: mpTime(4), OperationID: mpOperation, Plan: "plan-a", PlanUID: "plan-a-uid"},
		}},
	}
}

type mpRow struct {
	mutate func(*ptahv1alpha1.PtahMigrationStatus)
	says   string
}

// mpRefused holds a predicate to its fixture and to refusing each mutation, naming
// what each refusal has to say.
func mpRefused(t *testing.T, check func(ptahv1alpha1.PtahMigrationStatus) error, rows map[string]mpRow) {
	t.Helper()
	if err := check(mpRecorded()); err != nil {
		t.Fatalf("the operator's reading was refused: %v", err)
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := mpRecorded()
			row.mutate(&status)
			err := check(status)
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("error = %v, want one naming %q", err, row.says)
			}
		})
	}
}

func TestRunIdentityRecordedRefusesEachClause(t *testing.T) {
	t.Parallel()
	mpRefused(t, func(status ptahv1alpha1.PtahMigrationStatus) error { return runIdentityRecorded(status, mpPlan()) }, map[string]mpRow{
		"no run":              {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "no finished run"},
		"an unfinished run":   {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.FinishedAt = nil }, "no finished run"},
		"no operation":        {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.OperationID = "" }, "names operation"},
		"no plan":             {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef = nil }, "names plan"},
		"another plan's name": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef.Name = "plan-b" }, "names plan"},
		"another plan's UID":  {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef.UID = "plan-b-uid" }, "names plan"},
		"another fingerprint": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanFingerprint = mpHistory }, "names fingerprint"},
		"no artifact":         {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.ArtifactDigest = "" }, "names artifact"},
		"a source the plan does not name": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Source = &ptahv1alpha1.PlanSourceBinding{Kind: ptahv1alpha1.SourceKindGit, Revision: mpHistory, ContentDigest: mpHistory}
		}, "names source"},
		"another database":                  {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityDigest = mpHistory }, "names database"},
		"a database the run did not report": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityReported = false }, "did not report"},
		"another binding":                   {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.ExecutionBindingID = "" }, "names execution binding"},
	})
}

func TestRunApprovedByRefusesEachClause(t *testing.T) {
	t.Parallel()
	mpRefused(t, func(status ptahv1alpha1.PtahMigrationStatus) error { return runApprovedBy(status, mpApproval()) }, map[string]mpRow{
		"no run":           {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "records nothing"},
		"no authorization": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization = nil }, "records nothing"},
		"the policy": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Kind = ptahv1alpha1.MigrationRunAuthorizedByPolicy
		}, "authorized by"},
		"no approval":      {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovalRef = nil }, "names approval"},
		"another approval": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovalRef.Name = "approve-b" }, "names approval"},
		"a reused name":    {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovalRef.UID = "approve-b-uid" }, "names approval"},
		"no approver":      {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.Approver = nil }, "names approver"},
		"another approver": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.Approver.Username = "bob" }, "names approver"},
		"no stamp time":    {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovedAt = nil }, "dates the approval"},
		"another stamp time": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			later := mpTime(9)
			s.LastRun.Authorization.ApprovedAt = &later
		}, "dates the approval"},
	})
}

func TestRunAllowedByPolicyRefusesADecision(t *testing.T) {
	t.Parallel()
	policy := func() ptahv1alpha1.PtahMigrationStatus {
		status := mpRecorded()
		status.LastRun.Authorization = &ptahv1alpha1.MigrationRunAuthorizationStatus{Kind: ptahv1alpha1.MigrationRunAuthorizedByPolicy}
		return status
	}
	if err := runAllowedByPolicy(policy()); err != nil {
		t.Fatalf("a run under the policy was refused: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"no run":           func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil },
		"no authorization": func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization = nil },
		"an approval's kind": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Kind = ptahv1alpha1.MigrationRunAuthorizedByApproval
		},
		"an approval named": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.ApprovalRef = &ptahv1alpha1.ImmutableObjectReference{Name: "approve-a"}
		},
		"an approver named": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Approver = &ptahv1alpha1.ApprovalIdentity{Username: "alice"}
		},
		"a stamp time": func(s *ptahv1alpha1.PtahMigrationStatus) {
			stamped := mpTime(1)
			s.LastRun.Authorization.ApprovedAt = &stamped
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := policy()
			mutate(&status)
			if runAllowedByPolicy(status) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestRunVerifiedInSyncRefusesEachClause(t *testing.T) {
	t.Parallel()
	mpRefused(t, runVerifiedInSync, map[string]mpRow{
		"no run":     {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "no reading"},
		"no end":     {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.FinishedAt = nil }, "no reading"},
		"no reading": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Verification = nil }, "no reading"},
		"a blocked reading": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Verification.Result = ptahv1alpha1.MigrationRunVerifiedBlocked
		}, "found"},
		"a pending reading": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Verification.Result = ptahv1alpha1.MigrationRunVerifiedPending
		}, "found"},
		"no history":                    {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Verification.HistoryFingerprint = "" }, "names history"},
		"no versions applied":           {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.AppliedVersions = nil }, "no versions applied"},
		"a reading from before the run": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Verification.CurrentVersion = 2 }, "before version 3"},
	})
}

func TestRunTargetNotReportedRefusesEachClause(t *testing.T) {
	t.Parallel()
	held := func() ptahv1alpha1.PtahMigrationStatus {
		status := mpRecorded()
		status.LastRun.TargetIdentityReported, status.LastRun.Verification = false, nil
		return status
	}
	if err := runTargetNotReported(held()); err != nil {
		t.Fatalf("a run that reported no database was refused: %v", err)
	}
	for name, row := range map[string]mpRow{
		"no run":          {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "no run"},
		"a report":        {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityReported = true }, "named database"},
		"no planned one":  {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityDigest = "" }, "planned database"},
		"a reading on it": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Verification = mpRecorded().LastRun.Verification }, "a reading verified"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := held()
			row.mutate(&status)
			if err := runTargetNotReported(status); err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("error = %v, want one naming %q", err, row.says)
			}
		})
	}
}

func TestRunJournaledRefusesEachClause(t *testing.T) {
	t.Parallel()
	entry := func(s *ptahv1alpha1.PtahMigrationStatus, kind ptahv1alpha1.JournalEntryType) *ptahv1alpha1.JournalEntry {
		for index := range s.Journal.Entries {
			if s.Journal.Entries[index].Type == kind {
				return &s.Journal.Entries[index]
			}
		}
		t.Fatalf("the fixture holds no %s", kind)
		return nil
	}
	mpRefused(t, runJournaled, map[string]mpRow{
		"no run":                        {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "nothing to join"},
		"no journal":                    {func(s *ptahv1alpha1.PtahMigrationStatus) { s.Journal = nil }, "nothing to join"},
		"a run that names no operation": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.OperationID = "" }, "nothing to join"},
		"an end under another operation": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).OperationID = mpHistory
		}, "0 RunFinished"},
		"an acceptance of another approval": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalApprovalAccepted).ApprovalUID = "approve-b-uid"
		}, "0 ApprovalAccepted"},
		"a run under the policy": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization = &ptahv1alpha1.MigrationRunAuthorizationStatus{Kind: ptahv1alpha1.MigrationRunAuthorizedByPolicy}
		}, "nothing to join"},
		"no reading": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalVerified).OperationID = ""
		}, "0 Verified"},
		"the end twice": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Journal.Entries = append(s.Journal.Entries, *entry(s, ptahv1alpha1.JournalRunFinished))
		}, "2 RunFinished"},
		"an entry on another plan's UID": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).PlanUID = "plan-b-uid"
		}, "names plan"},
		"an entry with no plan": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalVerified).Plan = ""
		}, "names plan"},
		"an end on another Job": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).JobUID = "job-b-uid"
		}, "names Job UID"},
		"an end on no Job": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).JobUID = ""
			s.LastRun.JobUID = ""
		}, "names Job UID"},
	})
}
