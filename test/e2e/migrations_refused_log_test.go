package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// rlReadingFile is what refusedLogProof printed for the MySQL migration whose
// log entries the database refused, matched on the status write that recorded
// the run. testdata/e2e/readings/README.md says in which run.
const rlReadingFile = "../../testdata/e2e/readings/refused-log-mysql.json"

// rlRead decodes the reading afresh, so a mutation changes one copy only.
func rlRead(t *testing.T) *ptahv1alpha1.PtahMigration {
	t.Helper()
	content, err := os.ReadFile(rlReadingFile)
	if err != nil {
		t.Fatal(err)
	}
	var migration ptahv1alpha1.PtahMigration
	if err := json.Unmarshal(content, &migration); err != nil {
		t.Fatalf("decode %s: %v", rlReadingFile, err)
	}
	return &migration
}

// rlEnd is the reading's RunFinished entry of its last run.
func rlEnd(t *testing.T, migration *ptahv1alpha1.PtahMigration) *ptahv1alpha1.JournalEntry {
	t.Helper()
	for index, entry := range migration.Status.Journal.Entries {
		if entry.Type == ptahv1alpha1.JournalRunFinished && entry.OperationID == migration.Status.LastRun.OperationID {
			return &migration.Status.Journal.Entries[index]
		}
	}
	t.Fatal("the reading holds no RunFinished entry of its last run")
	return nil
}

func rlLogged(t *testing.T, migration *ptahv1alpha1.PtahMigration) *metav1.Condition {
	t.Helper()
	logged := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationRunLogged)
	if logged == nil {
		t.Fatal("the reading holds no RunLogged")
	}
	return logged
}

// The operator's own reading passes, and each copy of it with one clause
// broken is refused for that clause: the ways a run's record could say less, or
// something else, than that the database refused every entry it was given.
func TestEveryLogEntryLostRefusesEachClause(t *testing.T) {
	t.Parallel()

	reading := rlRead(t)
	if err := everyLogEntryLost(reading); err != nil {
		t.Fatalf("the operator's reading was refused: %v", err)
	}
	if applied := len(reading.Status.LastRun.AppliedVersions); applied == 0 || reading.Status.LastRun.MigrationLog.Lost != int32(2*applied) {
		t.Fatalf("the reading is not a run that lost every entry: %+v", reading.Status.LastRun)
	}
	for name, row := range map[string]struct {
		mutate func(*testing.T, *ptahv1alpha1.PtahMigration)
		says   string
	}{
		"a run that failed": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed
		}, "want an applied run"},
		"a run with no record": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog = nil
		}, "status.lastRun.migrationLog: no record"},
		"an entry written": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Written = 1
		}, "1 written"},
		"one entry fewer lost": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Lost--
		}, "want 0 and"},
		"an entry left unnamed": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			log := m.Status.LastRun.MigrationLog
			log.Losses = log.Losses[:len(log.Losses)-1]
		}, "lost entries of"},
		"a version the run did not apply": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Losses[0].Version = 999
		}, "names version 999"},
		"another answer": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Losses[0].ErrorNumber = 1366
		}, "error 1366"},
		"another SQLSTATE": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Losses[1].SQLState = "HY000"
		}, `SQLSTATE "HY000"`},
		"a start named twice": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.MigrationLog.Losses[1].State = ptahv1alpha1.MigrationLogEntryStarted
		}, "its start and then its outcome"},
		"a journal entry with no record": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			rlEnd(t, m).MigrationLog = nil
		}, "want Succeeded with the log lastRun holds"},
		"a journal entry with another count": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			end := rlEnd(t, m)
			end.MigrationLog = end.MigrationLog.DeepCopy()
			end.MigrationLog.Lost--
		}, "want Succeeded with the log lastRun holds"},
		"a journal entry of another operation": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			rlEnd(t, m).OperationID = "sha256:" + strings.Repeat("0", 64)
		}, "0 RunFinished entries"},
		"a journal entry that failed": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			rlEnd(t, m).Outcome = ptahv1alpha1.JournalFailed
		}, "RunFinished reads Failed"},
		"RunLogged True": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			logged := rlLogged(t, m)
			logged.Status, logged.Reason = metav1.ConditionTrue, string(ptahv1alpha1.ReasonLogWritten)
		}, "want False with LogEntriesLost"},
		"no RunLogged": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			meta.RemoveStatusCondition(&m.Status.Conditions, ptahv1alpha1.ConditionMigrationRunLogged)
		}, "the migration has no RunLogged"},
		"RunLogged for an earlier generation": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			m.Generation++
		}, "written for generation"},
		"RunLogged that names another run": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			logged := rlLogged(t, m)
			logged.Message = strings.ReplaceAll(logged.Message, m.Status.LastRun.OperationID, "sha256:"+strings.Repeat("0", 64))
		}, "does not name \"operation sha256:"},
		"RunLogged that names no code": {func(t *testing.T, m *ptahv1alpha1.PtahMigration) {
			logged := rlLogged(t, m)
			logged.Message = strings.ReplaceAll(logged.Message, " (error 1406)", "")
		}, `does not name "SQLSTATE 22001 (error 1406)"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := rlRead(t)
			row.mutate(t, migration)
			err := everyLogEntryLost(migration)
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("everyLogEntryLost() = %v, want an error naming %q", err, row.says)
			}
		})
	}
}
