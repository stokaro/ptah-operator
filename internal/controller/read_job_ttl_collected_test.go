package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A read-only Job is created with its cleanup TTL, and the TTL controller deletes
// it that long after it finishes, whether or not the manager has read its result
// by then: a manager that was down longer loses the result of a finished read.
// The read runs again. These rows hold both families to that: a finished read
// whose Job the TTL controller has collected, or is collecting, moves its claim
// to a new attempt under a new name, rather than failing or waiting for a result
// that is gone.
//
// The TTL controller deletes a Job in the foreground: the Job keeps a deletion
// timestamp and the foregroundDeletion finalizer until the garbage collector has
// deleted its Pods, and only then goes. A pass can find either state.
//
// Every Job here comes from the real builder, and every Job write passes the real
// controller-write webhook.

// ttlCollection is how far the TTL controller got with a finished read's Job when
// the pass reads it.
type ttlCollection struct {
	name string
	// gone has the Job deleted outright; otherwise its Pods are gone and the
	// Job waits on the foregroundDeletion finalizer.
	gone bool
}

var ttlCollections = []ttlCollection{
	{name: "the Job is being deleted"},
	{name: "the Job is gone", gone: true},
}

// collectAsTheTTLControllerDoes takes the finished Job and its Pod out of the API
// the way the TTL controller's foreground deletion does, as far as collection
// says.
func collectAsTheTTLControllerDoes(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod, collection ttlCollection) {
	t.Helper()
	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if ttl := stored.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the read's Job carries TTL %s, so no TTL controller would collect it", ttlText(ttl))
	}
	if !jobTerminal(stored) {
		t.Fatal("the read's Job has not finished, so no TTL controller would collect it")
	}
	removeFromAPI(t, api, pod)
	if collection.gone {
		removeFromAPI(t, api, stored)
		return
	}
	stored.Finalizers = append(stored.Finalizers, metav1.FinalizerDeleteDependents)
	if err := api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	deleting := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), deleting); err != nil || deleting.DeletionTimestamp == nil {
		t.Fatalf("the Job is not held in deletion (%v), so the row measures nothing about it: %#v", err, deleting.ObjectMeta)
	}
}

// removeFromAPI deletes object for good, its finalizers first.
func removeFromAPI(t *testing.T, api client.Client, object client.Object) {
	t.Helper()
	ctx := context.Background()
	if err := api.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	if len(object.GetFinalizers()) != 0 {
		object.SetFinalizers(nil)
		if err := api.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.Delete(ctx, object); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

func TestASchemaReadWhoseJobItsTTLCollectedRunsAgain(t *testing.T) {
	t.Parallel()

	for _, collection := range ttlCollections {
		t.Run(collection.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := schemaReadDispatchedBy(t, runShape{dispatcher: current, recorded: true})
			reconciler, api, recorder := run.reconciler(t, current)
			finishSchemaRead(t, api, run)
			collectAsTheTTLControllerDoes(t, api, run.job, run.pod, collection)
			claimed := run.schema.Status.ActiveOperation

			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() over the collected Job: %v", err)
			}
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the read moved to attempt %d", operation, claimed.Attempt+1)
			}
			if operation.JobUID != "" || operation.JobName == claimed.JobName || operation.DispatchStarted {
				t.Fatalf("the retried read names Job %q (UID %q, dispatched %t), want a new name and nothing dispatched",
					operation.JobName, operation.JobUID, operation.DispatchStarted)
			}
			if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("a Job its TTL collected is named as staying: %q", events)
			}
		})
	}
}

func TestAMigrationReadWhoseJobItsTTLCollectedRunsAgain(t *testing.T) {
	t.Parallel()

	for _, collection := range ttlCollections {
		t.Run(collection.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
			reconciler, api := run.reconciler(t, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			finishMigrationJob(t, api, run, batchv1.JobComplete)
			collectAsTheTTLControllerDoes(t, api, run.job, run.pod, collection)
			claimed := run.migration.Status.ActiveOperation

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() over the collected Job: %v", err)
			}
			actual := readMigration(t, api, run.migration)
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the read moved to attempt %d", operation, claimed.Attempt+1)
			}
			if operation.JobUID != "" || operation.JobName == claimed.JobName {
				t.Fatalf("the retried read names Job %q (UID %q), want a new name and no UID", operation.JobName, operation.JobUID)
			}
			if actual.Status.LastRun != nil {
				t.Fatalf("a read settled a run: %#v", actual.Status.LastRun)
			}
			if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("a Job its TTL collected is named as staying: %q", events)
			}
		})
	}
}
