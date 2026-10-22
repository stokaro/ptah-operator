package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// A migration directory in a ConfigMap is read by the manager into a
// snapshot, and the history read that follows mounts the snapshot. There is
// no Resolve Job, no Verify step and no ArtifactVerified condition.
func TestMigrationConfigMapSourceResolvesInTheManager(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
		Name: "orders-migrations",
		Items: []operatorv1alpha1.ConfigMapSourceItem{
			{Key: "0001.up.sql", Path: "0001_init.up.sql"},
			{Key: "0002.up.sql", Path: "0002_orders.up.sql"},
		},
	}}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-migrations", UID: "configmap-uid"},
		Data:       map[string]string{"0001.up.sql": "CREATE TABLE a (id int);\n", "0002.up.sql": "CREATE TABLE b (id int);\n"},
	}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, source)

	// The first pass publishes the execution binding, the second reads the source.
	for range 2 {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
		t.Fatalf("phase %s, operation %#v; want Reading and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	artifact := actual.Status.Artifact
	if artifact == nil || artifact.Snapshot == nil || artifact.Digest != "" || artifact.Snapshot.SchemaFile != "" ||
		actual.Status.ConfigMap == nil || actual.Status.ConfigMap.UID != "configmap-uid" {
		t.Fatalf("artifact %#v, ConfigMap %#v", artifact, actual.Status.ConfigMap)
	}
	stored := &corev1.ConfigMap{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: artifact.Snapshot.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if owner, digest, err := sourcesnapshot.Verify(stored); err != nil || owner.Kind != "PtahMigration" ||
		digest != artifact.Snapshot.ContentDigest || len(stored.BinaryData) != 3 {
		t.Fatalf("stored snapshot owner %#v digest %q: %v", owner, digest, err)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified) != nil {
		t.Fatal("a ConfigMap source reports a verification it never had")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("reading a ConfigMap source created %d Jobs: %v", len(jobs.Items), err)
	}

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	history := readMigration(t, api, migration).Status.ActiveOperation
	if history == nil || history.Type != operatorv1alpha1.MigrationOperationHistory || history.Source == nil ||
		history.Source.Snapshot == nil || *history.Source.Snapshot != *artifact.Snapshot {
		t.Fatalf("history claim = %#v, want the snapshot binding", history)
	}
}
