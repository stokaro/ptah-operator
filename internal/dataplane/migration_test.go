package dataplane_test

import (
	"encoding/json"
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

// A database that bootstrapped from a checkpoint reports the versions the
// checkpoint replaced as pending, and reports at the same time that its next
// run would execute nothing. Both are true: the records say what happened to
// each file, and the selection says what runs next, and only the second is the
// question a plan asks. This document is what the pinned Ptah printed after a
// checkpoint bootstrap against PostgreSQL.
func TestDecodeMigrationStatus_ReadsTheSelectionRatherThanRecountingIt(t *testing.T) {
	document := `{"contract_version":1,"current_version":4,"total_migrations":4,
	  "applied_migrations":[3,4],"pending_migrations":[],"has_pending_changes":false,
	  "migrations":[
	    {"version":1,"checksum":"h1:one","state":"pending"},
	    {"version":2,"checksum":"h1:two","state":"pending"},
	    {"version":3,"checksum":"h1:three","applied_checksum":"h1:three","checkpoint":true,"state":"applied"},
	    {"version":4,"checksum":"h1:four","applied_checksum":"h1:four","state":"applied"}
	  ]}`

	report, err := dataplane.DecodeMigrationStatus([]byte(document))

	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	if pending := report.Pending(); len(pending) != 0 {
		t.Fatalf("pending = %v, want none: the checkpoint covers 1 and 2 and Ptah selected nothing", pending)
	}
}

// A document that carries no selection is read the old way rather than as an
// empty one, so a build that reports only states still produces a plan.
func TestDecodeMigrationStatus_FallsBackToTheStatesWhenNoSelectionIsPublished(t *testing.T) {
	document := `{"contract_version":1,"current_version":1,"total_migrations":2,"has_pending_changes":true,
	  "migrations":[
	    {"version":1,"checksum":"h1:one","applied_checksum":"h1:one","state":"applied"},
	    {"version":2,"checksum":"h1:two","state":"pending"}
	  ]}`

	report, err := dataplane.DecodeMigrationStatus([]byte(document))

	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	pending := report.Pending()
	if len(pending) != 1 || pending[0] != 2 {
		t.Fatalf("pending = %v, want [2]", pending)
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

// TestEmptySelectionSurvivesTheResultFrame measures the round trip the report
// actually makes, because that is where the answer was lost.
//
// The runner decodes Ptah's document, and encodes the same value again into its
// result frame; the controller decodes the frame. An empty selection means the
// next run would execute nothing, which is the whole answer after a checkpoint
// bootstrap -- the covered versions still read pending one by one, and only
// Ptah's own selection says they will not run. A field dropped on the way
// through reaches the controller as an absent one, and Pending falls back to
// the states, publishes a plan for migrations that cannot run, and publishes it
// again after every run that correctly did nothing.
func TestEmptySelectionSurvivesTheResultFrame(t *testing.T) {
	t.Parallel()

	// The document Ptah prints for a database that bootstrapped from the
	// checkpoint at version 3: three and four ran, one and two never will.
	bootstrapped := []byte(`{"contract_version":1,"current_version":4,` +
		`"pending_migrations":[],` +
		`"migrations":[{"version":1,"state":"pending"},{"version":2,"state":"pending"},` +
		`{"version":3,"state":"applied"},{"version":4,"state":"applied"}]}`)

	var report dataplane.MigrationStatusReport
	if err := json.Unmarshal(bootstrapped, &report); err != nil {
		t.Fatalf("decode Ptah's document: %v", err)
	}
	if got := report.Pending(); len(got) != 0 {
		t.Fatalf("Ptah's document decodes to pending %v, want none", got)
	}

	frame, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode the result frame: %v", err)
	}
	var carried map[string]json.RawMessage
	if err := json.Unmarshal(frame, &carried); err != nil {
		t.Fatalf("read the result frame: %v", err)
	}
	if _, present := carried["pending_migrations"]; !present {
		t.Fatal("the result frame dropped pending_migrations, so an empty selection cannot be told from an absent one")
	}

	var relayed dataplane.MigrationStatusReport
	if err := json.Unmarshal(frame, &relayed); err != nil {
		t.Fatalf("decode the result frame: %v", err)
	}
	if got := relayed.Pending(); len(got) != 0 {
		t.Fatalf("after the result frame pending is %v, want none", got)
	}
}
