package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A Pod can end with no status for its executor at all, so no executor ran in
// it and there is no log for `kubectl logs` to read. The kubelet leaves one
// when it refuses a Pod at admission: Failed, with its reason and message and
// no container status. Under backoffLimit 0 the Job fails on its backoff
// limit, and the failed Pod of a failed Job stays where a harvest reads it.
// Neither family points such a run at a Pod log (executorLogUnserved). The
// control rows are the same Job with an executor that exited 1 without a
// result, whose log holds whatever it wrote. The fake client runs no Job
// controller and no kubelet, so each row writes the state a cluster leaves.

// rejectedByTheKubelet is the status the kubelet gives a Pod it refuses at
// admission because the node has no room for it.
func rejectedByTheKubelet() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodFailed, Reason: "OutOfcpu",
		Message: "Pod was rejected: Node didn't have enough resource: cpu, requested: 100, used: 3950, capacity: 4000",
	}
}

// exitedWithoutAResult is the status of a Pod whose executor started and exited
// 1 before it wrote a result.
func exitedWithoutAResult() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
		}},
	}}}
}

// failedOnItsBackoffLimit marks job Failed as the Job controller does once its
// one Pod failed under backoffLimit 0.
func failedOnItsBackoffLimit(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	stored := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded,
	}}}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

// schemaApplyWhosePodEnded is a dispatched schema Apply whose one Pod, the one
// the Job controller makes from the template and admission lets through, ended
// as status says before any pass looked at it, failing the Job.
func schemaApplyWhosePodEnded(t *testing.T, status corev1.PodStatus) unstartedSchema {
	t.Helper()

	builder := workloadBuilderForMigrations()
	dispatched := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{dispatcher: builder, recorded: true}, builder)
	run := unstartedSchema{
		reconciler: dispatched.reconciler, api: dispatched.api, recorder: record.NewFakeRecorder(256),
		schema: dispatched.schema, job: dispatched.job,
	}
	run.reconciler.Recorder = run.recorder
	run.pod = terminalPodForJob(run.job)
	run.pod.Status = status
	if err := run.api.Create(context.Background(), run.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	failedOnItsBackoffLimit(t, run.api, run.job)
	return run
}

func TestASchemaApplyWhosePodEndedBeforeItsExecutorRanPointsAtNoLog(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		status corev1.PodStatus
		logs   *countedLogs
		// ran is an executor that started and stopped: its log is read, the
		// proof does not wait for the Apply's horizon, and the record points
		// at the log.
		ran bool
	}{
		{name: "a Pod the kubelet refused at admission", status: rejectedByTheKubelet(), logs: &countedLogs{err: kubeletRefusesALostContainer()}},
		{name: "a Pod whose executor exited", status: exitedWithoutAResult(), logs: &countedLogs{content: []byte("connection reset by peer\n")}, ran: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := schemaApplyWhosePodEnded(t, row.status)
			run.reconciler.Logs = row.logs
			claim := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
			horizon := applyHorizon(claim)
			// The Job finished a frame's arrival window ago, so the pass judges
			// the missing frame at once.
			datedJobEnd(t, run.api, run.job, run.reconciler.now().Add(-frameArrivalWindow))
			run.reconcileOnce(t)

			wantEvent, wantApplying := outcomeUnknownEvent, outcomeUnknownApplying
			if row.ran {
				wantEvent += unreadLogPointer
				wantApplying += unreadLogPointer
			}
			if failed := settledSchemaFailure(t, run.api, run.schema); failed.Message != unreadApplyResult {
				t.Errorf("ReconciliationFailed = %q, want %q", failed.Message, unreadApplyResult)
			}
			if event := settledSchemaEvent(t, run.recorder); !strings.HasPrefix(event, wantEvent) || !row.ran && event != wantEvent {
				t.Errorf("ApplyOutcomeUnknown = %q, want %q", event, wantEvent)
			}
			settled := safetyGetSchema(t, run.api, run.schema)
			applying := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionApplying)
			if applying == nil || !strings.HasPrefix(applying.Message, wantApplying) || !row.ran && applying.Message != wantApplying {
				t.Errorf("Applying = %#v, want %q", applying, wantApplying)
			}
			pending := settled.Status.PendingObservation
			switch {
			case pending.ApplyPodCount != 1:
				t.Errorf("the ending recorded %d Pods, want the one it read", pending.ApplyPodCount)
			case row.ran && pending.ObserveAfter != nil:
				t.Errorf("the proof waits until %s for an executor that stopped", pending.ObserveAfter)
			case !row.ran && (pending.ObserveAfter == nil || !pending.ObserveAfter.Equal(&horizon)):
				t.Errorf("the proof waits until %v, want %s, the Apply's execution horizon and its grace", pending.ObserveAfter, horizon)
			}
			switch reads := row.logs.count(); {
			case row.ran && reads == 0:
				t.Error("the harvest read no log of an executor that exited, so the row that reads none proves nothing")
			case !row.ran && reads != 0:
				t.Errorf("the harvest read the log of an executor that never ran %d times, want none", reads)
			}
		})
	}
}

func TestAMigrationRunWhosePodEndedBeforeItsExecutorRanPointsAtNoLog(t *testing.T) {
	t.Parallel()

	const pointer = "Anything the run wrote is in the Pod log for "
	for _, row := range []struct {
		name   string
		status corev1.PodStatus
		logs   *countedLogs
		// ran is an executor that started and stopped, whose log is read and
		// pointed at.
		ran bool
	}{
		{name: "a Pod the kubelet refused at admission", status: rejectedByTheKubelet(), logs: &countedLogs{err: kubeletRefusesALostContainer()}},
		{name: "a Pod whose executor exited", status: exitedWithoutAResult(), logs: &countedLogs{content: []byte("connection reset by peer\n")}, ran: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			builder := workloadBuilderForMigrations()
			dispatched := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: builder, recorded: true})
			dispatched.pod.Status = row.status
			reconciler, api := dispatched.reconciler(t, builder)
			run := unstartedMigration{
				reconciler: reconciler, api: api, recorder: record.NewFakeRecorder(256), builder: builder,
				migration: dispatched.migration, plan: dispatched.plan, job: dispatched.job, pod: dispatched.pod,
			}
			reconciler.Recorder = run.recorder
			reconciler.Logs = row.logs
			failedOnItsBackoffLimit(t, api, run.job)

			record := run.settle(t, 3)
			switch reads := row.logs.count(); {
			case row.ran && reads == 0:
				t.Error("the harvest read no log of an executor that exited, so the row that reads none proves nothing")
			case !row.ran && reads != 0:
				t.Errorf("the harvest read the log of an executor that never ran %d times, want none", reads)
			}
			event := findEvent(run.recorder, "Warning MigrationRunUncertain ")
			if event == "" {
				t.Fatal("no MigrationRunUncertain Event was recorded")
			}
			for subject, text := range map[string]string{"status.lastRun.message": record.Message, "MigrationRunUncertain": event} {
				if pointed := strings.Contains(text, pointer); pointed != row.ran {
					t.Errorf("%s = %q, points at the Pod log: %t, want %t", subject, text, pointed, row.ran)
				}
				if logs := strings.Contains(text, "kubectl logs"); logs != row.ran {
					t.Errorf("%s = %q, names kubectl logs: %t, want %t", subject, text, logs, row.ran)
				}
			}
		})
	}
}
