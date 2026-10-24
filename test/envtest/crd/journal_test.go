package crd_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func journalEntry(parts ...map[string]any) map[string]any {
	return merged(append([]map[string]any{{"type": "PlanPublished", "at": "2026-10-24T07:33:10Z", "plan": "ptah-plan-a"}}, parts...)...)
}

func journal(entries ...any) map[string]any {
	return map[string]any{"coveredSince": "2026-10-20T08:12:03Z", "entries": entries}
}

// The journal is written by the controller alone, and the schema is what keeps
// a malformed record from being published: an event the API does not name, a
// reason that is a message, an unbounded name, a fold of one attempt, a 65th
// entry.
func TestStatusRefusesAMalformedJournal(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "journal")
	schema := schemaBase(namespace)()
	migration := migrationBase(namespace)()
	for _, object := range []*unstructured.Unstructured{schema, migration} {
		if err := api.Create(context.Background(), object); err != nil {
			t.Fatalf("store %s %s: %v", object.GetKind(), object.GetName(), err)
		}
	}
	var tooMany []any
	for index := range 65 {
		tooMany = append(tooMany, journalEntry(map[string]any{"plan": fmt.Sprintf("ptah-plan-%02d", index)}))
	}

	for _, test := range []struct {
		name    string
		journal map[string]any
		want    cause
	}{
		{"an event the API does not name", journal(journalEntry(map[string]any{"type": "DriftDetected"})),
			cause{"status.journal.entries[0].type", "Unsupported value"}},
		{"a reason that is a message", journal(journalEntry(map[string]any{"type": "OperationFailed", "operation": "Observe", "reason": "observation failed"})),
			cause{"status.journal.entries[0].reason", "should match"}},
		{"an operation the controllers do not run", journal(journalEntry(map[string]any{"type": "OperationFailed", "operation": "Repair", "reason": "Failed"})),
			cause{"status.journal.entries[0].operation", "Unsupported value"}},
		{"an outcome the API does not name", journal(journalEntry(map[string]any{"type": "ApplyFinished", "outcome": "Done"})),
			cause{"status.journal.entries[0].outcome", "Unsupported value"}},
		{"a fold of one attempt", journal(journalEntry(map[string]any{"type": "OperationFailed", "operation": "Observe", "reason": "Failed", "count": int64(1)})),
			cause{"status.journal.entries[0].count", "greater than or equal to 2"}},
		{"an unbounded approver", journal(journalEntry(map[string]any{"type": "ApprovalAccepted", "approver": strings.Repeat("a", 513)})),
			cause{"status.journal.entries[0].approver", "Too long"}},
		{"a 65th entry", journal(tooMany...),
			cause{"status.journal.entries", "must have at most 64 items"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, object := range []*unstructured.Unstructured{schema, migration} {
				observed := reread(t, object)
				set(t, observed, test.journal, "status", "journal")
				err := api.Status().Update(context.Background(), observed, client.DryRunAll)
				if mismatch := refusalMismatch(err, test.want); mismatch != nil {
					t.Errorf("status of %s %s: %v", object.GetKind(), object.GetName(), mismatch)
				}
			}
		})
	}

	t.Run("a complete journal is published", func(t *testing.T) {
		t.Parallel()
		complete := journal(
			journalEntry(map[string]any{"statements": int64(2), "destructive": true}),
			journalEntry(map[string]any{"type": "ApprovalAccepted", "approval": "approve-a", "approver": "alice"}),
			journalEntry(map[string]any{"type": "ApplyFinished", "startedAt": "2026-10-24T07:41:53Z", "outcome": "Succeeded"}),
			journalEntry(map[string]any{"type": "Verified", "outcome": "InSync"}),
			journalEntry(map[string]any{
				"type": "RunFinished", "outcome": "Failed", "appliedVersions": int64(2), "startedAt": "2026-10-24T07:41:53Z",
				"failure": map[string]any{"version": int64(16), "statement": int64(2), "statements": int64(3), "sqlState": "42P01"},
			}),
			journalEntry(map[string]any{"type": "RunAcknowledged", "acknowledgment": "ack-a", "acknowledgedBy": "bob"}),
			journalEntry(map[string]any{
				"type": "OperationFailed", "operation": "Observe", "reason": "ObservationFailed",
				"lastAt": "2026-10-24T08:14:00Z", "count": int64(5),
			}),
		)
		for _, object := range []*unstructured.Unstructured{schema, migration} {
			observed := reread(t, object)
			set(t, observed, complete, "status", "journal")
			if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
				t.Fatalf("the API server refused a well-formed journal on %s %s: %v", object.GetKind(), object.GetName(), err)
			}
		}
	})
}

// status.lastRun.failure is held to the forms the run report's decoder admits,
// so a record the decoder accepted is never one the API server refuses.
func TestPtahMigrationStatusRefusesAMalformedRunFailure(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "run-failure")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store PtahMigration %s: %v", migration.GetName(), err)
	}
	run := func(failure map[string]any) map[string]any {
		return map[string]any{
			"outcome": "Failed", "startedAt": "2026-10-24T07:41:53Z",
			"failure": merged(map[string]any{"version": int64(16), "statement": int64(2), "statements": int64(3)}, failure),
		}
	}
	for _, test := range []struct {
		name    string
		failure map[string]any
		want    cause
	}{
		{"a SQLSTATE that is not one", map[string]any{"sqlState": "42p01"}, cause{"status.lastRun.failure.sqlState", "should match"}},
		{"a statement before the first", map[string]any{"statement": int64(0)}, cause{"status.lastRun.failure.statement", "greater than or equal to 1"}},
		{"an error number two bytes cannot hold", map[string]any{"errorNumber": int64(65536)}, cause{"status.lastRun.failure.errorNumber", "less than or equal to 65535"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, migration)
			set(t, observed, run(test.failure), "status", "lastRun")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("status of PtahMigration %s: %v", migration.GetName(), mismatch)
			}
		})
	}

	t.Run("a MySQL failure is published", func(t *testing.T) {
		t.Parallel()
		observed := reread(t, migration)
		set(t, observed, run(map[string]any{"sqlState": "23000", "errorNumber": int64(1062)}), "status", "lastRun")
		if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
			t.Fatalf("the API server refused a well-formed run failure on PtahMigration %s: %v", migration.GetName(), err)
		}
	})
}
