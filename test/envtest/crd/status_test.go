package crd_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// status.pendingBindingRetirement is what decides which leftovers of a retired
// execution binding the controller waits for, adopts and cleans up, so the
// schema is what keeps a malformed record from being published: an epoch that
// is not one, a Job under an operation that does not exist or with no name, a
// plan named by something that is not its identity.
func TestPtahSchemaStatusRefusesAMalformedBindingRetirement(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-retirement")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store PtahSchema %s: %v", schema.GetName(), err)
	}

	const retiredEpoch = "v1-0123456789abcdef0123456789abcdef"
	plan := func() map[string]any {
		return map[string]any{
			"name":        "ptah-plan-0123456789abcdef01234567",
			"uid":         "4f0c8a52-50b6-4b4e-9d53-6a1f0d8c2e11",
			"fingerprint": "sha256:" + strings.Repeat("c", 64),
		}
	}
	job := func() map[string]any {
		return map[string]any{
			"operation": "Apply",
			"name":      "ptah-apply-application-0123456789abcdef",
			"uid":       "a6b0f3c4-8f0e-4a3b-9d1c-2e5f7a9b0c1d",
		}
	}
	const field = "status.pendingBindingRetirement"

	tests := []struct {
		name   string
		record map[string]any
		want   cause
	}{
		{
			name:   "a record that names no retired epoch",
			record: map[string]any{"plan": plan()},
			want:   cause{field + ".retiredEpoch", "Required value"},
		},
		{
			name:   "a retired epoch that is not an epoch",
			record: map[string]any{"retiredEpoch": "v1-not-an-epoch", "plan": plan()},
			want:   cause{field + ".retiredEpoch", "should match"},
		},
		{
			name:   "a Job under an operation that does not exist",
			record: map[string]any{"retiredEpoch": retiredEpoch, "job": merged(job(), map[string]any{"operation": "Rehearse"})},
			want:   cause{field + ".job.operation", "Unsupported value"},
		},
		{
			name:   "a Job with no name",
			record: map[string]any{"retiredEpoch": retiredEpoch, "job": merged(job(), map[string]any{"name": ""})},
			want:   cause{field + ".job.name", "should be at least 1 chars long"},
		},
		{
			name:   "a plan named by something that is not its fingerprint",
			record: map[string]any{"retiredEpoch": retiredEpoch, "plan": merged(plan(), map[string]any{"fingerprint": "sha256:plan"})},
			want:   cause{field + ".plan.fingerprint", "should match"},
		},
		{
			name: "a plan without its UID",
			record: map[string]any{"retiredEpoch": retiredEpoch, "plan": map[string]any{
				"name": plan()["name"], "fingerprint": plan()["fingerprint"],
			}},
			want: cause{field + ".plan.uid", "Required value"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, schema)
			set(t, observed, test.record, "status", "pendingBindingRetirement")
			err := api.Status().Update(context.Background(), observed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("status of PtahSchema %s: %v", schema.GetName(), mismatch)
			}
		})
	}

	for _, admitted := range []struct {
		name   string
		record map[string]any
	}{
		{
			name:   "a rotation that retired a plan and a dispatched Apply",
			record: map[string]any{"retiredEpoch": retiredEpoch, "plan": plan(), "job": job()},
		},
		{
			// A create the retired claim started may commit later, so its UID
			// is not known yet.
			name: "a Job whose UID is still to be adopted",
			record: map[string]any{"retiredEpoch": retiredEpoch, "job": map[string]any{
				"operation": "Observe", "name": "ptah-observe-application-0123456789abcdef",
			}},
		},
	} {
		t.Run(admitted.name, func(t *testing.T) {
			t.Parallel()
			observed := reread(t, schema)
			set(t, observed, admitted.record, "status", "pendingBindingRetirement")
			if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
				t.Fatalf("the API server refused a well-formed retirement record on PtahSchema %s: %v", schema.GetName(), err)
			}
		})
	}
}

// Summaries are keyed by category, so the largest summary the schema can be
// sent names every category once. It publishes, which is why the list needs
// no way to say it was cut short: the controller copies the runner's
// summaries, which are the whole report, and the whole report fits. The row
// holds the enum to the list's bound, so the day the vocabulary outgrows it
// the row says a complete report no longer publishes.
func TestEveryDriftCategoryPublishesInOneSummary(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	categories := driftCategories(t)
	if bound := driftFindingsBound(t); int64(len(categories)) > bound {
		t.Fatalf("the drift category enum has %d values and status.target.driftFindings holds %d, so a report "+
			"that names every category no longer publishes", len(categories), bound)
	}

	namespace := newNamespace(t, "schema-every-category")
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
		"driftFindingCount": int64(len(categories)),
		"driftFindings":     summaries,
	}, "status", "target")
	if err := api.Status().Update(context.Background(), observed, client.DryRunAll); err != nil {
		t.Fatalf("the API server refused every category summarized once on PtahSchema %s: %v", schema.GetName(), err)
	}
}

// driftFindings reads status.target.driftFindings out of the CRD the API
// server installed, rather than out of a copy here that could drift from it.
func driftFindings(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	for _, crd := range plane.Environment.CRDs {
		if crd.Name != "ptahschemas.operator.ptah.run" {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if version.Storage {
				return version.Schema.OpenAPIV3Schema.Properties["status"].Properties["target"].Properties["driftFindings"]
			}
		}
	}
	t.Fatal("the PtahSchema CRD is not among the installed CRDs")
	return apiextensionsv1.JSONSchemaProps{}
}

// driftFindingsBound is the most summaries status.target.driftFindings holds.
func driftFindingsBound(t *testing.T) int64 {
	t.Helper()
	findings := driftFindings(t)
	if findings.MaxItems == nil {
		t.Fatal("status.target.driftFindings declares no maxItems")
	}
	return *findings.MaxItems
}

// driftCategories is the category enum of the installed CRD.
func driftCategories(t *testing.T) []string {
	t.Helper()
	findings := driftFindings(t)
	if findings.Items == nil || findings.Items.Schema == nil {
		t.Fatal("status.target.driftFindings declares no item schema")
	}
	var categories []string
	for _, value := range findings.Items.Schema.Properties["category"].Enum {
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
