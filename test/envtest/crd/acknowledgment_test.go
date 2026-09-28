package crd_test

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const unresolvedOperationID = "sha256:4c2a7e9b1d3f5a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c"

// runAcknowledgmentSpec is what admission stores for a person who settled a
// run: the decision, and the stamp.
func runAcknowledgmentSpec() map[string]any {
	return map[string]any{
		"migrationRef":       map[string]any{"name": "orders", "uid": migrationUID},
		"operationID":        unresolvedOperationID,
		"acknowledgedBy":     map[string]any{"username": "jane@example.com", "uid": "1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed", "groups": []any{"dba"}},
		"acknowledgedAt":     approvedAt,
		"mutationRequestUID": mutationRequestUID,
	}
}

// An acknowledgment names one run by its operation ID, and carries the stamp
// admission wrote.
func TestPtahMigrationRunAcknowledgmentRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "run-acknowledgment-refusals")
	assertRefusals(t, basedOn("PtahMigrationRunAcknowledgment", namespace, "orders-run-accounted-for", runAcknowledgmentSpec),
		[]refusal{
			{
				name:   "operationID is required",
				mutate: removing("spec", "operationID"),
				want:   []cause{{"spec.operationID", "Required value"}},
			},
			{
				name:   "operationID that is not an operation ID",
				mutate: setting("run-7", "spec", "operationID"),
				want:   []cause{{"spec.operationID", "should match"}},
			},
			{
				name:   "migrationRef.uid is required",
				mutate: removing("spec", "migrationRef", "uid"),
				want:   []cause{{"spec.migrationRef.uid", "Required value"}},
			},
			{
				name:   "acknowledgedBy is required",
				mutate: removing("spec", "acknowledgedBy"),
				want:   []cause{{"spec.acknowledgedBy", "Required value"}},
			},
			{
				name:   "acknowledgedBy in 65 groups",
				mutate: setting(numbered("group-", 65), "spec", "acknowledgedBy", "groups"),
				want:   []cause{{"spec.acknowledgedBy.groups", "must have at most 64 items"}},
			},
			{
				name:   "mutationRequestUID is required",
				mutate: removing("spec", "mutationRequestUID"),
				want:   []cause{{"spec.mutationRequestUID", "Required value"}},
			},
		})
}

// A resolution names the acknowledgment and the identity when a person made
// it, and neither when a reading did. The manager is the only writer of
// status, and the schema is what holds it to that shape.
func TestPtahMigrationStatusHoldsAResolutionToWhatSettledIt(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-resolution")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store PtahMigration %s: %v", migration.GetName(), err)
	}
	const message = "an acknowledged resolution names its acknowledgment and the identity that made it, and a resolution by reading names neither"
	byReading := map[string]any{
		"operationID": unresolvedOperationID, "outcome": "Unknown",
		"resolution": "HistoryRead", "resolvedAt": approvedAt,
	}
	acknowledged := merged(byReading, map[string]any{
		"resolution":        "Acknowledged",
		"acknowledgmentRef": map[string]any{"name": "orders-run-accounted-for", "uid": planUID},
		"acknowledgedBy":    map[string]any{"username": "jane@example.com"},
	})
	for _, test := range []struct {
		name       string
		resolution map[string]any
		want       *cause
	}{
		{name: "a resolution by reading", resolution: byReading},
		{name: "an acknowledged resolution", resolution: acknowledged},
		{
			name:       "an acknowledged resolution that names no identity",
			resolution: without(acknowledged, "acknowledgedBy"),
			want:       &cause{"status.resolvedRun", message},
		},
		{
			name:       "an acknowledged resolution that names no acknowledgment",
			resolution: without(acknowledged, "acknowledgmentRef"),
			want:       &cause{"status.resolvedRun", message},
		},
		{
			name:       "a resolution by reading that names a person",
			resolution: merged(byReading, map[string]any{"acknowledgedBy": map[string]any{"username": "jane@example.com"}}),
			want:       &cause{"status.resolvedRun", message},
		},
		{
			name:       "a resolution by something else",
			resolution: merged(byReading, map[string]any{"resolution": "Deleted"}),
			want:       &cause{"status.resolvedRun.resolution", "Unsupported value"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, migration)
			set(t, observed, test.resolution, "status", "resolvedRun")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if test.want == nil {
				if err != nil {
					t.Fatalf("the API server refused %s: %v", test.name, err)
				}
				return
			}
			if mismatch := refusalMismatch(err, *test.want); mismatch != nil {
				t.Errorf("status of PtahMigration %s: %v", migration.GetName(), mismatch)
			}
		})
	}
}

func without(object map[string]any, key string) map[string]any {
	copied := merged(object, nil)
	delete(copied, key)
	return copied
}
