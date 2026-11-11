package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A deleted PtahSchema waits for its running Apply and then ends it (#989).
// Every ending of an Apply that may have run leaves an ApplyOutcomeUnknown
// Event, because the status goes with the resource. These rows delete the
// schema while its Apply runs, stop the Pod the way the cluster does, and read
// the Event, the claim, and when the resource goes.

const (
	deletedApplyEventPrefix = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; " +
		"the PtahSchema is being deleted, so nothing will observe the database: "
	notProvenDuringDeletion = "Apply executor termination is not proven during deletion"
	endedDuringDeletion     = "Apply executor ended and its result was not read during deletion"
	lostKubeletContainer    = "The container could not be located when the pod was terminated"
)

// admittedUnstartedSchemaApplyRun is unstartedSchemaApplyRun with the Pod the Job
// controller makes from the template and admission lets through, so a read of
// the terminal Pod attributes it to the Apply rather than refusing its intent.
func admittedUnstartedSchemaApplyRun(t *testing.T) unstartedSchema {
	t.Helper()

	return schemaApplyUnderReport(t, func(api client.Client, job *batchv1.Job) *corev1.Pod {
		pod := terminalPodForJob(job)
		waitingOnASecret(pod)
		if err := api.Create(context.Background(), pod.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		return pod
	}, operatorv1alpha1.ReasonPodConfigurationError, missingSecretText)
}

// deadlineStoppedAWaitingPod is what the Job's deadline leaves of a Pod whose
// executor waited on a Secret. The Job fails and the Job controller deletes the
// Pod. The kubelet stops it and gives the container that never started the
// termination it gives every container it has no status for, with the Pod
// Failed. The Job's tracking finalizer keeps the Pod object while the pass that
// the Job's update wakes reads it.
func deadlineStoppedAWaitingPod(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	stored := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Finalizers = append(stored.Finalizers, batchv1.JobTrackingFinalizer)
	if err := api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	if stored.DeletionTimestamp == nil {
		t.Fatal("the Pod is not being deleted, so this is not the Pod the deadline leaves")
	}
	stored.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "ContainerStatusUnknown", Message: lostKubeletContainer,
		}},
	}}}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	storedJob := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), storedJob); err != nil {
		t.Fatal(err)
	}
	storedJob.Status = batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}}
	if err := api.Status().Update(ctx, storedJob); err != nil {
		t.Fatal(err)
	}
}

// deletedSchemaEnding is the claim the ending retired: none active, and the
// Apply's outcome recorded as unknown with the observation horizon want.
func deletedSchemaEnding(t *testing.T, run unstartedSchema, want *metav1.Time) *operatorv1alpha1.PtahSchema {
	t.Helper()

	settled := safetyGetSchema(t, run.api, run.schema)
	if settled.DeletionTimestamp == nil {
		t.Fatal("the schema is not being deleted, so this is not a deletion ending")
	}
	failed := settledSchemaFailure(t, run.api, run.schema)
	if !strings.Contains(failed.Message, "during deletion") {
		t.Fatalf("ReconciliationFailed = %q, want a deletion ending", failed.Message)
	}
	pending := settled.Status.PendingObservation
	switch {
	case want == nil && pending.ObserveAfter != nil:
		t.Fatalf("the ending waits until %s for an executor that stopped", pending.ObserveAfter)
	case want != nil && (pending.ObserveAfter == nil || !pending.ObserveAfter.Equal(want)):
		t.Fatalf("the ending waits until %v, want %s, the Apply's execution horizon and its grace", pending.ObserveAfter, want)
	}
	return settled
}

// schemaGone runs passes until the deleted schema is gone, and fails if it is
// still there.
func schemaGone(t *testing.T, run unstartedSchema, passes int) {
	t.Helper()

	for range passes {
		run.reconcileOnce(t)
		if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.schema), &operatorv1alpha1.PtahSchema{}); apierrors.IsNotFound(err) {
			return
		}
	}
	t.Fatalf("the deleted schema is still there after %d passes: %#v", passes, safetyGetSchema(t, run.api, run.schema).Status)
}

// A Pod the deadline stopped before its executor started reads as an executor
// that terminated, with ContainerStatusUnknown. That proves nothing about the
// run, so the deletion records the outcome as unknown, keeps why the Pod never
// started, and holds the database until the Apply's horizon has passed.
func TestADeletedSchemasApplyThatNeverStartedIsRecorded(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	deadlineStoppedAWaitingPod(t, run.api, run.job, run.pod)
	evidence, selected, err := run.reconciler.collectTerminalPodEvidence(context.Background(), safetyGetSchema(t, run.api, run.schema), run.job)
	if err != nil || !evidence.Trusted || selected == nil {
		t.Fatalf("the Pod reads as %+v, %v, want one terminated executor, or the row does not reach the ending it is about", evidence, err)
	}
	operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	horizon := metav1.NewTime(operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second))
	run.reconciler.Clock = func() time.Time { return operation.ExecutionNotAfter.Add(time.Second) }
	run.reconcileOnce(t)

	if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+notProvenDuringDeletion+lastReportedBefore+run.said; event != want {
		t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
	}
	deletedSchemaEnding(t, run, &horizon)

	// The database stays held until the horizon, and the resource with it.
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
		t.Fatalf("the deletion let the database go before the horizon %s", horizon)
	}
	run.reconciler.Clock = func() time.Time { return horizon.Add(time.Second) }
	schemaGone(t, run, 3)
}

// An Apply whose Pod started between two looks and ran to its end leaves the
// report standing on Ready. The deletion reads that the one Pod's executor ran
// and stopped: it records the outcome as unknown, because nothing read the
// result, and lets the resource go without waiting out the Apply's horizon.
// The report is out of date and stays out of the Event.
func TestADeletedSchemasApplyThatRanIsRecorded(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		end  func(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod)
	}{
		{name: "an executor that succeeded", end: ranToSuccess},
		{name: "an executor that failed", end: endedWithoutAResult},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := admittedUnstartedSchemaApplyRun(t)
			deleteSchemaWhileItsApplyRuns(t, run)
			row.end(t, run.api, run.job, run.pod)
			run.reconcileOnce(t)

			if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+endedDuringDeletion; event != want {
				t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != endedDuringDeletion {
				t.Fatalf("ReconciliationFailed = %q, want %q", failed.Message, endedDuringDeletion)
			}
			deletedSchemaEnding(t, run, nil)
			schemaGone(t, run, 3)
		})
	}
}

// A Pod that got its Secret between two looks and stands Running on a node that
// stopped answering when the Job fails at its deadline has not shown its
// executor stopped. The deletion records the outcome as unknown, leaves out the
// report the start made out of date, and holds the resource while the Pod
// stands, past the Apply's horizon.
func TestADeletedSchemasApplyPodThatStartedAndStandsTakesTheReportOff(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	pastTheDeadlineWithoutAPod(t, run.api, run.job)
	operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	horizon := metav1.NewTime(operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second))
	run.reconcileOnce(t)

	if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+notProvenDuringDeletion; event != want {
		t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
	}
	deletedSchemaEnding(t, run, &horizon)

	run.reconciler.Clock = func() time.Time { return horizon.Add(time.Second) }
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
		t.Fatal("the deletion let the database go while the Apply's Pod stands Running")
	}
	podsGone(t, run.api, run.job)
	schemaGone(t, run, 3)
}

// Only a termination a started container gets proves the executor ran and
// stopped, and only on the one Pod the read attributed the Apply to.
func TestExecutorRanAndStoppedNeedsATerminationAStartedContainerGets(t *testing.T) {
	t.Parallel()

	withExecutor := func(state corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}},
			{Name: executorContainerName, State: state},
		}}}
	}
	terminated := func(reason string, code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}
	}
	trusted := terminalEvidence{Trusted: true, PodCount: 1}
	for _, row := range []struct {
		name     string
		evidence terminalEvidence
		selected *corev1.Pod
		want     bool
	}{
		{name: "an executor that completed", evidence: trusted, selected: withExecutor(terminated("Completed", 0)), want: true},
		{name: "an executor that failed", evidence: trusted, selected: withExecutor(terminated("Error", 1)), want: true},
		{name: "an executor the kubelet had no status for", evidence: trusted, selected: withExecutor(terminated("ContainerStatusUnknown", 137))},
		{
			name: "a running executor", evidence: terminalEvidence{PodCount: 1},
			selected: withExecutor(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}),
		},
		{name: "a terminated executor the read did not trust", evidence: terminalEvidence{PodCount: 1}, selected: withExecutor(terminated("Error", 1))},
		{name: "no Pod", evidence: trusted},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := executorRanAndStopped(row.evidence, row.selected); got != row.want {
				t.Fatalf("executorRanAndStopped() = %t, want %t", got, row.want)
			}
		})
	}
}
