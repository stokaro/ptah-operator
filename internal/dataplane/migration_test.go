package dataplane_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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

// Ptah reports when each revision row was written and how long its migration
// ran, and no duration for a table too old to record one. The decoder reads
// both, and the result frame the runner writes carries them to the controller
// unchanged.
func TestDecodeMigrationStatus_ReadsWhenEachRowRan(t *testing.T) {
	t.Parallel()
	document := `{"contract_version":1,"current_version":2,"total_migrations":3,"has_pending_changes":true,
	  "pending_migrations":[3],
	  "migrations":[
	    {"version":1,"description":"create accounts","checksum":"h1:one","applied_checksum":"h1:one",
	     "applied_at":"2026-10-24T07:42:30.123456Z","execution_time_ms":1840,"state":"applied"},
	    {"version":2,"checksum":"h1:two","applied_checksum":"h1:two","applied_at":"2026-10-24T07:42:31Z","state":"applied"},
	    {"version":3,"checksum":"h1:three","state":"pending"}
	  ]}`
	ranAt := time.Date(2026, 10, 24, 7, 42, 30, 123456000, time.UTC)

	report, err := dataplane.DecodeMigrationStatus([]byte(document))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	frame, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode the result frame: %v", err)
	}
	relayed, err := dataplane.DecodeMigrationStatus(frame)
	if err != nil {
		t.Fatalf("decode the result frame: %v", err)
	}

	for _, read := range []dataplane.MigrationStatusReport{report, relayed} {
		first, second, third := read.Migrations[0], read.Migrations[1], read.Migrations[2]
		if first.AppliedAt == nil || !first.AppliedAt.Equal(ranAt) || first.ExecutionMs == nil || *first.ExecutionMs != 1840 {
			t.Fatalf("version 1 ran at %v for %v ms, want %v for 1840", first.AppliedAt, first.ExecutionMs, ranAt)
		}
		if second.AppliedAt == nil || second.ExecutionMs != nil {
			t.Fatalf("version 2 ran at %v for %v ms, want a time and no duration", second.AppliedAt, second.ExecutionMs)
		}
		if third.AppliedAt != nil || third.ExecutionMs != nil {
			t.Fatalf("pending version 3 ran at %v for %v ms, want neither", third.AppliedAt, third.ExecutionMs)
		}
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
		{
			name: "a time on a migration with no revision row",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":true,
			  "migrations":[{"version":1,"checksum":"a","applied_at":"2026-10-24T07:42:30Z","state":"pending"}]}`,
			message: `migration status reports when version 1 ran, and its state "pending" has no revision row`,
		},
		{
			name: "a duration on a checkpoint-covered migration",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","execution_time_ms":3,"state":"checkpoint-covered"}]}`,
			message: `migration status reports when version 1 ran, and its state "checkpoint-covered" has no revision row`,
		},
		{
			name: "a duration and no time",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","applied_checksum":"a","execution_time_ms":3,"state":"applied"}]}`,
			message: "migration status reports how long version 1 ran and not when",
		},
		{
			name: "a negative duration",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","applied_checksum":"a","applied_at":"2026-10-24T07:42:30Z",
			  "execution_time_ms":-1,"state":"applied"}]}`,
			message: "migration status reports version 1 ran for -1 milliseconds",
		},
		{
			name: "a description no file name holds",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"description":"` + strings.Repeat("d", 256) + `","checksum":"a","state":"applied"}]}`,
			message: "migration status reports a description of 256 bytes for version 1, longer than a file name holds",
		},
		{
			name: "a checksum no digest spells",
			document: `{"contract_version":1,"total_migrations":1,"has_pending_changes":false,
			  "migrations":[{"version":1,"checksum":"a","applied_checksum":"` + strings.Repeat("c", 129) + `","state":"applied"}]}`,
			message: "migration status reports a checksum longer than 128 bytes for version 1",
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

// runWithFailure is a run document carrying the failure given, for the rows
// that hold a failure to the run around it.
func runWithFailure(outcome, planned, applied, failure string) string {
	return `{"contract_version":1,"direction":"up","outcome":"` + outcome + `","planned":` + planned +
		`,"applied":` + applied + `,"failure":` + failure + `}`
}

// The two fixtures are `ptah migrations up --json` documents, captured from
// the Ptah v0.14.0 release image (ghcr.io/stokaro/ptah@sha256:6c7a5d62...)
// against PostgreSQL 17 and MySQL 8.4 and stored as it printed them. Both run two migrations; the second fails. On PostgreSQL its
// second of three statements reads a table that does not exist, and the run
// commits nothing of it. On MySQL its first statement creates a table, so the
// duplicate key that stops its third of four leaves two committed: a partial
// run, and an error sentence that quotes the row.
func loadMigrationRun(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestDecodeMigrationRun_ReadsPtahsFailure reads the failure out of what
// Ptah actually printed for each engine the operator supports.
func TestDecodeMigrationRun_ReadsPtahsFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		file    string
		outcome string
		want    dataplane.MigrationRunFailure
	}{
		{
			file:    "migrations-up-failed-postgres.json",
			outcome: dataplane.MigrationOutcomeFailed,
			want:    dataplane.MigrationRunFailure{Version: 2, Statement: 2, Statements: 3, SQLState: "42P01"},
		},
		{
			file:    "migrations-up-partial-mysql.json",
			outcome: dataplane.MigrationOutcomePartial,
			want:    dataplane.MigrationRunFailure{Version: 2, Statement: 3, Statements: 4, SQLState: "23000", ErrorNumber: 1062},
		},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeMigrationRun(loadMigrationRun(t, test.file))
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			if report.Outcome != test.outcome || report.Failure == nil || *report.Failure != test.want {
				t.Fatalf("outcome %q failure %+v, want %q %+v", report.Outcome, report.Failure, test.outcome, test.want)
			}
		})
	}
}

// TestDecodeMigrationRun_ReadsTheFailedStatement reads a failure on the
// outcomes and at the bounds the captures above do not reach.
func TestDecodeMigrationRun_ReadsTheFailedStatement(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		document string
		want     dataplane.MigrationRunFailure
	}{
		{
			name: "a lost connection leaves no answer",
			document: runWithFailure("unknown", `[2]`, `[]`,
				`{"version":2,"version_key":"2","statement":1,"statements":1}`),
			want: dataplane.MigrationRunFailure{Version: 2, Statement: 1, Statements: 1},
		},
		{
			name: "the largest counts and number the decoder admits",
			document: runWithFailure("failed", `[2]`, `[]`,
				`{"version":2,"statement":2147483647,"statements":2147483647,"sqlstate":"HY000","error_number":65535}`),
			want: dataplane.MigrationRunFailure{
				Version: 2, Statement: 2147483647, Statements: 2147483647, SQLState: "HY000", ErrorNumber: 65535,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeMigrationRun([]byte(test.document))
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			if report.Failure == nil || *report.Failure != test.want {
				t.Fatalf("failure = %+v, want %+v", report.Failure, test.want)
			}
		})
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
		{
			name:     "a failed statement in a run that applied everything",
			document: runWithFailure("applied", `[2]`, `[2]`, `{"version":2,"statement":1,"statements":1}`),
			message:  `migration run reports outcome "applied" and a failed statement`,
		},
		{
			name:     "a failed statement in a run that was up to date",
			document: runWithFailure("up-to-date", `[2]`, `[]`, `{"version":2,"statement":1,"statements":1}`),
			message:  `migration run reports outcome "up-to-date" and a failed statement`,
		},
		{
			name:     "a failed statement in a version the run did not select",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":3,"statement":1,"statements":1}`),
			message: "migration run reports a statement of version 3 failed, " +
				"and that version is not one it selected and left unapplied",
		},
		{
			name:     "a failed statement in a version the run applied",
			document: runWithFailure("failed", `[1,2]`, `[1]`, `{"version":1,"statement":1,"statements":1}`),
			message: "migration run reports a statement of version 1 failed, " +
				"and that version is not one it selected and left unapplied",
		},
		{
			name:     "statement zero",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":0,"statements":3}`),
			message:  "migration run reports statement 0 of 3 failed",
		},
		{
			name:     "a statement past the last one",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":4,"statements":3}`),
			message:  "migration run reports statement 4 of 3 failed",
		},
		{
			name:     "more statements than an int32 counts",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":1,"statements":2147483648}`),
			message:  "migration run reports statement 1 of 2147483648 failed",
		},
		{
			name:     "a SQLSTATE in lower case",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":1,"statements":1,"sqlstate":"42p01"}`),
			message:  "migration run reports a SQLSTATE that is not five digits or capital letters",
		},
		{
			name:     "a SQLSTATE of four characters",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":1,"statements":1,"sqlstate":"42P0"}`),
			message:  "migration run reports a SQLSTATE that is not five digits or capital letters",
		},
		{
			name: "a SQLSTATE that carries a value",
			document: runWithFailure("failed", `[2]`, `[]`,
				`{"version":2,"statement":1,"statements":1,"sqlstate":"23505: Key (email)=(alice@example.com)"}`),
			message: "migration run reports a SQLSTATE that is not five digits or capital letters",
		},
		{
			name:     "a negative error number",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":1,"statements":1,"error_number":-1}`),
			message:  "migration run reports error number -1, which the two bytes MySQL sends cannot hold",
		},
		{
			name:     "an error number past two bytes",
			document: runWithFailure("failed", `[2]`, `[]`, `{"version":2,"statement":1,"statements":1,"error_number":65536}`),
			message:  "migration run reports error number 65536, which the two bytes MySQL sends cannot hold",
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
