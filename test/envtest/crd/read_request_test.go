package crd_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// status.lastReadRequest holds a request as given up to 64 bytes or its
// "sha256:" digest, so the schema bounds it at the digest's length and a
// longer value is refused rather than stored.
func TestStatusBoundsTheReadRequestItAnswered(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "read-request")
	schema := schemaBase(namespace)()
	migration := migrationBase(namespace)()
	for _, object := range []*unstructured.Unstructured{schema, migration} {
		if err := api.Create(context.Background(), object); err != nil {
			t.Fatalf("store %s %s: %v", object.GetKind(), object.GetName(), err)
		}
	}
	digest := "sha256:" + strings.Repeat("a", 64)

	for _, object := range []*unstructured.Unstructured{schema, migration} {
		refused := reread(t, object)
		set(t, refused, digest+"a", "status", "lastReadRequest")
		err := api.Status().Update(context.Background(), refused, client.DryRunAll)
		if mismatch := refusalMismatch(err, cause{"status.lastReadRequest", "Too long"}); mismatch != nil {
			t.Errorf("status of %s %s: %v", object.GetKind(), object.GetName(), mismatch)
		}

		stored := reread(t, object)
		set(t, stored, digest, "status", "lastReadRequest")
		if err := api.Status().Update(context.Background(), stored, client.DryRunAll); err != nil {
			t.Errorf("status of %s %s refused a digest: %v", object.GetKind(), object.GetName(), err)
		}
	}
}
