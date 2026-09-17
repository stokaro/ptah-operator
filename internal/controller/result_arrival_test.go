package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A frame the log ends inside is read again for a bounded time after the Job
// finished, and every other answer is judged at once (#154).
func TestAwaitFrameArrivalWaitsOnlyForAFrameThatMayStillArrive(t *testing.T) {
	t.Parallel()

	finished := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	finishedJob := func(conditionType batchv1.JobConditionType) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: conditionType, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finished),
		}}}}
	}
	incomplete := func() error {
		_, err := runner.ParseResultWithOptions([]byte("PTAH_RUNNER_RESULT_V1 12"), runner.ParseOptions{})
		return err
	}()
	if !errors.Is(incomplete, runner.ErrIncompleteFrame) {
		t.Fatalf("fixture error = %v, want an incomplete frame", incomplete)
	}
	wrong := fmt.Errorf("%w: the frame payload does not match the digest its header declares", runner.ErrMalformedFrame)

	tests := []struct {
		name     string
		job      *batchv1.Job
		parseErr error
		now      time.Time
		wait     bool
	}{
		{name: "a frame that parsed", job: finishedJob(batchv1.JobComplete), now: finished.Add(time.Second)},
		{name: "bytes that are wrong", job: finishedJob(batchv1.JobComplete), parseErr: wrong, now: finished.Add(time.Second)},
		{name: "an incomplete frame just after completion", job: finishedJob(batchv1.JobComplete), parseErr: incomplete, now: finished.Add(time.Second), wait: true},
		{name: "an incomplete frame just after failure", job: finishedJob(batchv1.JobFailed), parseErr: incomplete, now: finished.Add(time.Second), wait: true},
		{name: "no frame yet", job: finishedJob(batchv1.JobComplete), parseErr: runner.ErrFrameNotFound, now: finished.Add(time.Second), wait: true},
		{name: "an incomplete frame past the window", job: finishedJob(batchv1.JobComplete), parseErr: incomplete, now: finished.Add(frameArrivalWindow)},
		{name: "a Job with no recorded finish", job: &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}}}, parseErr: incomplete, now: finished.Add(time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requeue, wait := awaitFrameArrival(test.job, test.parseErr, test.now)
			if wait != test.wait {
				t.Fatalf("awaitFrameArrival() wait = %v, want %v", wait, test.wait)
			}
			if wait && requeue != frameArrivalPoll {
				t.Fatalf("awaitFrameArrival() requeue = %s, want %s", requeue, frameArrivalPoll)
			}
		})
	}
}

// A resolve Job that finished moments ago with its frame cut off is read again,
// and nothing about the operation is decided yet. Past the window the same log is
// judged, as it was before this read existed.
func TestReconcileReadsAFrameThatHasNotFinishedArrivingAgain(t *testing.T) {
	t.Parallel()

	finished := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
		JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.NewTime(finished.Add(-time.Minute)), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(finished)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	cut := frame[:len(frame)-len("\nPTAH_RUNNER_RESULT_END_V1\n")]

	within, withinAPI := fakeReconciler(t, staticLogs{content: cut}, schema.DeepCopy(), job.DeepCopy(), pod.DeepCopy())
	within.Clock = func() time.Time { return finished.Add(5 * time.Second) }
	result, err := within.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() within the window error = %v", err)
	}
	if result.RequeueAfter != frameArrivalPoll {
		t.Fatalf("Reconcile() within the window requeued after %s, want %s", result.RequeueAfter, frameArrivalPoll)
	}
	held := &operatorv1alpha1.PtahSchema{}
	if err := withinAPI.Get(context.Background(), client.ObjectKeyFromObject(schema), held); err != nil {
		t.Fatal(err)
	}
	if held.Status.ActiveOperation == nil || held.Status.ActiveOperation.Attempt != 1 ||
		held.Status.Phase != operatorv1alpha1.PhaseResolving {
		t.Fatalf("a frame still arriving decided the operation: %#v", held.Status)
	}

	past, pastAPI := fakeReconciler(t, staticLogs{content: cut}, schema.DeepCopy(), job.DeepCopy(), pod.DeepCopy())
	past.Clock = func() time.Time { return finished.Add(frameArrivalWindow + time.Second) }
	result, err = past.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() past the window error = %v", err)
	}
	judged := &operatorv1alpha1.PtahSchema{}
	if err := pastAPI.Get(context.Background(), client.ObjectKeyFromObject(schema), judged); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(judged.Status, held.Status) && result.RequeueAfter == frameArrivalPoll {
		t.Fatalf("a frame past the window was still waited on: %#v", judged.Status)
	}
}
