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
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A live PtahSchema decides how its Apply ended from the executor's own state,
// as a deleted one does (#994). The kubelet writes ContainerStatusUnknown, 137,
// with no container ID, for an executor it has no status for: one that never
// started, when the Pod is stopped, and one the container runtime stopped
// reporting while it ran. It serves no log for either and the termination
// proves no stop, so the harvest asks for no log, waits for the Apply's
// horizon, and points at no Pod log. The fake client runs no Job controller and
// no kubelet, so each row writes the state a supported cluster leaves.

const (
	outcomeUnknownEvent    = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; observing database state"
	outcomeUnknownApplying = "Apply outcome is uncertain; only read-only observation is permitted"
	unreadApplyResult      = "apply result is uncertain: ptah runner result frame not found"
	unreadLogPointer       = ". Anything the run wrote is in the Pod log for "
)

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
		// its log is read, the proof does not wait, and the Event points at
		// the log.
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
			run.reconcileOnce(t)

			wantFailure, wantEvent := unreadApplyResult, outcomeUnknownEvent
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
			if reads, want := row.logs.count(), map[bool]int{true: 1, false: 0}[row.stopped]; reads != want {
				t.Errorf("the harvest read the executor's log %d times, want %d", reads, want)
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
// after the last look left a stale report in the Event. A Job the schema does
// not own is not shown to be the claim's, so its Pods are not listed and say
// nothing of the claim's.
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
			if row.disown {
				want, pods = want+lastReportedBefore+run.said, 0
			}
			if event := settledSchemaEvent(t, run.recorder); event != want {
				t.Errorf("ApplyOutcomeUnknown = %q, want %q", event, want)
			}
			deletedSchemaEnding(t, run, operation, continuityLost, pods, &horizon)
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
		// pointed is a record whose message points at the executor's log.
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
			row.end(t, run)
			record := run.settle(t, 3)
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
