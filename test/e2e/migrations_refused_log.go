package e2e

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The answer MySQL gives an INSERT whose value is longer than its column, in
// the strict mode Ptah writes its log in: error 1406, SQLSTATE 22001.
const (
	valueTooLongSQLState    = "22001"
	valueTooLongErrorNumber = 1406
)

// narrowMigrationLogTable is Ptah's schema_migrations_log for MySQL with the
// actor column cut to 16 characters. Ptah creates the table only where it is
// missing, so a run writes into this one, and every name the manager records
// is longer: `system:ptah-operator:apply-always:` alone is 34.
const narrowMigrationLogTable = `CREATE TABLE schema_migrations_log (
    run_id VARCHAR(64) NOT NULL,
    seq BIGINT NOT NULL,
    operation VARCHAR(32) NOT NULL,
    version BIGINT NOT NULL,
    state VARCHAR(32) NOT NULL,
    actor VARCHAR(16) NULL,
    actor_source VARCHAR(32) NOT NULL,
    checksum VARCHAR(64) NULL,
    logged_at TIMESTAMP NOT NULL,
    error TEXT NULL,
    PRIMARY KEY (run_id, seq)
) ENGINE=InnoDB`

// everyLogEntryLost holds a migration's last run to a log the database refused
// in full: the run applied, its record counts no entry written and two lost for
// each migration it applied, a start and an outcome, each refused for a value
// too long. The run's one RunFinished entry carries the same record, and
// RunLogged says the database's record is incomplete, at the generation the run
// ran for and naming the run and the code, in the status write that recorded
// it.
func everyLogEntryLost(migration *ptahv1alpha1.PtahMigration) error {
	status := migration.Status
	run := status.LastRun
	if run == nil || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied || len(run.AppliedVersions) == 0 {
		return fmt.Errorf("the last run is %+v, want an applied run", run)
	}
	if err := allEntriesLost(run.MigrationLog, run.AppliedVersions); err != nil {
		return fmt.Errorf("status.lastRun.migrationLog: %w", err)
	}
	var ends []ptahv1alpha1.JournalEntry
	if status.Journal != nil {
		for _, entry := range status.Journal.Entries {
			if entry.Type == ptahv1alpha1.JournalRunFinished && entry.OperationID == run.OperationID {
				ends = append(ends, entry)
			}
		}
	}
	if len(ends) != 1 {
		return fmt.Errorf("the journal holds %d RunFinished entries for operation %s, want one", len(ends), run.OperationID)
	}
	if end := ends[0]; end.Outcome != ptahv1alpha1.JournalSucceeded || !reflect.DeepEqual(end.MigrationLog, run.MigrationLog) {
		return fmt.Errorf("the run's RunFinished reads %s with log %+v, want Succeeded with the log lastRun holds", end.Outcome, end.MigrationLog)
	}
	logged := meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionMigrationRunLogged)
	if logged == nil {
		return fmt.Errorf("the migration has no RunLogged, want False with %s", ptahv1alpha1.ReasonLogEntriesLost)
	}
	if logged.Status != metav1.ConditionFalse || logged.Reason != string(ptahv1alpha1.ReasonLogEntriesLost) {
		return fmt.Errorf("RunLogged is %s with %s, want False with %s", logged.Status, logged.Reason, ptahv1alpha1.ReasonLogEntriesLost)
	}
	if logged.ObservedGeneration != migration.Generation {
		return fmt.Errorf("RunLogged was written for generation %d, and the migration is at %d", logged.ObservedGeneration, migration.Generation)
	}
	lost := run.MigrationLog.Lost
	for _, want := range []string{
		fmt.Sprintf("refused %d of the %d entries", lost, lost),
		"operation " + run.OperationID,
		fmt.Sprintf("SQLSTATE %s (error %d)", valueTooLongSQLState, valueTooLongErrorNumber),
	} {
		if !strings.Contains(logged.Message, want) {
			return fmt.Errorf("RunLogged says %q, which does not name %q", logged.Message, want)
		}
	}
	return nil
}

// allEntriesLost holds a log record to the one a run of applied leaves when
// the database refuses every entry for a value too long.
func allEntriesLost(log *ptahv1alpha1.MigrationLogStatus, applied []int64) error {
	if log == nil {
		return fmt.Errorf("no record")
	}
	if want := 2 * len(applied); log.Written != 0 || int(log.Lost) != want {
		return fmt.Errorf("%d written and %d lost, want 0 and %d for %d applied migrations", log.Written, log.Lost, want, len(applied))
	}
	if kept := min(int(log.Lost), 16); len(log.Losses) != kept {
		return fmt.Errorf("names %d lost entries of %d, want %d", len(log.Losses), log.Lost, kept)
	}
	named := map[int64][]ptahv1alpha1.MigrationLogEntryState{}
	for _, loss := range log.Losses {
		if !slices.Contains(applied, loss.Version) {
			return fmt.Errorf("names version %d, which the run did not apply", loss.Version)
		}
		if loss.SQLState != valueTooLongSQLState || loss.ErrorNumber != valueTooLongErrorNumber {
			return fmt.Errorf("version %d %s was refused with SQLSTATE %q and error %d, want %s and %d",
				loss.Version, loss.State, loss.SQLState, loss.ErrorNumber, valueTooLongSQLState, valueTooLongErrorNumber)
		}
		named[loss.Version] = append(named[loss.Version], loss.State)
	}
	if len(log.Losses) < int(log.Lost) {
		return nil
	}
	for _, version := range applied {
		want := []ptahv1alpha1.MigrationLogEntryState{ptahv1alpha1.MigrationLogEntryStarted, ptahv1alpha1.MigrationLogEntryApplied}
		if !slices.Equal(named[version], want) {
			return fmt.Errorf("names %v for version %d, want its start and then its outcome", named[version], version)
		}
	}
	return nil
}
