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
		{"an unbounded approval UID", journal(journalEntry(map[string]any{"type": "ApprovalAccepted", "approvalUID": strings.Repeat("a", 129)})),
			cause{"status.journal.entries[0].approvalUID", "Too long"}},
		{"an operation ID that is not one", journal(journalEntry(map[string]any{"type": "RunFinished", "operationID": "apply-1"})),
			cause{"status.journal.entries[0].operationID", "should match"}},
		{"an authorization the API does not name", journal(journalEntry(map[string]any{"type": "RunFinished", "authorization": "Somebody"})),
			cause{"status.journal.entries[0].authorization", "Unsupported value"}},
		{"an unbounded plan UID", journal(journalEntry(map[string]any{"type": "RunFinished", "planUID": strings.Repeat("a", 129)})),
			cause{"status.journal.entries[0].planUID", "Too long"}},
		{"an unbounded plan fingerprint", journal(journalEntry(map[string]any{"type": "RunFinished", "planFingerprint": strings.Repeat("a", 129)})),
			cause{"status.journal.entries[0].planFingerprint", "Too long"}},
		{"an unbounded Job name", journal(journalEntry(map[string]any{"type": "RunFinished", "jobName": strings.Repeat("a", 254)})),
			cause{"status.journal.entries[0].jobName", "Too long"}},
		{"an unbounded Job UID", journal(journalEntry(map[string]any{"type": "RunFinished", "jobUID": strings.Repeat("a", 129)})),
			cause{"status.journal.entries[0].jobUID", "Too long"}},
		{"an unbounded approver", journal(journalEntry(map[string]any{"type": "ApprovalAccepted", "approver": strings.Repeat("a", 513)})),
			cause{"status.journal.entries[0].approver", "Too long"}},
		{"an approver one character past the bound, of three bytes each", journal(journalEntry(map[string]any{"type": "ApprovalAccepted", "approver": strings.Repeat("日", 513)})),
			cause{"status.journal.entries[0].approver", "Too long"}},
		{"an unbounded acknowledgment author", journal(journalEntry(map[string]any{"type": "RunAcknowledged", "acknowledgedBy": strings.Repeat("a", 513)})),
			cause{"status.journal.entries[0].acknowledgedBy", "Too long"}},
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
			journalEntry(map[string]any{"type": "ApprovalAccepted", "approval": "approve-a", "approvalUID": "uid-a", "approver": "alice"}),
			// The bound counts characters, so 1,536 bytes fit it. The journal cuts a name
			// at this length and no sooner, and this is what makes that the right place.
			journalEntry(map[string]any{"type": "ApprovalAccepted", "approval": "approve-b", "approvalUID": "uid-b", "approver": strings.Repeat("日", 512)}),
			journalEntry(map[string]any{"type": "RunAcknowledged", "acknowledgment": "ack-b", "acknowledgedBy": strings.Repeat("日", 512)}),
			journalEntry(map[string]any{"type": "ApplyFinished", "startedAt": "2026-10-24T07:41:53Z", "outcome": "Succeeded"}),
			journalEntry(map[string]any{"type": "Verified", "outcome": "InSync"}),
			journalEntry(map[string]any{
				"type": "RunFinished", "outcome": "Failed", "appliedVersions": int64(2), "startedAt": "2026-10-24T07:41:53Z",
				"failure": map[string]any{"version": int64(16), "statement": int64(2), "statements": int64(3), "sqlState": "42P01"},
			}),
			journalEntry(map[string]any{
				"type": "RunFinished", "outcome": "Succeeded", "operationID": "sha256:" + strings.Repeat("a", 64),
				"plan": "plan-a", "planUID": "plan-uid", "planFingerprint": "sha256:" + strings.Repeat("b", 64),
				"authorization": "Approval", "approval": "approve-a", "approvalUID": "approval-uid", "approver": "alice",
				"jobName": "job-a", "jobUID": "job-uid",
			}),
			journalEntry(map[string]any{"type": "Verified", "outcome": "Pending", "operationID": "sha256:" + strings.Repeat("a", 64)}),
			journalEntry(map[string]any{"type": "Verified", "outcome": "Blocked", "operationID": "sha256:" + strings.Repeat("a", 64)}),
			journalEntry(map[string]any{"type": "RunAcknowledged", "acknowledgment": "ack-a", "acknowledgedBy": "bob", "operationID": "sha256:" + strings.Repeat("a", 64)}),
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

// status.lastRun names what a run carried out, and the schema keeps a malformed
// identity and a contradictory authorization from being published.
func TestPtahMigrationStatusHoldsARunsProvenance(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "run-provenance")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store PtahMigration %s: %v", migration.GetName(), err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	approval := map[string]any{"name": "approve-a", "uid": "approve-a-uid"}
	run := func(extra map[string]any) map[string]any {
		return merged(map[string]any{"outcome": "Applied", "startedAt": "2026-10-24T07:41:53Z"}, extra)
	}
	const contradictory = "an approval authorization names its approval, and a policy authorization names no decision"
	for _, test := range []struct {
		name  string
		extra map[string]any
		want  cause
	}{
		{"an operation ID that is not one", map[string]any{"operationID": "apply-1"}, cause{"status.lastRun.operationID", "should match"}},
		{"a database that is not a digest", map[string]any{"targetIdentityDigest": "somewhere"}, cause{"status.lastRun.targetIdentityDigest", "should match"}},
		{"an execution binding that is not one", map[string]any{"executionBindingID": "v2"}, cause{"status.lastRun.executionBindingID", "should match"}},
		{"an unbounded fingerprint", map[string]any{"planFingerprint": strings.Repeat("a", 129)}, cause{"status.lastRun.planFingerprint", "Too long"}},
		{"a plan with no UID", map[string]any{"planRef": map[string]any{"name": "plan-a"}}, cause{"status.lastRun.planRef.uid", "Required value"}},
		{"an authorization the API does not name", map[string]any{"authorization": map[string]any{"kind": "Somebody"}}, cause{"status.lastRun.authorization.kind", "Unsupported value"}},
		{"an approval that names no approval", map[string]any{"authorization": map[string]any{"kind": "Approval"}}, cause{"status.lastRun.authorization", contradictory}},
		{"a policy that names an approval", map[string]any{"authorization": map[string]any{"kind": "Policy", "approvalRef": approval}}, cause{"status.lastRun.authorization", contradictory}},
		{"a policy that names an approver", map[string]any{"authorization": map[string]any{"kind": "Policy", "approver": map[string]any{"username": "alice"}}}, cause{"status.lastRun.authorization", contradictory}},
		{"a policy with a stamp time", map[string]any{"authorization": map[string]any{"kind": "Policy", "approvedAt": "2026-10-24T07:41:53Z"}}, cause{"status.lastRun.authorization", contradictory}},
		{"a reading that found something the API does not name", map[string]any{"verification": map[string]any{
			"observedAt": "2026-10-24T07:41:53Z", "historyFingerprint": digest, "currentVersion": int64(3), "result": "Fine",
		}}, cause{"status.lastRun.verification.result", "Unsupported value"}},
		{"a reading of no history", map[string]any{"verification": map[string]any{
			"observedAt": "2026-10-24T07:41:53Z", "historyFingerprint": "none", "currentVersion": int64(3), "result": "InSync",
		}}, cause{"status.lastRun.verification.historyFingerprint", "should match"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, migration)
			set(t, observed, run(test.extra), "status", "lastRun")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("status of PtahMigration %s: %v", migration.GetName(), mismatch)
			}
		})
	}

	for _, test := range []struct {
		name  string
		extra map[string]any
	}{
		{"a run from before the record, which names none of it", nil},
		{"a run under an approval, read back", map[string]any{
			"operationID": digest, "planRef": map[string]any{"name": "plan-a", "uid": "plan-a-uid"}, "planFingerprint": digest,
			"artifactDigest": digest, "targetIdentityDigest": digest, "executionBindingID": "v1-" + strings.Repeat("a", 32),
			"authorization": map[string]any{
				"kind": "Approval", "approvalRef": approval, "approver": map[string]any{"username": "alice"}, "approvedAt": "2026-10-24T07:41:53Z",
			},
			"verification": map[string]any{
				"observedAt": "2026-10-24T07:41:53Z", "historyFingerprint": digest, "currentVersion": int64(3), "result": "Pending",
			},
		}},
		{"a run under an approval whose stamp nobody copied", map[string]any{
			"authorization": map[string]any{"kind": "Approval", "approvalRef": approval},
		}},
		{"a run under the policy", map[string]any{"authorization": map[string]any{"kind": "Policy"}}},
		{"a run from a Git source", map[string]any{"source": map[string]any{
			"kind": "Git", "revision": digest, "contentDigest": digest,
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, migration)
			set(t, observed, run(test.extra), "status", "lastRun")
			if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
				t.Fatalf("the API server refused a well-formed run on PtahMigration %s: %v", migration.GetName(), err)
			}
		})
	}

	// A claim carries the same copies.
	t.Run("a claim that copies what its run will name", func(t *testing.T) {
		t.Parallel()
		observed := reread(t, migration)
		set(t, observed, map[string]any{
			"type": "Apply", "id": digest, "inputFingerprint": digest, "jobName": "job-a", "startedAt": "2026-10-24T07:41:53Z",
			"attempt": int64(1), "executionBindingID": "v1-" + strings.Repeat("a", 32),
			"planFingerprint": digest, "approvedBy": map[string]any{"username": "alice"}, "approvedAt": "2026-10-24T07:41:53Z",
		}, "status", "activeOperation")
		if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
			t.Fatalf("the API server refused a well-formed claim on PtahMigration %s: %v", migration.GetName(), err)
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
