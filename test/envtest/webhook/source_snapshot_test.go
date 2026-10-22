package webhook_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// createConfigMapSchema stores a PtahSchema whose desired state is one file of
// a ConfigMap, and the ConfigMap.
func createConfigMapSchema(t *testing.T, namespace, name, schemaSQL string) *operatorv1alpha1.PtahSchema {
	t.Helper()
	ctx := context.Background()
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name + "-schema"},
		Data:       map[string]string{"schema.sql": schemaSQL, "README": "not selected\n"},
	}
	if err := admin.Create(ctx, source); err != nil {
		t.Fatalf("create ConfigMap %s/%s: %v", namespace, source.Name, err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahSchema",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/" + name + "-primary",
				"urlFrom":         map[string]any{"name": name + "-database", "key": "url"},
			},
			"desired": map[string]any{"configMap": map[string]any{
				"name":  source.Name,
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}},
			}},
		},
	}}
	if err := admin.Create(ctx, object); err != nil {
		t.Fatalf("create PtahSchema %s/%s: %v", namespace, name, err)
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(object), schema); err != nil {
		t.Fatalf("read PtahSchema %s/%s back: %v", namespace, name, err)
	}
	return schema
}

func snapshotOf(t *testing.T, schema *operatorv1alpha1.PtahSchema, schemaSQL string) *corev1.ConfigMap {
	t.Helper()
	owner := sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: schema.UID}
	snapshot, _, err := sourcesnapshot.Build(schema.Namespace, owner,
		[]sourcecontent.File{{Path: "schema.sql", Data: []byte(schemaSQL)}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// The manager may store the snapshot of a ConfigMap source only as the
// webhook rebuilds it from the source as it is now: the selected files of the
// ConfigMap the resource names, owned by that resource.
func TestControllerWriteWebhookRebuildsSourceSnapshots(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	namespace := newNamespace(t, "snapshots")
	grant(t, namespace, "manager-snapshots", managerSubject(t),
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}})
	managerAPI := clientAs(t, manager.username)
	const schemaSQL = "CREATE TABLE orders (id bigint PRIMARY KEY);\n"
	schema := createConfigMapSchema(t, namespace, "orders", schemaSQL)
	ociSchema := createSchema(t, namespace, "ledger")

	t.Run("the snapshot of the source", func(t *testing.T) {
		if err := managerAPI.Create(ctx, snapshotOf(t, schema, schemaSQL), client.DryRunAll); err != nil {
			t.Fatalf("the API server refused the snapshot of the schema's source: %v", err)
		}
	})
	t.Run("a snapshot of files the source does not hold", func(t *testing.T) {
		snapshot := snapshotOf(t, schema, "DROP TABLE orders;\n")
		requireDenied(t, managerAPI.Create(ctx, snapshot, client.DryRunAll), controllerWriteWebhook,
			"does not hold the selected files of its source")
	})
	t.Run("a snapshot for a schema whose source is an OCI artifact", func(t *testing.T) {
		requireDenied(t, managerAPI.Create(ctx, snapshotOf(t, ociSchema, schemaSQL), client.DryRunAll), controllerWriteWebhook,
			"does not read a ConfigMap source")
	})
	t.Run("a snapshot naming a schema that was replaced", func(t *testing.T) {
		replaced := schema.DeepCopy()
		replaced.UID = "00000000-0000-4000-8000-000000000000"
		requireDenied(t, managerAPI.Create(ctx, snapshotOf(t, replaced, schemaSQL), client.DryRunAll), controllerWriteWebhook,
			"not the live schema it names")
	})
	t.Run("a snapshot with a file its manifest does not name", func(t *testing.T) {
		snapshot := snapshotOf(t, schema, schemaSQL)
		snapshot.BinaryData[sourcecontent.SnapshotFileKey(1)] = []byte("DROP TABLE orders;\n")
		requireDenied(t, managerAPI.Create(ctx, snapshot, client.DryRunAll), controllerWriteWebhook,
			"not a well-formed source snapshot")
	})
}
