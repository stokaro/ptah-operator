package controller

import (
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
// controls to a bound, so the version and the counts after it survive the 256
// bytes the MigrationRunFinished Event keeps of the message.
func TestARunMessageKeepsItsPointInsideTheEvent(t *testing.T) {
	t.Parallel()

	// Three bytes before the two-byte runes put the cut inside one.
	long := "a\x1bb" + strings.Repeat("é", 40) + strings.Repeat("x", 300)
	report := dataplane.MigrationRunReport{
		Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{9223372036854775807},
		Status: &dataplane.MigrationStatusReport{
			Migrations: []dataplane.MigrationRecord{{Version: 9223372036854775807, Description: long}},
			DirtyRevision: &dataplane.MigrationDirty{
				Version: 9223372036854775807, Applied: 2147483646, Total: 2147483647,
			},
		},
	}
	message := migrationRunMessage(report)
	if len(message) > 256 {
		t.Errorf("the message is %d bytes, more than the Event keeps: %q", len(message), message)
	}
	if !utf8.ValidString(message) {
		t.Errorf("the message is not valid UTF-8: %q", message)
	}
	const tail = "committed 2147483646 of its 2147483647 statements and not the rest; retrying the file would run them twice"
	if !strings.HasPrefix(message, "Migration 9223372036854775807 (a b"+strings.Repeat("é", 30)+"...)") ||
		!strings.HasSuffix(message, tail) {
		t.Errorf("message = %q", message)
	}
}
