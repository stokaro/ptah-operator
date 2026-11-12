package controller

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahSchema deleted while its Apply ran ends the Apply with its outcome
// unknown and then waits for the pending observation's observeAfter, the
// Apply's execution deadline and its Pod's termination grace, before it hands
// the database Lease back (#1011). A live schema shows that instant in
// status.pendingObservation.observeAfter; a deleted one only stays Terminating.
// Where no active Pod of the Apply's Job is left, an ApplyPodGone Warning says
// why it waits and until when, with the backoff PodStuckTerminating has: that
// the API server has no Pod of the Job, or which stopped Pod still stands
// (#1020).
//
// The fake client runs no Job controller and no kubelet: each row writes what
// the Job's deadline leaves. A Pod the kubelet stopped is often removed before
// the pass the Job's update wakes reads it, and a Pod force-deleted, or removed
// with its Node object, is gone as well. Where the claim never recorded its
// Job's UID, nothing is named. Every pass leaves the wait as the ending wrote
// it: the pending observation, the finalizer and the database Lease's holder.
// The Events are read with applyPodGoneEvents
// (migration_apply_pod_gone_test.go).

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

// aStoppedSecondPod is deadlineStoppedAWaitingPod with a second Pod of the Job
// that stopped the same way and stands, not being deleted. The Job runs one
// Pod, so the state is the fake client's; the row that uses it holds the
// Warning's bound on what it names.
func aStoppedSecondPod(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	deadlineStoppedAWaitingPod(t, api, job, pod)
	ctx := context.Background()
	second := pod.DeepCopy()
	second.ResourceVersion = ""
	second.Name = generatedTerminalPodName(job.Name, "def34")
	second.UID = "replacement-pod-uid"
	if err := api.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	second.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "ContainerStatusUnknown", Message: lostKubeletContainer,
		}},
	}}}
	if err := api.Status().Update(ctx, second); err != nil {
		t.Fatal(err)
	}
}

// applyPodGoneText is the ApplyPodGone Warning a deleted schema records for the
// Apply's Job named by operation, waiting until horizon, where no Pod of the
// Job is left.
func applyPodGoneText(operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time) string {
	return fmt.Sprintf("%sDeletion waits for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
		"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
		"still runs on a node the API server cannot reach, so the database Lease stays held until the "+
		"Apply's execution deadline and its Pod's termination grace have passed.",
		applyPodGoneEvent, operation.JobName, operation.JobUID, horizon.UTC().Format(time.RFC3339))
}

// applyPodStandsText is the same Warning where Pod pod of the Job stands with
// phase, the first of pods stopped Pods of the Job.
func applyPodStandsText(
	operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time, pod string, phase corev1.PodPhase, pods int,
) string {
	others := ""
	if pods > 1 {
		others = fmt.Sprintf(" (one of %d Pods of the Job, none of them active)", pods)
	}
	return fmt.Sprintf("%sDeletion waits for Job %s, UID %s, until %s: Pod %s of the Job reads %s%s, and nothing the "+
		"Apply's ending read says its executor ran and stopped. A terminal phase does not say so either: PodGC writes "+
		"Failed on a Pod whose Node object is gone while its container can still run on a node the API server "+
		"cannot reach, so the database Lease stays held until the Apply's execution deadline and its Pod's "+
		"termination grace have passed.",
		applyPodGoneEvent, operation.JobName, operation.JobUID, horizon.UTC().Format(time.RFC3339), pod, phase, others)
}

func TestADeletedSchemaWaitingWithItsApplyPodGoneSaysUntilWhen(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		run  func(t *testing.T) unstartedSchema
		end  func(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod)
		// want is the Warning that names the wait, given the Apply's claim,
		// the wait's end and the row's first Pod, or nil where the record
		// names no Job UID and nothing is named.
		want func(operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time, pod *corev1.Pod) string
	}{
		{
			name: "with its Pod gone past the Job's deadline", run: admittedUnstartedSchemaApplyRun, end: pastTheDeadline,
			want: func(operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time, _ *corev1.Pod) string {
				return applyPodGoneText(operation, horizon)
			},
		},
		{
			name: "with its stopped Pod still there", run: admittedUnstartedSchemaApplyRun, end: deadlineStoppedAWaitingPod,
			want: func(operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time, pod *corev1.Pod) string {
				return applyPodStandsText(operation, horizon, pod.Name, corev1.PodFailed, 1)
			},
		},
		{
			name: "with two stopped Pods still there", run: admittedUnstartedSchemaApplyRun, end: aStoppedSecondPod,
			want: func(operation *operatorv1alpha1.ActiveOperationStatus, horizon time.Time, pod *corev1.Pod) string {
				// The Pods are named in UID order: "pod-<Job UID>" comes
				// before "replacement-pod-uid".
				return applyPodStandsText(operation, horizon, pod.Name, corev1.PodFailed, 2)
			},
		},
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
			case row.want != nil && pending.ApplyJobUID == "":
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
			named := row.want != nil
			want := ""
			if named {
				want = row.want(operation, horizon, run.pod)
			}
			applyPodGoneEvents(run.recorder)

			// Each step is a pass at an offset from the ending, and whether it
			// names the wait: at once, then thirty seconds later.
			for _, step := range []struct {
				at    time.Duration
				named bool
			}{
				{at: 0, named: named},
				{at: 0},
				{at: 29 * time.Second},
				{at: 30 * time.Second, named: named},
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
				deletionWaitHeld(t, run, pending, holder, step.at)
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

// deletionWaitHeld fails unless the deleted schema still waits as its Apply's
// ending left it, at a pass at after the ending: no claim, the pending
// observation pending unchanged, the active-operation finalizer kept and the
// database Lease held by holder.
func deletionWaitHeld(
	t *testing.T, run unstartedSchema, pending *operatorv1alpha1.PendingObservationStatus, holder string, at time.Duration,
) {
	t.Helper()

	waiting := safetyGetSchema(t, run.api, run.schema)
	switch {
	case waiting.Status.ActiveOperation != nil:
		t.Fatalf("a pass %s after the ending holds a claim %#v, want none", at, waiting.Status.ActiveOperation)
	case !reflect.DeepEqual(waiting.Status.PendingObservation, pending):
		t.Fatalf("a pass %s after the ending moved the wait:\n got %#v\nwant %#v", at, waiting.Status.PendingObservation, pending)
	case !controllerutil.ContainsFinalizer(waiting, activeOperationFinalizer):
		t.Fatalf("a pass %s after the ending dropped the active-operation finalizer", at)
	}
	if held := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace)); held != holder {
		t.Fatalf("the database Lease is held by %q %s after the ending, want %q", held, at, holder)
	}
}

// A stopped Pod that stands and then goes leaves the same wait, so the Warning
// keeps its backoff: the pass after the Pod went names the wait only once the
// interval the standing Pod's Warning started has passed, and in the words for
// a Pod that is gone.
func TestADeletedSchemaWhoseStoppedApplyPodGoesKeepsTheBackoff(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	deadlineStoppedAWaitingPod(t, run.api, run.job, run.pod)
	operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	horizon := operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second)
	run.reconcileOnce(t)
	pending := safetyGetSchema(t, run.api, run.schema).Status.PendingObservation
	if pending == nil || pending.ObserveAfter == nil || !pending.ObserveAfter.Time.Equal(horizon) || pending.ApplyJobUID == "" {
		t.Fatalf("the Apply did not end into a wait until %s under its Job's UID: %#v", horizon, pending)
	}
	holder := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace))
	if holder == "" {
		t.Fatal("the deletion holds no database Lease, so the row cannot show it stays held")
	}
	start := run.reconciler.now()
	if !start.Add(30 * time.Second).Before(horizon) {
		t.Fatalf("the wait ends at %s, too soon after %s for the row to see the backoff", horizon, start)
	}
	applyPodGoneEvents(run.recorder)

	pass := func(at time.Duration) []string {
		t.Helper()
		now := start.Add(at)
		run.reconciler.Clock = func() time.Time { return now }
		run.reconcileOnce(t)
		deletionWaitHeld(t, run, pending, holder, at)
		return applyPodGoneEvents(run.recorder)
	}
	if want, events := applyPodStandsText(operation, horizon, run.pod.Name, corev1.PodFailed, 1), pass(0); len(events) != 1 || events[0] != want {
		t.Fatalf("the pass with the stopped Pod standing recorded %q, want one %q", events, want)
	}

	// The kubelet reclaims the Pod: the last finalizer goes, and with it the
	// Pod the deletion had already been asked for.
	ctx := context.Background()
	stored := &corev1.Pod{}
	if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Finalizers = nil
	if err := run.api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.pod), stored); !apierrors.IsNotFound(err) {
		t.Fatalf("the Pod still reads %v after its last finalizer went, so the row cannot show a Pod that went", err)
	}

	if events := pass(29 * time.Second); len(events) != 0 {
		t.Fatalf("the pass after the Pod went recorded %q before the backoff passed, want nothing", events)
	}
	if want, events := applyPodGoneText(operation, horizon), pass(30*time.Second); len(events) != 1 || events[0] != want {
		t.Fatalf("the pass once the backoff passed recorded %q, want one %q", events, want)
	}
}
