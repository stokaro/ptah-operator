package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A read-only migration claim that cannot confirm the Job under its reserved
// name moves to a new attempt under a new name. The Job it refused used to be
// left with no cleanup TTL, so it stayed until the PtahMigration was deleted,
// and nothing named it. The controller-write webhook admits that TTL only on a
// finished Job the active claim names by UID, so the claim now stays until such
// a Job stops, gives it the TTL a harvested read-only Job gets, and then moves
// on. A Job whose TTL admission would refuse is left at once, and a Warning
// names it.
//
// Every Job here comes from the real builder, and every Job write passes the
// real controller-write webhook.

// finishMigrationJob ends the run's Job the way the Job controller does once its
// Pod has stopped: complete, or failed at its own deadline.
func finishMigrationJob(t *testing.T, api client.Client, run dispatchedRun, ending batchv1.JobConditionType) {
	t.Helper()
	if ending == batchv1.JobComplete {
		stopMigrationWorkload(t, api, run, "")
		return
	}
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 0
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "DeadlineExceeded"
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// assertRefusedOnceStopped takes a read-only claim that refused a running Job
// admission would collect, after the pass that refused it. It fails unless that
// pass kept the claim over the Job and left the Job alone, then stops the Job
// and runs one more pass through the real controller-write webhook, and fails
// unless that pass gave the Job the TTL a harvested read-only Job gets and moved
// the claim to a new attempt for the reason want names.
func assertRefusedOnceStopped(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.WithWatch,
	run dispatchedRun,
	ending batchv1.JobConditionType,
	want string,
) {
	t.Helper()
	ctx := context.Background()

	claimed := run.migration.Status.ActiveOperation
	held := readMigration(t, api, run.migration)
	if !equality.Semantic.DeepEqual(held.Status.ActiveOperation, claimed) {
		t.Fatalf("the claim did not wait for the running Job it refused: operation=%#v", held.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
	}

	reconciler.Client = admittedJobWrites(api, workloadBuilderForMigrations())
	// Admission refuses the TTL while the Job runs, as it does on a cluster. A
	// harness that admitted it would not tell a claim that waits from one that
	// asks at once.
	running := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), running); err != nil {
		t.Fatal(err)
	}
	if err := scheduleJobCleanup(ctx, reconciler.Client, running, jobclaim.CleanupTTLSeconds); !apierrors.IsForbidden(err) {
		t.Fatalf("a TTL on the running Job was answered %v, want admission to refuse it", err)
	}

	finishMigrationJob(t, api, run, ending)
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() after the Job stopped: %v", err)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the stopped Job's TTL = %s, want the %d seconds a harvested read-only Job gets",
			ttlText(got), jobclaim.CleanupTTLSeconds)
	}
	actual := readMigration(t, api, run.migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
		t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
	}
	if operation.JobUID != "" || operation.JobName == claimed.JobName {
		t.Fatalf("the retried claim names Job %q (UID %q), want a new name and no UID", operation.JobName, operation.JobUID)
	}
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || !strings.Contains(progressing.Message, want) {
		t.Fatalf("Progressing = %#v, want it to say %q", progressing, want)
	}
	if actual.Status.LastRun != nil {
		t.Fatalf("a read-only claim settled a run from a Job it refused: %#v", actual.Status.LastRun)
	}
}

// cleanupDeferredFor reports whether events hold the Warning that says the named
// Job stays, for the reason want names.
func cleanupDeferredFor(events []string, job, want string) bool {
	return slices.ContainsFunc(events, func(event string) bool {
		return strings.HasPrefix(event, corev1.EventTypeWarning+" MigrationJobCleanupDeferred ") &&
			strings.Contains(event, `"`+job+`"`) && strings.Contains(event, want)
	})
}

// TestARefusedReadOnlyJobIsCollectedOnceItStops is a read-only claim that cannot
// confirm the running Job under its name, where admission would admit that Job's
// TTL: the claim recorded the Job's UID, and the Job is the one the claim fixes
// without a rebuild. The claim waits for the Job to stop, as it would for a Job
// it supervised, and the pass that sees it stopped schedules its cleanup before
// the claim moves on. A Job that fails at its deadline is collected the same way.
func TestARefusedReadOnlyJobIsCollectedOnceItStops(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, row := range []struct {
		name   string
		shape  runShape
		jobs   MigrationJobBuilder
		ending batchv1.JobConditionType
		want   string
	}{
		{
			name:  "built by a release that changed the Pod template, completing",
			shape: runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true},
			jobs:  current, ending: batchv1.JobComplete, want: "active Job intent changed",
		},
		{
			name:  "built by a release that changed the Pod template, failing at its deadline",
			shape: runShape{dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true},
			jobs:  current, ending: batchv1.JobFailed, want: "active Job intent changed",
		},
		{
			name:  "under a builder that refuses the claim",
			shape: runShape{dispatcher: current, recorded: true},
			jobs:  failingMigrationBuildJobs{}, ending: batchv1.JobComplete, want: "rebuild immutable Job intent",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, row.shape)
			reconciler, api := run.reconciler(t, row.jobs)
			reconciler.Client = admittedJobWrites(api, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			result, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration))
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatalf("the claim waiting for its refused Job asked for no later pass: %#v", result)
			}
			assertRefusedOnceStopped(t, reconciler, api, run, row.ending, row.want)
			if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("the Job that got its TTL is named as staying: %q", events)
			}
		})
	}
}

// TestARefusedReadOnlyJobAdmissionWouldNotCollectIsNamed is a read-only claim
// that cannot confirm the Job under its name, where admission would refuse that
// Job's TTL whenever it is asked. Waiting for the Job would hold the retry for
// nothing, so the claim moves on at once, and a Warning names the Job and why it
// stays. A Job that already carries a TTL is collected by it and is not named.
func TestARefusedReadOnlyJobAdmissionWouldNotCollectIsNamed(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, row := range []struct {
		name  string
		shape runShape
		// moved edits the spec under the run, so the claim holds the Job to what
		// it fixes without a rebuild.
		moved bool
		// collected gives the Job a TTL before the pass, as one that already
		// has its cleanup.
		collected bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name:  "a claim that never recorded the Job's UID",
			shape: runShape{dispatcher: predecessor, alter: withAnExecutorSetting},
			stays: "its claim never recorded the Job's UID",
		},
		{
			name:  "a Job under another epoch",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			stays: "the Job runs under another execution epoch than its claim",
		},
		{
			name:  "a Job whose Pod template is not its snapshot's",
			shape: runShape{dispatcher: current, misrecord: recordedForAnotherTemplate, recorded: true},
			stays: "does not match the persisted admission snapshot",
		},
		{
			name:  "a Job outside its claim after the inputs moved",
			shape: runShape{dispatcher: current, alter: underAnotherClaim, recorded: true},
			moved: true,
			stays: "the Job's labels are not its claim's",
		},
		{
			name:      "a Job under another epoch that already has its TTL",
			shape:     runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			collected: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, row.shape)
			if row.moved {
				moveMigrationInputs(run.migration)
			}
			if row.collected {
				run.job.Spec.TTLSecondsAfterFinished = ptr(jobclaim.CleanupTTLSeconds)
			}
			reconciler, api := run.reconciler(t, current)
			if row.moved {
				requireMigrationInputsMoved(t, reconciler, run.migration)
			}
			reconciler.Client = admittedJobWrites(api, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			claimed := run.migration.Status.ActiveOperation
			operation := readMigration(t, api, run.migration).Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the claim moved to attempt %d at once", operation, claimed.Attempt+1)
			}
			if got, want := ttlText(storedTTL(t, api, run.job)), ttlText(run.job.Spec.TTLSecondsAfterFinished); got != want {
				t.Fatalf("the Job the claim left has TTL %s, want %s", got, want)
			}
			events := migrationEvents(recorder)
			if row.stays == "" {
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job its own TTL collects is named as staying: %q", events)
				}
				return
			}
			if !cleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}

// TestARefusedReadOnlyJobsTTLWriteIsSettledByItsAnswer is the write itself. A
// refusal says admission will not collect the Job however often it is asked, so
// the claim moves on and a Warning names the Job. Any other failure may pass, so
// the pass fails with the claim kept, and the next pass asks again.
func TestARefusedReadOnlyJobsTTLWriteIsSettledByItsAnswer(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	refused := apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal"))
	for _, row := range []struct {
		name   string
		answer error
		// retried is whether the pass the write failed in moved the claim on.
		retried bool
	}{
		{name: "refused", answer: refused, retried: true},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{
				dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true,
			})
			reconciler, api := run.reconciler(t, current)
			finishMigrationJob(t, api, run, batchv1.JobComplete)
			admitted := admittedJobWrites(api, current)
			var asked int
			reconciler.Client = interceptor.NewClient(admitted, interceptor.Funcs{
				Patch: func(
					ctx context.Context,
					writer client.WithWatch,
					object client.Object,
					patch client.Patch,
					options ...client.PatchOption,
				) error {
					if _, ok := object.(*batchv1.Job); ok {
						asked++
						if asked == 1 {
							return row.answer
						}
					}
					return writer.Patch(ctx, object, patch, options...)
				},
			})
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			claimed := run.migration.Status.ActiveOperation
			operation := readMigration(t, api, run.migration).Status.ActiveOperation
			if asked != 1 {
				t.Fatalf("the pass asked for the stopped Job's TTL %d times, want once", asked)
			}
			if row.retried {
				if err != nil {
					t.Fatalf("Reconcile() error = %v, want a refused TTL not to fail the pass", err)
				}
				if operation == nil || operation.Attempt != claimed.Attempt+1 {
					t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
				}
				if events := migrationEvents(recorder); !cleanupDeferredFor(events, run.job.Name, "injected refusal") {
					t.Fatalf("Events = %q, want a Warning that Job %q stays because admission refused its TTL", events, run.job.Name)
				}
				return
			}
			if err == nil {
				t.Fatal("Reconcile() error = nil, want the lost TTL write to fail the pass")
			}
			if !equality.Semantic.DeepEqual(operation, claimed) {
				t.Fatalf("a lost TTL write moved the claim on: operation=%#v", operation)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() after the lost write: %v", err)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
				t.Fatalf("the stopped Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
			}
			if next := readMigration(t, api, run.migration).Status.ActiveOperation; next == nil ||
				next.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v after the next pass, want the claim moved to attempt %d", next, claimed.Attempt+1)
			}
			if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("a lost TTL write named the Job as staying: %q", events)
			}
		})
	}
}
