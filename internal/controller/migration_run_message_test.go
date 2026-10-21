package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// TestARunMessageNamesTheMigrationItStoppedIn holds the message a failed or
// partial run leaves in lastRun and in its Event to the migration the run's
// own report says it stopped in (#678), and to nothing the engine said.
func TestARunMessageNamesTheMigrationItStoppedIn(t *testing.T) {
	t.Parallel()

	described := func(dirty *dataplane.MigrationDirty, records ...dataplane.MigrationRecord) *dataplane.MigrationStatusReport {
		return &dataplane.MigrationStatusReport{
			ContractVersion: dataplane.SupportedMigrationStatusContract,
			Migrations:      records,
			DirtyRevision:   dirty,
		}
	}
	ledger := []dataplane.MigrationRecord{
		{Version: 1, Description: "Create Journal", State: dataplane.MigrationStateApplied},
		{Version: 2, Description: "Read Ledger", State: dataplane.MigrationStateDirty},
		{Version: 3, Description: "Index Journal", State: dataplane.MigrationStatePending},
	}
	failed := func(status *dataplane.MigrationStatusReport) dataplane.MigrationRunReport {
		return dataplane.MigrationRunReport{
			Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{1, 2, 3}, Applied: []int64{1},
			Error: `relation "ledger_entries" does not exist`, Status: status,
		}
	}
	partial := func(status *dataplane.MigrationStatusReport) dataplane.MigrationRunReport {
		return dataplane.MigrationRunReport{
			Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{2, 3},
			Error: `Duplicate entry 'DE' for key 'countries.PRIMARY'`, Status: status,
		}
	}
	failing := func(report dataplane.MigrationRunReport, failure dataplane.MigrationRunFailure) dataplane.MigrationRunReport {
		report.Failure = &failure
		return report
	}
	missingTable := dataplane.MigrationRunFailure{Version: 2, Statement: 2, Statements: 3, SQLState: "42P01"}
	duplicateKey := dataplane.MigrationRunFailure{Version: 2, Statement: 3, Statements: 5, SQLState: "23000", ErrorNumber: 1062}
	for _, row := range []struct {
		name   string
		report dataplane.MigrationRunReport
		want   string
	}{
		{
			name:   "the revision row the failed run left dirty",
			report: failed(described(&dataplane.MigrationDirty{Version: 2, Total: 3}, ledger...)),
			want:   "Migration 2 (Read Ledger) failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			// A run that left no dirty row stopped before the migration it
			// selected next, and that migration is the one it names.
			name:   "no dirty row, so the first selected version the run did not apply",
			report: failed(described(nil, ledger...)),
			want:   "Migration 2 (Read Ledger) failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name:   "the dirty row wins over the selection",
			report: failed(described(&dataplane.MigrationDirty{Version: 3, Total: 1}, ledger...)),
			want:   "Migration 3 (Index Journal) failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name:   "no history after the run",
			report: failed(nil),
			want:   "Migration 2 failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name: "a version the history does not describe",
			report: failed(described(nil, dataplane.MigrationRecord{
				Version: 2, Description: " \t\n", State: dataplane.MigrationStateDirty,
			})),
			want: "Migration 2 failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name: "a run that selected nothing it did not apply",
			report: dataplane.MigrationRunReport{
				Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{1}, Applied: []int64{1},
			},
			want: "A migration failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name:   "the statement that failed and the SQLSTATE the database gave",
			report: failing(failed(described(nil, ledger...)), missingTable),
			want: "Migration 2 (Read Ledger) failed at statement 2 of 3 with SQLSTATE 42P01 and committed nothing; " +
				"1 migrations before it are recorded applied",
		},
		{
			name: "MySQL's error number beside its SQLSTATE",
			report: failing(failed(described(&dataplane.MigrationDirty{Version: 2, Total: 3}, ledger...)),
				dataplane.MigrationRunFailure{Version: 2, Statement: 1, Statements: 3, SQLState: "42S02", ErrorNumber: 1146}),
			want: "Migration 2 (Read Ledger) failed at statement 1 of 3 with SQLSTATE 42S02 (error 1146) and committed nothing; " +
				"1 migrations before it are recorded applied",
		},
		{
			name: "an error number with no SQLSTATE",
			report: failing(failed(described(nil, ledger...)),
				dataplane.MigrationRunFailure{Version: 2, Statement: 1, Statements: 3, ErrorNumber: 1146}),
			want: "Migration 2 (Read Ledger) failed at statement 1 of 3 with error 1146 and committed nothing; " +
				"1 migrations before it are recorded applied",
		},
		{
			name: "a statement the database never answered",
			report: failing(failed(described(nil, ledger...)),
				dataplane.MigrationRunFailure{Version: 2, Statement: 2, Statements: 3}),
			want: "Migration 2 (Read Ledger) failed at statement 2 of 3 and committed nothing; " +
				"1 migrations before it are recorded applied",
		},
		{
			// The dirty row names migration 3, so the failure about migration 2
			// stays out rather than lend 3 a statement it did not run.
			name: "a failure about another migration than the one named",
			report: failing(failed(described(&dataplane.MigrationDirty{Version: 3, Total: 1}, ledger...)),
				missingTable),
			want: "Migration 3 (Index Journal) failed and committed nothing; 1 migrations before it are recorded applied",
		},
		{
			name:   "a partial run's failed statement and the database's answer",
			report: failing(partial(described(&dataplane.MigrationDirty{Version: 2, Applied: 2, Total: 5}, ledger...)), duplicateKey),
			want: "Migration 2 (Read Ledger) committed 2 of its 5 statements, then failed with SQLSTATE 23000 (error 1062); " +
				"retrying the file would run them twice",
		},
		{
			name: "a partial run the database never answered",
			report: failing(partial(described(&dataplane.MigrationDirty{Version: 2, Applied: 2, Total: 5}, ledger...)),
				dataplane.MigrationRunFailure{Version: 2, Statement: 3, Statements: 5}),
			want: "Migration 2 (Read Ledger) committed 2 of its 5 statements and not the rest; " +
				"retrying the file would run them twice",
		},
		{
			name: "a partial run's failure about another migration",
			report: failing(partial(described(&dataplane.MigrationDirty{Version: 2, Applied: 2, Total: 5}, ledger...)),
				dataplane.MigrationRunFailure{Version: 3, Statement: 1, Statements: 1, SQLState: "23000"}),
			want: "Migration 2 (Read Ledger) committed 2 of its 5 statements and not the rest; " +
				"retrying the file would run them twice",
		},
		{
			name:   "a partial run's dirty row counts its statements",
			report: partial(described(&dataplane.MigrationDirty{Version: 2, Applied: 2, Total: 5}, ledger...)),
			want: "Migration 2 (Read Ledger) committed 2 of its 5 statements and not the rest; " +
				"retrying the file would run them twice",
		},
		{
			name:   "a partial run with no dirty row to count",
			report: partial(described(nil, ledger...)),
			want:   "A migration committed some of its statements and not the rest; retrying the file would run them twice",
		},
		{
			name:   "a dirty row that counts no statements",
			report: partial(described(&dataplane.MigrationDirty{Version: 2, Applied: 2}, ledger...)),
			want:   "A migration committed some of its statements and not the rest; retrying the file would run them twice",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := migrationRunMessage(row.report); got != row.want {
				t.Errorf("migrationRunMessage() = %q\nwant %q", got, row.want)
			}
		})
	}
}

// TestARunMessageKeepsItsPointInsideTheEvent holds a description the artifact
// controls to a bound, so the version, the counts and the database's code
// after it survive the 256 bytes the MigrationRunFinished Event keeps of the
// message. Every number is the largest the decoder admits.
func TestARunMessageKeepsItsPointInsideTheEvent(t *testing.T) {
	t.Parallel()

	const version = 9223372036854775807
	// Three bytes before the two-byte runes put the cut inside one.
	long := "a\x1bb" + strings.Repeat("é", 40) + strings.Repeat("x", 300)
	status := func(dirty *dataplane.MigrationDirty) *dataplane.MigrationStatusReport {
		return &dataplane.MigrationStatusReport{
			Migrations:    []dataplane.MigrationRecord{{Version: version, Description: long}},
			DirtyRevision: dirty,
		}
	}
	failure := &dataplane.MigrationRunFailure{
		Version: version, Statement: 2147483647, Statements: 2147483647, SQLState: "HY000", ErrorNumber: 65535,
	}
	for _, row := range []struct {
		name   string
		report dataplane.MigrationRunReport
		tail   string
	}{
		{
			name: "a partial run",
			report: dataplane.MigrationRunReport{
				Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{version},
				Status: status(&dataplane.MigrationDirty{Version: version, Applied: 2147483646, Total: 2147483647}),
			},
			tail: " committed 2147483646 of its 2147483647 statements and not the rest; retrying the file would run them twice",
		},
		{
			name: "a partial run with the database's answer",
			report: dataplane.MigrationRunReport{
				Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{version}, Failure: failure,
				Status: status(&dataplane.MigrationDirty{Version: version, Applied: 2147483646, Total: 2147483647}),
			},
			tail: " committed 2147483646 of its 2147483647 statements, then failed with SQLSTATE HY000 (error 65535); " +
				"retrying the file would run them twice",
		},
		{
			name: "a failed run with the statement and the database's answer",
			report: dataplane.MigrationRunReport{
				Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{version}, Failure: failure,
				Status: status(&dataplane.MigrationDirty{Version: version, Total: 2147483647}),
			},
			tail: " failed at statement 2147483647 of 2147483647 with SQLSTATE HY000 (error 65535) and committed nothing; " +
				"0 migrations before it are recorded applied",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			message := migrationRunMessage(row.report)
			if len(message) > 256 {
				t.Errorf("the message is %d bytes, more than the Event keeps: %q", len(message), message)
			}
			if !utf8.ValidString(message) {
				t.Errorf("the message is not valid UTF-8: %q", message)
			}
			const head = "Migration 9223372036854775807 (a b"
			if !strings.HasPrefix(message, head+strings.Repeat("é", 30)+"...)") || !strings.HasSuffix(message, row.tail) {
				t.Errorf("message = %q", message)
			}
		})
	}
}

// TestARunMessageFromPtahsOwnDocuments writes the message for the run
// documents Ptah v0.14.0 printed (see internal/dataplane/testdata), so the
// sentence a reader sees is the one a real failure on each engine produces.
func TestARunMessageFromPtahsOwnDocuments(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		file string
		want string
	}{
		{
			file: "migrations-up-failed-postgres.json",
			want: "Migration 2 (Read Ledger) failed at statement 2 of 3 with SQLSTATE 42P01 and committed nothing; " +
				"1 migrations before it are recorded applied",
		},
		{
			file: "migrations-up-partial-mysql.json",
			want: "Migration 2 (Fill Ledger) committed 2 of its 4 statements, then failed with SQLSTATE 23000 (error 1062); " +
				"retrying the file would run them twice",
		},
	} {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", row.file))
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeMigrationRun(data)
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			if got := migrationRunMessage(report); got != row.want {
				t.Errorf("migrationRunMessage() = %q\nwant %q", got, row.want)
			}
		})
	}
}
