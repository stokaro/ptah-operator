package controller

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// An Apply whose Pod the kubelet cannot start ends in one of several ways before
// anything reads its result: a person fixes urlFrom after reading
// `secret ... not found`, deletes or replaces the stuck Job, deletes the
// resource, a release moves an execution component under it, or the database
// Lease changes hands. Each ending writes a sentence of its own, and the record
// kept only that sentence, with no trace of why the Pod never started (#972).
// These rows start from a Pod the kubelet reports, read the words the resource
// wrote from it, end the Apply each way, and read the record that is left once
// nothing can still write. The rows that end in a Pod that started after all
// hold the words back.

const lastReportedBefore = "; last reported before that: "

// waitingOnASecret leaves pod as the kubelet leaves one whose container it
// cannot configure because the Secret it names does not exist.
func waitingOnASecret(pod *corev1.Pod) {
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: createContainerConfigError, Message: missingSecretText,
		}},
	}}}
}

// unstartedMigrationApply is a dispatched Apply whose Job runs and whose Pod
// waits on a Secret that does not exist, after the pass that reports it. The Job
// comes from the real builder, so a pass whose inputs moved holds it to its
// claim and finds it the claim's own (moved_inputs_job_envelope_test.go says
// why the fixture builder cannot). It returns the words the resource wrote on
// Progressing.
func unstartedMigrationApply(t *testing.T) (
	*MigrationReconciler, client.WithWatch, *record.FakeRecorder,
	*operatorv1alpha1.PtahMigration, *batchv1.Job, *corev1.Pod, string,
) {
	t.Helper()

	builder := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: builder, recorded: true})
	waitingOnASecret(run.pod)
	reconciler, api := run.reconciler(t, builder)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder

	stored := reconcileMigration(t, reconciler, api, run.migration)
	progressing := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) ||
		!strings.Contains(progressing.Message, missingSecretText) {
		t.Fatalf("Progressing while the Pod waits = %#v, want PodConfigurationError naming the Secret, "+
			"or nothing below has words to keep", progressing)
	}
	if stored.Status.ActiveOperation == nil || stored.Status.ActiveOperation.JobUID != run.job.UID {
		t.Fatalf("the report retired the claim: %#v", stored.Status.ActiveOperation)
	}
	return reconciler, api, recorder, run.migration, run.job, run.pod, progressing.Message
}

// pastTheDeadline does what the Job controller does at activeDeadlineSeconds:
// the Job fails, and the Pod it could not start is deleted.
func pastTheDeadline(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
}

// settledMigrationRun is the record a migration keeps once its claim is gone. It
// fails unless the Apply was retired as a run nobody accounted for.
func settledMigrationRun(
	t *testing.T,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) *operatorv1alpha1.MigrationRunStatus {
	t.Helper()

	settled := readMigration(t, api, migration)
	if settled.Status.ActiveOperation != nil {
		t.Fatalf("the claim is still active, so this is not the record that stays: %#v", settled.Status.ActiveOperation)
	}
	run := settled.Status.LastRun
	if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want an unknown outcome", run)
	}
	if settled.Status.UnresolvedRun == nil {
		t.Fatal("the run was not recorded as unresolved")
	}
	return run
}

func TestAMigrationApplyWhoseInputsChangedKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, _, migration, job, pod, said := unstartedMigrationApply(t)

	// The person read the words and pointed urlFrom at the Secret that exists.
	// The Job keeps the Pod it made, and that Pod still names the old one.
	edited := readMigration(t, api, migration)
	edited.Spec.Target.URLFrom.Name = "application-database-fixed"
	edited.Generation++
	if err := api.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun != nil {
		t.Fatalf("the edit settled an Apply whose Job is still running, or refused its Job: claim %#v, last run %#v",
			stored.Status.ActiveOperation, stored.Status.LastRun)
	}

	pastTheDeadline(t, api, job, pod)
	reconcileMigration(t, reconciler, api, migration)

	run := settledMigrationRun(t, api, migration)
	if want := "the operation inputs changed while the Job was running" + lastReportedBefore + said; !strings.Contains(run.Message, want) {
		t.Fatalf("last run message = %q, want it to carry %q", run.Message, want)
	}
}

func TestAMigrationApplyWhoseJobWentKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, _, migration, job, pod, said := unstartedMigrationApply(t)

	// The person read the words and deleted the Job. The Pod stays until the
	// garbage collector reaches it, and while it stands it may still start.
	if err := api.Delete(ctx, job.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun == nil {
		t.Fatalf("a missing Job did not record its run, or let the claim go while its Pod stands: claim %#v, unresolved %#v",
			stored.Status.ActiveOperation, stored.Status.UnresolvedRun)
	}
	if err := api.Delete(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	reconcileMigration(t, reconciler, api, migration)

	run := settledMigrationRun(t, api, migration)
	if want := "the dispatched Apply Job is missing and will not be recreated" + lastReportedBefore + said; !strings.Contains(run.Message, want) {
		t.Fatalf("last run message = %q, want it to carry %q", run.Message, want)
	}
}

func TestAMigrationApplyOutlivingItsComponentsKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, _, migration, job, pod, said := unstartedMigrationApply(t)

	// A release moved the executor while the Pod waited for its Secret.
	moved := readMigration(t, api, migration)
	moved.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	if err := api.Status().Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun == nil {
		t.Fatalf("the rollout did not record a run its Job could still perform: claim %#v, unresolved %#v",
			stored.Status.ActiveOperation, stored.Status.UnresolvedRun)
	}

	pastTheDeadline(t, api, job, pod)
	reconcileMigration(t, reconciler, api, migration)

	run := settledMigrationRun(t, api, migration)
	if want := "an execution component changed while the Apply was dispatched" + lastReportedBefore + said; !strings.Contains(run.Message, want) {
		t.Fatalf("last run message = %q, want it to carry %q", run.Message, want)
	}
}

// A deleted resource takes its record with it, so what stays is the Event the
// settling pass recorded.
func TestADeletedMigrationsLastEventKeepsWhyItsApplyPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, recorder, migration, job, pod, said := unstartedMigrationApply(t)
	recordedConfigurationErrors(recorder)

	if err := api.Delete(ctx, readMigration(t, api, migration)); err != nil {
		t.Fatal(err)
	}
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun != nil {
		t.Fatalf("the deletion settled the claim while its Pod could still start: claim %#v, last run %#v",
			stored.Status.ActiveOperation, stored.Status.LastRun)
	}
	pastTheDeadline(t, api, job, pod)
	// One pass records the run, and the next lets the resource go.
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the deletion did not complete, so this is not what stays: error %v, finalizers %#v", err, gone.Finalizers)
	}

	event := findEvent(recorder, corev1.EventTypeWarning+" MigrationRunUncertain ")
	if event == "" {
		t.Fatal("the deletion recorded no MigrationRunUncertain Event")
	}
	if want := "the PtahMigration was deleted while a dispatched Apply was in flight" + lastReportedBefore + said; !strings.Contains(event, want) {
		t.Fatalf("MigrationRunUncertain = %q, want it to carry %q", event, want)
	}
}

// startedAfterAll is the Secret arriving after the last look: the kubelet
// starts the container, and the Job counts a ready Pod.
func startedAfterAll(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	storedPod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), storedPod); err != nil {
		t.Fatal(err)
	}
	runningExecutorPod(storedPod)
	if err := api.Status().Update(ctx, storedPod); err != nil {
		t.Fatal(err)
	}
	storedJob := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), storedJob); err != nil {
		t.Fatal(err)
	}
	ready := int32(1)
	storedJob.Status = batchv1.JobStatus{Active: 1, Ready: &ready}
	if err := api.Status().Update(ctx, storedJob); err != nil {
		t.Fatal(err)
	}
}

// ranToSuccess is a Pod that started after the last look and ran the Apply to
// the end: it succeeded, and the Job counts it and completes.
func ranToSuccess(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	storedPod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), storedPod); err != nil {
		t.Fatal(err)
	}
	storedPod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Reason: "Completed", StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
		}},
	}}}
	if err := api.Status().Update(ctx, storedPod); err != nil {
		t.Fatal(err)
	}
	storedJob := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), storedJob); err != nil {
		t.Fatal(err)
	}
	storedJob.Status = succeededJobStatus()
	if err := api.Status().Update(ctx, storedJob); err != nil {
		t.Fatal(err)
	}
}

// endedWithoutAResult is a Pod that started and then failed before it wrote a
// result: the container exited 1, and the Job used up its backoff limit.
func endedWithoutAResult(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	storedPod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), storedPod); err != nil {
		t.Fatal(err)
	}
	storedPod.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
		}},
	}}}
	if err := api.Status().Update(ctx, storedPod); err != nil {
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

func succeededJobStatus() batchv1.JobStatus {
	return batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}}
}

// replacedBySucceededJob is a person deleting the claim's Job and creating
// another under the same name, which then runs to success.
func replacedBySucceededJob(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	replacement := stored.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-job-uid"
	if err := api.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	replacement.Status = succeededJobStatus()
	if err := api.Status().Update(ctx, replacement); err != nil {
		t.Fatal(err)
	}
}

// disowned is a person removing the owner reference from the claim's Job. It
// is still the claim's Job by UID.
func disowned(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.OwnerReferences = nil
	if err := api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
}

// databaseLeasesGone is the database Lease lapsing or a person deleting it.
// The renewal that follows creates another under a new epoch.
func databaseLeasesGone(t *testing.T, api client.Client, namespace string) {
	t.Helper()

	ctx := context.Background()
	leases := &coordinationv1.LeaseList{}
	if err := api.List(ctx, leases, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) == 0 {
		t.Fatal("no database Lease exists, so removing it proves nothing")
	}
	for index := range leases.Items {
		if err := api.Delete(ctx, &leases.Items[index]); err != nil {
			t.Fatal(err)
		}
	}
}

// A Pod that starts after the last look and runs the Apply to the end leaves
// the words standing on Progressing, since no look came between. Every ending
// that holds the claim's own Job finds that it ran, and the record does not
// repeat a report the run proved out of date.
func TestAMigrationApplyWhosePodRanToTheEndRepeatsNoStaleReport(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		ended string
		end   func(t *testing.T, reconciler *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration, job *batchv1.Job)
		// passes is how many passes the ending takes to leave the record.
		passes  int
		deleted bool
	}{
		{
			name: "its inputs changed", ended: "the operation inputs changed while the Job was running", passes: 1,
			end: func(t *testing.T, _ *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				edited := readMigration(t, api, migration)
				edited.Spec.Execution.ConnectTimeout = metav1.Duration{Duration: 45 * time.Second}
				edited.Generation++
				if err := api.Update(context.Background(), edited); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "its Job lost its owner", ended: "the dispatched Apply Job lost its owner", passes: 1,
			end: func(t *testing.T, _ *MigrationReconciler, api client.Client, _ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				disowned(t, api, job)
			},
		},
		{
			name: "an execution component changed", ended: "an execution component changed while the Apply was dispatched", passes: 1,
			end: func(t *testing.T, _ *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				moved := readMigration(t, api, migration)
				moved.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
				if err := api.Status().Update(context.Background(), moved); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// One pass records the lost continuity, and the next ends the Apply.
			name: "its database Lease changed hands", ended: "the database lock epoch changed under the dispatched run", passes: 2,
			end: func(t *testing.T, reconciler *MigrationReconciler, api client.Client, _ *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				databaseLeasesGone(t, api, reconciler.LockNamespace)
			},
		},
		{
			// One pass records the run, and the next lets the resource go.
			name: "the resource was deleted", ended: "the PtahMigration was deleted while a dispatched Apply was in flight",
			passes: 2, deleted: true,
			end: func(t *testing.T, _ *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				if err := api.Delete(context.Background(), readMigration(t, api, migration)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			reconciler, api, recorder, migration, job, pod, said := unstartedMigrationApply(t)
			ranToSuccess(t, api, job, pod)
			row.end(t, reconciler, api, migration, job)
			for range row.passes {
				if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}

			var record string
			if row.deleted {
				gone := &operatorv1alpha1.PtahMigration{}
				if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
					t.Fatalf("the deletion did not complete, so this is not what stays: error %v, finalizers %#v", err, gone.Finalizers)
				}
				record = findEvent(recorder, corev1.EventTypeWarning+" MigrationRunUncertain ")
			} else {
				record = settledMigrationRun(t, api, migration).Message
			}
			if !strings.Contains(record, row.ended) {
				t.Fatalf("record = %q, want the sentence this ending writes, %q", record, row.ended)
			}
			if strings.Contains(record, said) {
				t.Fatalf("record = %q repeats a report the Job's success proved out of date", record)
			}
		})
	}
}

// A Job someone created under the claim's name is not the claim's Job, so its
// success says nothing about the claim's Pod.
func TestAMigrationApplyWhoseJobWasReplacedKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, _, migration, job, pod, said := unstartedMigrationApply(t)

	replacedBySucceededJob(t, api, job)
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun == nil {
		t.Fatalf("a replaced Job did not record its run, or let the claim go while its Pod stands: claim %#v, unresolved %#v",
			stored.Status.ActiveOperation, stored.Status.UnresolvedRun)
	}
	if err := api.Delete(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	reconcileMigration(t, reconciler, api, migration)

	run := settledMigrationRun(t, api, migration)
	if want := "the dispatched Apply Job was replaced" + lastReportedBefore + said; !strings.Contains(run.Message, want) {
		t.Fatalf("last run message = %q, want it to carry %q", run.Message, want)
	}
}

// The database Lease went while the Pod waited: it lapsed, or a person deleted
// it. The renewal that follows creates another under a new epoch.
func TestAMigrationApplyThatLostItsDatabaseLockKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	reconciler, api, _, migration, job, pod, said := unstartedMigrationApply(t)

	databaseLeasesGone(t, api, reconciler.LockNamespace)
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil ||
		!stored.Status.ActiveOperation.LeaseContinuityLost || stored.Status.UnresolvedRun != nil {
		t.Fatalf("the renewal did not record the lost continuity, or settled the run itself: claim %#v, unresolved %#v",
			stored.Status.ActiveOperation, stored.Status.UnresolvedRun)
	}
	if stored := reconcileMigration(t, reconciler, api, migration); stored.Status.ActiveOperation == nil || stored.Status.UnresolvedRun == nil {
		t.Fatalf("the lost continuity did not record the run, or let the claim go while its Pod stands: claim %#v, unresolved %#v",
			stored.Status.ActiveOperation, stored.Status.UnresolvedRun)
	}

	pastTheDeadline(t, api, job, pod)
	reconcileMigration(t, reconciler, api, migration)

	run := settledMigrationRun(t, api, migration)
	if want := "the database lock epoch changed under the dispatched run" + lastReportedBefore + said; !strings.Contains(run.Message, want) {
		t.Fatalf("last run message = %q, want it to carry %q", run.Message, want)
	}
}

// A deleted resource waits for its Pod, and looks at it while it waits as
// supervision does. A Pod that starts in that wait and then ends without a
// result does not leave the Event a report the look found out of date.
func TestADeletedMigrationWhosePodStartedWhileItWaitedRepeatsNoStaleReport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, recorder, migration, job, pod, said := unstartedMigrationApply(t)
	recordedConfigurationErrors(recorder)

	if err := api.Delete(ctx, readMigration(t, api, migration)); err != nil {
		t.Fatal(err)
	}
	reconcileMigration(t, reconciler, api, migration)
	startedAfterAll(t, api, job, pod)
	waiting := reconcileMigration(t, reconciler, api, migration)
	if progressing := meta.FindStatusCondition(waiting.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing); waiting.Status.ActiveOperation == nil ||
		progressing == nil || progressing.Reason == string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("the deletion wait did not look at the Pod that started: claim %#v, Progressing %#v",
			waiting.Status.ActiveOperation, progressing)
	}

	endedWithoutAResult(t, api, job, pod)
	// One pass records the run, and the next lets the resource go.
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the deletion did not complete, so this is not what stays: error %v, finalizers %#v", err, gone.Finalizers)
	}

	event := findEvent(recorder, corev1.EventTypeWarning+" MigrationRunUncertain ")
	if !strings.Contains(event, "the PtahMigration was deleted while a dispatched Apply was in flight") {
		t.Fatalf("MigrationRunUncertain = %q, want the sentence a deletion writes", event)
	}
	if strings.Contains(event, said) {
		t.Fatalf("MigrationRunUncertain = %q repeats a report the look found out of date", event)
	}
}

// unstartedSchemaApply is unstartedMigrationApply for a schema, which reports a
// Pod that cannot start on Ready. reconcileOnce runs one pass.
func unstartedSchemaApply(t *testing.T) (
	client.Client, *operatorv1alpha1.PtahSchema, *batchv1.Job, *corev1.Pod, string, func(),
) {
	t.Helper()

	run := unstartedSchemaApplyRun(t)
	return run.api, run.schema, run.job, run.pod, run.said, func() {
		t.Helper()
		run.reconcileOnce(t)
	}
}

// unstartedSchema is a schema Apply after the pass that reported why its Pod
// is not running, with what the rows that end it need.
type unstartedSchema struct {
	reconciler *SchemaReconciler
	api        client.Client
	recorder   *record.FakeRecorder
	schema     *operatorv1alpha1.PtahSchema
	job        *batchv1.Job
	// pod is the Pod the kubelet cannot start, and nil when the API server
	// refused to create one.
	pod *corev1.Pod
	// said is what Ready said about the Pod.
	said string
}

func (run unstartedSchema) reconcileOnce(t *testing.T) {
	t.Helper()
	if _, err := run.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

// unstartedSchemaApplyRun is unstartedSchemaApply with the reconciler and an
// Event recorder.
func unstartedSchemaApplyRun(t *testing.T) unstartedSchema {
	t.Helper()

	return schemaApplyUnderReport(t, func(api client.Client, job *batchv1.Job) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: job.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"), UID: "unstarted-pod-uid",
				Labels: maps.Clone(job.Spec.Template.Labels), Annotations: maps.Clone(job.Spec.Template.Annotations),
				OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
			},
			Spec: *job.Spec.Template.Spec.DeepCopy(),
		}
		waitingOnASecret(pod)
		if err := api.Create(context.Background(), pod.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		return pod
	}, operatorv1alpha1.ReasonPodConfigurationError, missingSecretText)
}

// schemaApplyUnderReport dispatches a schema Apply, lets stand return the Pod the
// Job stands with, and runs the pass that reports it. It fails unless Ready
// then carries reason with words in its message.
func schemaApplyUnderReport(
	t *testing.T,
	stand func(api client.Client, job *batchv1.Job) *corev1.Pod,
	reason operatorv1alpha1.ConditionReason,
	words string,
) unstartedSchema {
	t.Helper()

	builder := workloadBuilderForMigrations()
	dispatched := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{dispatcher: builder, recorded: true}, builder)
	run := unstartedSchema{
		reconciler: dispatched.reconciler, api: dispatched.api, recorder: record.NewFakeRecorder(256),
		schema: dispatched.schema, job: dispatched.job,
	}
	run.reconciler.Recorder = run.recorder
	run.pod = stand(run.api, run.job)

	run.reconcileOnce(t)
	ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(reason) || !strings.Contains(ready.Message, words) {
		t.Fatalf("Ready while the Pod is not running = %#v, want %s with %q, "+
			"or nothing below has words to keep", ready, reason, words)
	}
	run.said = ready.Message
	return run
}

// settledSchemaFailure is what a schema keeps once its claim is gone. It fails
// unless the Apply's outcome was recorded as unknown, and returns
// ReconciliationFailed, which carries the sentence that says how the Apply
// ended as well as the words; the ApplyOutcomeUnknown Event carries the words
// alone (rarer_apply_endings_test.go).
func settledSchemaFailure(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) *metav1.Condition {
	t.Helper()

	settled := safetyGetSchema(t, api, schema)
	if settled.Status.ActiveOperation != nil {
		t.Fatalf("the claim is still active, so this is not the record that stays: %#v", settled.Status.ActiveOperation)
	}
	if pending := settled.Status.PendingObservation; pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("pending observation = %#v, want the Apply's outcome unknown", pending)
	}
	failed := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Reason != string(operatorv1alpha1.ReasonApplyOutcomeUnknown) {
		t.Fatalf("ReconciliationFailed = %#v, want ApplyOutcomeUnknown", failed)
	}
	return failed
}

func TestASchemaApplyWhoseInputsChangedKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, schema, job, pod, said, reconcileOnce := unstartedSchemaApply(t)

	edited := safetyGetSchema(t, api, schema)
	edited.Spec.Target.URLFrom.Name = "application-database-fixed"
	edited.Generation++
	if err := api.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()
	if stored := safetyGetSchema(t, api, schema); stored.Status.ActiveOperation == nil || stored.Status.PendingObservation != nil {
		t.Fatalf("the edit settled an Apply whose Job is still running, or refused its Job: conditions %#v",
			stored.Status.Conditions)
	}

	pastTheDeadline(t, api, job, pod)
	reconcileOnce()

	failed := settledSchemaFailure(t, api, schema)
	if want := "operation inputs changed while the Job was running" + lastReportedBefore + said; !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
	}
}

func TestASchemaApplyWhoseJobWentKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, schema, job, _, said, reconcileOnce := unstartedSchemaApply(t)

	if err := api.Delete(ctx, job.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()

	failed := settledSchemaFailure(t, api, schema)
	if want := "dispatched Apply Job is missing and will not be recreated" + lastReportedBefore + said; !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
	}
}

func TestASchemaApplyWhoseJobWasReplacedKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	api, schema, job, _, said, reconcileOnce := unstartedSchemaApply(t)

	replacedBySucceededJob(t, api, job)
	reconcileOnce()

	failed := settledSchemaFailure(t, api, schema)
	if want := "dispatched Apply Job was replaced" + lastReportedBefore + said; !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
	}
}

func TestASchemaApplyOutlivingItsComponentsKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, schema, _, _, said, reconcileOnce := unstartedSchemaApply(t)

	// A release moved the executor while the Pod waited for its Secret.
	moved := safetyGetSchema(t, api, schema)
	moved.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
	if err := api.Status().Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()

	failed := settledSchemaFailure(t, api, schema)
	if want := "execution binding changed after Apply dispatch" + lastReportedBefore + said; !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
	}
}

// TestAMigrationApplyWhosePodRanToTheEndRepeatsNoStaleReport for a schema.
func TestASchemaApplyWhosePodRanToTheEndRepeatsNoStaleReport(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		ended string
		end   func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job)
	}{
		{
			name: "its inputs changed", ended: "operation inputs changed while the Job was running",
			end: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, _ *batchv1.Job) {
				edited := safetyGetSchema(t, api, schema)
				edited.Spec.Target.URLFrom.Name = "application-database-fixed"
				edited.Generation++
				if err := api.Update(context.Background(), edited); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "its Job lost its owner", ended: "dispatched Apply Job lost schema ownership",
			end: func(t *testing.T, api client.Client, _ *operatorv1alpha1.PtahSchema, job *batchv1.Job) {
				disowned(t, api, job)
			},
		},
		{
			name: "an execution component changed", ended: "execution binding changed after Apply dispatch",
			end: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, _ *batchv1.Job) {
				moved := safetyGetSchema(t, api, schema)
				moved.Status.ExecutionBinding.ExecutorImage = "example.invalid/ptah@" + strings.Repeat("9", 64)
				if err := api.Status().Update(context.Background(), moved); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			api, schema, job, pod, said, reconcileOnce := unstartedSchemaApply(t)
			ranToSuccess(t, api, job, pod)
			row.end(t, api, schema, job)
			reconcileOnce()

			failed := settledSchemaFailure(t, api, schema)
			if !strings.Contains(failed.Message, row.ended) {
				t.Fatalf("ReconciliationFailed = %q, want the sentence this ending writes, %q", failed.Message, row.ended)
			}
			if strings.Contains(failed.Message, said) {
				t.Fatalf("ReconciliationFailed = %q repeats a report the Job's success proved out of date", failed.Message)
			}
		})
	}
}

func TestWithLastPodReportAddsWhatTheResourceReported(t *testing.T) {
	t.Parallel()

	const job = "ptah-m-apply-orders"
	reported := &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodConfigurationError),
		Message: "Job " + job + " cannot start its Pod: " + missingSecretText,
	}
	refused := &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodAdmissionRefused),
		Message: "Job " + job + " cannot create its Pod; the API server refused it: pods \"x\" is forbidden",
	}
	ended := errors.New("the operation inputs changed while the Job was running")
	for _, row := range []struct {
		name   string
		report lastPodReport
		want   string
	}{
		{
			name: "a Pod the kubelet could not start", report: lastPodReport{reported: reported, jobName: job},
			want: ended.Error() + lastReportedBefore + reported.Message,
		},
		{name: "no condition", report: lastPodReport{jobName: job}, want: ended.Error()},
		{
			name: "a Pod a look found started",
			report: lastPodReport{jobName: job, reported: &metav1.Condition{
				Reason: string(operatorv1alpha1.ReasonOperationInProgress), Message: "Apply operation is in progress",
			}},
			want: ended.Error(),
		},
		{
			name:   "a Pod the API server refused to create",
			report: lastPodReport{jobName: job, reported: refused},
			want:   ended.Error() + lastReportedBefore + refused.Message,
		},
		{
			name: "another reason in the same words",
			report: lastPodReport{jobName: job, reported: &metav1.Condition{
				Reason: string(operatorv1alpha1.ReasonLeaseContinuityLost), Message: reported.Message,
			}},
			want: ended.Error(),
		},
		{
			name:   "a refusal the claim's Job ran past",
			report: lastPodReport{reported: refused, jobName: job, ran: true},
			want:   ended.Error(),
		},
		{
			name:   "a report about a Job whose name the claim's begins",
			report: lastPodReport{reported: reported, jobName: "ptah-m-apply-order"},
			want:   ended.Error(),
		},
		{
			name:   "a report about another Job",
			report: lastPodReport{reported: reported, jobName: "ptah-m-apply-billing"},
			want:   ended.Error(),
		},
		{
			name:   "a Pod the claim's Job ran to success since",
			report: lastPodReport{reported: reported, jobName: job, ran: true},
			want:   ended.Error(),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := withLastPodReport(ended, row.report)
			if got.Error() != row.want {
				t.Fatalf("withLastPodReport() = %q, want %q", got, row.want)
			}
			if !errors.Is(got, ended) {
				t.Fatalf("withLastPodReport() lost the failure it was given: %v", got)
			}
		})
	}
}

func TestClaimJobRanTakesOnlyTheClaimsJobsSuccess(t *testing.T) {
	t.Parallel()

	const recorded = types.UID("claim-job-uid")
	jobWith := func(uid types.UID, status batchv1.JobStatus) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ptah-m-apply-orders", UID: uid}, Status: status}
	}
	for _, row := range []struct {
		name     string
		job      *batchv1.Job
		recorded types.UID
		want     bool
	}{
		{name: "no Job", recorded: recorded},
		{name: "the claim's Job counting a succeeded Pod", job: jobWith(recorded, batchv1.JobStatus{Succeeded: 1}), recorded: recorded, want: true},
		{
			name: "the claim's Job complete", recorded: recorded, want: true,
			job: jobWith(recorded, batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}),
		},
		{
			name: "the claim's Job past its deadline", recorded: recorded,
			job: jobWith(recorded, batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
			}}}),
		},
		{name: "the claim's Job still running", job: jobWith(recorded, batchv1.JobStatus{Active: 1}), recorded: recorded},
		{name: "a replacement that succeeded", job: jobWith("replacement-job-uid", succeededJobStatus()), recorded: recorded},
		{name: "a succeeded Job and no UID on record", job: jobWith("some-job-uid", succeededJobStatus())},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := claimJobRan(row.job, row.recorded); got != row.want {
				t.Fatalf("claimJobRan() = %t, want %t", got, row.want)
			}
		})
	}
}
