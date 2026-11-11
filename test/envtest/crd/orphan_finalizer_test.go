package crd_test

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestNamedPropagationReleasesAnOrphanedResource holds the recovery the
// operations guide gives for a resource an orphaning deletion left Terminating
// before the orphan-deletion policy existed: deleting it again the way kubectl
// delete does, with Background propagation, takes the orphan finalizer off and
// leaves every other finalizer where it was. A deletion that names no
// propagation keeps the finalizer, which is why the policy refuses that one.
// No admission policy is installed here, so the orphaning deletion that builds
// the starting state is admitted, as it was before the upgrade.
func TestNamedPropagationReleasesAnOrphanedResource(t *testing.T) {
	plane.Require(t)
	namespace := newNamespace(t, "orphaned")
	const hold = "example.com/hold"
	for _, base := range []func() *unstructured.Unstructured{schemaBase(namespace), migrationBase(namespace)} {
		object := base()
		t.Run(object.GetKind(), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			// A finalizer of the test's own keeps the object readable once the
			// orphan finalizer is gone; nothing here would remove the operator's.
			object.SetFinalizers([]string{hold})
			if err := api.Create(ctx, object); err != nil {
				t.Fatalf("create %s: %v", object.GetName(), err)
			}
			finalizers := func(step string) []string {
				t.Helper()
				current := &unstructured.Unstructured{}
				current.SetGroupVersionKind(object.GroupVersionKind())
				if err := api.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
					t.Fatalf("%s: read %s: %v", step, object.GetName(), err)
				}
				if current.GetDeletionTimestamp() == nil {
					t.Fatalf("%s: %s has no deletionTimestamp", step, object.GetName())
				}
				return current.GetFinalizers()
			}

			if err := api.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationOrphan)); err != nil {
				t.Fatalf("delete %s with Orphan propagation: %v", object.GetName(), err)
			}
			if got := finalizers("after the orphaning deletion"); !slices.Contains(got, metav1.FinalizerOrphanDependents) {
				t.Fatalf("the orphaning deletion left finalizers %v, want %q among them", got, metav1.FinalizerOrphanDependents)
			}

			if err := api.Delete(ctx, object); err != nil {
				t.Fatalf("delete %s again with no options: %v", object.GetName(), err)
			}
			if got := finalizers("after a deletion that names no propagation"); !slices.Contains(got, metav1.FinalizerOrphanDependents) {
				t.Fatalf("a deletion that names no propagation left finalizers %v, want %q kept", got, metav1.FinalizerOrphanDependents)
			}

			if err := api.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				t.Fatalf("delete %s again with Background propagation: %v", object.GetName(), err)
			}
			if got := finalizers("after the Background deletion"); !slices.Equal(got, []string{hold}) {
				t.Fatalf("the Background deletion left finalizers %v, want only %q", got, hold)
			}
		})
	}
}
