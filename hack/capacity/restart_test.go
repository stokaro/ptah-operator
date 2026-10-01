package main

import (
	"context"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	opworkload "github.com/stokaro/ptah-operator/internal/workload"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestRestartRejectsMissingApprovalDespiteConvergedFleet(t *testing.T) {
	s := &scenarios{
		in:   inputs{namespace: "work", operatorNamespace: "operator"},
		load: workload{Settle: duration{time.Second}},
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList",
		}),
		clientset: fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: "operator", UID: "manager-uid"}}),
	}
	// The rest of the fleet is already converged. It cannot stand in for the
	// independently approved operation that the restart scenario promises.
	if err := s.restart(context.Background()); err == nil {
		t.Fatal("restart passed without building or admitting an approval")
	}
	if len(s.windows) != 1 || s.windows[0].Outcome["error"] == "" {
		t.Fatal("restart failure is missing from the report")
	}
	for _, action := range s.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() == "delete" {
			preconditions := action.(clienttesting.DeleteAction).GetDeleteOptions().Preconditions
			if preconditions == nil || preconditions.UID == nil || *preconditions.UID != "manager-uid" {
				t.Fatal("manager deletion can delete a replacement Pod")
			}
		}
	}
}

func restartFixture(t *testing.T) (*scenarios, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *batchv1.Job) {
	t.Helper()
	instant := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	toObject := func(value any) *unstructured.Unstructured {
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
		if err != nil {
			t.Fatal(err)
		}
		return &unstructured.Unstructured{Object: object}
	}
	migration := &operatorv1alpha1.PtahMigration{
		TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration"},
		ObjectMeta: metav1.ObjectMeta{Name: approvalResource, Namespace: "work", UID: "migration-uid", ResourceVersion: "10", Generation: 1, CreationTimestamp: metav1.NewTime(instant.Add(-time.Hour))},
	}
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	migration.Status.ObservedGeneration = 1
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: "plan", UID: "plan-uid"}
	migration.Status.Conditions = []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "ApprovalRequired", LastTransitionTime: metav1.NewTime(instant)}}
	original := toObject(migration)
	plan := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(), "kind": "PtahMigrationPlan",
		"metadata": map[string]any{"name": "plan", "namespace": "work", "uid": "plan-uid"},
		"spec":     map[string]any{"migrationRef": map[string]any{"name": approvalResource, "uid": "migration-uid"}, "fingerprint": "plan-fingerprint"},
	}}
	s := &scenarios{in: inputs{namespace: "work"}, load: workload{Settle: duration{time.Minute}}, dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), original, plan)}
	approval, err := s.approvalFor(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	approval.SetUID("approval-uid")
	approval.SetCreationTimestamp(metav1.NewTime(instant))
	op := &operatorv1alpha1.MigrationOperationStatus{
		Type: operatorv1alpha1.MigrationOperationApply, ID: "sha256:" + strings.Repeat("a", 64), InputFingerprint: "sha256:" + strings.Repeat("b", 64),
		JobName: "approved-apply", JobUID: "job-uid", StartedAt: metav1.NewTime(instant.Add(time.Second)), ExecutionBindingID: "v1-" + strings.Repeat("c", 32),
		ApprovalRef: &operatorv1alpha1.ImmutableObjectReference{Name: approvalResource, UID: approval.GetUID()}, PlanRef: migration.Status.Plan.DeepCopy(),
	}
	migration.Status.ActiveOperation = op
	controller, block := true, true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: op.JobName, Namespace: "work", UID: op.JobUID, CreationTimestamp: metav1.NewTime(instant.Add(2 * time.Second)),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration", Name: migration.Name, UID: migration.UID, Controller: &controller, BlockOwnerDeletion: &block}},
		Labels:          map[string]string{opworkload.LabelManagedBy: "ptah-operator", opworkload.LabelComponent: opworkload.ComponentMigrationOperation, opworkload.LabelMigration: migration.Name, opworkload.LabelOperation: "apply", opworkload.LabelOperationID: opworkload.OperationIDLabelValue(op.ID)},
		Annotations:     map[string]string{opworkload.AnnotationSafeToEvict: "false", opworkload.AnnotationOperationID: op.ID, opworkload.AnnotationInputFingerprint: op.InputFingerprint, opworkload.AnnotationExecutionBindingID: op.ExecutionBindingID, opworkload.AnnotationPtahVersion: "v0.11.0", opworkload.AnnotationControllerStateVersion: "1", opworkload.AnnotationControllerImage: "example.test/controller@sha256:" + strings.Repeat("d", 64), opworkload.AnnotationControllerRevision: "test-revision"},
	}}
	job.Spec.Template.ObjectMeta = metav1.ObjectMeta{Labels: job.Labels, Annotations: job.Annotations}
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "ptah", Image: "example.test/executor"}}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	op.AdmissionSnapshot = &operatorv1alpha1.PodAdmissionSnapshot{Version: podintent.SnapshotVersion, TemplateDigest: templateDigest, ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{Object: operatorv1alpha1.AdmissionObjectBinding{Name: "executor", UID: "sa-uid", ResourceVersion: "1"}}}
	op.AdmissionSnapshot.Digest, err = fingerprint.DigestCanonicalJSON(*op.AdmissionSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	job.Annotations[opworkload.AnnotationAdmissionSnapshotDigest] = op.AdmissionSnapshot.Digest
	s.clientset = fake.NewClientset(job)
	return s, original, approval, toObject(migration), job
}

func TestRestartBindsApprovalClaimAndJob(t *testing.T) {
	for _, mutation := range []string{"valid", "old job", "wrong owner", "wrong job UID", "wrong approval", "wrong plan", "replaced migration", "changed generation", "old claim", "different operation", "missing job", "closed watch", "stale history", "unresolved apply"} {
		t.Run(mutation, func(t *testing.T) {
			s, original, approval, claimed, job := restartFixture(t)
			switch mutation {
			case "old job":
				job.CreationTimestamp = metav1.NewTime(approval.GetCreationTimestamp().Add(-time.Second))
			case "wrong owner":
				job.OwnerReferences[0].UID = "another-migration"
			case "wrong job UID":
				job.UID = "replacement-job"
			case "wrong approval":
				_ = unstructured.SetNestedField(claimed.Object, "another-approval", "status", "activeOperation", "approvalRef", "uid")
			case "wrong plan":
				_ = unstructured.SetNestedField(claimed.Object, "another-plan", "status", "activeOperation", "planRef", "uid")
			case "replaced migration":
				claimed.SetUID("replacement-migration")
			case "changed generation":
				claimed.SetGeneration(2)
			case "old claim":
				_ = unstructured.SetNestedField(claimed.Object, approval.GetCreationTimestamp().Add(-time.Second).Format(time.RFC3339), "status", "activeOperation", "startedAt")
			case "different operation":
				job.Annotations[opworkload.AnnotationOperationID] = "sha256:" + strings.Repeat("e", 64)
			}
			s.clientset = fake.NewClientset(job)
			if mutation == "missing job" {
				s.clientset = fake.NewClientset()
			}
			// The latest GET is already past Apply. The watch must retain the
			// preceding claim, while recovery must prove fresh History readiness.
			recovered := original.DeepCopy()
			_ = unstructured.SetNestedSlice(recovered.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "HistoryMatched", "observedGeneration": int64(1), "lastTransitionTime": job.CreationTimestamp.Add(time.Second).Format(time.RFC3339)}}, "status", "conditions")
			_ = unstructured.SetNestedField(recovered.Object, job.CreationTimestamp.Add(time.Second).Format(time.RFC3339), "status", "history", "observedAt")
			if mutation == "stale history" {
				_ = unstructured.SetNestedField(recovered.Object, job.CreationTimestamp.Add(-time.Minute).Format(time.RFC3339), "status", "history", "observedAt")
			}
			if mutation == "unresolved apply" {
				_ = unstructured.SetNestedField(recovered.Object, map[string]any{"outcome": "Unknown"}, "status", "unresolvedRun")
			}
			_, err := s.dynamic.Resource(migrationResource).Namespace("work").Update(t.Context(), recovered, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stream := watch.NewRaceFreeFake()
			defer stream.Stop()
			if mutation == "closed watch" {
				stream.Stop()
			} else {
				stream.Modify(claimed)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			outcome, err := s.waitApprovedDispatch(ctx, approval.GetCreationTimestamp().Time, original, approval, stream, map[string]string{})
			if mutation == "valid" {
				if err != nil || outcome["approvalJobUID"] != "job-uid" || outcome["approvalConverged"] == "" {
					t.Fatalf("valid dispatch and recovery failed: %v %v", outcome, err)
				}
			} else if err == nil {
				t.Fatal("unrelated or stale Apply passed")
			}
		})
	}
}

func TestRestartRejectsStaleApprovalGateAndPlan(t *testing.T) {
	for _, mutation := range []string{"old generation", "old condition", "wrong plan UID", "wrong plan owner", "missing fingerprint"} {
		t.Run(mutation, func(t *testing.T) {
			s, original, _, _, _ := restartFixture(t)
			plan, err := s.dynamic.Resource(migrationPlanResource).Namespace("work").Get(t.Context(), "plan", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "old generation":
				original.SetGeneration(2)
			case "old condition":
				conditions, _, _ := unstructured.NestedSlice(original.Object, "status", "conditions")
				conditions[0].(map[string]any)["observedGeneration"] = int64(0)
				_ = unstructured.SetNestedSlice(original.Object, conditions, "status", "conditions")
			case "wrong plan UID":
				plan.SetUID("replacement-plan")
			case "wrong plan owner":
				_ = unstructured.SetNestedField(plan.Object, "another-migration", "spec", "migrationRef", "uid")
			case "missing fingerprint":
				unstructured.RemoveNestedField(plan.Object, "spec", "fingerprint")
			}
			if _, err := s.dynamic.Resource(migrationPlanResource).Namespace("work").Update(t.Context(), plan, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.approvalFor(t.Context(), original); err == nil {
				t.Fatal("stale approval binding accepted")
			}
		})
	}
}

func TestRestartRejectsExistingApprovalAndCanceledAdmission(t *testing.T) {
	for _, existing := range []bool{true, false} {
		s, original, approval, _, _ := restartFixture(t)
		if existing {
			if _, err := s.dynamic.Resource(approvalGVR).Namespace("work").Create(t.Context(), approval, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		s.dynamic.(*dynamicfake.FakeDynamicClient).PrependWatchReactor("ptahmigrations", func(clienttesting.Action) (bool, watch.Interface, error) { return true, watch.NewRaceFreeFake(), nil })
		ctx, cancel := context.WithCancel(t.Context())
		if !existing {
			cancel()
		}
		_, err := s.approveAndWait(ctx, time.Now())
		cancel()
		if err == nil {
			t.Fatalf("existing=%v: admission passed without a fresh confirmed create for %s", existing, original.GetUID())
		}
	}
}

func TestRestartWatchesBeforeAdmissionAndKeepsFastApply(t *testing.T) {
	s, original, approval, claimed, job := restartFixture(t)
	client := s.dynamic.(*dynamicfake.FakeDynamicClient)
	stream := watch.NewRaceFreeFake()
	watchStarted := false
	client.PrependWatchReactor("ptahmigrations", func(action clienttesting.Action) (bool, watch.Interface, error) {
		restrictions := action.(clienttesting.WatchAction).GetWatchRestrictions()
		if restrictions.ResourceVersion != original.GetResourceVersion() || restrictions.Fields.String() != "metadata.name="+approvalResource {
			t.Fatal("watch is not bound to the original resource reading")
		}
		watchStarted = true
		return true, stream, nil
	})
	client.PrependReactor("create", "ptahmigrationapprovals", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !watchStarted {
			t.Fatal("approval was submitted before its claim watch")
		}
		stream.Modify(claimed)
		recovered := original.DeepCopy()
		_ = unstructured.SetNestedSlice(recovered.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "HistoryMatched", "observedGeneration": int64(1), "lastTransitionTime": job.CreationTimestamp.Add(time.Second).Format(time.RFC3339)}}, "status", "conditions")
		_ = unstructured.SetNestedField(recovered.Object, job.CreationTimestamp.Add(time.Second).Format(time.RFC3339), "status", "history", "observedAt")
		if err := client.Tracker().Update(migrationResource, recovered, "work"); err != nil {
			t.Fatal(err)
		}
		return true, approval, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	outcome, err := s.approveAndWait(ctx, approval.GetCreationTimestamp().Time)
	if err != nil || outcome["approvalUID"] != string(approval.GetUID()) || outcome["approvalJobUID"] != string(job.UID) || outcome["approvalConverged"] == "" {
		t.Fatalf("fast Apply lost its binding or recovery: %v %v", outcome, err)
	}
}
