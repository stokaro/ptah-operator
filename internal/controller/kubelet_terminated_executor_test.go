package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A live PtahSchema decides how its Apply ended from the executor's own state,
// as a deleted one does (#994). The kubelet writes ContainerStatusUnknown, 137,
// with no container ID, for an executor it has no status for: one that never
// started, when the Pod is stopped, and one the container runtime stopped
// reporting while it ran. It serves no log for either and the termination
// proves no stop, so the harvest asks for no log, takes it as gone at once,
// waits for the Apply's horizon, and points at no Pod log. The fake client runs
// no Job controller and no kubelet, so each row writes the state a supported
// cluster leaves.

const (
	outcomeUnknownEvent    = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; observing database state"
	outcomeUnknownApplying = "Apply outcome is uncertain; only read-only observation is permitted"
	unreadApplyResult      = "apply result is uncertain: ptah runner result frame not found"
	unreadLogPointer       = ". Anything the run wrote is in the Pod log for "
	// durableReceiver is the endpoint a ready installation gives the builder,
	// which makes the Jobs it builds deliver their result durably.
	durableReceiver = "https://receiver.operator.svc:9444"
)

// lostExecutorLogText is what a harvest says of the log of pod's executor
// once the kubelet lost it (lostExecutorLog).
func lostExecutorLogText(pod *corev1.Pod) string {
	return `the result log is gone: the kubelet ended container "ptah" of Pod ` + pod.Name +
		" with ContainerStatusUnknown and serves no log for it"
}

// datedJobEnd dates job's terminal condition at, as the Job controller stamps it
// when it marks the Job. A frame that may still arrive holds a harvest for
// frameArrivalWindow from then (awaitFrameArrival).
func datedJobEnd(t *testing.T, api client.Client, job *batchv1.Job, at time.Time) {
	t.Helper()

	stored := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	dated := 0
	for index := range stored.Status.Conditions {
		condition := &stored.Status.Conditions[index]
		if condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete {
			condition.LastTransitionTime = metav1.NewTime(at)
			dated++
		}
	}
	if dated != 1 {
		t.Fatalf("the Job carries %d terminal conditions, want the one to date", dated)
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

// publishedResults answers a poll as the result store does for the one Pod of
// the run it serves: with value when that Pod published it, and with
// ErrIncomplete when it published nothing.
type publishedResults struct {
	pod   *corev1.Pod
	value *runner.Result
}

func (p *publishedResults) Poll(_ context.Context, request resultconsumer.Request) (resultconsumer.Result, error) {
	if p.value == nil {
		return resultconsumer.Result{}, resultstore.ErrIncomplete
	}
	binding := resultstore.Binding{
		Namespace: request.Namespace, Kind: request.Kind, Name: request.Name, UID: request.UID,
		Generation: request.Generation, ExecutionBindingID: request.ExecutionBindingID,
		InputFingerprint: request.InputFingerprint, Operation: request.Operation, OperationID: request.OperationID,
		JobName: request.JobName, JobUID: request.JobUID, PodName: p.pod.Name, PodUID: p.pod.UID,
	}
	name, err := resultstore.Name(binding)
	if err != nil {
		return resultconsumer.Result{}, err
	}
	return resultconsumer.Result{
		Binding: binding, Receipt: resultstore.Receipt{Name: name, UID: "receipt-uid", Digest: testDigest, Size: 1},
		Value: *p.value,
	}, nil
}

// durableBuilder builds Jobs that deliver their result durably.
func durableBuilder() workload.Builder {
	builder := workloadBuilderForMigrations()
	builder.ResultEndpoint = durableReceiver
	return builder
}

// countedLogs answers every pod/log read with err, or with content when err is
// nil, and counts the reads.
type countedLogs struct {
	mu      sync.Mutex
	content []byte
	err     error
	reads   int
}

func (l *countedLogs) Read(context.Context, string, string, string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.err != nil {
		return nil, l.err
	}
	return append([]byte(nil), l.content...), nil
}

func (l *countedLogs) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// kubeletRefusesALostContainer is the kubelet's answer to a log read of a
// terminated container that has no container ID (validateContainerLogStatus),
// as the API server returns it.
func kubeletRefusesALostContainer() error {
	return apierrors.NewBadRequest(`container "ptah" in pod "apply" is terminated`)
}

// lostWhileItRan is a Pod whose executor ran and that the container runtime
// stopped reporting. The kubelet gives the executor ContainerStatusUnknown, 137,
// with no container ID, and under restartPolicy Never the Pod is Failed and the
// Job fails on its backoff limit.
func lostWhileItRan(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	stored := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, RestartCount: 1,
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
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded,
	}}}
	if err := api.Status().Update(ctx, storedJob); err != nil {
		t.Fatal(err)
	}
}

// startedAndLookedAt is a Pod that got its Secret and started, and the pass that
// looked at it, which takes the report off Ready.
func startedAndLookedAt(t *testing.T, run unstartedSchema) {
	t.Helper()

	firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	run.reconcileOnce(t)
	ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || strings.Contains(ready.Message, run.said) {
		t.Fatalf("the look at the started Pod left Ready = %#v, so the row would read a stale report", ready)
	}
}

// applyHorizon is the instant an Apply claim's proof waits for: its execution
// deadline and its Pods' grace.
func applyHorizon(operation *operatorv1alpha1.ActiveOperationStatus) metav1.Time {
	return metav1.NewTime(operation.ExecutionNotAfter.Add(time.Duration(operation.TerminationGracePeriodSeconds) * time.Second))
}

// Each row's Job finished at the pass that reads it. A log the API server
// serves may still receive its frame, so that pass waits for it; the log of an
// executor the kubelet lost receives nothing, so the Apply ends at once.
func TestALiveApplyWhoseExecutorTheKubeletLostWaitsAndPointsAtNoLog(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// end leaves the run's Pod and Job as the row's cluster does.
		end func(t *testing.T, run unstartedSchema)
		// logs answers a read of the executor's log.
		logs *countedLogs
		// reported is whether the run's report about its waiting Pod is still
		// standing when the Apply ends.
		reported bool
		// stopped is an executor whose termination proves it ran and stopped:
		// its log is read, the frame is waited for, the proof does not wait,
		// and the Event points at the log.
		stopped bool
	}{
		{
			name: "an executor that never started", reported: true,
			end: func(t *testing.T, run unstartedSchema) {
				deadlineStoppedAWaitingPod(t, run.api, run.job, run.pod)
			},
			logs: &countedLogs{err: kubeletRefusesALostContainer()},
		},
		{
			name: "an executor the runtime lost while it ran",
			end: func(t *testing.T, run unstartedSchema) {
				startedAndLookedAt(t, run)
				lostWhileItRan(t, run.api, run.job, run.pod)
			},
			logs: &countedLogs{err: kubeletRefusesALostContainer()},
		},
		{
			name: "an executor that exited with an error", stopped: true,
			end: func(t *testing.T, run unstartedSchema) {
				startedAndLookedAt(t, run)
				endedWithoutAResult(t, run.api, run.job, run.pod)
			},
			logs: &countedLogs{content: []byte("connection reset by peer\n")},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := admittedUnstartedSchemaApplyRun(t)
			run.reconciler.Logs = row.logs
			claim := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			horizon := applyHorizon(claim)
			row.end(t, run)
			datedJobEnd(t, run.api, run.job, run.reconciler.now())
			run.reconcileOnce(t)
			if row.stopped {
				if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.ActiveOperation == nil {
					t.Fatal("the harvest did not wait for a frame that may still arrive, so the rows that end at once prove nothing")
				}
				datedJobEnd(t, run.api, run.job, run.reconciler.now().Add(-frameArrivalWindow))
				run.reconcileOnce(t)
			}

			wantFailure, wantEvent := unreadApplyResult, outcomeUnknownEvent
			if !row.stopped {
				wantFailure = "apply result is uncertain: " + lostExecutorLogText(run.pod)
			}
			if row.reported {
				wantFailure += lastReportedBefore + run.said
				wantEvent += lastReportedBefore + run.said
			}
			wantApplying := outcomeUnknownApplying
			if row.stopped {
				wantEvent += unreadLogPointer
				wantApplying += unreadLogPointer
			}
			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != wantFailure {
				t.Errorf("ReconciliationFailed = %q, want %q", failed.Message, wantFailure)
			}
			if event := settledSchemaEvent(t, run.recorder); !strings.HasPrefix(event, wantEvent) ||
				!row.stopped && event != wantEvent {
				t.Errorf("ApplyOutcomeUnknown = %q, want %q", event, wantEvent)
			}
			settled := safetyGetSchema(t, run.api, run.schema)
			applying := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionApplying)
			if applying == nil || !strings.HasPrefix(applying.Message, wantApplying) || !row.stopped && applying.Message != wantApplying {
				t.Errorf("Applying = %#v, want %q", applying, wantApplying)
			}
			pending := settled.Status.PendingObservation
			switch {
			case pending.ApplyPodCount != 1:
				t.Errorf("the ending recorded %d Pods, want the one it read", pending.ApplyPodCount)
			case row.stopped && pending.ObserveAfter != nil:
				t.Errorf("the proof waits until %s for an executor that stopped", pending.ObserveAfter)
			case !row.stopped && (pending.ObserveAfter == nil || !pending.ObserveAfter.Equal(&horizon)):
				t.Errorf("the proof waits until %v, want %s, the Apply's execution horizon and its grace", pending.ObserveAfter, horizon)
			}
			switch reads := row.logs.count(); {
			case row.stopped && reads == 0:
				t.Error("the harvest read no log of an executor that exited, so the rows that read none prove nothing")
			case !row.stopped && reads != 0:
				t.Errorf("the harvest read the log of an executor the kubelet lost %d times, want none", reads)
			}
		})
	}
}

// A read-only operation whose executor the kubelet lost is retried at once: its
// log is gone without a read, and nothing arrives in it later. On master the
// kubelet's refusal of the read said the same, at the cost of the read. The
// control row is an executor that exited, whose log may still receive its
// frame, so the pass that finds none waits for it.
func TestAReadWhoseExecutorTheKubeletLostRetriesAtOnce(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name  string
		state corev1.ContainerStateTerminated
		logs  *countedLogs
		lost  bool
	}{
		{
			name: "an executor the kubelet lost", lost: true,
			state: corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown", Message: lostKubeletContainer},
			logs:  &countedLogs{err: kubeletRefusesALostContainer()},
		},
		{
			name:  "an executor that exited with an error",
			state: corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
			logs:  &countedLogs{content: []byte("connection reset by peer\n")},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, operatorv1alpha1.OperationResolve, runShape{dispatcher: current, recorded: true})
			// The Pod the Job controller makes, so the harvest attributes the
			// result to it rather than refusing its intent.
			run.pod = terminalPodForJob(run.job)
			reconciler, api, _ := run.reconciler(t, current)
			reconciler.Logs = row.logs
			pod := &corev1.Pod{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
				Name: executorContainerName, State: corev1.ContainerState{Terminated: row.state.DeepCopy()},
			}}}
			if err := api.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
				t.Fatal(err)
			}
			job.Status.Active, job.Status.Failed = 0, 1
			job.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded,
				LastTransitionTime: metav1.NewTime(reconciler.now()),
			}}
			if err := api.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}

			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			stored := safetyGetSchema(t, api, run.schema)
			operation := stored.Status.ActiveOperation
			reads := row.logs.count()
			if !row.lost {
				if operation == nil || operation.Attempt != 1 || reads == 0 {
					t.Fatalf("the pass did not read and wait for a frame that may still arrive: operation %#v, %d reads, "+
						"so the row that retries at once proves nothing", operation, reads)
				}
				return
			}
			if operation == nil || operation.Attempt != 2 {
				t.Fatalf("the Resolve was not retried at once: %#v", operation)
			}
			failed := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if want := "read Resolve result: " + lostExecutorLogText(pod); failed == nil || failed.Message != want {
				t.Errorf("ReconciliationFailed = %#v, want %q", failed, want)
			}
			if reads != 0 {
				t.Errorf("the harvest read the log of an executor the kubelet lost %d times, want none", reads)
			}
		})
	}
}

// durableSchemaApplyRun is admittedUnstartedSchemaApplyRun with the Job built
// for durable delivery: the reconciler issues the Pod its credential and reads
// the result from results, and must not ask for the Pod log.
func durableSchemaApplyRun(t *testing.T, results *publishedResults) unstartedSchema {
	t.Helper()

	run := admittedUnstartedSchemaApplyRunBy(t, durableBuilder(), func(r *SchemaReconciler) {
		r.ResultCredentials = &issuanceProbe{}
		r.Results = results
		r.Logs = forbiddenResultLogs{t}
	})
	if !durableDeliveryRequested(run.job) {
		t.Fatal("the Job delivers its result through its log, so nothing below reads a durable result")
	}
	results.pod = run.pod
	return run
}

// A durable result needs no log. An Apply whose executor the kubelet lost ends
// on what the store holds and waits for its horizon, since the termination
// proves no stop, whether or not the Pod published a result before the kubelet
// lost it. It points at no Pod log. The control row is an executor that exited,
// whose log the API server serves.
//
// A lost executor fails its Pod and so its Job, and a schema harvest reads no
// result code from a Job that did not complete, so the coded failure these
// results carry reaches no reading of the log here. The migration family's
// harvest settles from the result whatever the Job's status, and its rows
// read it (TestADurableMigrationRunWhoseExecutorTheKubeletLostPointsAtNoLog).
func TestADurableApplyWhoseExecutorTheKubeletLostWaitsAndPointsAtNoLog(t *testing.T) {
	t.Parallel()

	const unsuccessful = "apply result is uncertain: Apply Job did not complete successfully"
	failure := &runner.ResultError{Code: "apply_failed", Message: "the statement failed"}
	lost := func(t *testing.T, run unstartedSchema) {
		startedAndLookedAt(t, run)
		lostWhileItRan(t, run.api, run.job, run.pod)
	}
	for _, row := range []struct {
		name string
		end  func(t *testing.T, run unstartedSchema)
		// published is the runner's error in the result the Pod published,
		// and nil for a Pod that published nothing.
		published *runner.ResultError
		// stopped is an executor whose termination proves it ran and stopped.
		stopped bool
		failure string
		// pointer is what Applying says of the Pod log after its sentence.
		pointer string
	}{
		{name: "an executor the kubelet lost, which published nothing", end: lost, failure: unreadApplyResult},
		{name: "an executor the kubelet lost, which published a failure", end: lost, published: failure, failure: unsuccessful},
		{
			name: "an executor that exited with an error, which published a failure", published: failure, stopped: true,
			end: func(t *testing.T, run unstartedSchema) {
				startedAndLookedAt(t, run)
				endedWithoutAResult(t, run.api, run.job, run.pod)
			},
			failure: unsuccessful, pointer: unreadLogPointer,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			results := &publishedResults{}
			run := durableSchemaApplyRun(t, results)
			claim := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			horizon := applyHorizon(claim)
			if row.published != nil {
				results.value = &runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: claim.ID,
					ChildExitCode: 1, MutationStarted: true, Error: row.published,
				}
			}
			row.end(t, run)
			run.reconcileOnce(t)

			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != row.failure {
				t.Errorf("ReconciliationFailed = %q, want %q", failed.Message, row.failure)
			}
			settled := safetyGetSchema(t, run.api, run.schema)
			applying := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionApplying)
			want := outcomeUnknownApplying + row.pointer
			if applying == nil || !strings.HasPrefix(applying.Message, want) || row.pointer == "" && applying.Message != want {
				t.Errorf("Applying = %#v, want %q", applying, want)
			}
			switch pending := settled.Status.PendingObservation; {
			case row.stopped && pending.ObserveAfter != nil:
				t.Errorf("the proof waits until %s for an executor that stopped", pending.ObserveAfter)
			case !row.stopped && (pending.ObserveAfter == nil || !pending.ObserveAfter.Equal(&horizon)):
				t.Errorf("the proof waits until %v, want %s, the Apply's execution horizon and its grace", pending.ObserveAfter, horizon)
			}
		})
	}
}

// The live endings that do not read a terminal Pod list the Job's Pods before
// they decide the report, as the deleted schema's do (#993): a Pod that started
// after the last look takes it off. Each row has its Pod start, or fail after it
// started, with no pass between, so Ready still carries the report when the
// ending runs.
func TestTheLiveEndingsLeaveOutAReportAStartedPodOutdated(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		ended string
		lose  func(t *testing.T, run unstartedSchema)
		// pointed is an ending whose Job stands with a Pod that ran, so the
		// Event points at its log.
		pointed bool
	}{
		{
			name: "its Job was deleted", ended: "dispatched Apply Job is missing and will not be recreated",
			lose: func(t *testing.T, run unstartedSchema) {
				firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
				if err := run.api.Delete(context.Background(), run.job.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "its inputs changed", ended: "operation inputs changed while the Job was running", pointed: true,
			lose: func(t *testing.T, run unstartedSchema) {
				edited := safetyGetSchema(t, run.api, run.schema)
				edited.Spec.Target.URLFrom.Name = "application-database-fixed"
				edited.Generation++
				if err := run.api.Update(context.Background(), edited); err != nil {
					t.Fatal(err)
				}
				run.reconcileOnce(t)
				if stored := safetyGetSchema(t, run.api, run.schema); stored.Status.ActiveOperation == nil {
					t.Fatal("the edit ended an Apply whose Job is still running")
				}
				endedWithoutAResult(t, run.api, run.job, run.pod)
			},
		},
		{
			// The harvest reads this terminal Pod, and its Pods read come
			// from that listing.
			name: "its result was not read", ended: unreadApplyResult, pointed: true,
			lose: func(t *testing.T, run unstartedSchema) {
				run.reconciler.Logs = &countedLogs{content: []byte("connection reset by peer\n")}
				endedWithoutAResult(t, run.api, run.job, run.pod)
				datedJobEnd(t, run.api, run.job, run.reconciler.now().Add(-frameArrivalWindow))
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := admittedUnstartedSchemaApplyRun(t)
			row.lose(t, run)
			ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady)
			if ready == nil || ready.Message != run.said {
				t.Fatalf("Ready = %#v before the ending, want the report %q standing, or the row proves nothing", ready, run.said)
			}
			run.reconcileOnce(t)

			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != row.ended {
				t.Errorf("ReconciliationFailed = %q, want %q alone", failed.Message, row.ended)
			}
			want := outcomeUnknownEvent
			if row.pointed {
				want += unreadLogPointer
			}
			if event := settledSchemaEvent(t, run.recorder); !strings.HasPrefix(event, want) || !row.pointed && event != want {
				t.Errorf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			if pending := safetyGetSchema(t, run.api, run.schema).Status.PendingObservation; pending.ApplyPodCount != 1 {
				t.Errorf("the ending recorded %d Pods, want the one it listed", pending.ApplyPodCount)
			}
		})
	}
}

// A claim that never recorded its Job's UID, as when the manager stopped
// between creating the Job and recording it, still has its Pods listed when its
// Lease changes hands during deletion: under the Job its name holds, where the
// claim's schema owns that Job. The deleted schema's finisher is not given the
// Job, and listing by the claim's empty UID found nothing, so a Pod that started
// after the last look left a stale report in the Event. The pending observation
// names that Job's UID, so the deletion's wait lists the same Pods and holds
// for one still running past the Apply's horizon. A Job the schema does not own
// is not shown to be the claim's, so its Pods are not listed, say nothing of
// the claim's, and the record names no UID: the deletion waits for the horizon
// alone.
func TestADeletedSchemaWhoseClaimNeverRecordedItsJobListsThatJobsPods(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// disown leaves the Job under the claim's name without its owner.
		disown bool
	}{
		{name: "a Job the schema owns"},
		{name: "a Job the schema does not own", disown: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := admittedUnstartedSchemaApplyRun(t)
			deleteSchemaWhileItsApplyRuns(t, run)
			unrecorded := safetyGetSchema(t, run.api, run.schema)
			unrecorded.Status.ActiveOperation.JobUID = ""
			if err := run.api.Status().Update(context.Background(), unrecorded); err != nil {
				t.Fatal(err)
			}
			if row.disown {
				disowned(t, run.api, run.job)
			}
			operation := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			horizon := applyHorizon(operation)
			firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
			// One pass records the lost continuity, and the next ends the Apply.
			run.reconcileOnce(t)
			run.reconcileOnce(t)

			want, pods := deletedApplyEventPrefix+continuityLost, int32(1)
			named := operation.DeepCopy()
			named.JobUID = run.job.UID
			if row.disown {
				want, pods = want+lastReportedBefore+run.said, 0
				named.JobUID = ""
			}
			if event := settledSchemaEvent(t, run.recorder); event != want {
				t.Errorf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			deletedSchemaEnding(t, run, named, continuityLost, pods, &horizon)

			// Past the horizon the Pod is still running. The wait that lists it
			// holds; the one that cannot list it lets the schema go.
			past := horizon.Add(time.Second)
			run.reconciler.Clock = func() time.Time { return past }
			run.reconcileOnce(t)
			err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.schema), &operatorv1alpha1.PtahSchema{})
			switch gone := apierrors.IsNotFound(err); {
			case err != nil && !gone:
				t.Fatal(err)
			case row.disown && !gone:
				t.Errorf("the deletion still waits past the horizon with no Job UID recorded: %#v",
					safetyGetSchema(t, run.api, run.schema).Status.PendingObservation)
			case !row.disown && gone:
				t.Error("the deletion went past the horizon while the Pod of the Job its record names still runs")
			case !row.disown && safetyGetSchema(t, run.api, run.schema).Status.PendingObservation == nil:
				t.Error("the deletion dropped its pending observation while the Pod of the Job it names still runs")
			}
		})
	}
}

// A durable result needs no log, so the harvest reads it from a Pod whose
// executor the kubelet lost as from any other. The evidence still says that
// termination proves no stop, which is what holds an Apply ending there to its
// horizon.
func TestADurableResultFromALostExecutorProvesNoStop(t *testing.T) {
	for _, row := range []struct {
		name    string
		state   corev1.ContainerStateTerminated
		stopped bool
	}{
		{name: "an executor the kubelet lost", state: corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown", Message: lostKubeletContainer}},
		{name: "an executor that exited with an error", stopped: true, state: corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := resulttest.New(t, "schema-apply-admitted-scheduling")
			f.Pod.Status.Phase = corev1.PodFailed
			f.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: executorContainerName, State: corev1.ContainerState{Terminated: row.state.DeepCopy()},
			}}
			apiClient := &durableTestAPI{Client: f.Client(t)}
			result := runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(f.Identity.Binding.Operation),
				OperationID: f.Identity.Binding.OperationID, ChildExitCode: 1,
				Error: &runner.ResultError{Code: "statement_failed", Message: "the statement failed"},
			}
			payload, err := resultdelivery.Encode(f.Identity, result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (resultstore.Store{Client: apiClient, Reader: apiClient}).Publish(t.Context(), f.Identity.Binding, payload, fingerprint.DigestBytes(payload)); err != nil {
				t.Fatal(err)
			}
			r := &SchemaReconciler{Client: apiClient, APIReader: apiClient, Results: controllerConsumer(t, apiClient), Logs: forbiddenResultLogs{t}}
			evidence, err := pollEvidence(t, func() (terminalEvidence, error) {
				return r.terminalLogs(t.Context(), f.Subject.(*operatorv1alpha1.PtahSchema), f.Job)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !evidence.Durable || evidence.Result == nil || !evidence.Trusted {
				t.Fatalf("the durable result was not read from the terminated executor: %+v", evidence)
			}
			if evidence.Stopped != row.stopped {
				t.Fatalf("evidence.Stopped = %t, want %t", evidence.Stopped, row.stopped)
			}
		})
	}
}

// A migration's record shares the schema's reading of what its Pod log holds
// (failedApplyLogReading), so a run whose every executor the kubelet ended with
// ContainerStatusUnknown points at no Pod log either: kubectl logs answers such
// a container with an error. The control row is an executor that exited, whose
// log holds whatever it wrote.
func TestAMigrationRunWhoseExecutorTheKubeletLostPointsAtNoLog(t *testing.T) {
	t.Parallel()

	const pointer = "Anything the run wrote is in the Pod log for "
	for _, row := range []struct {
		name string
		end  func(t *testing.T, run unstartedMigration)
		// pointed is a record whose message points at the executor's log,
		// which is the log the harvest reads.
		pointed bool
	}{
		{
			name: "an executor that never started",
			end: func(t *testing.T, run unstartedMigration) {
				deadlineStoppedAWaitingPod(t, run.api, run.job, run.pod)
			},
		},
		{
			name: "an executor the runtime lost while it ran",
			end: func(t *testing.T, run unstartedMigration) {
				firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
				reconcileMigration(t, run.reconciler, run.api, run.migration)
				lostWhileItRan(t, run.api, run.job, run.pod)
			},
		},
		{
			name: "an executor that exited with an error", pointed: true,
			end: func(t *testing.T, run unstartedMigration) {
				firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
				reconcileMigration(t, run.reconciler, run.api, run.migration)
				endedWithoutAResult(t, run.api, run.job, run.pod)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := unstartedMigrationApplyRun(t)
			logs := &countedLogs{err: kubeletRefusesALostContainer()}
			if row.pointed {
				logs = &countedLogs{content: []byte("connection reset by peer\n")}
			}
			run.reconciler.Logs = logs
			row.end(t, run)
			record := run.settle(t, 3)
			switch reads := logs.count(); {
			case row.pointed && reads == 0:
				t.Error("the harvest read no log of an executor that exited, so the rows that read none prove nothing")
			case !row.pointed && reads != 0:
				t.Errorf("the harvest read the log of an executor the kubelet lost %d times, want none", reads)
			}
			// The log is gone without a read, and the record says so.
			if gone := strings.Contains(record.Message, lostExecutorLogText(run.pod)); gone == row.pointed {
				t.Errorf("status.lastRun.message = %q, says the kubelet lost the executor's log: %t, want %t",
					record.Message, gone, !row.pointed)
			}
			event := findEvent(run.recorder, "Warning MigrationRunUncertain ")
			if event == "" {
				t.Fatal("no MigrationRunUncertain Event was recorded")
			}
			for subject, text := range map[string]string{"status.lastRun.message": record.Message, "MigrationRunUncertain": event} {
				if pointed := strings.Contains(text, pointer); pointed != row.pointed {
					t.Errorf("%s = %q, points at the Pod log: %t, want %t", subject, text, pointed, row.pointed)
				}
				if logs := strings.Contains(text, "kubectl logs"); logs != row.pointed {
					t.Errorf("%s = %q, names kubectl logs: %t, want %t", subject, text, logs, row.pointed)
				}
			}
		})
	}
}

// durableMigrationApplyRun is unstartedMigrationApplyRun with the Job built for
// durable delivery: the reconciler issues the Pod its credential and reads the
// result from results, and must not ask for the Pod log.
func durableMigrationApplyRun(t *testing.T, results *publishedResults) unstartedMigration {
	t.Helper()

	run := unstartedMigrationApplyRunBy(t, durableBuilder(), func(r *MigrationReconciler) {
		r.ResultCredentials = &issuanceProbe{}
		r.Results = results
		r.Logs = forbiddenResultLogs{t}
	})
	if !durableDeliveryRequested(run.job) {
		t.Fatal("the Job delivers its result through its log, so nothing below reads a durable result")
	}
	results.pod = run.pod
	return run
}

// A migration's durable result outlives the Pod log too. A run whose executor
// the kubelet lost points at no Pod log, whether its result reported the run
// or only a failure, as every reading of a log is asked first whether the API
// server serves it (servedLogReading). The control rows are an executor that
// exited, whose log holds what Ptah reported.
func TestADurableMigrationRunWhoseExecutorTheKubeletLostPointsAtNoLog(t *testing.T) {
	t.Parallel()

	const pointer = "What Ptah reported is in the Pod log for "
	lost := func(t *testing.T, run unstartedMigration) {
		firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		reconcileMigration(t, run.reconciler, run.api, run.migration)
		lostWhileItRan(t, run.api, run.job, run.pod)
	}
	exited := func(t *testing.T, run unstartedMigration) {
		firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		reconcileMigration(t, run.reconciler, run.api, run.migration)
		endedWithoutAResult(t, run.api, run.job, run.pod)
	}
	for _, row := range []struct {
		name string
		end  func(t *testing.T, run unstartedMigration)
		// reported is a result that reports the run, rather than only the
		// failure that ended it.
		reported bool
		pointed  bool
	}{
		{name: "a failure from an executor the kubelet lost", end: lost},
		{name: "a report from an executor the kubelet lost", end: lost, reported: true},
		{name: "a failure from an executor that exited", end: exited, pointed: true},
		{name: "a report from an executor that exited", end: exited, reported: true, pointed: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			results := &publishedResults{}
			run := durableMigrationApplyRun(t, results)
			stored := readMigration(t, run.api, run.migration)
			operation := stored.Status.ActiveOperation
			result := runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply, OperationID: operation.ID,
				ChildExitCode: 1, MutationStarted: true,
				CoordinationDigest: operation.CoordinationDigest, TargetIdentityDigest: stored.Status.History.TargetIdentityDigest,
				Error: &runner.ResultError{Code: "apply_failed", Message: "the statement failed"},
			}
			if row.reported {
				result.Error = &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"}
				result.MigrationRun = &dataplane.MigrationRunReport{
					ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeFailed,
				}
			}
			results.value = &result
			row.end(t, run)
			for range 3 {
				if reconcileMigration(t, run.reconciler, run.api, run.migration).Status.ActiveOperation == nil {
					break
				}
			}
			settled := readMigration(t, run.api, run.migration)
			if settled.Status.ActiveOperation != nil || settled.Status.LastRun == nil {
				t.Fatalf("the run was not recorded: claim %#v, last run %#v", settled.Status.ActiveOperation, settled.Status.LastRun)
			}
			wantOutcome := operatorv1alpha1.MigrationRunOutcomeUnknown
			if row.reported {
				wantOutcome = operatorv1alpha1.MigrationRunOutcomeFailed
			}
			record := settled.Status.LastRun
			if record.Outcome != wantOutcome {
				t.Fatalf("the run was recorded %s, want %s, the ending the result decides: %q", record.Outcome, wantOutcome, record.Message)
			}
			if pointed := strings.Contains(record.Message, pointer); pointed != row.pointed {
				t.Errorf("status.lastRun.message = %q, points at the Pod log: %t, want %t", record.Message, pointed, row.pointed)
			}
			if logs := strings.Contains(record.Message, "kubectl logs"); logs != row.pointed {
				t.Errorf("status.lastRun.message = %q, names kubectl logs: %t, want %t", record.Message, logs, row.pointed)
			}
		})
	}
}
