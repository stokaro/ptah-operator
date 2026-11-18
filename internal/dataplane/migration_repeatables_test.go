package dataplane_test

import (
	"reflect"
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
// the names and the state, which a refusal needs, and nothing a record says
// about an attempt. A status that lists none reads as before.
func TestDecodeMigrationStatusListsTheRepeatableMigrations(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationStatus([]byte(repeatableStatus))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	want := []dataplane.MigrationRepeatable{{Identity: "account_names", Path: "R__account_names.sql", State: "applied"}}
	if !reflect.DeepEqual(report.Repeatables, want) {
		t.Fatalf("Repeatables = %#v, want %#v", report.Repeatables, want)
	}

	orphaned := strings.Replace(repeatableStatus, `"path": "R__account_names.sql"`, `"path": ""`, 1)
	if report, err := dataplane.DecodeMigrationStatus([]byte(orphaned)); err != nil || len(report.Repeatables) != 1 ||
		report.Repeatables[0].Path != "" {
		t.Fatalf("a row no file carries: %#v, %v", report.Repeatables, err)
	}

	none := strings.Replace(repeatableStatus, `"repeatables": [`, `"ignored": [`, 1)
	if report, err := dataplane.DecodeMigrationStatus([]byte(none)); err != nil || report.Repeatables != nil {
		t.Fatalf("a status with no repeatable: %#v, %v", report.Repeatables, err)
	}

	for name, document := range map[string]string{
		"no identity":            strings.Replace(repeatableStatus, `"identity": "account_names"`, `"identity": ""`, 1),
		"a long identity":        strings.Replace(repeatableStatus, `"identity": "account_names"`, `"identity": "`+strings.Repeat("a", 256)+`"`, 1),
		"a long path":            strings.Replace(repeatableStatus, `"path": "R__account_names.sql"`, `"path": "`+strings.Repeat("d/", 513)+`"`, 1),
		"an object for the list": strings.Replace(repeatableStatus, `"repeatables": [{`, `"repeatables": {"x": [{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodeMigrationStatus([]byte(document)); err == nil {
				t.Fatal("DecodeMigrationStatus() accepted the status")
			}
		})
	}
}

// The runner harness's status of a directory that holds a versioned migration
// and an R__ file, read from the artifact a push made of it at the pinned
// build. The file reaches the operator, and the status names it.
func TestTheCapturedStatusListsTheRepeatableMigration(t *testing.T) {
	t.Parallel()

	report, err := dataplane.DecodeMigrationStatus(loadCapture(t, "migrations-status-repeatable-postgres.json"))
	if err != nil {
		t.Fatalf("DecodeMigrationStatus() error = %v", err)
	}
	want := []dataplane.MigrationRepeatable{{Identity: "item_names", Path: "R__item_names.sql", State: "pending"}}
	if !reflect.DeepEqual(report.Repeatables, want) || !reflect.DeepEqual(report.PendingMigrations, []int64{1}) {
		t.Fatalf("Repeatables = %#v and pending %v, want %#v beside version 1", report.Repeatables, report.PendingMigrations, want)
	}
}
