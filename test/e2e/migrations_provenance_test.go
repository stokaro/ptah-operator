package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// mpReadingFile is what assertRunProvenance printed for the PostgreSQL migration
// once its approved run was read back: the migration's identity and status, the
// plan the run carried out and the approval that allowed it.
// testdata/e2e/readings/README.md says in which run.
const mpReadingFile = "../../testdata/e2e/readings/approved-run-postgresql.json"

// mpOther is a digest nothing in the reading carries.
const mpOther = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

type mpReading struct {
	Migration ptahv1alpha1.PtahMigration         `json:"migration"`
	Plan      ptahv1alpha1.PtahMigrationPlan     `json:"plan"`
	Approval  ptahv1alpha1.PtahMigrationApproval `json:"approval"`
}

var mpReadingContent = sync.OnceValues(func() ([]byte, error) { return os.ReadFile(mpReadingFile) })

// mpRead decodes the reading afresh, so a mutation changes one copy only.
func mpRead(t *testing.T) mpReading {
	t.Helper()
	content, err := mpReadingContent()
	if err != nil {
		t.Fatal(err)
	}
	var reading mpReading
	if err := json.Unmarshal(content, &reading); err != nil {
		t.Fatalf("decode %s: %v", mpReadingFile, err)
	}
	return reading
}

func mpPlan(t *testing.T) *ptahv1alpha1.PtahMigrationPlan {
	reading := mpRead(t)
	return &reading.Plan
}

func mpApproval(t *testing.T) *ptahv1alpha1.PtahMigrationApproval {
	reading := mpRead(t)
	return &reading.Approval
}

// mpRecorded is the status of one approved run the operator recorded and read back.
func mpRecorded(t *testing.T) ptahv1alpha1.PtahMigrationStatus { return mpRead(t).Migration.Status }

type mpRow struct {
	mutate func(*ptahv1alpha1.PtahMigrationStatus)
	says   string
}

// mpRefused holds a predicate to its fixture and to refusing each mutation, naming
// what each refusal has to say.
func mpRefused(t *testing.T, check func(ptahv1alpha1.PtahMigrationStatus) error, rows map[string]mpRow) {
	t.Helper()
	if err := check(mpRecorded(t)); err != nil {
		t.Fatalf("the operator's reading was refused: %v", err)
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := mpRecorded(t)
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
	plan := mpPlan(t)
	mpRefused(t, func(status ptahv1alpha1.PtahMigrationStatus) error { return runIdentityRecorded(status, plan) }, map[string]mpRow{
		"no run":              {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "no finished run"},
		"an unfinished run":   {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.FinishedAt = nil }, "no finished run"},
		"no operation":        {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.OperationID = "" }, "names operation"},
		"no plan":             {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef = nil }, "names plan"},
		"another plan's name": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef.Name = "another-plan" }, "names plan"},
		"another plan's UID":  {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanRef.UID = "another-plan-uid" }, "names plan"},
		"another fingerprint": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.PlanFingerprint = mpOther }, "names fingerprint"},
		"no artifact":         {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.ArtifactDigest = "" }, "names artifact"},
		"a source the plan does not name": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Source = &ptahv1alpha1.PlanSourceBinding{Kind: ptahv1alpha1.SourceKindGit, Revision: mpOther, ContentDigest: mpOther}
		}, "names source"},
		"another database": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityDigest = mpOther }, "names database"},
		"no database":      {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.TargetIdentityDigest = "" }, "names database"},
		"another binding":  {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.ExecutionBindingID = "" }, "names execution binding"},
	})
}

func TestRunApprovedByRefusesEachClause(t *testing.T) {
	t.Parallel()
	approval := mpApproval(t)
	mpRefused(t, func(status ptahv1alpha1.PtahMigrationStatus) error { return runApprovedBy(status, approval) }, map[string]mpRow{
		"no run":           {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "records nothing"},
		"no authorization": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization = nil }, "records nothing"},
		"the policy": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Kind = ptahv1alpha1.MigrationRunAuthorizedByPolicy
		}, "authorized by"},
		"no approval": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovalRef = nil }, "names approval"},
		"another approval": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.ApprovalRef.Name = "another-approval"
		}, "names approval"},
		"a reused name": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.ApprovalRef.UID = "another-approval-uid"
		}, "names approval"},
		"no approver":      {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.Approver = nil }, "names approver"},
		"another approver": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.Approver.Username = "bob" }, "names approver"},
		"no stamp time":    {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization.ApprovedAt = nil }, "dates the approval"},
		"another stamp time": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			later := metav1.NewTime(s.LastRun.Authorization.ApprovedAt.Add(time.Second))
			s.LastRun.Authorization.ApprovedAt = &later
		}, "dates the approval"},
	})
}

// The isolated-node row's migration applies under Always, and its reading is
// the run nobody decided.
func TestRunAllowedByPolicyRefusesADecision(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../testdata/e2e/readings/isolated-node-durable-migration.json")
	if err != nil {
		t.Fatal(err)
	}
	policy := func(t *testing.T) ptahv1alpha1.PtahMigrationStatus {
		t.Helper()
		migration := &ptahv1alpha1.PtahMigration{}
		if err := json.Unmarshal(content, migration); err != nil {
			t.Fatal(err)
		}
		return migration.Status
	}
	if err := runAllowedByPolicy(policy(t)); err != nil {
		t.Fatalf("a run under the policy was refused: %v", err)
	}
	approved := mpRecorded(t).LastRun.Authorization
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"no run":           func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil },
		"no authorization": func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Authorization = nil },
		"an approval's kind": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Kind = ptahv1alpha1.MigrationRunAuthorizedByApproval
		},
		"an approval named": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.ApprovalRef = approved.ApprovalRef.DeepCopy()
		},
		"an approver named": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.Approver = approved.Approver.DeepCopy()
		},
		"a stamp time": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.LastRun.Authorization.ApprovedAt = approved.ApprovedAt.DeepCopy()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := policy(t)
			mutate(&status)
			if runAllowedByPolicy(status) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestRunVerifiedInSyncRefusesEachClause(t *testing.T) {
	t.Parallel()
	plan := mpPlan(t)
	verifiedAgainst := func(status ptahv1alpha1.PtahMigrationStatus) error { return runVerifiedInSync(status, plan) }
	mpRefused(t, verifiedAgainst, map[string]mpRow{
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
		"a reading from before the run": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Verification.CurrentVersion = 2 }, "before version 3"},
	})

	// The Pod's log is gone and the termination summary holds no versions: the run
	// is as good as it was.
	t.Run("a run with no list of versions", func(t *testing.T) {
		t.Parallel()
		status := mpRecorded(t)
		status.LastRun.AppliedVersions = nil
		if err := verifiedAgainst(status); err != nil {
			t.Fatalf("a correct run was refused: %v", err)
		}
	})
	t.Run("a plan that names no migration", func(t *testing.T) {
		t.Parallel()
		plan := mpPlan(t)
		plan.Spec.Migrations = nil
		if err := runVerifiedInSync(mpRecorded(t), plan); err == nil || !strings.Contains(err.Error(), "names no migration") {
			t.Fatalf("error = %v, want one naming the empty plan", err)
		}
	})
}

// The uncertain row's run left no result behind, so its record names no
// database. Its readings are the ones the uncertain row printed for each engine.
func TestRunNamesNoDatabaseRefusesEachClause(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../testdata/e2e/readings/uncertain-migration-retained-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	held := func(t *testing.T, engine string) ptahv1alpha1.PtahMigrationStatus {
		t.Helper()
		var readings map[string]*ptahv1alpha1.PtahMigration
		if err := json.Unmarshal(content, &readings); err != nil {
			t.Fatal(err)
		}
		if readings[engine] == nil {
			t.Fatalf("the readings hold no %s migration", engine)
		}
		return readings[engine].Status
	}
	approved := mpRecorded(t).LastRun
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			if err := runNamesNoDatabase(held(t, engine)); err != nil {
				t.Fatalf("a run that named no database was refused: %v", err)
			}
			for name, row := range map[string]mpRow{
				"no run": {func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }, "no run"},
				"a database": {func(s *ptahv1alpha1.PtahMigrationStatus) {
					s.LastRun.TargetIdentityDigest = approved.TargetIdentityDigest
				}, "names database"},
				"a reading on it": {func(s *ptahv1alpha1.PtahMigrationStatus) {
					s.LastRun.Verification = approved.Verification.DeepCopy()
				}, "a reading verified"},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					status := held(t, engine)
					row.mutate(&status)
					if err := runNamesNoDatabase(status); err == nil || !strings.Contains(err.Error(), row.says) {
						t.Fatalf("error = %v, want one naming %q", err, row.says)
					}
				})
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
			entry(s, ptahv1alpha1.JournalRunFinished).OperationID = mpOther
		}, "0 RunFinished"},
		"an acceptance of another approval": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalApprovalAccepted).ApprovalUID = "another-approval-uid"
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
			entry(s, ptahv1alpha1.JournalRunFinished).PlanUID = "another-plan-uid"
		}, "names plan"},
		"an entry with no plan": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalVerified).Plan = ""
		}, "names plan"},
		"an end on another Job": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).JobUID = "another-job-uid"
		}, "names Job UID"},
		"an end on no Job": {func(s *ptahv1alpha1.PtahMigrationStatus) {
			entry(s, ptahv1alpha1.JournalRunFinished).JobUID = ""
			s.LastRun.JobUID = ""
		}, "names Job UID"},
	})
}
