package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A Job whose Pod the API server refuses -- a namespace policy, a mutating
// webhook the Pod-intent webhook then refuses, a quota -- stands with nothing
// active and nothing terminal until its deadline, and until #447 the resource
// said only that an operation was in progress. The Job controller's
// FailedCreate Event is the one record of why, and these rows read it into the
// condition and back out again when the Pod arrives after all.

const refusalText = `Error creating: pods "ptah-apply-orders-" is forbidden: ValidatingAdmissionPolicy 'mesh-opt-out' with binding 'mesh-opt-out' denied request: operation Pods must opt out of sidecar injection`

// recordedRefusal drains the recorder and reports whether a PodAdmissionRefused
// Event was among what the passes so far recorded; the dispatch records Events
// of its own beside it.
func recordedRefusal(recorder *record.FakeRecorder) bool {
	found := false
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, string(operatorv1alpha1.ReasonPodAdmissionRefused)) {
			found = true
		}
	}
	return found
}

func failedCreateEvent(job *batchv1.Job, name, message string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: name},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job",
			Namespace: job.Namespace, Name: job.Name, UID: job.UID,
		},
		Reason: failedCreateEventReason, Type: corev1.EventTypeWarning, Message: message,
		LastTimestamp: metav1.NewTime(at), Count: 1,
		Source: corev1.EventSource{Component: "job-controller"},
	}
}

func TestASchemaReportsAPodTheAPIServerRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, jobName := dispatchableApply(t, false)
	recorder := record.NewFakeRecorder(10)
	reconciler.Recorder = recorder
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if applyDispatched(t, api, schema.Namespace, jobName) {
			break
		}
	}
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: jobName}, job); err != nil {
		t.Fatalf("the harness never dispatched, so nothing below proves anything: %v", err)
	}

	// A Job with no Pod and no Event is an operation in progress, not a
	// refusal: the Job controller has not spoken yet.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Ready before any Event = %#v, want OperationInProgress", ready)
	}

	// An older Event with another message and the refusal that stands now:
	// the condition carries the latest one.
	older := failedCreateEvent(job, "older", "Error creating: something earlier", reconciler.now().Add(-time.Minute))
	latest := failedCreateEvent(job, "latest", refusalText+"\n\twith a\x00control character", reconciler.now())
	for _, event := range []*corev1.Event{older, latest} {
		if err := api.Create(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := safetyGetSchema(t, api, schema)
	ready = meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(operatorv1alpha1.ReasonPodAdmissionRefused) {
		t.Fatalf("Ready after the refusal = %#v, want False/PodAdmissionRefused", ready)
	}
	for _, want := range []string{jobName, "denied request: operation Pods must opt out of sidecar injection", "with a control character"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("Ready message %q does not carry %q", ready.Message, want)
		}
	}
	if strings.Contains(ready.Message, "something earlier") || strings.ContainsAny(ready.Message, "\x00\n\t") {
		t.Errorf("Ready message %q carries the older refusal or a control character", ready.Message)
	}
	if stored.Status.ActiveOperation == nil || stored.Status.ActiveOperation.JobName != jobName {
		t.Fatalf("the refusal retired the claim: %#v", stored.Status.ActiveOperation)
	}
	if !recordedRefusal(recorder) {
		t.Fatal("no Event was recorded for the refusal")
	}

	// The same refusal on the next pass changes nothing and records nothing.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if recordedRefusal(recorder) {
		t.Fatal("an unchanged refusal was recorded again")
	}

	// The Pod arrives -- the policy changed, or a person declared what it
	// wanted -- and the condition goes back to an operation in progress.
	job.Status.Active = 1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready = meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) ||
		!strings.Contains(ready.Message, "Apply operation is in progress") {
		t.Fatalf("Ready after the Pod arrived = %#v, want OperationInProgress", ready)
	}
}

// A Job younger than the grace is not looked up: the Job controller has not
// had its second yet, and a read there is a read on every dispatch.
func TestAYoungJobWithoutAPodIsNotARefusal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, jobName := dispatchableApply(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if applyDispatched(t, api, schema.Namespace, jobName) {
			break
		}
	}
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: jobName}, job); err != nil {
		t.Fatal(err)
	}
	job.CreationTimestamp = metav1.NewTime(reconciler.now().Add(-podCreationRefusalGrace / 2))
	if err := api.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, failedCreateEvent(job, "early", refusalText, reconciler.now())); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Ready for a Job inside the grace = %#v, want OperationInProgress", ready)
	}
}

func TestAMigrationReportsAPodTheAPIServerRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.AdmissionSnapshot = nil
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	reconciler.Jobs = workloadBuilderForMigrations()
	recorder := record.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	// Two passes persist the snapshot and dispatch the Job.
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("dispatched Jobs = %d, want the one this row refuses a Pod to", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if err := api.Create(ctx, failedCreateEvent(job, "refused", refusalText, reconciler.now())); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := readMigration(t, api, migration)
	progressing := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionFalse ||
		progressing.Reason != string(operatorv1alpha1.ReasonPodAdmissionRefused) ||
		!strings.Contains(progressing.Message, "denied request") {
		t.Fatalf("Progressing after the refusal = %#v, want False/PodAdmissionRefused naming the policy", progressing)
	}
	if stored.Status.ActiveOperation == nil || stored.Status.ActiveOperation.JobName != job.Name {
		t.Fatalf("the refusal retired the claim: %#v", stored.Status.ActiveOperation)
	}
	if !recordedRefusal(recorder) {
		t.Fatal("no Event was recorded for the refusal")
	}

	job.Status.Active = 1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	progressing = meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Progressing after the Pod arrived = %#v, want True/OperationInProgress", progressing)
	}
}

// The message is the API server's refusal, bounded and without control
// characters, after the Job it is about.
func TestPodCreationRefusalMessageIsBoundedAndPlain(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ptah-resolve-orders-0123456789abcdef"}}
	long := strings.Repeat("policy text ", 200)
	message := podCreationRefusalMessage(job, "line one\r\nline\ttwo\x1b[31m "+long)
	if !strings.HasPrefix(message, "Job ptah-resolve-orders-0123456789abcdef cannot create its Pod; the API server refused it: line one line two") {
		t.Fatalf("message = %q", message)
	}
	if strings.ContainsAny(message, "\r\n\t\x1b") {
		t.Fatalf("message carries a control character: %q", message)
	}
	if len(message) > len("Job ptah-resolve-orders-0123456789abcdef cannot create its Pod; the API server refused it: ")+podCreationRefusalMessageLimit+len("...") {
		t.Fatalf("message length = %d, want it bounded", len(message))
	}
	if !strings.HasSuffix(message, "...") {
		t.Fatalf("a truncated message does not say so: %q", message)
	}
}
