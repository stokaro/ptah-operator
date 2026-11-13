package dataplane_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// runWithLog is an applied run of versions 1 and 2 carrying the log report
// given, with the history Ptah read after the run.
func runWithLog(log string) string {
	return `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied",` +
		`"planned":[1,2],"planned_keys":["1","2"],"applied":[1,2],"applied_keys":["1","2"],"migration_log":` + log +
		`,"status":{"contract_version":2,"current_version":2,"total_migrations":2,"has_pending_changes":false}}`
}

// lostActor is one entry as Ptah v0.26.0 reports an actor its MySQL column
// cannot hold, message included.
const lostActor = `{"operation":"up","version":1,"version_key":"1","state":"started","sqlstate":"22001",` +
	`"error_number":1406,"message":"Error 1406 (22001): Data too long for column 'actor' at row 1"}`

func TestDecodeMigrationRun_ReadsTheMigrationLog(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		log  string
		want *dataplane.MigrationLogReport
	}{
		{
			name: "every entry written",
			log:  `{"written":4}`,
			want: &dataplane.MigrationLogReport{Written: 4},
		},
		{
			name: "an entry refused",
			log:  `{"written":3,"lost":[` + lostActor + `]}`,
			want: &dataplane.MigrationLogReport{Written: 3, Lost: []dataplane.MigrationLogLoss{{
				Operation: "up", Version: 1, VersionKey: "1", State: dataplane.MigrationLogStarted,
				SQLState: "22001", ErrorNumber: 1406,
				Message: "Error 1406 (22001): Data too long for column 'actor' at row 1",
			}}},
		},
		{
			// Two for each selected migration is the most a run writes, and
			// a server that gave no answer leaves no code.
			name: "every entry refused, with and without an answer",
			log: `{"written":0,"lost":[` +
				`{"operation":"up","version":1,"version_key":"1","state":"started","sqlstate":"HY000","error_number":65535,"message":"m"},` +
				`{"operation":"up","version":1,"version_key":"1","state":"applied","message":"m"},` +
				`{"operation":"up","version":2,"version_key":"","state":"started","message":"m"},` +
				`{"operation":"up","version":2,"version_key":"2","state":"failed","message":"m"}]}`,
			want: &dataplane.MigrationLogReport{Lost: []dataplane.MigrationLogLoss{
				{Operation: "up", Version: 1, VersionKey: "1", State: "started", SQLState: "HY000", ErrorNumber: 65535, Message: "m"},
				{Operation: "up", Version: 1, VersionKey: "1", State: "applied", Message: "m"},
				{Operation: "up", Version: 2, State: "started", Message: "m"},
				{Operation: "up", Version: 2, VersionKey: "2", State: "failed", Message: "m"},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeMigrationRun([]byte(runWithLog(test.log)))
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			if !reflect.DeepEqual(report.MigrationLog, test.want) {
				t.Fatalf("migration log = %+v, want %+v", report.MigrationLog, test.want)
			}
			// The outcome reads the revision table, and a lost entry does not
			// move it.
			if report.Outcome != dataplane.MigrationOutcomeApplied {
				t.Fatalf("outcome = %q, want applied", report.Outcome)
			}
		})
	}
}

// The pinned Ptah wrote this run against a MySQL whose log table holds an
// actor of 16 characters, so the database refused every entry with error 1406
// while every migration applied.
func TestDecodeMigrationRun_ReadsPtahsRefusedLog(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationRun(loadMigrationRun(t, "migrations-up-log-lost-mysql.json"))
	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	if report.Outcome != dataplane.MigrationOutcomeApplied || !reflect.DeepEqual(report.Applied, []int64{1, 2, 3}) {
		t.Fatalf("outcome %q applied %v, want every version applied", report.Outcome, report.Applied)
	}
	log := report.MigrationLog
	if log == nil || log.Written != 0 || len(log.Lost) != 6 {
		t.Fatalf("migration log = %+v, want all six entries lost", log)
	}
	for index, loss := range log.Lost {
		state := []string{dataplane.MigrationLogStarted, dataplane.MigrationLogApplied}[index%2]
		if loss.Operation != "up" || loss.Version != int64(index/2+1) || loss.State != state ||
			loss.SQLState != "22001" || loss.ErrorNumber != 1406 || !strings.Contains(loss.Message, "Data too long for column 'actor'") {
			t.Fatalf("lost entry %d = %+v, want version %d %s refused with 22001/1406", index, loss, index/2+1, state)
		}
	}
}

// A run that keeps no log, and one from a Ptah before v0.23.0, carries no
// report, and that is not a report of nothing lost.
func TestDecodeMigrationRun_ReadsNoLogAsNoReport(t *testing.T) {
	t.Parallel()

	document := `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[1],"applied":[1],` +
		`"status":{"contract_version":2,"current_version":1,"total_migrations":1,"has_pending_changes":false}}`
	report, err := dataplane.DecodeMigrationRun([]byte(document))
	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	if report.MigrationLog != nil {
		t.Fatalf("migration log = %+v, want none", report.MigrationLog)
	}
	if lost := report.MigrationLog.LostCount(); lost != 0 {
		t.Fatalf("LostCount() of no report = %d, want 0", lost)
	}
}

func TestDecodeMigrationRun_RefusesALogTheRunCannotHaveWritten(t *testing.T) {
	t.Parallel()

	entry := func(fields string) string {
		return `{"written":0,"lost":[{` + fields + `,"message":"m"}]}`
	}
	for _, test := range []struct {
		name string
		log  string
		says string
	}{
		{
			name: "a negative count written",
			log:  `{"written":-1}`,
			says: "migration run reports -1 migration log entries written",
		},
		{
			name: "more entries written than two for each selected migration",
			log:  `{"written":5}`,
			says: "migration run reports 5 migration log entries for 2 selected migrations, and a run writes two for each",
		},
		{
			name: "more entries written and lost than two for each selected migration",
			log:  `{"written":4,"lost":[` + lostActor + `]}`,
			says: "migration run reports 5 migration log entries for 2 selected migrations, and a run writes two for each",
		},
		{
			name: "a rollback's entry",
			log:  entry(`"operation":"down","version":1,"version_key":"1","state":"rolled_back"`),
			says: `migration run reports a lost migration log entry of operation "down"; this operator dispatches up only`,
		},
		{
			name: "a rollback's entry that names no migration",
			log:  entry(`"operation":"down","version":0,"version_key":""`),
			says: `migration run reports a lost migration log entry of operation "down"; this operator dispatches up only`,
		},
		{
			name: "a state only a rollback writes",
			log:  entry(`"operation":"up","version":1,"version_key":"1","state":"rolled_back"`),
			says: `migration run reports a lost migration log entry in state "rolled_back", which an up run does not write`,
		},
		{
			name: "no state",
			log:  entry(`"operation":"up","version":1,"version_key":"1"`),
			says: `migration run reports a lost migration log entry in state "", which an up run does not write`,
		},
		{
			name: "a version the run did not select",
			log:  entry(`"operation":"up","version":3,"version_key":"3","state":"started"`),
			says: "migration run reports a lost migration log entry of version 3, which is not one it selected",
		},
		{
			name: "version zero",
			log:  entry(`"operation":"up","version":0,"version_key":"0","state":"started"`),
			says: "migration run reports a lost migration log entry of version 0, which is not one it selected",
		},
		{
			name: "a version key longer than a file name",
			log:  entry(`"operation":"up","version":1,"version_key":"` + strings.Repeat("1", 256) + `","state":"started"`),
			says: "migration run reports a lost migration log entry whose version key is 256 bytes, longer than a file name holds",
		},
		{
			name: "a SQLSTATE that carries a value",
			log:  entry(`"operation":"up","version":1,"version_key":"1","state":"started","sqlstate":"22001: oidc:José"`),
			says: "migration run reports a lost migration log entry with a SQLSTATE that is not five digits or capital letters",
		},
		{
			name: "a SQLSTATE in lower case",
			log:  entry(`"operation":"up","version":1,"version_key":"1","state":"started","sqlstate":"hy000"`),
			says: "migration run reports a lost migration log entry with a SQLSTATE that is not five digits or capital letters",
		},
		{
			name: "a negative error number",
			log:  entry(`"operation":"up","version":1,"version_key":"1","state":"started","error_number":-1`),
			says: "migration run reports a lost migration log entry with error number -1, which the two bytes MySQL sends cannot hold",
		},
		{
			name: "an error number past two bytes",
			log:  entry(`"operation":"up","version":1,"version_key":"1","state":"started","error_number":65536`),
			says: "migration run reports a lost migration log entry with error number 65536, which the two bytes MySQL sends cannot hold",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := dataplane.DecodeMigrationRun([]byte(runWithLog(test.log)))
			if err == nil || err.Error() != test.says {
				t.Fatalf("DecodeMigrationRun() error = %v, want %q", err, test.says)
			}
		})
	}
}

// An up-to-date run selected nothing and writes no entry, so the most it may
// report is nothing written.
func TestDecodeMigrationRun_RefusesALogOfARunThatSelectedNothing(t *testing.T) {
	t.Parallel()

	upToDate := func(log string) string {
		return `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"up-to-date","migration_log":` + log +
			`,"status":{"contract_version":2,"current_version":2,"total_migrations":2,"has_pending_changes":false}}`
	}
	if _, err := dataplane.DecodeMigrationRun([]byte(upToDate(`{"written":0}`))); err != nil {
		t.Fatalf("DecodeMigrationRun() refused an up-to-date run that wrote nothing: %v", err)
	}
	_, err := dataplane.DecodeMigrationRun([]byte(upToDate(`{"written":1}`)))
	if want := "migration run reports 1 migration log entries for 0 selected migrations, and a run writes two for each"; err == nil || err.Error() != want {
		t.Fatalf("DecodeMigrationRun() error = %v, want %q", err, want)
	}
}

// What leaves the runner is the report without any message, and it decodes
// on the other side as the same entries.
func TestClearMessagesKeepsEveryEntryAndItsCode(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationRun([]byte(runWithLog(`{"written":3,"lost":[` + lostActor + `]}`)))
	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	report.MigrationLog.ClearMessages()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Data too long") || strings.Contains(string(encoded), `"message"`) {
		t.Fatalf("the encoded report still carries the message: %s", encoded)
	}
	var decoded dataplane.MigrationRunReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	want := &dataplane.MigrationLogReport{Written: 3, Lost: []dataplane.MigrationLogLoss{{
		Operation: "up", Version: 1, VersionKey: "1", State: "started", SQLState: "22001", ErrorNumber: 1406,
	}}}
	if !reflect.DeepEqual(decoded.MigrationLog, want) {
		t.Fatalf("decoded log = %+v, want %+v", decoded.MigrationLog, want)
	}
	var none *dataplane.MigrationLogReport
	none.ClearMessages()
}
