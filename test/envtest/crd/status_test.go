package crd_test

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const truncationMessage = "truncated drift findings require exactly 64 published summaries"

func finding(category string) map[string]any {
	return map[string]any{"category": category, "count": int64(1), "severity": "safe"}
}

// The status rules bind the controller, not a person: status is written
// through its own subresource, and the schema is what stops a malformed
// observation from being published there.
func TestPtahSchemaStatusRefusesAMalformedDriftSummary(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-status")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store PtahSchema %s: %v", schema.GetName(), err)
	}

	tests := []struct {
		name   string
		target map[string]any
		want   cause
	}{
		{
			name:   "truncated with one summary",
			target: map[string]any{"driftFindingCount": int64(70), "driftFindings": []any{finding("tables_added")}, "driftFindingsTruncated": true},
			want:   cause{"status.target", truncationMessage},
		},
		{
			name:   "truncated with no summaries",
			target: map[string]any{"driftFindingCount": int64(70), "driftFindingsTruncated": true},
			want:   cause{"status.target", truncationMessage},
		},
		{
			name:   "one category summarized twice",
			target: map[string]any{"driftFindingCount": int64(2), "driftFindings": []any{finding("tables_added"), finding("tables_added")}},
			want:   cause{"status.target.driftFindings[1]", "Duplicate value"},
		},
		{
			name:   "a category outside its enum",
			target: map[string]any{"driftFindingCount": int64(1), "driftFindings": []any{finding("views_added")}},
			want:   cause{"status.target.driftFindings[0].category", "Unsupported value"},
		},
		{
			name:   "a severity outside its enum",
			target: map[string]any{"driftFindingCount": int64(1), "driftFindings": []any{merged(finding("tables_added"), map[string]any{"severity": "fatal"})}},
			want:   cause{"status.target.driftFindings[0].severity", "Unsupported value"},
		},
		{
			name:   "a summary of nothing",
			target: map[string]any{"driftFindingCount": int64(0), "driftFindings": []any{merged(finding("tables_added"), map[string]any{"count": int64(0)})}},
			want:   cause{"status.target.driftFindings[0].count", "greater than or equal to 1"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, schema)
			set(t, observed, test.target, "status", "target")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("status of PtahSchema %s: %v", schema.GetName(), mismatch)
			}
		})
	}

	t.Run("a complete summary is published", func(t *testing.T) {
		t.Parallel()
		observed := reread(t, schema)
		set(t, observed, map[string]any{
			"driftFindingCount": int64(3),
			"driftFindings":     []any{merged(finding("tables_removed"), map[string]any{"severity": "destructive"}), merged(finding("tables_added"), map[string]any{"count": int64(2)})},
		}, "status", "target")
		if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
			t.Fatalf("the API server refused a well-formed drift summary on PtahSchema %s: %v", schema.GetName(), err)
		}
	})

	// A grant is drift the report has no category for. The controller
	// records it as a safe highest severity with no summaries and no count,
	// and the schema has to take that shape as it takes any other.
	t.Run("drift in no category is published", func(t *testing.T) {
		t.Parallel()
		observed := reread(t, schema)
		set(t, observed, map[string]any{"highestDriftSeverity": "safe"}, "status", "target")
		if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
			t.Fatalf("the API server refused drift in no category on PtahSchema %s: %v", schema.GetName(), err)
		}
	})
}

// The truncation rule demands exactly 64 summaries, and summaries are keyed by
// category. While the category enum is shorter than 64, no status can satisfy
// the rule with the flag set: the truncated branch is unreachable, and the
// runner's own protocol check refuses such a frame first for the same reason.
// This row walks the largest summary the schema admits -- every category once
// -- and holds the enum below 64, so the day it reaches 64 the row says the
// branch became reachable and wants a proof that a truncated summary publishes.
func TestTruncatedDriftSummariesAreUnreachable(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	categories := driftCategories(t)
	if len(categories) >= 64 {
		t.Fatalf("the drift category enum has %d values, so a truncated summary of 64 distinct categories is now "+
			"possible: replace this row with one that publishes it", len(categories))
	}

	namespace := newNamespace(t, "schema-truncation")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store PtahSchema %s: %v", schema.GetName(), err)
	}
	summaries := make([]any, len(categories))
	for index, category := range categories {
		summaries[index] = finding(category)
	}
	observed := reread(t, schema)
	set(t, observed, map[string]any{
		"driftFindingCount":      int64(len(categories) + 1),
		"driftFindings":          summaries,
		"driftFindingsTruncated": true,
	}, "status", "target")
	err := api.Status().Update(context.Background(), observed, client.DryRunAll)
	if mismatch := refusalMismatch(err, cause{"status.target", truncationMessage}); mismatch != nil {
		t.Fatalf("status of PtahSchema %s with all %d categories and the truncation flag: %v", schema.GetName(), len(categories), mismatch)
	}

	// Without the flag the same summary publishes, so the refusal above is
	// the truncation rule's and not the list's.
	set(t, observed, false, "status", "target", "driftFindingsTruncated")
	set(t, observed, int64(len(categories)), "status", "target", "driftFindingCount")
	if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
		t.Fatalf("the API server refused every category summarized once on PtahSchema %s: %v", schema.GetName(), err)
	}
}

// driftCategories reads the category enum out of the CRD the API server
// installed, rather than out of a list here that could drift from it.
func driftCategories(t *testing.T) []string {
	t.Helper()
	for _, crd := range plane.Environment.CRDs {
		if crd.Name != "ptahschemas.operator.ptah.run" {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if !version.Storage {
				continue
			}
			category := version.Schema.OpenAPIV3Schema.
				Properties["status"].Properties["target"].Properties["driftFindings"].Items.Schema.Properties["category"]
			var categories []string
			for _, value := range category.Enum {
				var name string
				if err := json.Unmarshal(value.Raw, &name); err != nil {
					t.Fatalf("read a drift category out of the CRD: %v", err)
				}
				categories = append(categories, name)
			}
			if len(categories) == 0 {
				t.Fatal("the PtahSchema CRD declares no drift categories, so this row would measure nothing")
			}
			return categories
		}
	}
	t.Fatal("the PtahSchema CRD is not among the installed CRDs")
	return nil
}

func TestPtahMigrationPlanStatusKeysConditionsByType(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-plan-status")
	plan := resource("PtahMigrationPlan", namespace, "ptah-mplan-19581e27de7ced00ff1ce50b", migrationPlanSpec())
	if err := api.Create(context.Background(), plan); err != nil {
		t.Fatalf("store PtahMigrationPlan %s: %v", plan.GetName(), err)
	}
	condition := map[string]any{
		"type": "Published", "status": "True", "reason": "ChunksPublished",
		"message": "", "lastTransitionTime": "2026-09-20T09:12:45Z",
	}

	observed := reread(t, plan)
	set(t, observed, []any{condition, merged(condition, map[string]any{"status": "False"})}, "status", "conditions")
	err := api.Status().Update(context.Background(), observed, client.DryRunAll)
	if mismatch := refusalMismatch(err, cause{"status.conditions[1]", "Duplicate value"}); mismatch != nil {
		t.Errorf("two Published conditions on PtahMigrationPlan %s: %v", plan.GetName(), mismatch)
	}

	observed = reread(t, plan)
	set(t, observed, []any{merged(condition, map[string]any{"reason": "chunks published"})}, "status", "conditions")
	err = api.Status().Update(context.Background(), observed, client.DryRunAll)
	if mismatch := refusalMismatch(err, cause{"status.conditions[0].reason", "should match"}); mismatch != nil {
		t.Errorf("a reason with a space on PtahMigrationPlan %s: %v", plan.GetName(), mismatch)
	}

	observed = reread(t, plan)
	set(t, observed, []any{condition}, "status", "conditions")
	if err := api.Status().Update(context.Background(), observed); err != nil {
		t.Fatalf("the API server refused one well-formed condition on PtahMigrationPlan %s: %v", plan.GetName(), err)
	}
	if conditions, _, _ := unstructured.NestedSlice(reread(t, plan).Object, "status", "conditions"); len(conditions) != 1 {
		t.Fatalf("PtahMigrationPlan %s stored %d conditions, want 1", plan.GetName(), len(conditions))
	}
}
