package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A deleted PtahSchema waits for its running Apply and then ends it (#989).
// Every ending of an Apply that may have run leaves an ApplyOutcomeUnknown
// Event, because the status goes with the resource. These rows delete the
// schema while its Apply runs, leave the Job and its Pods in a state the ending
// reads, and read the Event, the claim, and when the resource goes. The fake
// client runs no Job controller and no kubelet, so each row says which of
// those states a supported cluster reaches and which the row holds the ending
// to defensively.

const (
	deletedApplyEventPrefix = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; " +
		"the PtahSchema is being deleted, so nothing will observe the database: "
	notProvenDuringDeletion = "Apply executor termination is not proven during deletion"
	endedDuringDeletion     = "Apply executor ended and its result was not read during deletion"
	identityLostOnDeletion  = "dispatched Apply Job identity was lost during deletion"
	continuityLost          = "database lock continuity was lost during Apply"
	lostKubeletContainer    = "The container could not be located when the pod was terminated"
)

// admittedUnstartedSchemaApplyRun is unstartedSchemaApplyRun with the Pod the Job
// controller makes from the template and admission lets through, so a read of
// the terminal Pod attributes it to the Apply rather than refusing its intent.
func admittedUnstartedSchemaApplyRun(t *testing.T) unstartedSchema {
	t.Helper()

	return admittedUnstartedSchemaApplyRunBy(t, workloadBuilderForMigrations(), nil)
}

// admittedUnstartedSchemaApplyRunBy is admittedUnstartedSchemaApplyRun with the
// Job built by builder and the reconciler set up by configure.
func admittedUnstartedSchemaApplyRunBy(t *testing.T, builder workload.Builder, configure func(*SchemaReconciler)) unstartedSchema {
	t.Helper()

	return schemaApplyUnderReportBy(t, builder, configure, func(api client.Client, job *batchv1.Job) *corev1.Pod {
		pod := terminalPodForJob(job)
		waitingOnASecret(pod)
		if err := api.Create(context.Background(), pod.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		return pod
	}, operatorv1alpha1.ReasonPodConfigurationError, missingSecretText)
}

// deadlineStoppedAWaitingPod is what the Job's deadline can leave of a Pod whose
// executor waited on a Secret. The Job controller deletes the Pod, and the
// kubelet stops its containers within the Pod's grace period, deletes the Pod
// object with grace 0, and gives the container that never started
// the termination it gives every container it has no status for
// (ContainerStatusUnknown, 137), with the Pod Failed. The Job reads Failed
// once the Pod is terminal and counted. On a cluster the Pod object is still
// there while the kubelet waits to reclaim its resources, which is often, not
// always, when the pass the Job's update wakes reads it; a Pod already gone
// reads as no Pod and takes the same ending. The job-tracking finalizer here is
// a fake-client device to keep the deleted Pod readable: a real Job's Pods lose
// it before the Job is marked finished.
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

// deletedSchemaEnding is what the ending ended did with claim, the Apply's
// claim before it: retired, and the Apply's outcome recorded as unknown, under
// the claim's Job and the pods Pods the ending read, with the observation
// horizon want.
func deletedSchemaEnding(
	t *testing.T, run unstartedSchema, claim *operatorv1alpha1.ActiveOperationStatus, ended string, pods int32, want *metav1.Time,
) *operatorv1alpha1.PtahSchema {
	t.Helper()

	settled := safetyGetSchema(t, run.api, run.schema)
	if settled.DeletionTimestamp == nil {
		t.Fatal("the schema is not being deleted, so this is not a deletion ending")
	}
	failed := settledSchemaFailure(t, run.api, run.schema)
	if !strings.HasPrefix(failed.Message, ended) {
		t.Fatalf("ReconciliationFailed = %q, want the ending %q", failed.Message, ended)
	}
	if settled.Status.ActiveOperation != nil {
		t.Fatalf("the ending left the claim active: %#v", settled.Status.ActiveOperation)
	}
	pending := settled.Status.PendingObservation
	switch {
	case pending == nil:
		t.Fatal("the ending recorded no outcome, so the claim was discarded")
	case pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown:
		t.Fatalf("the ending recorded outcome %q, want %q", pending.Outcome, operatorv1alpha1.PendingObservationOutcomeUnknown)
	case pending.ApplyOperationID != claim.ID || pending.ApplyJobName != claim.JobName || pending.ApplyJobUID != claim.JobUID:
		t.Fatalf("the ending recorded Apply %q under Job %q (%q), want the claim's %q under %q (%q)",
			pending.ApplyOperationID, pending.ApplyJobName, pending.ApplyJobUID, claim.ID, claim.JobName, claim.JobUID)
	case pending.ApplyPodCount != pods:
		t.Fatalf("the ending recorded %d Pods, want the %d it read", pending.ApplyPodCount, pods)
	}
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
	deletedSchemaEnding(t, run, operation, notProvenDuringDeletion, 1, &horizon)

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
			operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			observations := &telemetryObservation{}
			run.reconciler.Telemetry = observations
			run.reconcileOnce(t)

			if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+endedDuringDeletion; event != want {
				t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != endedDuringDeletion {
				t.Fatalf("ReconciliationFailed = %q, want %q", failed.Message, endedDuringDeletion)
			}
			deletedSchemaEnding(t, run, operation, endedDuringDeletion, 1, nil)
			// An Apply nobody read the result of counts as uncertain, once, and
			// not as an operation that was canceled.
			switch {
			case !slices.Equal(observations.applies, []telemetry.ApplyOutcome{telemetry.ApplyUncertain}):
				t.Fatalf("applies = %v, want one uncertain", observations.applies)
			case !slices.Equal(observations.operations, []telemetry.OperationOutcome{telemetry.OperationUncertain}):
				t.Fatalf("operations = %v, want one uncertain", observations.operations)
			case !slices.Equal(observations.failures, []telemetry.FailureCategory{telemetry.FailureUncertain}) ||
				!slices.Equal(observations.failureStages, []telemetry.FailureStage{telemetry.FailureStageApply}):
				t.Fatalf("failures = %v at %v, want one uncertain outcome at apply", observations.failures, observations.failureStages)
			}
			schemaGone(t, run, 3)
			if slices.Contains(observations.operations, telemetry.OperationCanceled) {
				t.Fatalf("operations = %v, and the Apply was not canceled", observations.operations)
			}
		})
	}
}

// A Pod that got its Secret between two looks and stands Running on a node that
// stopped answering has not shown its executor stopped. The row is defensive:
// on the supported minors the Job controller marks a Job Failed only once none
// of its Pods is running or terminating, so the deletion's wait for a running
// Job holds the resource first. Should a Job read terminal with such a Pod
// left, the deletion records the outcome as unknown, leaves out the report the
// start made out of date, and holds the resource while the Pod stands, past
// the Apply's horizon.
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
	deletedSchemaEnding(t, run, operation, notProvenDuringDeletion, 1, &horizon)

	run.reconciler.Clock = func() time.Time { return horizon.Add(time.Second) }
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
		t.Fatal("the deletion let the database go while the Apply's Pod stands Running")
	}
	podsGone(t, run.api, run.job)
	schemaGone(t, run, 3)
}

// A Job that left two Pods cannot attribute the Apply to one Pod. The deletion
// records the outcome as unknown rather than failing every pass, keeps why the
// Pods never started, and holds the resource while the Pods stand. The Pods
// aSecondPod leaves are not terminal under a failed Job, which the supported
// minors do not reach: the Job is marked Failed only once its Pods have
// stopped, and the deletion's wait for a running Job holds first. The row is
// defensive, and the multiplicity refusal is what it reaches.
func TestADeletedSchemaWhoseApplyJobLeftTwoPodsIsRecorded(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	aSecondPod(t, run.api, run.job, run.pod)
	operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	horizon := metav1.NewTime(operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second))
	run.reconcileOnce(t)

	if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+notProvenDuringDeletion+lastReportedBefore+run.said; event != want {
		t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
	}
	deletedSchemaEnding(t, run, operation, notProvenDuringDeletion, 2, &horizon)

	run.reconciler.Clock = func() time.Time { return horizon.Add(time.Second) }
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
		t.Fatal("the deletion let the database go while the Apply's Pods stand")
	}
	podsGone(t, run.api, run.job)
	schemaGone(t, run, 3)
}

// A deleted schema whose database Lease changed hands while it waited for its
// Apply ends the Apply through the Lease continuity recovery, which the
// deletion runs first. The Event opens as every deleted schema's does: the
// prefix is decided by the deletion, not by the ending's sentence.
func TestADeletedSchemaWhoseLeaseChangedHandsSaysItIsBeingDeleted(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	horizon := metav1.NewTime(operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second))
	databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
	// One pass records the lost continuity, and the next ends the Apply.
	run.reconcileOnce(t)
	run.reconcileOnce(t)

	if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+continuityLost+lastReportedBefore+run.said; event != want {
		t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
	}
	deletedSchemaEnding(t, run, operation, continuityLost, 1, &horizon)

	run.reconciler.Clock = func() time.Time { return horizon.Add(time.Second) }
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.PendingObservation == nil {
		t.Fatal("the deletion let the database go while the Apply's Pod waits")
	}
	podsGone(t, run.api, run.job)
	schemaGone(t, run, 3)
}

// The endings that do not read a terminal Pod list the Job's Pods before they
// decide the report, so a Pod that started after the last look takes it off
// there as well: one whose Lease changed hands, and one whose Job went.
func TestADeletedSchemasLostEndingsLeaveOutAReportAStartedPodOutdated(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		passes int
		ended  string
		lose   func(t *testing.T, run unstartedSchema)
	}{
		{
			name: "its Lease changed hands", passes: 2, ended: continuityLost,
			lose: func(t *testing.T, run unstartedSchema) {
				databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
			},
		},
		{
			name: "its Job was deleted", passes: 1, ended: identityLostOnDeletion,
			lose: func(t *testing.T, run unstartedSchema) {
				if err := run.api.Delete(context.Background(), run.job.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := admittedUnstartedSchemaApplyRun(t)
			deleteSchemaWhileItsApplyRuns(t, run)
			// The Pod gets its Secret after the last look, so Ready still
			// carries the report when the ending runs.
			firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			row.lose(t, run)
			for range row.passes {
				run.reconcileOnce(t)
			}

			if event, want := settledSchemaEvent(t, run.recorder), deletedApplyEventPrefix+row.ended; event != want {
				t.Fatalf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != row.ended {
				t.Fatalf("ReconciliationFailed = %q, want %q", failed.Message, row.ended)
			}
		})
	}
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
