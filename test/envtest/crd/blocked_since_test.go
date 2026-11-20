package crd_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// status.blockedSince is the date ptah_operator_blocked_seconds reads. The
// controller publishes it through the status subresource, the schema holds it
// to a date, and a write to the resource itself cannot set it.
func TestBlockedSinceIsAStatusDate(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "blocked-since")
	for _, base := range []func() *unstructured.Unstructured{schemaBase(namespace), migrationBase(namespace)} {
		object := base()
		kind := object.GetKind()
		if err := api.Create(context.Background(), object); err != nil {
			t.Fatalf("store %s %s: %v", kind, object.GetName(), err)
		}
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			const since = "2026-11-20T10:00:00Z"
			observed := reread(t, object)
			set(t, observed, "yesterday", "status", "blockedSince")
			if mismatch := refusalMismatch(api.Status().Update(context.Background(), observed, client.DryRunAll),
				cause{"status.blockedSince", "date-time"}); mismatch != nil {
				t.Errorf("status of %s %s: %v", kind, object.GetName(), mismatch)
			}

			observed = reread(t, object)
			set(t, observed, since, "status", "blockedSince")
			if err := api.Status().Update(context.Background(), observed); err != nil {
				t.Fatalf("the API server refused a date on %s %s: %v", kind, object.GetName(), err)
			}
			if got, _, _ := unstructured.NestedString(reread(t, object).Object, "status", "blockedSince"); got != since {
				t.Fatalf("%s %s read back status.blockedSince %q, want %q", kind, object.GetName(), got, since)
			}

			edited := reread(t, object)
			set(t, edited, "2026-11-21T00:00:00Z", "status", "blockedSince")
			if err := api.Update(context.Background(), edited); err != nil {
				t.Fatalf("update %s %s: %v", kind, object.GetName(), err)
			}
			if got, _, _ := unstructured.NestedString(reread(t, object).Object, "status", "blockedSince"); got != since {
				t.Fatalf("a write to %s %s itself moved status.blockedSince to %q", kind, object.GetName(), got)
			}
		})
	}
}
