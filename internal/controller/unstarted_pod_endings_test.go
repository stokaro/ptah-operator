package controller

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// An Apply whose Pod the kubelet cannot start ends in one of several ways before
// anything reads its result: a person fixes urlFrom after reading
// `secret ... not found`, deletes the stuck Job, deletes the resource, or a
// release moves an execution component under it. Each ending writes a sentence
// of its own, and the record kept only that sentence, with no trace of why the
// Pod never started (#972). These rows start from a Pod the kubelet reports,
// read the words the resource wrote from it, end the Apply each way, and read
// the record that is left once nothing can still write.

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

func TestASchemaApplyWhoseInputsChangedKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	builder := workloadBuilderForMigrations()
	run := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{dispatcher: builder, recorded: true}, builder)
	reconciler, api, schema, job := run.reconciler, run.api, run.schema, run.job
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"), UID: "unstarted-pod-uid",
			Labels: maps.Clone(job.Spec.Template.Labels), Annotations: maps.Clone(job.Spec.Template.Annotations),
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	waitingOnASecret(pod)
	if err := api.Create(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	reconcileOnce := func() {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	reconcileOnce()
	ready := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) ||
		!strings.Contains(ready.Message, missingSecretText) {
		t.Fatalf("Ready while the Pod waits = %#v, want PodConfigurationError naming the Secret, "+
			"or nothing below has words to keep", ready)
	}
	said := ready.Message

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

	settled := safetyGetSchema(t, api, schema)
	if settled.Status.ActiveOperation != nil {
		t.Fatalf("the claim is still active, so this is not the record that stays: %#v", settled.Status.ActiveOperation)
	}
	if pending := settled.Status.PendingObservation; pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("pending observation = %#v, want the Apply's outcome unknown", pending)
	}
	failed := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	want := "operation inputs changed while the Job was running" + lastReportedBefore + said
	if failed == nil || failed.Reason != string(operatorv1alpha1.ReasonApplyOutcomeUnknown) || !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %#v, want ApplyOutcomeUnknown carrying %q", failed, want)
	}
}

func TestWithLastPodReportAddsWhatTheResourceReported(t *testing.T) {
	t.Parallel()

	reported := &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodConfigurationError),
		Message: "Job ptah-m-apply-orders cannot start its Pod: " + missingSecretText,
	}
	ended := errors.New("the operation inputs changed while the Job was running")
	for _, row := range []struct {
		name     string
		reported *metav1.Condition
		want     string
	}{
		{
			name: "a Pod the kubelet could not start", reported: reported,
			want: ended.Error() + lastReportedBefore + reported.Message,
		},
		{name: "no condition", want: ended.Error()},
		{
			name: "a Pod a look found started",
			reported: &metav1.Condition{
				Reason: string(operatorv1alpha1.ReasonOperationInProgress), Message: "Apply operation is in progress",
			},
			want: ended.Error(),
		},
		{
			name: "a Pod admission refused",
			reported: &metav1.Condition{
				Reason: string(operatorv1alpha1.ReasonPodAdmissionRefused), Message: "pods \"x\" is forbidden",
			},
			want: ended.Error(),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := withLastPodReport(ended, row.reported)
			if got.Error() != row.want {
				t.Fatalf("withLastPodReport() = %q, want %q", got, row.want)
			}
			if !errors.Is(got, ended) {
				t.Fatalf("withLastPodReport() lost the failure it was given: %v", got)
			}
		})
	}
}
