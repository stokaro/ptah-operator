package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A dispatched Apply whose Job is no longer the one it dispatched is an
// outcome nobody can account for, not a retry. The migration family had five
// tests saying so and the schema family had none: flipping its claim to
// read-only, so that a replaced or disowned Job produced a retry instead,
// passed the entire suite.
//
// That is the asymmetry #225 is about, in the one place where the two verbs
// cost different things. A retry here dispatches a second Apply beside a Job
// that may still be executing SQL.

// dispatchedApplySchema is a schema whose Apply crossed its dispatch boundary
// and recorded the Job it created.
func dispatchedApplySchema(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()

	schema := safetyApplySchema(t)
	schema.Status.ActiveOperation.DispatchStarted = true
	schema.Status.ActiveOperation.JobUID = types.UID("the-dispatched-job")
	return schema
}

// impostorJob stands under the claim's reserved name and is not the claim's
// Job. owned says whether it at least belongs to this schema.
func impostorJob(schema *operatorv1alpha1.PtahSchema, uid types.UID, owned bool) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      schema.Status.ActiveOperation.JobName,
			UID:       uid,
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
		}},
	}
	if owned {
		controller := true
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "PtahSchema",
			Name:       schema.Name,
			UID:        schema.UID,
			Controller: &controller,
		}}
	}
	return job
}

func assertApplyWentUnaccounted(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()

	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil {
		t.Fatalf("the Apply was not recorded as owing proof: phase=%q operation=%#v",
			actual.Status.Phase, actual.Status.ActiveOperation)
	}
	if pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("the Apply was recorded with outcome %q, want an unknown one", pending.Outcome)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the claim survived as %#v, so the pass retried rather than accounted",
			actual.Status.ActiveOperation)
	}
}

// A Job that took the name and is not the one the claim dispatched.
func TestADispatchedApplyWhoseJobWasReplacedIsUnaccountedFor(t *testing.T) {
	t.Parallel()

	schema := dispatchedApplySchema(t)
	job := impostorJob(schema, types.UID("someone-elses-job"), true)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertApplyWentUnaccounted(t, api, schema)
}

// A Job under the claim's name that this schema does not own.
func TestADispatchedApplyWhoseJobLostItsOwnerIsUnaccountedFor(t *testing.T) {
	t.Parallel()

	schema := dispatchedApplySchema(t)
	job := impostorJob(schema, schema.Status.ActiveOperation.JobUID, false)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertApplyWentUnaccounted(t, api, schema)
}
