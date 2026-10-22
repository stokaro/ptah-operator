package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

func configMapSchemaFixture() *operatorv1alpha1.PtahSchema {
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
		Name:  "app-schema",
		Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}},
	}}
	return schema
}

func sourceConfigMap(schemaSQL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-schema", UID: "configmap-uid"},
		Data:       map[string]string{"schema.sql": schemaSQL, "README": "not part of the source\n"},
	}
}

func reconcileSchema(t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PtahSchema {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

func snapshots(t *testing.T, api client.Client) []corev1.ConfigMap {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := api.List(context.Background(), list, client.InNamespace("team-a")); err != nil {
		t.Fatal(err)
	}
	var found []corev1.ConfigMap
	for _, configMap := range list.Items {
		if sourcesnapshot.IsName(configMap.Name) {
			found = append(found, configMap)
		}
	}
	return found
}

func editSource(t *testing.T, api client.Client, edit func(*corev1.ConfigMap)) {
	t.Helper()
	source := &corev1.ConfigMap{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "app-schema"}, source); err != nil {
		t.Fatal(err)
	}
	edit(source)
	if err := api.Update(context.Background(), source); err != nil {
		t.Fatal(err)
	}
}

// A ConfigMap source is read by the manager: the files are copied into an
// immutable snapshot owned by the schema, status names the ConfigMap, the
// snapshot and the content digest, and the schema goes straight to
// observation. No Job is created and no verification is claimed.
func TestConfigMapSourceResolvesInTheManager(t *testing.T) {
	t.Parallel()
	schema := configMapSchemaFixture()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE orders (id int);\n"))

	actual := reconcileSchema(t, reconciler, api, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("status after a ConfigMap read = phase %s, operation %#v; want Observing and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("a ConfigMap source created Jobs: %d, %v", len(jobs.Items), err)
	}
	source := actual.Status.Source
	if source.ConfigMap == nil || source.ConfigMap.Name != "app-schema" || source.ConfigMap.UID != "configmap-uid" ||
		source.ConfigMap.ResourceVersion == "" || source.Snapshot == nil || source.Snapshot.SchemaFile != "schema.sql" ||
		source.Verified || source.Digest != "" {
		t.Fatalf("source status = %#v", source)
	}
	stored := snapshots(t, api)
	if len(stored) != 1 || stored[0].Name != source.Snapshot.Name || stored[0].UID != source.Snapshot.UID {
		t.Fatalf("snapshots = %v, want the one status names", stored)
	}
	owner, digest, err := sourcesnapshot.Verify(&stored[0])
	if err != nil || owner.UID != schema.UID || digest != source.Snapshot.ContentDigest {
		t.Fatalf("stored snapshot = %#v, %q, %v", owner, digest, err)
	}
	if _, ok := stored[0].BinaryData[sourcecontent.SnapshotFileKey(1)]; ok {
		t.Fatal("the snapshot holds a key the source does not select")
	}
	resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.Reason != string(operatorv1alpha1.ReasonSnapshotTaken) {
		t.Fatalf("ArtifactResolved = %#v, want True SnapshotTaken", resolved)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified) != nil {
		t.Fatal("a ConfigMap source reports a verification it never had")
	}
	if actual.Status.NextReconciliationTime == nil || !actual.Status.NextReconciliationTime.After(reconciler.now()) {
		t.Fatalf("next refresh = %v, want one interval out", actual.Status.NextReconciliationTime)
	}

	// The next pass claims the observation, reading the snapshot.
	observed := reconcileSchema(t, reconciler, api, actual)
	operation := observed.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationObserve || operation.Source == nil ||
		operation.Source.Snapshot == nil || *operation.Source.Snapshot != *source.Snapshot || operation.Source.Digest != "" {
		t.Fatalf("observe claim = %#v, want the snapshot binding", operation)
	}
}

// An edit to a key the source does not select reads the ConfigMap again at
// once and keeps the snapshot and the plan; an edit to the selected key gives
// a new snapshot, retires the plan, and deletes the snapshot nothing names.
func TestConfigMapSourceEditsAreReadAtOnce(t *testing.T) {
	t.Parallel()
	schema := configMapSchemaFixture()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE orders (id int);\n"))
	first := reconcileSchema(t, reconciler, api, schema)
	original := *first.Status.Source.Snapshot

	// Settle the schema the way a finished cycle does: in sync, next refresh
	// far away, a plan computed from this snapshot.
	before := first.DeepCopy()
	first.Status.Phase = operatorv1alpha1.PhaseInSync
	later := metav1.NewTime(reconciler.now().Add(time.Hour))
	first.Status.NextReconciliationTime = &later
	binding := first.Status.ExecutionBinding
	first.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "ptah-plan-0123456789abcdef01234567", UID: "plan-uid",
		Source:             &operatorv1alpha1.PlanSourceBinding{Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "configmap-uid", ContentDigest: original.ContentDigest},
		ExecutionBindingID: binding.Epoch, ControllerStateVersion: binding.ControllerStateVersion,
		PtahVersion: binding.PtahVersion, ExecutorImage: binding.ExecutorImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
	}
	if err := api.Status().Patch(context.Background(), first, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	quiet := reconcileSchema(t, reconciler, api, first)
	if quiet.Status.Phase != operatorv1alpha1.PhaseInSync {
		t.Fatalf("an unchanged ConfigMap moved the schema to %s", quiet.Status.Phase)
	}

	editSource(t, api, func(source *corev1.ConfigMap) { source.Data["README"] = "edited\n" })
	unselected := reconcileSchema(t, reconciler, api, quiet)
	if unselected.Status.Phase != operatorv1alpha1.PhaseObserving || unselected.Status.Source.Snapshot == nil ||
		*unselected.Status.Source.Snapshot != original || unselected.Status.Plan == nil {
		t.Fatalf("an unselected edit gave phase %s, snapshot %#v, plan %#v; want the same snapshot and plan, read again",
			unselected.Status.Phase, unselected.Status.Source.Snapshot, unselected.Status.Plan)
	}
	if unselected.Status.Source.ConfigMap.ResourceVersion == first.Status.Source.ConfigMap.ResourceVersion {
		t.Fatal("the read did not record the edited resourceVersion")
	}

	editSource(t, api, func(source *corev1.ConfigMap) { source.Data["schema.sql"] = "CREATE TABLE orders (id bigint);\n" })
	selected := reconcileSchema(t, reconciler, api, unselected)
	if selected.Status.Source.Snapshot == nil || selected.Status.Source.Snapshot.ContentDigest == original.ContentDigest ||
		selected.Status.Plan != nil {
		t.Fatalf("a selected edit kept snapshot %#v and plan %#v", selected.Status.Source.Snapshot, selected.Status.Plan)
	}
	stored := snapshots(t, api)
	if len(stored) != 1 || stored[0].Name != selected.Status.Source.Snapshot.Name {
		t.Fatalf("snapshots after the edit = %d, want only the new one", len(stored))
	}
}

func TestConfigMapSourceThatGivesNoSnapshotFailsWithoutWritingOne(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		objects []client.Object
		mutate  func(*operatorv1alpha1.PtahSchema)
		want    string
	}{
		"a missing ConfigMap": {want: `ConfigMap "app-schema" does not exist`},
		"a missing key": {
			objects: []client.Object{sourceConfigMap("x")},
			mutate: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Desired.ConfigMap.Items[0].Key = "absent.sql"
			},
			want: `"absent.sql"`,
		},
		"a path the rules refuse": {
			objects: []client.Object{sourceConfigMap("x")},
			mutate: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Desired.ConfigMap.Items[0].Path = strings.Repeat("a", 256) + ".sql"
			},
			want: "invalid source path",
		},
		"a name another ConfigMap holds": {
			objects: []client.Object{sourceConfigMap("x"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: snapshotNameFor(t, "x"), UID: "foreign-uid",
			}}},
			want: "is not this resource's source snapshot",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := configMapSchemaFixture()
			if test.mutate != nil {
				test.mutate(schema)
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, append([]client.Object{schema}, test.objects...)...)
			actual := reconcileSchema(t, reconciler, api, schema)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed || failed == nil || !strings.Contains(failed.Message, test.want) ||
				resolved == nil || resolved.Reason != string(operatorv1alpha1.ReasonResolveFailed) {
				t.Fatalf("phase %s, failure %#v, resolved %#v; want Failed naming %q", actual.Status.Phase, failed, resolved, test.want)
			}
			if actual.Status.ObservedGeneration != actual.Generation || actual.Status.Source.Snapshot != nil {
				t.Fatalf("a refused source left status %#v", actual.Status.Source)
			}
			for _, stored := range snapshots(t, api) {
				if stored.UID != "foreign-uid" {
					t.Fatalf("a refused source wrote the snapshot %s", stored.Name)
				}
			}
		})
	}
}

// snapshotNameFor is the snapshot the fixture schema keeps for a source whose
// one file holds schemaSQL.
func snapshotNameFor(t *testing.T, schemaSQL string) string {
	t.Helper()
	_, _, digest, err := sourcecontent.Canonical([]sourcecontent.File{{Path: "schema.sql", Data: []byte(schemaSQL)}})
	if err != nil {
		t.Fatal(err)
	}
	return sourcesnapshot.Name("schema-uid", digest)
}
