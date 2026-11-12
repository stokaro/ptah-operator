package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A read-only PtahSchema claim that lets go of the Job under its reserved name,
// because it cannot confirm the Job or because the resource was suspended, used
// to leave that Job with no cleanup TTL: admission admits the TTL only while a
// claim names the Job by UID, so once the claim was gone nothing could schedule
// it. The claim now settles what it owes the Job first, by the rule both families
// keep (releaseReadOnlyJob).
//
// Every Job here comes from the real builder, and every Job write passes the real
// controller-write webhook.

// dispatchedSchemaRead is a Resolve a PtahSchema claimed and dispatched.
type dispatchedSchemaRead struct {
	schema *operatorv1alpha1.PtahSchema
	job    *batchv1.Job
	pod    *corev1.Pod
}

// schemaReadDispatchedBy stands up a running Resolve whose Job shape.dispatcher
// built, altered by shape.alter, with its UID recorded on the claim where
// shape.recorded says so.
func schemaReadDispatchedBy(t *testing.T, shape runShape) dispatchedSchemaRead {
	t.Helper()
	return schemaOperationDispatchedBy(t, operatorv1alpha1.OperationResolve, shape)
}

// schemaOperationDispatchedBy is schemaReadDispatchedBy for a read-only operation
// of the type given. A Plan's claim holds the realm Lease, which the reconciler
// the run is served to is seeded with.
func schemaOperationDispatchedBy(
	t *testing.T,
	operationType operatorv1alpha1.OperationType,
	shape runShape,
) dispatchedSchemaRead {
	t.Helper()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = mutationlifecycle.SchemaOperation(operationType).Phase
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operationType, ID: strings.ToLower(string(operationType)) + "-dispatched-before-the-pass",
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 11, 59, 0, 0, time.UTC)), Attempt: 1,
	}
	if operationType == operatorv1alpha1.OperationPlan {
		schema.Status.Target = operatorv1alpha1.TargetStatus{
			Engine: schema.Spec.Target.Engine, CoordinationDigest: testCoordinationDigest,
			IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
		}
	}
	if shape.proof {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		schema.Status.PendingObservation = safetyPostApplyObserveSchema(t).Status.PendingObservation
		schema.Status.PendingObservation.PlanRequired = operationType == operatorv1alpha1.OperationPlan
	}
	bindActiveInput(t, schema)
	operation := schema.Status.ActiveOperation
	operation.DispatchStarted = schemaClaimHoldsLock(schema)
	name, err := workload.NameFor(schema, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	build := func() *batchv1.Job {
		t.Helper()
		job, err := shape.dispatcher.Build(schema, *operation, nil)
		if err != nil {
			t.Fatalf("build the dispatched Resolve Job: %v", err)
		}
		if shape.alter != nil {
			shape.alter(job)
		}
		return job
	}
	operation.AdmissionSnapshot = testAdmissionSnapshotOf(&build().Spec.Template)

	job := build()
	job.UID = types.UID("dispatched-job-uid")
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	job.Status.Active = 1
	started := operation.StartedAt
	job.Status.StartTime = &started
	if shape.recorded {
		operation.JobUID = job.UID
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: job.Name + "-pod", UID: "dispatched-pod-uid",
		Labels:          maps.Clone(job.Spec.Template.Labels),
		Annotations:     maps.Clone(job.Spec.Template.Annotations),
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}}
	pod.Spec = *job.Spec.Template.Spec.DeepCopy()
	runningExecutorPod(pod)
	return dispatchedSchemaRead{schema: schema, job: job, pod: pod}
}

// reconciler serves the run to a schema reconciler that builds its Jobs with
// jobs, with every Job write passing the real controller-write webhook, and
// Events recorded.
func (run dispatchedSchemaRead) reconciler(
	t *testing.T,
	jobs workload.Builder,
) (*SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	reconciler, api := fakeReconciler(t, staticLogs{}, run.schema, run.job, run.pod)
	reconciler.Jobs = jobs
	reconciler.Client = admittedJobWrites(api.(client.WithWatch), jobs)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	return reconciler, api.(client.WithWatch), recorder
}

func (run dispatchedSchemaRead) request() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
}

// finishSchemaRead ends the run's Job the way the Job controller does once its
// Pod has stopped.
func finishSchemaRead(t *testing.T, api client.Client, run dispatchedSchemaRead) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 0
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
	}}
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// schemaCleanupDeferredFor reports whether events hold the Warning that says the
// named Job stays, for the reason want names.
func schemaCleanupDeferredFor(events []string, job, want string) bool {
	return slices.ContainsFunc(events, func(event string) bool {
		return strings.HasPrefix(event, corev1.EventTypeWarning+" SchemaJobCleanupDeferred ") &&
			strings.Contains(event, `"`+job+`"`) && strings.Contains(event, want)
	})
}

// assertSchemaWaitSaidOnce fails unless the claim stayed over its running Job,
// naming it by UID whether or not it had recorded it, with no TTL on the Job, and
// Ready said want, and unless one more pass over the same running Job writes
// nothing at all to the resource.
func assertSchemaWaitSaidOnce(t *testing.T, reconciler *SchemaReconciler, api client.Client, run dispatchedSchemaRead, want string) {
	t.Helper()
	kept := run.schema.Status.ActiveOperation.DeepCopy()
	kept.JobUID = run.job.UID
	held := safetyGetSchema(t, api, run.schema)
	if !equality.Semantic.DeepEqual(held.Status.ActiveOperation, kept) {
		t.Fatalf("the claim did not stay over its running Job, naming it by UID %q: operation=%#v",
			run.job.UID, held.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
	}
	ready := meta.FindStatusCondition(held.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Message != want {
		t.Fatalf("Ready = %#v, want it False and saying %q", ready, want)
	}
	result, err := reconciler.Reconcile(context.Background(), run.request())
	if err != nil {
		t.Fatalf("Reconcile() on the next pass of the wait: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("the next pass of the wait asked for no later pass: %#v", result)
	}
	if again := safetyGetSchema(t, api, run.schema); again.ResourceVersion != held.ResourceVersion {
		t.Fatalf("the next pass of the wait wrote the resource again: status before %#v, after %#v",
			held.Status, again.Status)
	}
}

// TestARefusedReadOnlySchemaJobIsCollectedOnceItStops is a Resolve that cannot
// confirm the running Job under its name, built by a release that changed the
// Pod template, where admission would admit that Job's TTL: the Job is the one
// the claim fixes without a rebuild, and the claim recorded its UID or records it
// now. The claim waits for the Job to stop, and the pass that sees it stopped
// gives it the TTL a harvested read-only Job gets before the claim moves to its
// next attempt.
func TestARefusedReadOnlySchemaJobIsCollectedOnceItStops(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, recorded := range []bool{true, false} {
		name := "a claim that recorded the Job's UID"
		if !recorded {
			name = "a claim that never recorded the Job's UID"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaReadDispatchedBy(t, runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: recorded})
			reconciler, api, recorder := run.reconciler(t, current)

			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatalf("the claim waiting for its refused Job asked for no later pass: %#v", result)
			}
			assertSchemaWaitSaidOnce(t, reconciler, api, run, "Waiting for Job "+run.job.Name+
				", which this release cannot confirm, to stop before the Resolve operation runs again")

			finishSchemaRead(t, api, run)
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() after the Job stopped: %v", err)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != 300 {
				t.Fatalf("the stopped Job's TTL = %s, want the 300 seconds a harvested read-only Job gets", ttlText(got))
			}
			claimed := run.schema.Status.ActiveOperation
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 ||
				operation.JobUID != "" || operation.JobName == claimed.JobName {
				t.Fatalf("operation = %#v, want the claim moved to attempt %d under a new name", operation, claimed.Attempt+1)
			}
			if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("the Job that got its TTL is named as staying: %q", events)
			}
		})
	}
}

// countingSchemaWrites counts, in front of writer, the status writes that record
// a Job UID on the claim and the patches that reach a Job, including the ones
// admission would refuse.
func countingSchemaWrites(writer client.WithWatch, recordings, jobPatches *int) client.WithWatch {
	return interceptor.NewClient(writer, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, writer client.Client, subResource string, object client.Object, patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			if schema, ok := object.(*operatorv1alpha1.PtahSchema); ok &&
				schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.JobUID != "" {
				*recordings++
			}
			return writer.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob {
				*jobPatches++
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
}

// unrebuildableJobs is a release that cannot rebuild the Job a claim dispatched,
// the way a builder that refuses an old claim's shape does.
type unrebuildableJobs struct{ workload.Builder }

func (unrebuildableJobs) Build(
	*operatorv1alpha1.PtahSchema,
	operatorv1alpha1.ActiveOperationStatus,
	*operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	return nil, errors.New("injected rebuild refusal")
}

// TestARefusedReadOnlySchemaJobAdmissionWouldNotCollectIsNamed is a Resolve that
// cannot confirm the Job under its name where admission would refuse that Job's
// TTL, or where the Job already has one. The claim does not wait for the Job: it
// moves to its next attempt in the same pass, asks nothing of the Job, and
// records no UID of a Job outside what it fixes. A Warning names a Job left with
// no cleanup and says why.
func TestARefusedReadOnlySchemaJobAdmissionWouldNotCollectIsNamed(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, row := range []struct {
		name  string
		shape runShape
		// moved edits the spec under the run, so the claim holds the Job to what
		// it fixes without a rebuild.
		moved bool
		// collected gives the Job a TTL before the pass, as one that already has
		// its cleanup.
		collected bool
		// stale starts the Job one second further before the pass than the Job
		// it is gets to stop: its deadline, its Pods' grace and the 30 seconds of
		// slack.
		stale bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name:  "a claim that never recorded the UID of a Job under another epoch",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch},
			stays: "its claim never recorded the Job's UID, and the Job is not the one the claim fixes",
		},
		{
			name:  "a Job under another epoch",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			stays: "the Job runs under another execution epoch than its claim",
		},
		{
			name:  "a Job under another epoch after the inputs moved",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			moved: true,
			stays: "the Job runs under another execution epoch than its claim",
		},
		{
			name:      "a Job under another epoch that already has its TTL",
			shape:     runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			collected: true,
		},
		{
			name:  "a Job that did not stop by its own deadline and its Pods' grace",
			shape: runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true},
			stale: true,
			stays: "the Job did not stop by its own deadline and the grace its Pods are given",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaReadDispatchedBy(t, row.shape)
			if row.moved {
				run.schema.Spec.Desired.OCIRef += "-moved"
			}
			if row.collected {
				run.job.Spec.TTLSecondsAfterFinished = ptr(jobclaim.CleanupTTLSeconds)
			}
			if row.stale {
				spec := run.job.Spec
				stopping := time.Duration(*spec.ActiveDeadlineSeconds)*time.Second +
					time.Duration(*spec.Template.Spec.TerminationGracePeriodSeconds)*time.Second + 30*time.Second
				started := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC).Add(-stopping - time.Second))
				run.job.Status.StartTime = &started
			}
			reconciler, api, recorder := run.reconciler(t, current)
			if row.moved {
				if inputs, err := reconciler.operationInputFingerprint(run.schema, operatorv1alpha1.OperationResolve); err != nil ||
					inputs == run.schema.Status.ActiveOperation.InputFingerprint {
					t.Fatalf("the edited spec left the claim's inputs as they were (error %v), so the row would not move them", err)
				}
			}
			var recordings, jobPatches int
			reconciler.Client = countingSchemaWrites(reconciler.Client.(client.WithWatch), &recordings, &jobPatches)

			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			claimed := run.schema.Status.ActiveOperation
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the claim moved to attempt %d at once", operation, claimed.Attempt+1)
			}
			if jobPatches != 0 {
				t.Fatalf("%d writes reached the Job the claim left, want none", jobPatches)
			}
			if got, want := ttlText(storedTTL(t, api, run.job)), ttlText(run.job.Spec.TTLSecondsAfterFinished); got != want {
				t.Fatalf("the Job the claim left has TTL %s, want %s", got, want)
			}
			if !row.shape.recorded && recordings != 0 {
				t.Fatalf("%d status writes recorded the UID of a Job outside what the claim fixes, want none", recordings)
			}
			events := migrationEvents(recorder)
			if row.stays == "" {
				if schemaCleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job its own TTL collects is named as staying: %q", events)
				}
				return
			}
			if !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}

// TestASchemaJobItsReleaseCannotRebuildIsCollectedOnceItStops is a Resolve whose
// release cannot rebuild the Job the claim dispatched, over a Job admission would
// collect. The claim waits for the Job to stop, as it does for a Job whose
// rebuild it refuses, and gives it its TTL before the next attempt.
func TestASchemaJobItsReleaseCannotRebuildIsCollectedOnceItStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := schemaReadDispatchedBy(t, runShape{dispatcher: current, recorded: true})
	reconciler, api, _ := run.reconciler(t, current)
	reconciler.Jobs = unrebuildableJobs{current}

	if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	assertSchemaWaitSaidOnce(t, reconciler, api, run, "Waiting for Job "+run.job.Name+
		", which this release cannot confirm, to stop before the Resolve operation runs again")

	finishSchemaRead(t, api, run)
	if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
		t.Fatalf("Reconcile() after the Job stopped: %v", err)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != 300 {
		t.Fatalf("the stopped Job's TTL = %s, want the 300 seconds a harvested read-only Job gets", ttlText(got))
	}
	claimed := run.schema.Status.ActiveOperation
	if operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; operation == nil ||
		operation.Attempt != claimed.Attempt+1 {
		t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
	}
}

// TestAPlanWaitingForItsRefusedJobKeepsItsLease is a Plan that cannot confirm the
// running Job under its name, where admission would collect it. The Job's Pod can
// still reach the database while the claim waits, so the claim renews the realm
// Lease it holds, as it does while it supervises a Job, and asks for its next pass
// within the 5 seconds a Lease holder renews in.
func TestAPlanWaitingForItsRefusedJobKeepsItsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := schemaOperationDispatchedBy(t, operatorv1alpha1.OperationPlan, runShape{
		dispatcher: managerOnlyPredecessor(current), alter: withAnExecutorSetting, recorded: true,
	})
	reconciler, api, _ := run.reconciler(t, current)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)
	if holder == "" {
		t.Fatal("the running Plan's claim holds no Lease, so nothing below measures one")
	}
	clock := &safetyClock{now: time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)}
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)

	result, err := reconciler.Reconcile(ctx, run.request())
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("the waiting Plan asked for its next pass after %s, want 5s", result.RequeueAfter)
	}
	ready := meta.FindStatusCondition(safetyGetSchema(t, api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	if want := "Waiting for Job " + run.job.Name +
		", which this release cannot confirm, to stop before the Plan operation runs again"; ready == nil || ready.Message != want {
		t.Fatalf("Ready = %#v, want it saying %q", ready, want)
	}
	leaseName, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: reconciler.LockNamespace, Name: leaseName}, lease); err != nil {
		t.Fatalf("Get(Lease): %v", err)
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(clock.now) {
		t.Fatalf("the Lease was renewed at %v, want %s, the pass that waited", lease.Spec.RenewTime, clock.now)
	}
	if got := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); got != holder {
		t.Fatalf("the Lease is held by %q, want the waiting claim's %q", got, holder)
	}
}

// suspendSchema sets spec.suspend on the stored resource.
func suspendSchema(t *testing.T, api client.Client, run dispatchedSchemaRead) {
	t.Helper()
	schema := safetyGetSchema(t, api, run.schema)
	schema.Spec.Suspend = true
	if err := api.Update(context.Background(), schema); err != nil {
		t.Fatal(err)
	}
}

// TestASuspendedReadOnlySchemaClaimNamesTheRunningJobItLeaves is a read-only
// PtahSchema claim over a running Job admission would collect once it stopped,
// when the resource is suspended: one the claim supervises, one whose UID it never
// recorded, and one it was already waiting for after refusing it. Suspension is
// the stop button, so the claim is discarded in that pass without waiting for the
// Job, and a Warning names the Job, which nothing can collect once the claim is
// gone. Nothing is asked of the Job, and the claim records no UID it would never
// use.
func TestASuspendedReadOnlySchemaClaimNamesTheRunningJobItLeaves(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, row := range []struct {
		name  string
		shape runShape
		// refused runs a pass before the suspension, in which the claim refuses
		// the Job and starts to wait for it.
		refused bool
	}{
		{name: "a Job the claim supervises", shape: runShape{dispatcher: current, recorded: true}},
		{name: "a Job the claim never recorded", shape: runShape{dispatcher: current}},
		{
			name:    "a Job the claim was waiting for after refusing it",
			shape:   runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true},
			refused: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaReadDispatchedBy(t, row.shape)
			reconciler, api, recorder := run.reconciler(t, current)
			admitted := reconciler.Client.(client.WithWatch)
			if row.refused {
				if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
					t.Fatalf("Reconcile() before the suspension: %v", err)
				}
				assertSchemaWaitSaidOnce(t, reconciler, api, run, "Waiting for Job "+run.job.Name+
					", which this release cannot confirm, to stop before the Resolve operation runs again")
			}

			suspendSchema(t, api, run)
			var recordings, jobPatches int
			reconciler.Client = countingSchemaWrites(admitted, &recordings, &jobPatches)
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() after the suspension: %v", err)
			}
			settled := safetyGetSchema(t, api, run.schema)
			if settled.Status.ActiveOperation != nil || settled.Status.Phase != operatorv1alpha1.PhaseSuspended {
				t.Fatalf("the suspended claim waited for its running Job: phase %s, operation %#v",
					settled.Status.Phase, settled.Status.ActiveOperation)
			}
			if jobPatches != 0 {
				t.Fatalf("%d writes reached the running Job, want none", jobPatches)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the running Job's TTL = %d, want none", *got)
			}
			if !row.shape.recorded && recordings != 0 {
				t.Fatalf("%d status writes recorded the UID of a Job the claim could not collect, want none", recordings)
			}
			if events := migrationEvents(recorder); !schemaCleanupDeferredFor(events, run.job.Name, notWaited) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, notWaited)
			}
		})
	}
}

// TestASuspendedReadOnlySchemaClaimGoesAtOnceWhereItOwesItsJobNothingItCanGive is
// a read-only PtahSchema claim over a Job that is not running when the resource
// is suspended: the Job has finished, already has its TTL, is not the claim's, or
// is one admission would refuse a TTL. The claim is discarded in that pass. A
// finished Job gets its TTL first, the claim recording its UID where it never
// did, a Job admission would refuse is named by a Warning, and nothing is asked
// of a Job that is not the claim's.
func TestASuspendedReadOnlySchemaClaimGoesAtOnceWhereItOwesItsJobNothingItCanGive(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name     string
		shape    runShape
		finished bool
		// collected gives the Job a TTL before the pass.
		collected bool
		// replaced records another UID on the claim than the Job's.
		replaced bool
		// recordsIt is whether the claim records the UID it never recorded.
		recordsIt bool
		// patches is how many writes reach the Job, and ttl the TTL it has.
		patches int
		ttl     string
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name: "a finished Job", shape: runShape{dispatcher: current, recorded: true},
			finished: true, patches: 1, ttl: "300",
		},
		{
			name: "a finished Job the claim never recorded", shape: runShape{dispatcher: current},
			finished: true, recordsIt: true, patches: 1, ttl: "300",
		},
		{
			name: "a Job that already has its TTL", shape: runShape{dispatcher: current, recorded: true},
			collected: true, ttl: "300",
		},
		{
			name: "a Job that replaced the claim's", shape: runShape{dispatcher: current, recorded: true},
			replaced: true, ttl: "none",
		},
		{
			name: "a Job under another epoch", shape: runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			ttl: "none", stays: "the Job runs under another execution epoch than its claim",
		},
		{
			name:  "a Job under another epoch whose UID the claim never recorded",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch}, ttl: "none",
			stays: "its claim never recorded the Job's UID, and the Job is not the one the claim fixes",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaReadDispatchedBy(t, row.shape)
			if row.collected {
				run.job.Spec.TTLSecondsAfterFinished = ptr(jobclaim.CleanupTTLSeconds)
			}
			if row.replaced {
				run.schema.Status.ActiveOperation.JobUID = types.UID("the-recorded-job-uid")
			}
			run.schema.Spec.Suspend = true
			reconciler, api, recorder := run.reconciler(t, current)
			if row.finished {
				finishSchemaRead(t, api, run)
			}
			var recordings, jobPatches int
			reconciler.Client = countingSchemaWrites(reconciler.Client.(client.WithWatch), &recordings, &jobPatches)

			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			settled := safetyGetSchema(t, api, run.schema)
			if settled.Status.ActiveOperation != nil || settled.Status.Phase != operatorv1alpha1.PhaseSuspended {
				t.Fatalf("the suspended claim stayed: phase %s, operation %#v", settled.Status.Phase, settled.Status.ActiveOperation)
			}
			if jobPatches != row.patches {
				t.Fatalf("%d writes reached the Job, want %d", jobPatches, row.patches)
			}
			if got := ttlText(storedTTL(t, api, run.job)); got != row.ttl {
				t.Fatalf("the Job's TTL = %s, want %s", got, row.ttl)
			}
			if !row.shape.recorded && (recordings != 0) != row.recordsIt {
				t.Fatalf("%d status writes recorded the UID the claim never recorded, want the UID recorded %t",
					recordings, row.recordsIt)
			}
			events := migrationEvents(recorder)
			if row.stays == "" {
				if schemaCleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job the claim owes nothing more is named as staying: %q", events)
				}
				return
			}
			if !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}

// refusingJobTTL answers the first Job patch in front of writer with answer, as
// admission refusing a cleanup TTL does.
func refusingJobTTL(writer client.WithWatch, answer error) client.WithWatch {
	var asked bool
	return interceptor.NewClient(writer, interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob && !asked {
				asked = true
				return answer
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
}

// TestARefusedSchemaJobTTLWriteIsSettledByItsAnswer is the TTL write a read-only
// PtahSchema claim makes before it lets go of a stopped Job. A refusal, Forbidden
// from the controller-write webhook or Invalid from a ValidatingAdmissionPolicy,
// says admission will not collect the Job however often it is asked, so the claim
// moves on, and a Warning names the Job. A Job deleted since the pass read it
// needs no TTL, so the claim moves on and nothing is named. Any other failure may
// pass, so the pass fails with the claim kept, and the next pass asks again.
func TestARefusedSchemaJobTTLWriteIsSettledByItsAnswer(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, row := range []struct {
		name   string
		answer error
		// retried is whether the pass the write failed in moved the claim on.
		retried bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name:    "refused",
			answer:  apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal")),
			retried: true, stays: "injected refusal",
		},
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			retried: true, stays: "injected policy refusal",
		},
		{name: "deleted since it was read", answer: apierrors.NewNotFound(batchv1.Resource("jobs"), ""), retried: true},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaReadDispatchedBy(t, runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true})
			reconciler, api, recorder := run.reconciler(t, current)
			finishSchemaRead(t, api, run)
			reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), row.answer)

			_, err := reconciler.Reconcile(ctx, run.request())
			claimed := run.schema.Status.ActiveOperation
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if row.retried {
				if err != nil {
					t.Fatalf("Reconcile(): %v, want the answer not to fail the pass", err)
				}
				if operation == nil || operation.Attempt != claimed.Attempt+1 {
					t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
				}
				events := migrationEvents(recorder)
				if row.stays == "" {
					if schemaCleanupDeferredFor(events, run.job.Name, "") {
						t.Fatalf("a Job that is gone is named as staying: %q", events)
					}
					return
				}
				if !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
					t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
				}
				return
			}
			if !errors.Is(err, row.answer) {
				t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
			}
			if !equality.Semantic.DeepEqual(operation, claimed) {
				t.Fatalf("a lost TTL write moved the claim on: operation=%#v", operation)
			}
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() after the lost write: %v", err)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
				t.Fatalf("the stopped Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
			}
			if next := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; next == nil ||
				next.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v after the next pass, want the claim moved to attempt %d", next, claimed.Attempt+1)
			}
			if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("a lost TTL write named the Job as staying: %q", events)
			}
		})
	}
}

// TestASuspendedReadOnlySchemaClaimKeepsAFinishedJobWhoseTTLWriteIsLost is a
// suspended PtahSchema whose read-only claim asks for the finished Job's TTL, and
// the write fails with an answer that may pass. Discarding the claim then would
// leave the Job with no TTL for good, since no claim would name it again, so the
// pass fails with the claim kept, and the next pass asks again and lets go.
func TestASuspendedReadOnlySchemaClaimKeepsAFinishedJobWhoseTTLWriteIsLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := schemaReadDispatchedBy(t, runShape{dispatcher: current, recorded: true})
	run.schema.Spec.Suspend = true
	reconciler, api, recorder := run.reconciler(t, current)
	finishSchemaRead(t, api, run)
	lost := errors.New("connection reset by peer")
	reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), lost)

	if _, err := reconciler.Reconcile(ctx, run.request()); !errors.Is(err, lost) {
		t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
	}
	if kept := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; !equality.Semantic.DeepEqual(
		kept, run.schema.Status.ActiveOperation) {
		t.Fatalf("a lost TTL write let the suspended claim go: operation=%#v", kept)
	}
	if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
		t.Fatalf("Reconcile() after the lost write: %v", err)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the finished Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
	}
	if operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; operation != nil {
		t.Fatalf("the suspended claim stayed once its Job had its TTL: %#v", operation)
	}
	if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
		t.Fatalf("a lost TTL write named the Job as staying: %q", events)
	}
}

// TestASuspendedProofReadWaitsForTheJobItRefused is the Observe that proves an
// Apply, over the running Job its release cannot rebuild, when the resource is
// suspended. Proof outranks suspension, so suspension does not discard the claim,
// and the claim waits for the Job it refused to stop, renewing the Apply's Lease,
// as it would on a resource that is not suspended. Once the Job stops it gets the
// TTL a harvested read-only Job gets before the claim moves to its next attempt.
// Letting go at once left that Job with no cleanup, running beside the proof read
// the resume starts.
func TestASuspendedProofReadWaitsForTheJobItRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema, reconciler, api := builtProofFixture(t, operatorv1alpha1.OperationObserve)
	job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
	dispatched := storedSchemaOf(t, api, schema).Status.ActiveOperation
	if dispatched == nil || dispatched.JobUID != job.UID {
		t.Fatalf("the proof read did not record the Job it dispatched: %#v", dispatched)
	}
	started := metav1.NewTime(reconciler.now().Add(-time.Minute))
	job.Status.StartTime = &started
	job.Status.Active = 1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	suspended := storedSchemaOf(t, api, schema)
	suspended.Spec.Suspend = true
	if err := api.Update(ctx, suspended); err != nil {
		t.Fatal(err)
	}
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	reconciler.Jobs = unrebuildableJobs{reconciler.Jobs.(workload.Builder)}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() over the refused Job: %v", err)
	}
	waiting := storedSchemaOf(t, api, schema)
	if !equality.Semantic.DeepEqual(waiting.Status.ActiveOperation, dispatched) || waiting.Status.PendingObservation == nil {
		t.Fatalf("the suspended proof read did not stay over the Job it refused: operation %#v, proof %#v",
			waiting.Status.ActiveOperation, waiting.Status.PendingObservation)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("the waiting proof read asked for its next pass after %s, want the 5s a Lease holder renews in", result.RequeueAfter)
	}
	ready := meta.FindStatusCondition(waiting.Status.Conditions, operatorv1alpha1.ConditionReady)
	if want := "Waiting for Job " + job.Name +
		", which this release cannot confirm, to stop before the Observe operation runs again"; ready == nil || ready.Message != want {
		t.Fatalf("Ready = %#v, want it saying %q", ready, want)
	}
	if proofLeaseHolder(t, reconciler, api) == "" {
		t.Fatal("the waiting proof read released the Apply's Lease")
	}
	if got := storedTTL(t, api, job); got != nil {
		t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
	}

	finished := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), finished); err != nil {
		t.Fatal(err)
	}
	finished.Status.Active = 0
	finished.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, finished); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() after the Job stopped: %v", err)
	}
	if got := storedTTL(t, api, job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the stopped Job's TTL = %s, want the %d seconds a harvested read-only Job gets",
			ttlText(got), jobclaim.CleanupTTLSeconds)
	}
	if next := storedSchemaOf(t, api, schema).Status.ActiveOperation; next == nil ||
		next.Attempt != dispatched.Attempt+1 || next.JobName == dispatched.JobName {
		t.Fatalf("operation = %#v, want the proof read moved to attempt %d under a new name", next, dispatched.Attempt+1)
	}
	if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, job.Name, "") {
		t.Fatalf("the Job that got its TTL is named as staying: %q", events)
	}
}

// TestAReadUnderALostLeaseWaitsForItsJobOnlyUntilItsStopBound is a Plan whose
// realm Lease epoch moved while its Job ran (LeaseContinuityLost). Its result is
// discarded, but the claim first waits for the Job to stop, renewing the Lease,
// since the Job's Pod may still reach the database. It waits by the rule every
// read-only claim waiting for its Job keeps. It waits no longer than
// jobStopBound, dated from what the built Job carries: a Job past its deadline
// carries only FailureTarget until every Pod has terminated, and would otherwise
// hold the realm forever. It does not wait at all for a suspended resource,
// unless the claim proves an Apply, which outranks suspension. A resource being
// deleted waits by the bound alone, suspended or not, as deletion waits on the
// running Job of every claim that holds the lock. A claim that waits renews the
// Lease on the pass that finds it within the bound. A claim that stops waiting
// renews nothing on that pass (#1006): it is discarded with its Lease handed
// back, and a Warning names the Job, which has no TTL.
func TestAReadUnderALostLeaseWaitsForItsJobOnlyUntilItsStopBound(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name string
		// past dates the pass at the Job's stop bound rather than a second
		// before it, suspend suspends the resource, proof makes the claim the
		// Plan that proves an Apply, and deleting deletes the resource first.
		past, suspend, proof, deleting bool
		// stays is what the Warning says of the Job the claim stops waiting
		// for, or empty where the claim waits on.
		stays string
	}{
		{name: "inside its stop bound"},
		{name: "at its stop bound", past: true, stays: "the Job did not stop by its own deadline"},
		{name: "suspended inside its stop bound", suspend: true, stays: notWaited},
		{name: "suspended while it proves an Apply", suspend: true, proof: true},
		{name: "suspended and being deleted inside its stop bound", suspend: true, deleting: true},
		{
			name: "being deleted at its stop bound", past: true, deleting: true,
			stays: "the Job did not stop by its own deadline",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, operatorv1alpha1.OperationPlan,
				runShape{dispatcher: current, recorded: true, proof: row.proof})
			run.schema.Status.ActiveOperation.LeaseContinuityLost = true
			run.schema.Spec.Suspend = row.suspend
			reconciler, api, recorder := run.reconciler(t, current)
			if row.deleting {
				if err := api.Delete(ctx, run.schema.DeepCopy()); err != nil {
					t.Fatal(err)
				}
				if deleting := safetyGetSchema(t, api, run.schema); deleting.DeletionTimestamp == nil {
					t.Fatal("the resource is not being deleted, so the row measures nothing about deletion")
				}
			}
			holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)
			if holder == "" {
				t.Fatal("the running Plan's claim holds no Lease, so nothing below measures one")
			}
			stopBy := jobStopBound(run.job)
			clock := &safetyClock{now: stopBy.Add(-time.Second)}
			if row.past {
				clock.now = stopBy
			}
			reconciler.Clock = clock.Now
			writes := &leaseWrites{}
			reconciler.Locks = targetlock.New(api, writes.writer(api), clock)

			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			held := writes.take()
			// A deletion that lets go of the claim also lets go of the resource.
			got := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.schema), got); err != nil &&
				!(row.deleting && row.stays != "" && apierrors.IsNotFound(err)) {
				t.Fatal(err)
			}
			events := migrationEvents(recorder)
			if ttl := storedTTL(t, api, run.job); ttl != nil {
				t.Fatalf("the running Job's TTL = %d, want none", *ttl)
			}
			if row.stays == "" {
				if got.Status.ActiveOperation == nil || result.RequeueAfter != maxLockContentionPoll {
					t.Fatalf("the claim did not wait for its running Job: result %#v, operation %#v",
						result, got.Status.ActiveOperation)
				}
				if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) != holder ||
					lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(clock.now) {
					t.Fatalf("the waiting claim's Lease is held by %q, renewed %v, want %q renewed at %s",
						leaseHolder(lease), lease.Spec.RenewTime, holder, clock.now)
				}
				if schemaCleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job the claim still waits for is named as staying: %q", events)
				}
				return
			}
			if got.Status.ActiveOperation != nil {
				t.Fatalf("the claim waited for its running Job, whose stop bound is %s: %#v", stopBy, got.Status.ActiveOperation)
			}
			// The release is seen through the same writer, so a renewal would
			// have been too.
			if !slices.Contains(held, "") {
				t.Fatalf("the pass wrote the Lease %q and no release among them, so it measures no write", held)
			}
			if renewals := slices.DeleteFunc(slices.Clone(held), func(h string) bool { return h == "" }); len(renewals) != 0 {
				t.Fatalf("the claim that stopped waiting renewed the Lease for %q before it let go of it", renewals)
			}
			if leased := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); leased != "" {
				t.Fatalf("the discarded claim's Lease is still held by %q", leased)
			}
			if !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}

// TestAReadUnderALostLeaseIsDiscardedWhoeverHoldsTheLease is a Plan whose realm
// Lease epoch moved, and whose Lease a sibling's operation has since taken, as
// after a manager that stayed down past the Lease's duration. A claim that still
// waits for its running Job asks for the Lease and waits behind the sibling. One
// that waits no longer, at its Job's stop bound or over a Job that stopped, does
// not ask (#1006): it is discarded on that pass and the sibling keeps the Lease,
// since the release names the claim's own epoch. A claim that asked first would
// wait for the sibling past its own bound. The claim that waits asks again when
// the sibling's Lease would run out, not later.
func TestAReadUnderALostLeaseIsDiscardedWhoeverHoldsTheLease(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name string
		// past dates the pass at the Job's stop bound rather than a second
		// before it, and stopped finishes the Job first.
		past, stopped bool
		discarded     bool
	}{
		{name: "inside its Job's stop bound"},
		{name: "at its Job's stop bound", past: true, discarded: true},
		{name: "over a Job that stopped", stopped: true, discarded: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, operatorv1alpha1.OperationPlan, runShape{dispatcher: current, recorded: true})
			run.schema.Status.ActiveOperation.LeaseContinuityLost = true
			reconciler, api, recorder := run.reconciler(t, current)
			if row.stopped {
				finishSchemaRead(t, api, run)
			}
			stopBy := jobStopBound(run.job)
			measured := stopBy.Add(-time.Second)
			if row.past {
				measured = stopBy
			}
			// A claim that waits first meets the sibling's Lease two seconds
			// short of that Lease's term (takeRealmForASibling takes it for five
			// minutes) before the pass measured. The locker honors a Lease it
			// did not take for its full term from the moment it first saw it, so
			// two seconds of it are left at the measured pass, under the poll a
			// contended claim waits at most (maxLockContentionPoll): the pass
			// waits as long as the Lease says, not as long as the poll.
			const siblingTerm, siblingLeft = 5 * time.Minute, 2 * time.Second
			clock := &safetyClock{now: measured}
			if !row.discarded {
				clock.now = measured.Add(-siblingTerm + siblingLeft)
			}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, api, clock)
			claimed := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)
			takeRealmForASibling(t, reconciler, api, testCoordinationDigest)
			sibling := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)
			if claimed == "" || sibling == "" || sibling == claimed {
				t.Fatalf("the claim held the Lease as %q and the sibling as %q, so nothing below measures a sibling's Lease", claimed, sibling)
			}
			if !row.discarded {
				if result, err := reconciler.Reconcile(ctx, run.request()); err != nil || result.RequeueAfter != maxLockContentionPoll {
					t.Fatalf("Reconcile() on first meeting the sibling's Lease = %#v, %v; want a wait of %s",
						result, err, maxLockContentionPoll)
				}
				clock.now = measured
			}

			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if leased := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); leased != sibling {
				t.Fatalf("the Lease is held by %q after the pass, want the sibling's %q", leased, sibling)
			}
			events := migrationEvents(recorder)
			if !row.discarded {
				if operation == nil || operation.ID != run.schema.Status.ActiveOperation.ID {
					t.Fatalf("the claim inside its Job's stop bound did not wait for the Lease: %#v", operation)
				}
				if result.RequeueAfter != siblingLeft {
					t.Fatalf("the claim waiting for the sibling asks again after %s, want the %s the sibling's Lease has left",
						result.RequeueAfter, siblingLeft)
				}
				return
			}
			if operation != nil {
				t.Fatalf("the claim that waits no longer waited for the sibling's Lease: %#v, events %q", operation, events)
			}
			if row.stopped {
				if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
					t.Fatalf("the stopped Job's TTL = %s, want the %d seconds a harvested read-only Job gets",
						ttlText(got), jobclaim.CleanupTTLSeconds)
				}
				return
			}
			if !schemaCleanupDeferredFor(events, run.job.Name, "the Job did not stop by its own deadline") {
				t.Fatalf("Events = %q, want a Warning that Job %q stays past its stop bound", events, run.job.Name)
			}
		})
	}
}

// TestAReadUnderALostLeaseSettlesItsJobsTTLWriteByItsAnswer is a Plan whose realm
// Lease epoch moved, over a Job that has stopped, when the Job's TTL write fails.
// A refusal says admission will not collect the Job however often it is asked, so
// the claim is discarded as it would have been, and a Warning names the Job. Any
// other failure may pass, so the pass fails with the claim kept.
func TestAReadUnderALostLeaseSettlesItsJobsTTLWriteByItsAnswer(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name   string
		answer error
		// discarded is whether the pass the write failed in discarded the claim.
		discarded bool
		stays     string
	}{
		{
			name:      "refused",
			answer:    apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal")),
			discarded: true, stays: "injected refusal",
		},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, operatorv1alpha1.OperationPlan, runShape{dispatcher: current, recorded: true})
			run.schema.Status.ActiveOperation.LeaseContinuityLost = true
			reconciler, api, recorder := run.reconciler(t, current)
			finishSchemaRead(t, api, run)
			reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), row.answer)

			_, err := reconciler.Reconcile(ctx, run.request())
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if !row.discarded {
				if !errors.Is(err, row.answer) {
					t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
				}
				if operation == nil || operation.ID != run.schema.Status.ActiveOperation.ID {
					t.Fatalf("a lost TTL write discarded the claim: operation=%#v", operation)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(): %v, want the refusal not to fail the pass", err)
			}
			if operation != nil {
				t.Fatalf("the claim stayed over a Job admission will not collect: %#v", operation)
			}
			if events := migrationEvents(recorder); !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}
