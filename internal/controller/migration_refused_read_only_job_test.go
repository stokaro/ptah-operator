package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
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

// assertWaitSaidOnce fails unless the pass that kept the claim over the running
// Job it refused said so in Progressing, naming the Job, and unless one more pass
// over the same running Job writes nothing at all to the resource.
func assertWaitSaidOnce(t *testing.T, reconciler *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
	t.Helper()
	ctx := context.Background()

	held := readMigration(t, api, run.migration)
	progressing := meta.FindStatusCondition(held.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	want := "Waiting for Job " + run.job.Name + ", which this release cannot confirm, to stop"
	if progressing == nil || progressing.Status != metav1.ConditionTrue || !strings.Contains(progressing.Message, want) {
		t.Fatalf("Progressing = %#v, want it True and saying %q", progressing, want)
	}
	result, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
	if err != nil {
		t.Fatalf("Reconcile() on the next pass of the wait: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("the next pass of the wait asked for no later pass: %#v", result)
	}
	if again := readMigration(t, api, run.migration); again.ResourceVersion != held.ResourceVersion {
		t.Fatalf("the next pass of the wait wrote the resource again: status before %#v, after %#v",
			held.Status, again.Status)
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
			assertWaitSaidOnce(t, reconciler, api, run)
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
// the claim moves on and a Warning names the Job. A Job deleted since the pass
// read it needs no TTL, so the claim moves on and nothing is named. Any other
// failure may pass, so the pass fails with the claim kept, and the next pass
// asks again.
func TestARefusedReadOnlyJobsTTLWriteIsSettledByItsAnswer(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	refused := apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal"))
	// A ValidatingAdmissionPolicy refuses a write with Invalid, as the
	// controller-object guard does a Job a predecessor built.
	refusedByPolicy := apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
		field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
	})
	gone := apierrors.NewNotFound(batchv1.Resource("jobs"), "")
	for _, row := range []struct {
		name   string
		answer error
		// retried is whether the pass the write failed in moved the claim on.
		retried bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{name: "refused", answer: refused, retried: true, stays: "injected refusal"},
		{name: "refused by a policy", answer: refusedByPolicy, retried: true, stays: "injected policy refusal"},
		{name: "deleted since it was read", answer: gone, retried: true},
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
					t.Fatalf("Reconcile() error = %v, want the answer not to fail the pass", err)
				}
				if operation == nil || operation.Attempt != claimed.Attempt+1 {
					t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
				}
				events := migrationEvents(recorder)
				if row.stays == "" {
					if cleanupDeferredFor(events, run.job.Name, "") {
						t.Fatalf("a Job that is gone is named as staying: %q", events)
					}
					return
				}
				if !cleanupDeferredFor(events, run.job.Name, row.stays) {
					t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
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

// TestARefusedReadOnlyJobThatDoesNotStopIsLeft is a read-only claim waiting for a
// refused Job admission would collect, where the Job does not finish. Since
// Kubernetes 1.31 a Job past its deadline carries only FailureTarget until every
// Pod has terminated, and a Pod on a node that is gone may never terminate. The
// claim waits for the Job's own deadline, counted from its start, plus the
// termination grace its Pods are given and jobStopReportSlack, or for that grace
// and slack past a condition that says the Job controller is terminating the Pods,
// whichever comes first. Then it leaves the Job, a Warning names it, and the claim
// runs again. Every bound is dated from what the built Job carries.
func TestARefusedReadOnlyJobThatDoesNotStopIsLeft(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	// limits is what the built Job carries: its deadline, and its Pods' grace plus
	// the slack, which is how long past the moment its Pods must have stopped the
	// claim still waits.
	type limits struct{ deadline, stopping time.Duration }
	for _, row := range []struct {
		name string
		// started is how long before the pass the Job started, or, with
		// unstarted, was created.
		started   func(limits) time.Duration
		unstarted bool
		// condition is the condition the Job controller set, standing long before
		// the pass, or none.
		condition batchv1.JobConditionType
		reason    string
		standing  func(limits) time.Duration
		// bound is how far after the start the claim stops waiting, where it does.
		bound func(limits) time.Duration
		left  bool
	}{
		{
			name:      "past its deadline, with a Pod still terminating",
			started:   func(l limits) time.Duration { return l.deadline + l.stopping + time.Second },
			condition: batchv1.JobFailureTarget, reason: batchv1.JobReasonDeadlineExceeded,
			standing: func(l limits) time.Duration { return l.stopping + time.Second },
			bound:    func(l limits) time.Duration { return l.deadline + l.stopping },
			left:     true,
		},
		{
			name:      "past its deadline, inside the grace",
			started:   func(l limits) time.Duration { return l.deadline + l.stopping - time.Second },
			condition: batchv1.JobFailureTarget, reason: batchv1.JobReasonDeadlineExceeded,
			standing: func(l limits) time.Duration { return l.stopping - time.Second },
		},
		{
			name:    "past its deadline, with nothing set",
			started: func(l limits) time.Duration { return l.deadline + l.stopping + time.Second },
			bound:   func(l limits) time.Duration { return l.deadline + l.stopping },
			left:    true,
		},
		{
			name:      "failing before its deadline, past the grace",
			started:   func(l limits) time.Duration { return l.stopping + time.Minute },
			condition: batchv1.JobFailureTarget, reason: batchv1.JobReasonBackoffLimitExceeded,
			standing: func(l limits) time.Duration { return l.stopping + time.Second },
			bound:    func(l limits) time.Duration { return time.Minute - time.Second + l.stopping },
			left:     true,
		},
		{
			name:      "failing before its deadline, inside the grace",
			started:   func(l limits) time.Duration { return l.stopping + time.Minute },
			condition: batchv1.JobFailureTarget, reason: batchv1.JobReasonBackoffLimitExceeded,
			standing: func(l limits) time.Duration { return l.stopping - time.Second },
		},
		{
			name:      "succeeding before its deadline, past the grace",
			started:   func(l limits) time.Duration { return l.stopping + time.Minute },
			condition: batchv1.JobSuccessCriteriaMet, reason: batchv1.JobReasonCompletionsReached,
			standing: func(l limits) time.Duration { return l.stopping + time.Second },
			bound:    func(l limits) time.Duration { return time.Minute - time.Second + l.stopping },
			left:     true,
		},
		{
			name:      "created before its deadline, not started",
			started:   func(l limits) time.Duration { return time.Minute },
			unstarted: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{
				dispatcher: predecessor, alter: withAnExecutorSetting, recorded: true,
			})
			deadline, grace := run.job.Spec.ActiveDeadlineSeconds, run.job.Spec.Template.Spec.TerminationGracePeriodSeconds
			if deadline == nil || grace == nil {
				t.Fatalf("the built Job carries deadline %v and grace %v, so the row dates nothing from them", deadline, grace)
			}
			bounds := limits{
				deadline: time.Duration(*deadline) * time.Second,
				stopping: time.Duration(*grace)*time.Second + jobStopReportSlack,
			}
			if row.unstarted {
				run.job.Status.StartTime = nil
			}
			reconciler, api := run.reconciler(t, current)
			// The reconciler's clock is fixed, and the Job is dated back from it.
			now := reconciler.now()
			start := now.Add(-row.started(bounds))
			stored := &batchv1.Job{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), stored); err != nil {
				t.Fatal(err)
			}
			if row.unstarted {
				stored.CreationTimestamp = metav1.NewTime(start)
				if err := api.Update(ctx, stored); err != nil {
					t.Fatal(err)
				}
			} else {
				stored.Status.StartTime = &metav1.Time{Time: start}
			}
			if row.condition != "" {
				stored.Status.Active = 0
				stored.Status.Terminating = ptr(int32(1))
				stored.Status.Conditions = []batchv1.JobCondition{{
					Type: row.condition, Status: corev1.ConditionTrue, Reason: row.reason,
					LastTransitionTime: metav1.NewTime(now.Add(-row.standing(bounds))),
				}}
			}
			if err := api.Status().Update(ctx, stored); err != nil {
				t.Fatal(err)
			}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), stored); err != nil {
				t.Fatal(err)
			}
			dated := stored.CreationTimestamp.Time
			if !row.unstarted && stored.Status.StartTime != nil {
				dated = stored.Status.StartTime.Time
			}
			if !dated.Equal(start) || (row.unstarted && stored.Status.StartTime != nil) {
				t.Fatalf("the stored Job starts at %s (created %s, startTime %v), want it dated %s",
					dated, stored.CreationTimestamp, stored.Status.StartTime, start)
			}
			reconciler.Client = admittedJobWrites(api, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			result, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			claimed := run.migration.Status.ActiveOperation
			operation := readMigration(t, api, run.migration).Status.ActiveOperation
			events := migrationEvents(recorder)
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job that has not finished has TTL %d, want none", *got)
			}
			if !row.left {
				if !equality.Semantic.DeepEqual(operation, claimed) {
					t.Fatalf("the claim did not wait for a Job that may still stop: operation=%#v", operation)
				}
				if result.RequeueAfter <= 0 {
					t.Fatalf("the waiting claim asked for no later pass: %#v", result)
				}
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job that may still stop is named as staying: %q", events)
				}
				return
			}
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
			}
			by := start.Add(row.bound(bounds)).UTC().Format(time.RFC3339)
			if !cleanupDeferredFor(events, run.job.Name, "did not stop by its own deadline and the grace its Pods are given, "+by) {
				t.Fatalf("Events = %q, want a Warning that Job %q did not stop by %s", events, run.job.Name, by)
			}
		})
	}
}

// TestAJobsStopBoundIsWhatItCarries holds migrationJobStopBound to the Job's own
// values on inputs the built Jobs never vary: every read-only Job the builder
// writes carries the default termination grace and a deadline, and the Job
// controller sets its conditions True with a time.
func TestAJobsStopBoundIsWhatItCarries(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	started := created.Add(10 * time.Second)
	// slack is the 30 seconds operations.md promises past the grace, written here
	// rather than read from jobStopReportSlack.
	const slack = 30 * time.Second
	condition := func(conditionType batchv1.JobConditionType, status corev1.ConditionStatus, at time.Time) batchv1.JobCondition {
		return batchv1.JobCondition{Type: conditionType, Status: status, LastTransitionTime: metav1.NewTime(at)}
	}
	for _, row := range []struct {
		name       string
		grace      *int64
		deadline   *int64
		conditions []batchv1.JobCondition
		want       time.Time
	}{
		{
			name: "a grace of its own", grace: ptr(int64(90)), deadline: ptr(int64(600)),
			want: started.Add(600*time.Second + 90*time.Second + slack),
		},
		{
			name: "no grace set", deadline: ptr(int64(600)),
			want: started.Add(600*time.Second + 30*time.Second + slack),
		},
		{
			name: "no deadline", grace: ptr(int64(30)),
			want: started.Add(30*time.Second + slack),
		},
		{
			name: "a FailureTarget set False", grace: ptr(int64(30)), deadline: ptr(int64(600)),
			conditions: []batchv1.JobCondition{condition(batchv1.JobFailureTarget, corev1.ConditionFalse, started)},
			want:       started.Add(600*time.Second + 30*time.Second + slack),
		},
		{
			name: "a FailureTarget with no time", grace: ptr(int64(30)), deadline: ptr(int64(600)),
			conditions: []batchv1.JobCondition{condition(batchv1.JobFailureTarget, corev1.ConditionTrue, time.Time{})},
			want:       started.Add(600*time.Second + 30*time.Second + slack),
		},
		{
			name: "another condition", grace: ptr(int64(30)), deadline: ptr(int64(600)),
			conditions: []batchv1.JobCondition{condition(batchv1.JobSuspended, corev1.ConditionTrue, started)},
			want:       started.Add(600*time.Second + 30*time.Second + slack),
		},
		{
			name: "a FailureTarget with a grace of its own", grace: ptr(int64(90)), deadline: ptr(int64(600)),
			conditions: []batchv1.JobCondition{condition(batchv1.JobFailureTarget, corev1.ConditionTrue, started.Add(time.Minute))},
			want:       started.Add(time.Minute + 90*time.Second + slack),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: row.deadline,
					Template:              corev1.PodTemplateSpec{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: row.grace}},
				},
				Status: batchv1.JobStatus{StartTime: &metav1.Time{Time: started}, Conditions: row.conditions},
			}
			if got := migrationJobStopBound(job); !got.Equal(row.want) {
				t.Fatalf("migrationJobStopBound() = %s, want %s", got, row.want)
			}
		})
	}
}

// saysItWaits reports whether Progressing says the claim waits for a Job it refused.
func saysItWaits(migration *operatorv1alpha1.PtahMigration) bool {
	progressing := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	return progressing != nil && strings.HasPrefix(progressing.Message, "Waiting for Job ")
}

// TestAFailedWriteOfTheWaitFailsThePass is the status write that says a read-only
// claim waits for the running Job it refused, failing. The pass fails, so it is
// retried with the claim kept, and the next pass writes what the claim waits for.
func TestAFailedWriteOfTheWaitFailsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{
		dispatcher: managerOnlyPredecessor(current), alter: withAnExecutorSetting, recorded: true,
	})
	reconciler, api := run.reconciler(t, current)
	failures := 0
	reconciler.Client = interceptor.NewClient(admittedJobWrites(api, current), interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, writer client.Client, subResource string, object client.Object, patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			// Only the write that says the claim waits fails, so the row measures
			// that write and no other one the pass makes.
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok && failures == 0 &&
				saysItWaits(migration) {
				failures++
				return apierrors.NewConflict(operatorv1alpha1.GroupVersion.WithResource("ptahmigrations").GroupResource(),
					object.GetName(), errors.New("the object has been modified"))
			}
			return writer.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	})

	_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
	if failures != 1 {
		t.Fatalf("%d status writes reached the injected failure, want one", failures)
	}
	if err == nil {
		t.Fatal("Reconcile() returned no error, want the failed write of the wait to fail the pass")
	}
	claimed := run.migration.Status.ActiveOperation
	if operation := readMigration(t, api, run.migration).Status.ActiveOperation; !equality.Semantic.DeepEqual(operation, claimed) {
		t.Fatalf("the claim did not stay over the running Job after the failed write: operation=%#v", operation)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v on the pass after the failure", err)
	}
	progressing := meta.FindStatusCondition(readMigration(t, api, run.migration).Status.Conditions,
		operatorv1alpha1.ConditionMigrationProgressing)
	if want := "Waiting for Job " + run.job.Name; progressing == nil || !strings.Contains(progressing.Message, want) {
		t.Fatalf("Progressing = %#v after the next pass, want it to say %q", progressing, want)
	}
}
