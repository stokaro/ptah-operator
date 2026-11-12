package controller

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahSchema deleted while its Apply ran ends the Apply with its outcome
// unknown and then waits for the pending observation's observeAfter, the
// Apply's execution deadline and its Pod's termination grace, before it hands
// the database Lease back (#1011). A live schema shows that instant in
// status.pendingObservation.observeAfter; a deleted one only stays Terminating.
// Where no Pod of the Apply's Job is left to name, an ApplyPodGone Warning says
// why it waits and until when, with the backoff PodStuckTerminating has.
//
// The fake client runs no Job controller and no kubelet: each row writes what
// the Job's deadline leaves. A Pod the kubelet stopped is often removed before
// the pass the Job's update wakes reads it, and a Pod force-deleted, or removed
// with its Node object, is gone as well. Where the Pod still stands, stopped,
// or the claim never recorded its Job's UID, nothing is named. The Events are
// read with applyPodGoneEvents (migration_apply_pod_gone_test.go).

// unrecordedSchemaApplyRun is a schema Apply whose claim never recorded its
// Job's UID, over a Pod waiting on its Secret.
func unrecordedSchemaApplyRun(t *testing.T) unstartedSchema {
	t.Helper()

	builder := workloadBuilderForMigrations()
	dispatched := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{dispatcher: builder}, builder)
	run := unstartedSchema{
		reconciler: dispatched.reconciler, api: dispatched.api, recorder: record.NewFakeRecorder(256),
		schema: dispatched.schema, job: dispatched.job,
	}
	run.reconciler.Recorder = run.recorder
	if operation := run.schema.Status.ActiveOperation; operation == nil || !operation.DispatchStarted || operation.JobUID != "" {
		t.Fatalf("the claim = %#v, want a dispatch that never recorded its Job", operation)
	}
	run.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: run.job.Namespace, Name: generatedTerminalPodName(run.job.Name, "abc12"), UID: "unstarted-pod-uid",
			Labels: maps.Clone(run.job.Spec.Template.Labels), Annotations: maps.Clone(run.job.Spec.Template.Annotations),
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(run.job)},
		},
		Spec: *run.job.Spec.Template.Spec.DeepCopy(),
	}
	waitingOnASecret(run.pod)
	if err := run.api.Create(context.Background(), run.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestADeletedSchemaWaitingWithItsApplyPodGoneSaysUntilWhen(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		run  func(t *testing.T) unstartedSchema
		end  func(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod)
		// named is a wait the Warning names: the record names the Job's UID,
		// and the API server has no Pod of that Job.
		named bool
	}{
		{name: "with its Pod gone past the Job's deadline", run: admittedUnstartedSchemaApplyRun, end: pastTheDeadline, named: true},
		{name: "with its stopped Pod still there", run: admittedUnstartedSchemaApplyRun, end: deadlineStoppedAWaitingPod},
		{name: "under a claim that never recorded its Job", run: unrecordedSchemaApplyRun, end: pastTheDeadline},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := row.run(t)
			deleteSchemaWhileItsApplyRuns(t, run)
			row.end(t, run.api, run.job, run.pod)
			operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			horizon := operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second)
			run.reconcileOnce(t)

			// The ending recorded the wait this row is about.
			ended := safetyGetSchema(t, run.api, run.schema)
			pending := ended.Status.PendingObservation
			switch {
			case ended.Status.ActiveOperation != nil || pending == nil:
				t.Fatalf("the Apply did not end into a pending observation: claim %#v, pending %#v", ended.Status.ActiveOperation, pending)
			case pending.ObserveAfter == nil || !pending.ObserveAfter.Time.Equal(horizon):
				t.Fatalf("the deletion waits until %v, want %s, the Apply's execution deadline and its grace", pending.ObserveAfter, horizon)
			case pending.ApplyJobUID != operation.JobUID:
				t.Fatalf("the record names Job UID %q, want the claim's %q", pending.ApplyJobUID, operation.JobUID)
			case row.named && pending.ApplyJobUID == "":
				t.Fatal("the record names no Job UID, so the row cannot show the Warning names one")
			}
			holder := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace))
			if holder == "" {
				t.Fatal("the deletion holds no database Lease, so the row cannot show it stays held")
			}
			start := run.reconciler.now()
			if !start.Add(30 * time.Second).Before(horizon) {
				t.Fatalf("the wait ends at %s, too soon after %s for the row to see the backoff", horizon, start)
			}
			want := fmt.Sprintf("%sDeletion waits for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
				"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
				"still runs on a node the API server cannot reach, so the database Lease stays held until the "+
				"Apply's execution deadline and its Pod's termination grace have passed.",
				applyPodGoneEvent, operation.JobName, operation.JobUID, horizon.UTC().Format(time.RFC3339))
			applyPodGoneEvents(run.recorder)

			// Each step is a pass at an offset from the ending, and whether it
			// names the wait: at once, then thirty seconds later.
			for _, step := range []struct {
				at    time.Duration
				named bool
			}{
				{at: 0, named: row.named},
				{at: 0},
				{at: 29 * time.Second},
				{at: 30 * time.Second, named: row.named},
			} {
				at := start.Add(step.at)
				run.reconciler.Clock = func() time.Time { return at }
				run.reconcileOnce(t)
				events := applyPodGoneEvents(run.recorder)
				switch {
				case !step.named && len(events) != 0:
					t.Fatalf("a pass %s after the ending recorded %q, want nothing", step.at, events)
				case step.named && (len(events) != 1 || events[0] != want):
					t.Fatalf("a pass %s after the ending recorded %q, want one %q", step.at, events, want)
				}
				if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
					t.Fatalf("the deletion let the database go %s after the ending, before %s", step.at, horizon)
				}
				if held := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace)); held != holder {
					t.Fatalf("the database Lease is held by %q %s after the ending, want %q", held, step.at, holder)
				}
			}

			// At the horizon the deletion goes, and names no wait on its way.
			run.reconciler.Clock = func() time.Time { return horizon }
			schemaGone(t, run, 3)
			if events := applyPodGoneEvents(run.recorder); len(events) != 0 {
				t.Fatalf("the pass at the horizon recorded %q, want nothing", events)
			}
		})
	}
}
