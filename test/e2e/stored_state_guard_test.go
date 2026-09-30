package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func storedStateFixtures() []client.Object {
	schema, _, _, _ := schemaReplacementFixture()
	migration, _, _, _ := migrationReplacementFixture()
	schema.ResourceVersion, migration.ResourceVersion = "10", "10"
	schema.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: ftEpoch, ControllerStateVersion: 1}
	migration.Status.ExecutionBinding = schema.Status.ExecutionBinding.DeepCopy()
	schema.Status.Plan.ControllerStateVersion = 1
	return []client.Object{schema, migration}
}

func TestStoredStateHeldApprovalRejectsReplacementsAndConsumption(t *testing.T) {
	t.Parallel()
	meta := metav1.ObjectMeta{Name: "old-approval", Namespace: "test", UID: "admitted", Generation: 1}
	plan := ptahv1alpha1.ImmutableObjectReference{Name: "plan", UID: "plan-uid"}
	approvals := []client.Object{
		&ptahv1alpha1.PtahSchemaApproval{ObjectMeta: meta, Spec: ptahv1alpha1.PtahSchemaApprovalSpec{PlanRef: plan, PlanFingerprint: "decision"}},
		&ptahv1alpha1.PtahMigrationApproval{ObjectMeta: meta, Spec: ptahv1alpha1.PtahMigrationApprovalSpec{PlanRef: plan, PlanFingerprint: "decision"}},
	}
	for _, before := range approvals {
		if err := storedStateApprovalUnconsumed(before, before.DeepCopyObject().(client.Object)); err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"replacement", "edited decision", "consumed"} {
			current := before.DeepCopyObject().(client.Object)
			if change == "replacement" {
				current.SetUID("other")
			} else {
				switch approval := current.(type) {
				case *ptahv1alpha1.PtahSchemaApproval:
					if change == "edited decision" {
						approval.Spec.PlanFingerprint = "another decision"
					} else {
						approval.Status.Conditions = []metav1.Condition{{Type: "Consumed", Status: metav1.ConditionTrue}}
					}
				case *ptahv1alpha1.PtahMigrationApproval:
					if change == "edited decision" {
						approval.Spec.PlanFingerprint = "another decision"
					} else {
						approval.Status.Conditions = []metav1.Condition{{Type: "Consumed", Status: metav1.ConditionTrue}}
					}
				}
			}
			if storedStateApprovalUnconsumed(before, current) == nil {
				t.Fatalf("%s passed as the original unconsumed approval", change)
			}
		}
	}
	if storedStateApprovalUnconsumed(nil, approvals[0]) == nil || storedStateApprovalUnconsumed(approvals[0], (*ptahv1alpha1.PtahSchemaApproval)(nil)) == nil {
		t.Fatal("a missing admitted approval passed")
	}
}

func withStoredStateVersion(object client.Object, version int32) client.Object {
	copy := object.DeepCopyObject().(client.Object)
	switch resource := copy.(type) {
	case *ptahv1alpha1.PtahSchema:
		resource.Status.ExecutionBinding.ControllerStateVersion = version
	case *ptahv1alpha1.PtahMigration:
		resource.Status.ExecutionBinding.ControllerStateVersion = version
	}
	return copy
}

func TestStoredStateRefusalPreservesTheEntireResourceBoundary(t *testing.T) {
	t.Parallel()
	for _, before := range storedStateFixtures() {
		kind, _, _ := storedStateFamily(before)
		t.Run(kind, func(t *testing.T) {
			version, err := storedStateVersion(before)
			if err != nil || storedStateReady(before, version) != nil {
				t.Fatal("the supported-state fixture did not reach its approval gate")
			}
			held := withStoredStateVersion(before, version+1)
			if err := storedStateChangedOnlyByVersion(before, held, version+1); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(client.Object){
				"replacement":   func(o client.Object) { o.SetUID("other") },
				"generation":    func(o client.Object) { o.SetGeneration(o.GetGeneration() + 1) },
				"new finalizer": func(o client.Object) { o.SetFinalizers([]string{"other"}) },
				"deletion":      func(o client.Object) { now := metav1.Now(); o.SetDeletionTimestamp(&now) },
				"discarded plan": func(o client.Object) {
					switch r := o.(type) {
					case *ptahv1alpha1.PtahSchema:
						r.Status.Plan = nil
					case *ptahv1alpha1.PtahMigration:
						r.Status.Plan = nil
					}
				},
				"rotated epoch": func(o client.Object) {
					switch r := o.(type) {
					case *ptahv1alpha1.PtahSchema:
						r.Status.ExecutionBinding.Epoch = ftOtherEpoch
					case *ptahv1alpha1.PtahMigration:
						r.Status.ExecutionBinding.Epoch = ftOtherEpoch
					}
				},
				"suspension": func(o client.Object) {
					switch r := o.(type) {
					case *ptahv1alpha1.PtahSchema:
						r.Spec.Suspend = true
					case *ptahv1alpha1.PtahMigration:
						r.Spec.Suspend = true
					}
				},
				"interpreted phase": func(o client.Object) {
					switch r := o.(type) {
					case *ptahv1alpha1.PtahSchema:
						r.Status.Phase = ptahv1alpha1.PhaseFailed
					case *ptahv1alpha1.PtahMigration:
						r.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
					}
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed := held.DeepCopyObject().(client.Object)
					mutate(changed)
					if storedStateChangedOnlyByVersion(before, changed, version+1) == nil {
						t.Fatal("changed work or interpreted state passed the refusal predicate")
					}
				})
			}
			if storedStateChangedOnlyByVersion(before, before, version+1) == nil || storedStateReady(held, version) == nil {
				t.Fatal("the unsupported-state control accepted the wrong version")
			}
		})
	}
	if _, err := storedStateVersion((*ptahv1alpha1.PtahSchema)(nil)); err == nil {
		t.Fatal("a nil schema carried a state version")
	}
}

func TestStoredStateInjectionAndRestorationRequireTheExactAPIVersion(t *testing.T) {
	t.Parallel()
	for _, before := range storedStateFixtures() {
		version, _ := storedStateVersion(before)
		patch, err := storedStatePatch(before, version, version+1)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			t.Fatal(err)
		}
		document, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		updated, err := decoded.Apply(document)
		if err != nil {
			t.Fatal(err)
		}
		after := before.DeepCopyObject().(client.Object)
		if err := json.Unmarshal(updated, after); err != nil {
			t.Fatal(err)
		}
		if err := storedStateChangedOnlyByVersion(before, after, version+1); err != nil {
			t.Fatal(err)
		}
		stale := before.DeepCopyObject().(client.Object)
		stale.SetResourceVersion("11")
		staleDocument, _ := json.Marshal(stale)
		if _, err := decoded.Apply(staleDocument); err == nil {
			t.Fatal("an intervening API write was overwritten")
		}
		wrong := withStoredStateVersion(before, version+2)
		wrongDocument, _ := json.Marshal(wrong)
		if _, err := decoded.Apply(wrongDocument); err == nil {
			t.Fatal("an unrelated state version was overwritten")
		}
		restore, err := storedStatePatch(after, version+1, version)
		if err != nil {
			t.Fatal(err)
		}
		reverse, err := jsonpatch.DecodePatch(restore)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := reverse.Apply(updated)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(restored, after); err != nil {
			t.Fatal(err)
		}
		if err := storedStateChangedOnlyByVersion(before, after, version); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoredStateWriterGrantsOnlyOneNamedStatusPatch(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"ptahschemas", "ptahmigrations"} {
		role, binding, err := storedStateWriterObjects("test", "writer", resource, "held", "unique-group")
		if err != nil {
			t.Fatal(err)
		}
		if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].Resources, []string{resource + "/status"}) ||
			!slices.Equal(role.Rules[0].ResourceNames, []string{"held"}) || !slices.Equal(role.Rules[0].Verbs, []string{"patch"}) ||
			!slices.Equal(role.Rules[0].APIGroups, []string{"operator.ptah.run"}) || role.Namespace != "test" ||
			binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name || binding.Namespace != role.Namespace ||
			len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != "unique-group" {
			t.Fatal("the injection writer escaped its one-name status PATCH scope")
		}
	}
	for _, resource := range []string{"", "*", "ptahschemas/status", "secrets"} {
		if _, _, err := storedStateWriterObjects("test", "writer", resource, "held", "unique-group"); err == nil {
			t.Fatal("an unsupported resource acquired an injection grant")
		}
	}
}

func TestStoredStateManagerLogsNeedExactRepeatedRuntimeRefusals(t *testing.T) {
	t.Parallel()
	line := func(id, kind, name, message string) string {
		doc, _ := json.Marshal(map[string]string{"controllerKind": kind, "controllerGroup": "operator.ptah.run", "namespace": "test", "name": name, "reconcileID": id, "error": message})
		return "2026-09-30T00:00:00Z ERROR reconciliation failed " + string(doc) + "\n"
	}
	for _, kind := range []string{"PtahSchema", "PtahMigration"} {
		message := storedStateGuardMessage(kind, 2, 1)
		logs := line("first", kind, "held", message) + line("first", kind, "held", message) + line("second", kind, "held", message)
		ids, err := storedStateGuardReconciliations([]byte(logs), "test", "held", kind, 2, 1)
		if err != nil || len(ids) != 2 {
			t.Fatal("distinct exact runtime refusals were not recognized")
		}
		noise := line("", kind, "held", message) + line("other", kind, "rival", message) + line("wrong", kind, "held", strings.ReplaceAll(message, "version 2", "version 3")) +
			line("unrelated", "Pod", "held", message) + "invalid JSON mentioning " + message + "\n"
		ids, err = storedStateGuardReconciliations([]byte(noise), "test", "held", kind, 2, 1)
		if err != nil || len(ids) != 0 {
			t.Fatal("an unrelated request, version or unstructured message became a refusal witness")
		}
	}
}

func TestStoredStateClosedHistoryRejectsTransientStateLossAndMissingBoundaries(t *testing.T) {
	t.Parallel()
	before := storedStateFixtures()[0].(*ptahv1alpha1.PtahSchema)
	version, _ := storedStateVersion(before)
	injected := withStoredStateVersion(before, version+1).(*ptahv1alpha1.PtahSchema)
	injected.ResourceVersion = "20"
	end := injected.DeepCopy()
	end.ResourceVersion = "30"
	other := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{UID: "other", ResourceVersion: "19"}}
	bookmark := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "25"}}
	events := []watchEvent[*ptahv1alpha1.PtahSchema]{{Type: watch.Modified, Object: other}, {Type: watch.Modified, Object: injected}, {Type: watch.Bookmark, Object: bookmark}, {Type: watch.Modified, Object: end}}
	if err := storedStateHeldHistory(events, before, "20", "30", version+1); err != nil {
		t.Fatal(err)
	}
	transient := injected.DeepCopy()
	transient.ResourceVersion = "24"
	transient.Status.Plan = nil
	if storedStateHeldHistory([]watchEvent[*ptahv1alpha1.PtahSchema]{{Type: watch.Modified, Object: injected}, {Type: watch.Modified, Object: transient}, {Type: watch.Modified, Object: end}}, before, "20", "30", version+1) == nil {
		t.Fatal("transient loss hidden by the final reading passed")
	}
	if storedStateHeldHistory(append(events, watchEvent[*ptahv1alpha1.PtahSchema]{Type: watch.Modified, Object: transient}), before, "20", "30", version+1) == nil {
		t.Fatal("state loss after the sentinel but before natural watch EOF passed")
	}
	for _, broken := range [][]watchEvent[*ptahv1alpha1.PtahSchema]{nil, events[:2], events[2:], {{Type: watch.Modified, Object: injected}, {Type: watch.Deleted, Object: end}}, {{Type: watch.Modified, Object: nil}}} {
		if storedStateHeldHistory(broken, before, "20", "30", version+1) == nil {
			t.Fatal("an incomplete or deleted resource history passed")
		}
	}
	migration := storedStateFixtures()[1].(*ptahv1alpha1.PtahMigration)
	changed := withStoredStateVersion(migration, version+1).(*ptahv1alpha1.PtahMigration)
	changed.ResourceVersion = "20"
	last := changed.DeepCopy()
	last.ResourceVersion = "30"
	migrationEvents := []watchEvent[*ptahv1alpha1.PtahMigration]{{Type: watch.Modified, Object: changed}, {Type: watch.Modified, Object: last}}
	if err := storedStateHeldHistory(migrationEvents, migration, "20", "30", version+1); err != nil {
		t.Fatal(err)
	}
	lost := changed.DeepCopy()
	lost.Status.Plan = nil
	if storedStateHeldHistory(append(migrationEvents, watchEvent[*ptahv1alpha1.PtahMigration]{Type: watch.Modified, Object: lost}), migration, "20", "30", version+1) == nil {
		t.Fatal("migration state loss before natural watch EOF passed")
	}
}

func TestStoredStateWorkHistoryAndSQLControlsCannotPassOnNothing(t *testing.T) {
	t.Parallel()
	before := storedStateFixtures()[1]
	jobs := []watchEvent[*batchv1.Job]{{Type: watch.Modified, Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "publisher", UID: "sentinel-job"}}}}
	pods := []watchEvent[*corev1.Pod]{{Type: watch.Modified, Object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "database", UID: "sentinel-pod"}}}}
	if !storedStateCreatedNoWork(jobs, pods, before, labelMigration) {
		t.Fatal("observed unmanaged sentinels were refused")
	}
	if storedStateCreatedNoWork(nil, pods, before, labelMigration) || storedStateCreatedNoWork(jobs, nil, before, labelMigration) {
		t.Fatal("an empty collection proved no work")
	}
	created := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unexpected", OwnerReferences: []metav1.OwnerReference{{UID: before.GetUID()}}}}
	if storedStateCreatedNoWork(append(jobs, watchEvent[*batchv1.Job]{Type: watch.Added, Object: created}), pods, before, labelMigration) {
		t.Fatal("an unlabeled owned Job escaped the complete history")
	}
	createdPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unexpected", Labels: map[string]string{labelMigration: before.GetName()}}}
	if storedStateCreatedNoWork(jobs, append(pods, watchEvent[*corev1.Pod]{Type: watch.Added, Object: createdPod}), before, labelMigration) {
		t.Fatal("a mutating or diagnostic Pod escaped the complete history")
	}
	actor := operationSQLClient{resourceUID: string(before.GetUID()), jobUID: "history-job", podUID: "history-pod", operation: "history"}
	clients := map[string]operationSQLClient{"10.0.0.1": actor}
	counts := map[string]int{"10.0.0.1": 1}
	if err := storedStateHistorySQLControl(clients, counts, actor); err != nil {
		t.Fatal(err)
	}
	if storedStateHistorySQLControl(clients, nil, actor) == nil || storedStateHistorySQLControl(nil, counts, actor) == nil {
		t.Fatal("a missing native History SQL observation passed")
	}
	wrong := actor
	wrong.podUID = "other"
	if storedStateHistorySQLControl(clients, counts, wrong) == nil {
		t.Fatal("another History Pod supplied the allowed control")
	}
}
