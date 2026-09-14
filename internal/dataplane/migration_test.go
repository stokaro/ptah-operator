package dataplane_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const migrationStatusDocument = `{
  "contract_version": 1,
  "current_version": 2,
  "checkpoint_version": 0,
  "applied_migrations": [1, 2],
  "pending_migrations": [3],
  "out_of_order_migrations": [],
  "total_migrations": 3,
  "has_pending_changes": true,
  "migrations": [
    {"version": 1, "version_key": "1", "checksum": "h1:one", "applied_checksum": "h1:one", "state": "applied"},
    {"version": 2, "version_key": "2", "checksum": "h1:two", "applied_checksum": "h1:two", "state": "applied"},
    {"version": 3, "version_key": "3", "checksum": "h1:three", "state": "pending", "transaction_mode": "none"}
  ]
}`

func TestDecodeMigrationStatus_ReadsWhatAPlanNeeds(t *testing.T) {
	report, err := dataplane.DecodeMigrationStatus([]byte(migrationStatusDocument))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	if report.CurrentVersion != 2 {
		t.Fatalf("current version = %d, want 2", report.CurrentVersion)
	}
	pending := report.Pending()
	if len(pending) != 1 || pending[0] != 3 {
		t.Fatalf("pending = %v, want [3]", pending)
	}
	if len(report.Modified()) != 0 {
		t.Fatalf("modified = %v, want none", report.Modified())
	}
	if report.Migrations[2].TransactionMode != "none" {
		t.Fatalf("transaction mode = %q, want none", report.Migrations[2].TransactionMode)
	}
}

func TestDecodeMigrationStatus_NamesAModifiedMigration(t *testing.T) {
	document := `{"contract_version":1,"current_version":1,"total_migrations":1,"has_pending_changes":false,
	  "migrations":[{"version":1,"checksum":"h1:new","applied_checksum":"h1:old","state":"modified"}]}`

	report, err := dataplane.DecodeMigrationStatus([]byte(document))

	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	modified := report.Modified()
	if len(modified) != 1 || modified[0] != 1 {
		t.Fatalf("modified = %v, want [1]", modified)
	}
}

func TestDecodeMigrationStatus_FailurePath(t *testing.T) {
	tests := []struct {
		name     string
		document string
		message  string
	}{
		{
			name:     "unknown contract version",
			document: `{"contract_version":2,"total_migrations":0,"has_pending_changes":false}`,
			message:  "migration status contract version 2 is not the 1 this operator reads",
		},
		{
			name: "unknown state",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"h1:one","state":"quarantined"}]}`,
			message: `migration status reports state "quarantined" for version 1, which this operator does not know`,
		},
		{
			name: "repeated version",
			document: `{"contract_version":1,"total_migrations":2,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","state":"applied"},{"version":1,"checksum":"b","state":"applied"}]}`,
			message: "migration status lists version 1 twice",
		},
		{
			name: "more migrations than it counts",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","state":"applied"},{"version":2,"checksum":"b","state":"pending"}]}`,
			message: "migration status lists 2 migrations and counts 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := dataplane.DecodeMigrationStatus([]byte(test.document))
			if err == nil || err.Error() != test.message {
				t.Fatalf("DecodeMigrationStatus() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestDecodeMigrationRun_ReadsTheEvidence(t *testing.T) {
	document := `{"contract_version":1,"direction":"up","outcome":"partial","planned":[3],"applied":[],
	  "error":"failed to apply migration 3",
	  "status":{"contract_version":1,"current_version":2,"total_migrations":3,"has_pending_changes":true,
	    "dirty_revision":{"version":3,"applied":2,"total":5}}}`

	report, err := dataplane.DecodeMigrationRun([]byte(document))

	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	if report.Outcome != dataplane.MigrationOutcomePartial {
		t.Fatalf("outcome = %q, want partial", report.Outcome)
	}
	if report.Status == nil || report.Status.DirtyRevision == nil {
		t.Fatal("run document lost the dirty revision it carried")
	}
	if report.Status.DirtyRevision.Applied != 2 {
		t.Fatalf("dirty applied = %d, want 2", report.Status.DirtyRevision.Applied)
	}
}

func TestDecodeMigrationRun_FailurePath(t *testing.T) {
	tests := []struct {
		name     string
		document string
		message  string
	}{
		{
			name:     "unknown outcome",
			document: `{"contract_version":1,"direction":"up","outcome":"probably-fine"}`,
			message:  `migration run reports outcome "probably-fine", which this operator does not know`,
		},
		{
			name:     "down direction",
			document: `{"contract_version":1,"direction":"down","outcome":"applied"}`,
			message:  `migration run reports direction "down"; this operator dispatches up only`,
		},
		{
			name:     "applied what it never planned",
			document: `{"contract_version":1,"direction":"up","outcome":"applied","planned":[1],"applied":[1,2]}`,
			message:  "migration run reports version 2 applied and never planned",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := dataplane.DecodeMigrationRun([]byte(test.document))
			if err == nil || err.Error() != test.message {
				t.Fatalf("DecodeMigrationRun() error = %v, want %q", err, test.message)
			}
		})
	}
}
