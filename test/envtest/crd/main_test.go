// Package crd_test sends the operator's resources to a real kube-apiserver with
// the generated CRDs installed, and holds the API server's verdict to what the
// schemas and their CEL rules claim. The unit tests under hack/ evaluate the
// same rules with the libraries the API server links; here nothing stands
// between the object and the server that stores it.
package crd_test

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

const apiVersion = "operator.ptah.run/v1alpha1"

var (
	plane = harness.New(&envtest.Environment{
		CRDDirectoryPaths: []string{harness.CRDDirectory()},
	})
	api client.Client
)

func TestMain(m *testing.M) {
	os.Exit(plane.Main(m, func() error {
		var err error
		api, err = client.New(plane.Config, client.Options{})
		return err
	}))
}

// newNamespace creates a namespace of the test's own, so parallel tests never
// share a name. envtest runs no namespace controller, so nothing would delete
// one anyway; the control plane goes away with the package.
func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"}}
	if err := api.Create(context.Background(), namespace); err != nil {
		t.Fatalf("create a namespace for %s: %v", prefix, err)
	}
	return namespace.Name
}

// resource builds an object of kind from its spec. metadata carries the name
// and, for a namespaced kind, the namespace.
func resource(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
	if namespace != "" {
		object.SetNamespace(namespace)
	}
	return object
}

// set writes value at path, failing the test on a path that crosses a
// non-object. Values must already be JSON-shaped: int64 rather than int, []any
// rather than []string.
func set(t *testing.T, object *unstructured.Unstructured, value any, path ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(object.Object, value, path...); err != nil {
		t.Fatalf("set %v on %s: %v", path, object.GetKind(), err)
	}
}

func remove(object *unstructured.Unstructured, path ...string) {
	unstructured.RemoveNestedField(object.Object, path...)
}

// dryRunCreate asks the API server to validate object as a create without
// storing it, so refusal rows can share a name and a namespace.
func dryRunCreate(object *unstructured.Unstructured) error {
	return api.Create(context.Background(), object.DeepCopy(), client.DryRunAll)
}

// reread fetches object's stored form, which is what an update must start from.
func reread(t *testing.T, object *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(object.GroupVersionKind())
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(object), stored); err != nil {
		t.Fatalf("read %s %s back: %v", object.GetKind(), client.ObjectKeyFromObject(object), err)
	}
	return stored
}
