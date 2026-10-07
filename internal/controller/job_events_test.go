package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestOperationJobEventsIgnoreReadinessOnly(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "operation", Namespace: "work", UID: "original", ResourceVersion: "1"},
		Status: batchv1.JobStatus{Active: 1, Ready: ptr[int32](0)}}
	updated := job.DeepCopy()
	updated.ResourceVersion = "2"
	updated.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}}
	updated.Status.Ready = ptr[int32](1)
	if operationJobEvents().Update(event.UpdateEvent{ObjectOld: job, ObjectNew: updated}) {
		t.Fatal("readiness-only update repeated operation reconciliation")
	}
	if *job.Status.Ready != 0 || *updated.Status.Ready != 1 || updated.ResourceVersion != "2" || len(updated.ManagedFields) != 1 {
		t.Fatal("predicate mutated the shared watch objects")
	}
	if !operationJobEvents().Create(event.CreateEvent{Object: job}) ||
		!operationJobEvents().Delete(event.DeleteEvent{Object: job}) ||
		!operationJobEvents().Generic(event.GenericEvent{Object: job}) {
		t.Fatal("non-update event was lost")
	}
}

func TestOperationJobEventsPreserveOtherChanges(t *testing.T) {
	for name, change := range map[string]func(*batchv1.Job){
		"Pod started":   func(j *batchv1.Job) { j.Status.Active = 1 },
		"Pod succeeded": func(j *batchv1.Job) { j.Status.Succeeded = 1 },
		"Pod failed":    func(j *batchv1.Job) { j.Status.Failed = 1 },
		"Job complete": func(j *batchv1.Job) {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		},
		"Job failed": func(j *batchv1.Job) {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		},
		"UID replaced":     func(j *batchv1.Job) { j.UID = "replacement" },
		"deletion":         func(j *batchv1.Job) { j.DeletionTimestamp = &metav1.Time{Time: time.Unix(100, 0)} },
		"finalizer":        func(j *batchv1.Job) { j.Finalizers = []string{"foregroundDeletion"} },
		"owner replaced":   func(j *batchv1.Job) { j.OwnerReferences = []metav1.OwnerReference{{UID: "replacement"}} },
		"labels":           func(j *batchv1.Job) { j.Labels = map[string]string{"changed": "value"} },
		"annotations":      func(j *batchv1.Job) { j.Annotations = map[string]string{"changed": "value"} },
		"generation":       func(j *batchv1.Job) { j.Generation++ },
		"execution intent": func(j *batchv1.Job) { j.Spec.Template.Spec.ServiceAccountName = "replacement" },
	} {
		t.Run(name, func(t *testing.T) {
			before := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "operation", Namespace: "work", UID: "original"}}
			after := before.DeepCopy()
			after.Status.Ready = ptr[int32](1)
			change(after)
			if !operationJobEvents().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
				t.Fatal("a material update was hidden by the readiness change")
			}
		})
	}
}
