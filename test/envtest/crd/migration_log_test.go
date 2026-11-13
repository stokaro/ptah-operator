package crd_test

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// lostEntries is n entries of version 3 refused as MySQL refuses an actor
// longer than its column.
func lostEntries(n int) []any {
	entries := make([]any, 0, n)
	for range n {
		entries = append(entries, map[string]any{"version": int64(3), "state": "Started", "sqlState": "22001", "errorNumber": int64(1406)})
	}
	return entries
}

// A run's migration log record is held to the codes a database answers with
// and to its own count, on status.lastRun and on the journal entry that copies
// it, so a record that says less was lost than it names, or quotes a value in
// a code, is never published.
func TestPtahMigrationStatusHoldsARunsMigrationLog(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-log")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store PtahMigration %s: %v", migration.GetName(), err)
	}
	const counted = "losses lists the first lost entries, up to 16, and lost counts them all"
	loss := func(extra map[string]any) map[string]any {
		return map[string]any{"written": int64(1), "lost": int64(1), "losses": []any{merged(lostEntries(1)[0].(map[string]any), extra)}}
	}
	// Each record is written at both places it is kept, under the path the API
	// server names for each.
	places := []struct {
		path  []string
		value func(log map[string]any) any
		field string
	}{
		{
			path: []string{"status", "lastRun"},
			value: func(log map[string]any) any {
				return map[string]any{"outcome": "Applied", "startedAt": "2026-10-24T07:41:53Z", "migrationLog": log}
			},
			field: "status.lastRun.migrationLog",
		},
		{
			path: []string{"status", "journal"},
			value: func(log map[string]any) any {
				return journal(journalEntry(map[string]any{"type": "RunFinished", "outcome": "Succeeded", "migrationLog": log}))
			},
			field: "status.journal.entries[0].migrationLog",
		},
	}
	for _, test := range []struct {
		name  string
		log   map[string]any
		field string
		says  string
	}{
		{"a version before the first", loss(map[string]any{"version": int64(0)}), ".losses[0].version", "greater than or equal to 1"},
		{"a state no up run writes", loss(map[string]any{"state": "RolledBack"}), ".losses[0].state", "Unsupported value"},
		{"a SQLSTATE that is not one", loss(map[string]any{"sqlState": "22001: oidc:alice"}), ".losses[0].sqlState", "should match"},
		{"an error number two bytes cannot hold", loss(map[string]any{"errorNumber": int64(65536)}), ".losses[0].errorNumber", "less than or equal to 65535"},
		{"a negative error number", loss(map[string]any{"errorNumber": int64(-1)}), ".losses[0].errorNumber", "greater than or equal to 0"},
		{"a negative count written", map[string]any{"written": int64(-1), "lost": int64(0)}, ".written", "greater than or equal to 0"},
		{"no count written", map[string]any{"lost": int64(0)}, ".written", "Required value"},
		{"no count lost", map[string]any{"written": int64(2)}, ".lost", "Required value"},
		{"a 17th entry", map[string]any{"written": int64(0), "lost": int64(17), "losses": lostEntries(17)}, ".losses", "must have at most 16 items"},
		{"more lost than it names", map[string]any{"written": int64(0), "lost": int64(2), "losses": lostEntries(1)}, "", counted},
		{"fewer lost than it names", map[string]any{"written": int64(0), "lost": int64(1), "losses": lostEntries(2)}, "", counted},
		{"lost entries it names none of", map[string]any{"written": int64(0), "lost": int64(1)}, "", counted},
		{"fewer than sixteen named of more than sixteen lost", map[string]any{"written": int64(0), "lost": int64(18), "losses": lostEntries(15)}, "", counted},
		{"entries named and none lost", map[string]any{"written": int64(1), "lost": int64(0), "losses": lostEntries(1)}, "", counted},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, place := range places {
				observed := reread(t, migration)
				set(t, observed, place.value(test.log), place.path...)
				err := api.Status().Update(context.Background(), observed, client.DryRunAll)
				if mismatch := refusalMismatch(err, cause{place.field + test.field, test.says}); mismatch != nil {
					t.Errorf("%s of PtahMigration %s: %v", place.field, migration.GetName(), mismatch)
				}
			}
		})
	}

	// A field the schema does not declare is pruned without a complaint, so
	// what came back is what says the record reached the CRD.
	for _, test := range []struct {
		name string
		log  map[string]any
	}{
		{"every entry written", map[string]any{"written": int64(4), "lost": int64(0)}},
		{"an up-to-date run that wrote nothing", map[string]any{"written": int64(0), "lost": int64(0)}},
		{"a MySQL refusal", loss(nil)},
		{"a refusal with no answer", map[string]any{"written": int64(1), "lost": int64(1), "losses": []any{
			map[string]any{"version": int64(3), "state": "Applied"},
		}}},
		{"sixteen named of eighteen lost", map[string]any{"written": int64(0), "lost": int64(18), "losses": lostEntries(16)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, place := range places {
				observed := reread(t, migration)
				set(t, observed, place.value(test.log), place.path...)
				if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
					t.Fatalf("the API server refused a well-formed %s on PtahMigration %s: %v", place.field, migration.GetName(), err)
				}
				var kept map[string]any
				var err error
				if place.path[1] == "lastRun" {
					kept, _, err = unstructured.NestedMap(observed.Object, "status", "lastRun", "migrationLog")
				} else {
					entries, _, sliceErr := unstructured.NestedSlice(observed.Object, "status", "journal", "entries")
					if err = sliceErr; err == nil && len(entries) == 1 {
						kept, _ = entries[0].(map[string]any)["migrationLog"].(map[string]any)
					}
				}
				if err != nil || !reflect.DeepEqual(kept, test.log) {
					t.Fatalf("%s came back as %v, want %v: %v", place.field, kept, test.log, err)
				}
			}
		})
	}
}
