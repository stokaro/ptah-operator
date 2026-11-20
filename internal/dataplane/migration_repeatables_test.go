package dataplane_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// repeatableStatus is the status Ptah v0.30.0 wrote against PostgreSQL 17 after
// `migrations up` ran one versioned migration and one repeatable one, with the
// timings and the version keys as it wrote them.
const repeatableStatus = `{"current_version": 1, "applied_migrations": [1], "applied_migration_keys": ["1"],` +
	` "pending_migrations": [], "out_of_order_migrations": [], "total_migrations": 1, "has_pending_changes": false,` +
	` "contract_version": 2, "command": "migrations status", "migrations": [{"version": 1, "version_key": "1",` +
	` "description": "Create Accounts", "checksum": "710741faa5adccc9707585ccc493ac09c9a79ddc909f02eb63c6de5b093177d8",` +
	` "applied_checksum": "710741faa5adccc9707585ccc493ac09c9a79ddc909f02eb63c6de5b093177d8",` +
	` "applied_at": "2026-11-17T19:21:32.131856Z", "execution_time_ms": 4, "state": "applied"}],` +
	` "repeatables": [{"identity": "account_names", "path": "R__account_names.sql",` +
	` "checksum": "h1:j6+sdTOhl65koArSbkjIe9vIvm5m957K+Jh4OQARNAM=",` +
	` "applied_checksum": "h1:j6+sdTOhl65koArSbkjIe9vIvm5m957K+Jh4OQARNAM=", "state": "applied",` +
	` "attempt_checksum": "h1:j6+sdTOhl65koArSbkjIe9vIvm5m957K+Jh4OQARNAM=", "attempt_order": 1,` +
	` "attempted_at": "2026-11-17T19:21:32.139354Z", "applied_at": "2026-11-17T19:21:32.141573Z"}],` +
	` "current_version_key": "1"}`

// Since Ptah v0.30.0 a status lists repeatable migrations. The operator reads
// what a plan and a refusal need of each record, and nothing a record says
// about an attempt's error. A status that lists none reads as before.
func TestDecodeMigrationStatusListsTheRepeatableMigrations(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationStatus([]byte(repeatableStatus))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	const content = "h1:j6+sdTOhl65koArSbkjIe9vIvm5m957K+Jh4OQARNAM="
	// The record's state, which the migration's own "state" key precedes.
	const state = `"applied_checksum": "` + content + `", "state": "applied"`
	stated := func(value string) string {
		return strings.Replace(repeatableStatus, state, `"applied_checksum": "`+content+`", "state": "`+value+`"`, 1)
	}
	want := []dataplane.MigrationRepeatable{{
		Identity: "account_names", Path: "R__account_names.sql", Checksum: content, AppliedChecksum: content, State: "applied",
	}}
	if !reflect.DeepEqual(report.Repeatables, want) {
		t.Fatalf("Repeatables = %#v, want %#v", report.Repeatables, want)
	}

	// A record no file carries, as Ptah v0.33.1 writes one.
	orphaned := strings.Replace(strings.Replace(stated("orphaned"),
		`"path": "R__account_names.sql",`, `"path": "", "orphaned": true,`, 1),
		` "checksum": "`+content+`",`, ` "checksum": "",`, 1)
	if report, err := dataplane.DecodeMigrationStatus([]byte(orphaned)); err != nil || len(report.Repeatables) != 1 ||
		!report.Repeatables[0].Orphaned || report.Repeatables[0].Path != "" || report.Repeatables[0].Checksum != "" {
		t.Fatalf("a record no file carries: %#v, %v", report.Repeatables, err)
	}

	none := strings.Replace(repeatableStatus, `"repeatables": [`, `"ignored": [`, 1)
	if report, err := dataplane.DecodeMigrationStatus([]byte(none)); err != nil || report.Repeatables != nil {
		t.Fatalf("a status with no repeatable: %#v, %v", report.Repeatables, err)
	}

	second := `, {"identity": "account_names", "path": "R__account_names.sql", "checksum": "` + content + `", "state": "pending"}]`
	const (
		unnamed   = "with no identity, or with a name longer than a file name holds"
		malformed = "a repeatable migration file without a path, an identity a file name can carry, or an h1 digest"
	)
	for name, row := range map[string]struct{ document, says string }{
		"no identity":     {strings.Replace(repeatableStatus, `"identity": "account_names"`, `"identity": ""`, 1), unnamed},
		"a long identity": {strings.Replace(repeatableStatus, `"identity": "account_names"`, `"identity": "`+strings.Repeat("a", 256)+`"`, 1), unnamed},
		"a long path":     {strings.Replace(repeatableStatus, `"path": "R__account_names.sql"`, `"path": "`+strings.Repeat("d/", 513)+`"`, 1), unnamed},
		"an object for the list": {strings.Replace(repeatableStatus, `"repeatables": [{`, `"repeatables": {"x": [{`, 1),
			"decode migration status"},
		"a state nobody knows": {stated("skipped"), `repeatable state "skipped", which this operator does not know`},
		"a file identity a file name cannot carry": {strings.Replace(repeatableStatus,
			`"identity": "account_names"`, `"identity": "account names"`, 1), malformed},
		"a file without a path": {strings.Replace(repeatableStatus, `"path": "R__account_names.sql"`, `"path": ""`, 1), malformed},
		"a file without a digest": {strings.Replace(repeatableStatus,
			` "checksum": "`+content+`",`, ` "checksum": "",`, 1), malformed},
		"a digest that is not h1": {strings.Replace(repeatableStatus,
			` "checksum": "`+content+`",`, ` "checksum": "sha256:`+strings.Repeat("a", 64)+`",`, 1), malformed},
		"an applied digest that is not h1": {strings.Replace(repeatableStatus,
			`"applied_checksum": "`+content+`"`, `"applied_checksum": "h1:x"`, 1), "whose applied digest is not an h1 digest"},
		"applied content that is not the file's": {strings.Replace(repeatableStatus,
			`"applied_checksum": "`+content+`"`, `"applied_checksum": "h1:`+strings.Repeat("B", 43)+`="`, 1),
			"an applied repeatable migration whose content is not the applied one"},
		"a change that changed nothing":       {stated("changed"), "a changed repeatable migration whose content did not change"},
		"a pending file with applied content": {stated("pending"), "a pending repeatable migration with applied content"},
		"an orphaned state on a file":         {stated("orphaned"), "an orphaned repeatable migration that a file carries"},
		"a record no file carries, with a file": {strings.Replace(repeatableStatus,
			`"path": "R__account_names.sql",`, `"path": "R__account_names.sql", "orphaned": true,`, 1),
			"a repeatable record no file carries, with a file"},
		"one identity twice": {strings.Replace(repeatableStatus, `"applied_at": "2026-11-17T19:21:32.141573Z"}]`,
			`"applied_at": "2026-11-17T19:21:32.141573Z"}`+second, 1), "lists one repeatable migration twice"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if row.document == repeatableStatus {
				t.Fatal("the edit matched nothing, so the row would measure the unedited status")
			}
			_, err := dataplane.DecodeMigrationStatus([]byte(row.document))
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("DecodeMigrationStatus() error = %v, want one that says %q", err, row.says)
			}
		})
	}
}

// The runner harness's status of a directory that holds a versioned migration
// and an R__ file, read from the artifact a push made of it at the pinned
// build. The file reaches the operator, and the status names it as the next
// run's work.
func TestTheCapturedStatusListsTheRepeatableMigration(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationStatus(loadCapture(t, "migrations-status-repeatable-postgres.json"))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	want := []dataplane.MigrationRepeatable{{
		Identity: "item_names", Path: "R__item_names.sql", Checksum: "h1:AAkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4=", State: "pending",
	}}
	if !reflect.DeepEqual(report.Repeatables, want) || !reflect.DeepEqual(report.PendingMigrations, []int64{1}) ||
		!reflect.DeepEqual(report.PendingRepeatables(), want) {
		t.Fatalf("Repeatables = %#v and pending %v, want %#v beside version 1", report.Repeatables, report.PendingMigrations, want)
	}
}

// repeatableRun is a run that applied version 1 and then two repeatable
// migrations, one new and one changed, with every log entry written.
const repeatableRun = `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[1],"applied":[1],` +
	`"planned_repeatables":[{"identity":"a","path":"R__a.sql","checksum":"h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","reason":"new"},` +
	`{"identity":"b","path":"R__b.sql","checksum":"h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",` +
	`"applied_checksum":"h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=","reason":"changed"}],` +
	`"applied_repeatables":["a","b"],"migration_log":{"written":6},` +
	`"status":{"contract_version":2,"current_version":1,"total_migrations":1,"has_pending_changes":false}}`

// A run's repeatable selection is held to the run: each file once, with an
// identity a file name carries, an h1 digest and a reason Ptah gives, each
// applied one selected and in the order selected, and the log counted over
// both kinds. Each refusal is one edit of a document the decoder accepts.
func TestDecodeMigrationRunHoldsTheRepeatableSelectionToTheRun(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationRun([]byte(repeatableRun))
	if err != nil {
		t.Fatalf("DecodeMigrationRun() error = %v", err)
	}
	if !slices.Equal(report.PlannedRepeatableIdentities(), []string{"a", "b"}) || !slices.Equal(report.AppliedRepeatables, []string{"a", "b"}) {
		t.Fatalf("selected %v and applied %v, want both a and b", report.PlannedRepeatableIdentities(), report.AppliedRepeatables)
	}
	lostSelected := strings.Replace(repeatableRun, `"migration_log":{"written":6}`,
		`"migration_log":{"written":5,"lost":[{"operation":"up","version":0,"version_key":"","repeatable":"a","state":"applied"}]}`, 1)
	if report, err := dataplane.DecodeMigrationRun([]byte(lostSelected)); err != nil || report.MigrationLog.Lost[0].Repeatable != "a" {
		t.Fatalf("a lost entry of a selected repeatable migration: %#v, %v", report.MigrationLog, err)
	}

	const (
		unselected = "a repeatable migration applied that it did not select, or out of the order it selected them"
		unchanged  = "repeatable migration b as changed, and its content did not change"
		unnamed    = "without an identity a file name can carry, or without a path"
		lost       = "a lost migration log entry of a repeatable migration it did not select, or that also names a version"
	)
	for name, edit := range map[string][3]string{
		"an applied one it did not select":  {`"applied_repeatables":["a","b"]`, `"applied_repeatables":["a","c"]`, unselected},
		"applied out of the order selected": {`"applied_repeatables":["a","b"]`, `"applied_repeatables":["b","a"]`, unselected},
		"a reason nobody knows":             {`"reason":"new"`, `"reason":"maybe"`, `for reason "maybe", which this operator does not know`},
		"a new one with applied content": {`"reason":"new"`,
			`"applied_checksum":"h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=","reason":"new"`, "repeatable migration a as new, with applied content"},
		"a changed one that did not change": {`"applied_checksum":"h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC="`,
			`"applied_checksum":"h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="`, unchanged},
		"a changed one with no applied content": {`,"applied_checksum":"h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC="`, ``, unchanged},
		"one selected twice":                    {`"identity":"b"`, `"identity":"a"`, "selected repeatable migration a twice"},
		"an identity no file name carries":      {`"identity":"a"`, `"identity":"a b"`, unnamed},
		"a digest that is not h1": {`"checksum":"h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="`, `"checksum":"h1:x"`,
			"repeatable migration a without an h1 digest"},
		"a selected file without a path": {`"path":"R__a.sql"`, `"path":""`, unnamed},
		"more log entries than two for each": {`"migration_log":{"written":6}`, `"migration_log":{"written":7}`,
			"7 migration log entries for 3 selected migrations"},
		"a lost entry of one it did not select": {`"migration_log":{"written":6}`,
			`"migration_log":{"written":5,"lost":[{"operation":"up","version":0,"version_key":"","repeatable":"c","state":"applied"}]}`, lost},
		"a lost entry naming a version too": {`"migration_log":{"written":6}`,
			`"migration_log":{"written":5,"lost":[{"operation":"up","version":1,"version_key":"1","repeatable":"a","state":"applied"}]}`, lost},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := strings.Replace(repeatableRun, edit[0], edit[1], 1)
			if document == repeatableRun {
				t.Fatal("the edit matched nothing, so the row would measure the unedited run")
			}
			_, err := dataplane.DecodeMigrationRun([]byte(document))
			if err == nil || !strings.Contains(err.Error(), edit[2]) {
				t.Fatalf("DecodeMigrationRun() error = %v, want one that says %q", err, edit[2])
			}
		})
	}
}
