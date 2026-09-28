package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// TestDeletingASchemaSettlesAnApplyItCannotAccountFor is the schema half of
// TestDeletingAMigrationSettlesAnApplyItCannotAccountFor. Deletion is the one
// path where dropping the claim is also the last chance to say anything: the
// finalizer comes off at the end of it, and a claim discarded there leaves no
// record anywhere that a run may have reached the database. So a deleting
// schema whose dispatched Apply cannot be attributed to a standing Job has to
// persist the outcome as unknown before the object goes.
func TestDeletingASchemaSettlesAnApplyItCannotAccountFor(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		workload func(
			operation *operatorv1alpha1.ActiveOperationStatus,
			job *batchv1.Job,
			pod *corev1.Pod,
		) []client.Object
	}{
		{
			// The Job was collected while the claim still named it. The claim
			// is the only remaining evidence that a run was dispatched.
			name: "the dispatched Job is gone",
			workload: func(_ *operatorv1alpha1.ActiveOperationStatus, _ *batchv1.Job, _ *corev1.Pod) []client.Object {
				return nil
			},
		},
		{
			// A later attempt reserved the same name. The Job standing there
			// did not perform this run, so its progress says nothing about
			// what this claim dispatched.
			name: "another Job holds the name the claim reserved",
			workload: func(_ *operatorv1alpha1.ActiveOperationStatus, job *batchv1.Job, pod *corev1.Pod) []client.Object {
				job.UID = "replacement-job-uid"
				job.Status.Conditions = nil
				job.Status.Active = 1
				pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
				runningExecutorPod(pod)
				return []client.Object{job, pod}
			},
		},
		{
			// The Job under the reserved name is owned by something else.
			// Waiting on it would be waiting on another resource's run.
			name: "the Job under the reserved name is owned by something else",
			workload: func(_ *operatorv1alpha1.ActiveOperationStatus, job *batchv1.Job, pod *corev1.Pod) []client.Object {
				job.OwnerReferences = nil
				job.Status.Conditions = nil
				job.Status.Active = 1
				pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
				runningExecutorPod(pod)
				return []client.Object{job, pod}
			},
		},
		{
			// DispatchStarted is persisted before the create and JobUID only
			// after it, so this is what a lost answer leaves behind. Nothing
			// stands under the reserved name, and the claim's own record of
			// the attempt is all that says a run may have happened.
			name: "the claim started a dispatch and recorded no Job",
			workload: func(operation *operatorv1alpha1.ActiveOperationStatus, _ *batchv1.Job, _ *corev1.Pod) []client.Object {
				operation.DispatchStarted = true
				operation.JobUID = ""
				return nil
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyApplySchema(t)
			schema.Status.Plan.Name = "current-plan"
			schema.Status.Plan.UID = "current-plan-uid"
			schema.Status.Plan.Fingerprint = testDigest
			schema.Status.Plan.ContentDigest = safetyOtherDigest
			schema.Status.Plan.ControllerImage = testControllerImage
			schema.Status.Plan.ControllerRevision = testControllerRevision
			schema.Status.Plan.ControllerStateVersion = schema.Status.ExecutionBinding.ControllerStateVersion
			operation := schema.Status.ActiveOperation
			operation.StartedAt = metav1.NewTime(time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC))
			operation.DispatchNotAfter = nil
			operation.ExecutionNotAfter = nil
			operation.TerminationGracePeriodSeconds = 0
			operation.DispatchStarted = true
			operation.JobUID = "dispatched-apply-job-uid"
			bindActiveInput(t, schema)
			ensureTestAdmissionSnapshot(schema)
			jobName, err := workload.NameFor(schema, *operation)
			if err != nil {
				t.Fatal(err)
			}
			operation.JobName = jobName
			job, err := (fakeJobs{}).Build(schema, *operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			job.UID = operation.JobUID
			bindCurrentApplyJob(t, job, schema, operation, schema.Status.Plan)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       job.Namespace,
					Name:            job.Name + "-0",
					UID:             types.UID("unaccounted-apply-pod-uid"),
					OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
				},
			}
			deletedAt := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
			schema.DeletionTimestamp = &deletedAt

			objects := []client.Object{schema}
			objects = append(objects, row.workload(operation, job, pod)...)

			reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("Reconcile() deleting an unaccounted-for Apply = %v", err)
			}

			persisted := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), persisted); err != nil {
				if apierrors.IsNotFound(err) {
					t.Fatal("deletion removed the schema without recording that the Apply may have run")
				}
				t.Fatal(err)
			}
			pending := persisted.Status.PendingObservation
			if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
				t.Fatalf("deleting an unaccounted-for Apply recorded no unknown outcome: %#v", persisted.Status)
			}
			if persisted.Status.ActiveOperation != nil {
				t.Fatalf("the settled claim outlived its own settlement: %#v", persisted.Status.ActiveOperation)
			}
		})
	}
}

// A deleting schema waits on a running Job only while its claim holds the
// database lock. A Resolve, a Verify or an ordinary Observe holds none, so the
// claim is dropped and the finalizer comes off in the same pass, with the Job
// left to cascading deletion. The Job here has no Pod and no verdict, which is
// what a Pod refused at admission leaves: waiting on it would hold the
// resource for the Job's whole deadline.
//
// A Plan holds the lock, and the last row keeps it: the resource stays, and
// so does the claim that names the lock's epoch.
func TestDeletingASchemaWaitsOnlyForAJobThatHoldsTheLock(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		operation operatorv1alpha1.OperationType
		kept      bool
	}{
		{operation: operatorv1alpha1.OperationResolve},
		{operation: operatorv1alpha1.OperationVerify},
		{operation: operatorv1alpha1.OperationObserve},
		{operation: operatorv1alpha1.OperationPlan, kept: true},
	} {
		t.Run(string(row.operation), func(t *testing.T) {
			t.Parallel()

			schema := safetyLockedOperationSchema(row.operation)
			operation := schema.Status.ActiveOperation
			operation.JobName = "ptah-refused-read-only-job"
			operation.JobUID = "refused-read-only-job-uid"
			operation.DispatchStarted = true
			deletedAt := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
			schema.DeletionTimestamp = &deletedAt
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       schema.Namespace,
					Name:            operation.JobName,
					UID:             operation.JobUID,
					OwnerReferences: []metav1.OwnerReference{schemaControllerReference(schema)},
				},
			}

			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("Reconcile() deleting a schema with a %s Job standing = %v", row.operation, err)
			}

			persisted := &operatorv1alpha1.PtahSchema{}
			err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), persisted)
			switch {
			case row.kept && err != nil:
				t.Fatalf("a running %s Job that holds the lock did not keep the resource: %v", row.operation, err)
			case row.kept && persisted.Status.ActiveOperation == nil:
				t.Fatalf("the %s claim was dropped while its Job still runs under the lock", row.operation)
			case !row.kept && !apierrors.IsNotFound(err):
				t.Fatalf("a %s Job that holds no lock kept the resource: error %v, status %#v", row.operation, err, persisted.Status)
			}
		})
	}
}
