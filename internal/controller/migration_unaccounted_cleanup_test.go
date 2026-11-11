package controller

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// TestAJobStillRunningWhenItsRunWentUnknownIsCollectedOnceItStops is a migration
// Apply whose outcome went unknown while its Job was still running. Admission
// refuses a cleanup TTL on a Job that has not finished, so the claim stays while
// the Job may write, and the pass that retires the claim once the Job has stopped
// is the last one that can schedule it: the webhook admits the TTL only on the Job
// an active claim names. Every Job here comes from the real builder, and every Job
// write passes the real controller-write webhook.
//
// The TTL comes from the rule every migration run keeps its log by, so the rows
// vary what the run's Pod reported and whether a person approved it.
func TestAJobStillRunningWhenItsRunWentUnknownIsCollectedOnceItStops(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	for _, row := range []struct {
		name     string
		way      func() unknownWhileRunning
		approved bool
		reported string
		ttl      int32
	}{
		{name: "an executor rollout, nothing reported", way: executorRolledOut, ttl: long},
		{name: "an executor rollout, reported Applied", way: executorRolledOut, reported: dataplane.MigrationOutcomeApplied, ttl: short},
		{name: "an executor rollout, reported Failed", way: executorRolledOut, reported: dataplane.MigrationOutcomeFailed, ttl: short},
		{
			name: "an executor rollout, approved, reported Failed", way: executorRolledOut,
			approved: true, reported: dataplane.MigrationOutcomeFailed, ttl: long,
		},
		{name: "a release that changed the Job, nothing reported", way: jobRebuiltDifferently, ttl: long},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			way := row.way()
			if row.approved {
				way.shape.claim = func(operation *operatorv1alpha1.MigrationOperationStatus) {
					operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
				}
			}
			run, reconciler, api, recorder := way.reach(t)
			stopMigrationWorkload(t, api, run, row.reported)
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			settled := readMigration(t, api, run.migration)
			if settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != row.ttl {
				t.Fatalf("the stopped Job's TTL = %s once its claim went, want %d", ttlText(got), row.ttl)
			}
			events := migrationEvents(recorder)
			assertNoShortening(t, events)
			if deferred := slices.IndexFunc(events, func(event string) bool {
				return strings.Contains(event, "MigrationJobCleanupDeferred")
			}); deferred >= 0 {
				t.Fatalf("the Events say the cleanup was not scheduled, and it was: %q", events[deferred])
			}
		})
	}
}

// TestOnlyARefusalLetsAStoppedJobsClaimGoWithoutItsCleanup is what the pass does
// when the TTL write fails. The claim is the only thing that lets a later pass
// schedule the cleanup, so a failure a retry can get past keeps it, and the next
// pass schedules the TTL. A refusal does not: the claim would hold the database
// for a Job admission will not let the manager touch, so it goes, and a Warning
// says the Job stays.
func TestOnlyARefusalLetsAStoppedJobsClaimGoWithoutItsCleanup(t *testing.T) {
	t.Parallel()

	jobs := batchv1.Resource("jobs")
	for _, row := range []struct {
		name    string
		failure error
		// refused is whether the failure is an answer a retry cannot change, so
		// every TTL write gets it rather than the first.
		refused bool
	}{
		{name: "admission refused it", failure: apierrors.NewForbidden(jobs, "ptah", errors.New("an older guard")), refused: true},
		{name: "the API server found it invalid", failure: apierrors.NewInvalid(
			batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "ptah", nil), refused: true},
		{name: "the Job changed since it was read", failure: apierrors.NewConflict(jobs, "ptah", errors.New("the object has been modified"))},
		{name: "the webhook could not be reached", failure: apierrors.NewInternalError(errors.New("failed calling webhook"))},
		{name: "the connection dropped", failure: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := executorRolledOut().reach(t)
			stopMigrationWorkload(t, api, run, "")
			failures := 0
			reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if job, isJob := object.(*batchv1.Job); isJob && job.Spec.TTLSecondsAfterFinished != nil &&
						(row.refused || failures == 0) {
						failures++
						return row.failure
					}
					return writer.Patch(ctx, object, patch, options...)
				},
			})

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			if failures == 0 {
				t.Fatal("no TTL write reached the injected failure, so this row proved nothing")
			}
			current := readMigration(t, api, run.migration)
			if row.refused {
				if err != nil {
					t.Fatalf("Reconcile() error = %v, want a refused cleanup to let the pass finish", err)
				}
				if current.Status.ActiveOperation != nil {
					t.Fatalf("a refused cleanup kept the claim: %#v", current.Status.ActiveOperation)
				}
				if got := storedTTL(t, api, run.job); got != nil {
					t.Fatalf("the refused Job's TTL = %d, want none", *got)
				}
				events := migrationEvents(recorder)
				const says = "has no cleanup scheduled, so it stays until it is deleted by hand or with its owner"
				if !slices.ContainsFunc(events, func(event string) bool {
					return strings.Contains(event, "MigrationJobCleanupDeferred") && strings.Contains(event, says) &&
						strings.Contains(event, run.job.Name)
				}) {
					t.Fatalf("the Events %q do not say the Job %s stays", events, run.job.Name)
				}
				return
			}
			if err == nil {
				t.Fatal("Reconcile() returned no error, want the failed cleanup to fail the pass so it is retried")
			}
			if current.Status.ActiveOperation == nil {
				t.Fatal("a cleanup that failed for a retry to get past let the claim go")
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %d after the failed write, want none yet", *got)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v on the pass after the failure", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the retried pass kept the claim: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
				t.Fatalf("the Job's TTL = %s after the retried pass, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
			}
		})
	}
}

// TestAStoppedClaimLeavesAJobThatIsNotItsOwnAlone is the claim's Job gone and
// another standing under its name by the time the claim's Pod stops. The claim
// goes, and the other Job gets no write: admission would refuse its TTL, and the
// Warning that followed would name a Job the claim never ran.
func TestAStoppedClaimLeavesAJobThatIsNotItsOwnAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, recorder := executorRolledOut().reach(t)
	stopMigrationWorkload(t, api, run, "")
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	later := stored.DeepCopy()
	later.ResourceVersion = ""
	later.UID = "later-job-uid"
	if err := api.Create(ctx, later); err != nil {
		t.Fatal(err)
	}
	writes := 0
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob {
				writes++
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
		t.Fatalf("the stopped claim was kept: %#v", settled.Status.ActiveOperation)
	}
	if writes != 0 {
		t.Fatalf("%d writes reached a Job the claim never ran, want none", writes)
	}
	if got := storedTTL(t, api, later); got != nil {
		t.Fatalf("the later Job's TTL = %d, want none", *got)
	}
	events := migrationEvents(recorder)
	if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
		t.Fatalf("the Events %q report a cleanup for a Job the claim never ran", events)
	}
}

// unknownWhileRunning is a way a running Apply's outcome goes unknown: who built
// its Job, and the release that reconciles it next.
type unknownWhileRunning struct {
	shape runShape
	next  workload.Builder
	// says is what the unknown run's record names as the reason.
	says string
}

// executorRolledOut is a release that moved the executor. The execution binding
// changes, and the running Apply is recorded unknown under the binding that
// authorized it.
func executorRolledOut() unknownWhileRunning {
	current := workloadBuilderForMigrations()
	next := current
	next.ExecutorImage = "example.invalid/ptah@sha256:" + strings.Repeat("9", 64)
	return unknownWhileRunning{
		shape: runShape{dispatcher: current, recorded: true}, next: next,
		says: "an execution component changed",
	}
}

// jobRebuiltDifferently is a release that changed the Job under the same
// executor. The binding holds, and the claim cannot confirm the Job its
// predecessor built.
func jobRebuiltDifferently() unknownWhileRunning {
	current := workloadBuilderForMigrations()
	return unknownWhileRunning{
		shape: runShape{dispatcher: managerOnlyPredecessor(current), alter: withAnExecutorSetting, recorded: true},
		next:  current, says: "dispatched Apply Job intent changed",
	}
}

// unrecorded is the same way in for a claim whose pass created the Job and stopped
// before it recorded the Job's UID.
func (way unknownWhileRunning) unrecorded() unknownWhileRunning {
	way.shape.recorded = false
	return way
}

// reach takes the pass in which the running Apply's outcome goes unknown, through
// the real controller-write webhook, and fails unless the claim was kept over a
// Job that admission refuses a TTL while it runs.
func (way unknownWhileRunning) reach(t *testing.T) (dispatchedRun, *MigrationReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	ctx := context.Background()

	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	reconciler, api := run.reconciler(t, way.next)
	reconciler.Client = admittedJobWrites(api, way.next)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	held := readMigration(t, api, run.migration)
	if held.Status.ActiveOperation == nil || held.Status.UnresolvedRun == nil ||
		held.Status.LastRun == nil || !strings.Contains(held.Status.LastRun.Message, way.says) {
		t.Fatalf("the running Apply did not go unknown with its claim kept: claim=%#v unresolved=%#v lastRun=%#v",
			held.Status.ActiveOperation, held.Status.UnresolvedRun, held.Status.LastRun)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
	}
	// Admission in this harness refuses the TTL on the running Job, as it does on
	// a cluster. A harness that admitted it would let every row pass on old code.
	running := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), running); err != nil {
		t.Fatal(err)
	}
	if err := scheduleJobCleanup(ctx, reconciler.Client, running, jobclaim.CleanupTTLSeconds); !apierrors.IsForbidden(err) {
		t.Fatalf("a TTL on the running Job was answered %v, want admission to refuse it", err)
	}
	return run, reconciler, api, recorder
}

// ttlText names a stored TTL, or its absence.
func ttlText(ttl *int32) string {
	if ttl == nil {
		return "none"
	}
	return strconv.Itoa(int(*ttl))
}

// stopMigrationWorkload ends a dispatched run the way the Job controller and the
// kubelet do: the Job finished and its one Pod stopped, having reported reported,
// or nothing when reported is empty.
func stopMigrationWorkload(t *testing.T, api client.Client, run dispatchedRun, reported string) {
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
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if reported != "" {
		reportingPod(t, pod, run.migration.Status.ActiveOperation, reported)
	}
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// TestAJobItsClaimNeverRecordedIsNamedWhenItStays is a claim that never recorded
// its Job's UID, as when the manager that created the Job stopped before it
// recorded the create. Admission admits a cleanup TTL only on the Job a claim
// names by UID, so the Job stays until it is deleted by hand or with its owner,
// and a Warning names it when the claim goes. Recording the UID first, the step
// adoption takes, is #970.
//
// A release that changed the Job reads it before the claim is kept, and asks for
// its TTL then; admission refuses it, and that pass says so. A release that moved
// the executor reads no Job a claim did not record, so the pass that retires the
// claim is the first that names it.
func TestAJobItsClaimNeverRecordedIsNamedWhenItStays(t *testing.T) {
	t.Parallel()

	const stays = "has no cleanup scheduled, so it stays until it is deleted by hand or with its owner: " +
		"its claim never recorded the Job's UID"
	for _, row := range []struct {
		name string
		way  func() unknownWhileRunning
		// firstPass is whether the pass that recorded the run unknown already
		// asked for the TTL and was refused.
		firstPass bool
	}{
		{name: "an executor rollout", way: executorRolledOut},
		{name: "a release that changed the Job", way: jobRebuiltDifferently, firstPass: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := row.way().unrecorded().reach(t)
			first := migrationEvents(recorder)
			refused := slices.ContainsFunc(first, func(event string) bool {
				return strings.Contains(event, "MigrationJobCleanupDeferred") && strings.Contains(event, run.job.Name)
			})
			if refused != row.firstPass {
				t.Fatalf("the pass that recorded the run unknown said the cleanup was refused: %t, want %t; Events %q",
					refused, row.firstPass, first)
			}
			retiring := record.NewFakeRecorder(64)
			reconciler.Recorder = retiring
			stopMigrationWorkload(t, api, run, "")
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the unrecorded Job's TTL = %d, want none: admission admits it only by UID", *got)
			}
			events := migrationEvents(retiring)
			named := 0
			for _, event := range events {
				if strings.Contains(event, "MigrationJobCleanupDeferred") && strings.Contains(event, stays) &&
					strings.Contains(event, run.job.Name) {
					named++
				}
			}
			if named != 1 {
				t.Fatalf("%d Events say the Job %s stays because its claim recorded no UID, want 1: %q", named, run.job.Name, events)
			}
		})
	}
}

// TestAJobItsClaimNeverRecordedIsNamedWhenTheFirstPassLetsTheClaimGo is the same
// claim over a Job that had finished before the release that moved the executor
// reconciled it. Nothing can write, so the claim goes in the pass that records the
// run unknown, and that pass is the only one that can name the Job.
func TestAJobItsClaimNeverRecordedIsNamedWhenTheFirstPassLetsTheClaimGo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	way := executorRolledOut().unrecorded()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	reconciler, api := run.reconciler(t, way.next)
	reconciler.Client = admittedJobWrites(api, way.next)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	stopMigrationWorkload(t, api, run, "")
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	settled := readMigration(t, api, run.migration)
	if settled.Status.ActiveOperation != nil || settled.Status.UnresolvedRun == nil ||
		settled.Status.LastRun == nil || !strings.Contains(settled.Status.LastRun.Message, way.says) {
		t.Fatalf("the stopped Apply did not go unknown with its claim let go: claim=%#v unresolved=%#v lastRun=%#v",
			settled.Status.ActiveOperation, settled.Status.UnresolvedRun, settled.Status.LastRun)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the unrecorded Job's TTL = %d, want none: admission admits it only by UID", *got)
	}
	events := migrationEvents(recorder)
	if !slices.ContainsFunc(events, func(event string) bool {
		return strings.Contains(event, "MigrationJobCleanupDeferred") &&
			strings.Contains(event, "its claim never recorded the Job's UID") && strings.Contains(event, run.job.Name)
	}) {
		t.Fatalf("the Events %q do not say the Job %s stays", events, run.job.Name)
	}
}

// TestAnUnrecordedClaimNamesOnlyAJobThatStays holds the Warning to a Job the claim
// could have created and nothing collects: one another resource owns, or one that
// already carries a TTL, gets none.
func TestAnUnrecordedClaimNamesOnlyAJobThatStays(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		change func(t *testing.T, api client.WithWatch, job *batchv1.Job) *batchv1.Job
	}{
		{name: "another resource owns the Job under the name", change: ownedByAnotherMigration},
		{name: "the Job already carries a TTL", change: func(t *testing.T, api client.WithWatch, job *batchv1.Job) *batchv1.Job {
			t.Helper()
			stored := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
				t.Fatal(err)
			}
			stored.Spec.TTLSecondsAfterFinished = ptr(jobclaim.CleanupTTLSeconds)
			if err := api.Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			return stored
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := executorRolledOut().unrecorded().reach(t)
			migrationEvents(recorder)
			retiring := record.NewFakeRecorder(64)
			reconciler.Recorder = retiring
			stopMigrationWorkload(t, api, run, "")
			standing := row.change(t, api, run.job)
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if events := migrationEvents(retiring); slices.ContainsFunc(events, func(event string) bool {
				return strings.Contains(event, "MigrationJobCleanupDeferred")
			}) {
				t.Fatalf("the Events %q say the Job %s (UID %s) stays", events, standing.Name, standing.UID)
			}
		})
	}
}

// ownedByAnotherMigration replaces the Job under its name with a finished one
// another PtahMigration owns.
func ownedByAnotherMigration(t *testing.T, api client.WithWatch, job *batchv1.Job) *batchv1.Job {
	t.Helper()
	ctx := context.Background()

	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	other := stored.DeepCopy()
	other.ResourceVersion = ""
	other.UID = "another-migration-job-uid"
	other.OwnerReferences[0].UID = "another-migration-uid"
	if err := api.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	other.Status = stored.Status
	if err := api.Status().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	return other
}

// TestALostLeaseHandsTheRecordedJobToItsCleanup is a migration Apply whose
// database lock epoch changed under it. The pass that records its outcome unknown
// reads the Job the claim recorded and hands it on, so a Job that has finished
// takes its TTL in that pass, before the claim goes, and its run names the log. A
// Job still running keeps the claim, and the pass that retires it once the Job has
// stopped schedules the TTL.
func TestALostLeaseHandsTheRecordedJobToItsCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		finished bool
	}{
		{name: "the Job had finished", finished: true},
		{name: "the Job was still running"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: current, recorded: true})
			run.migration.Status.ActiveOperation.LeaseContinuityLost = true
			reconciler, api := run.reconciler(t, current)
			reconciler.Client = admittedJobWrites(api, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			if row.finished {
				stopMigrationWorkload(t, api, run, "")
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			recorded := readMigration(t, api, run.migration)
			if recorded.Status.UnresolvedRun == nil || recorded.Status.LastRun == nil ||
				!strings.Contains(recorded.Status.LastRun.Message, "the database lock epoch changed") {
				t.Fatalf("the Apply did not go unknown for its lost lease: unresolved=%#v lastRun=%#v",
					recorded.Status.UnresolvedRun, recorded.Status.LastRun)
			}
			if row.finished {
				const names = "Anything the run wrote is in the Pod log for 24 hours"
				if !strings.Contains(recorded.Status.LastRun.Message, names) {
					t.Fatalf("status.lastRun.message = %q, want it to say %q", recorded.Status.LastRun.Message, names)
				}
			} else {
				if recorded.Status.ActiveOperation == nil {
					t.Fatal("the running Job's claim was let go")
				}
				if got := storedTTL(t, api, run.job); got != nil {
					t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
				}
				stopMigrationWorkload(t, api, run, "")
				if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() error = %v on the pass after the Job stopped", err)
				}
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
				t.Fatalf("the stopped Job's TTL = %s once its claim went, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
			}
			events := migrationEvents(recorder)
			assertNoShortening(t, events)
			if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
				t.Fatalf("the Events %q say the cleanup was not scheduled, and it was", events)
			}
		})
	}
}

// TestAFailedReadOfTheStoppedJobKeepsItsClaim is a read of the stopped Job that
// fails before its TTL is written. The claim is what lets a later pass schedule
// the cleanup, so the pass fails and keeps it, and the next pass schedules the TTL.
func TestAFailedReadOfTheStoppedJobKeepsItsClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, _ := executorRolledOut().reach(t)
	stopMigrationWorkload(t, api, run, "")
	// The pass reads the Job and lists its Pods to learn that nothing can still
	// write, then reads the Job again to schedule its cleanup. Only that read fails.
	listed, failures := false, 0
	reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
		List: func(ctx context.Context, reader client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, pods := list.(*corev1.PodList); pods {
				listed = true
			}
			return reader.List(ctx, list, options...)
		},
		Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, isJob := object.(*batchv1.Job); isJob && listed && failures == 0 {
				failures++
				return apierrors.NewServiceUnavailable("the API server is restarting")
			}
			return reader.Get(ctx, key, object, options...)
		},
	})

	_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
	if failures == 0 {
		t.Fatal("no read of the stopped Job reached the injected failure, so this row proved nothing")
	}
	if err == nil {
		t.Fatal("Reconcile() returned no error, want the failed read to fail the pass so it is retried")
	}
	if current := readMigration(t, api, run.migration); current.Status.ActiveOperation == nil {
		t.Fatal("a failed read of the stopped Job let its claim go")
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the Job's TTL = %d after the failed read, want none yet", *got)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v on the pass after the failure", err)
	}
	if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
		t.Fatalf("the retried pass kept the claim: %#v", settled.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
		t.Fatalf("the Job's TTL = %s after the retried pass, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
	}
}

// TestADeletingResourceLetsAStoppedClaimGoWithoutAskingForATTL is the stopped
// Job's claim on a resource being deleted. The garbage collector takes the Job
// with its owner, so a TTL buys nothing, and a TTL write that kept failing would
// hold the finalizer. Every Job write here fails as a dropped connection does, and
// the resource still goes.
func TestADeletingResourceLetsAStoppedClaimGoWithoutAskingForATTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, _ := executorRolledOut().reach(t)
	stopMigrationWorkload(t, api, run, "")
	if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
		t.Fatal(err)
	}
	writes := 0
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob {
				writes++
				return errors.New("connection reset by peer")
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v, want the deleting resource's claim to go without a Job write", err)
	}
	if writes != 0 {
		t.Fatalf("%d Job writes on a deleting resource, want none", writes)
	}
	if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
		t.Fatalf("the deleting resource kept the stopped Job's claim: %#v", settled.Status.ActiveOperation)
	}
	// The passes after it release the database and the finalizer.
	for range 3 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() error = %v after the claim went", err)
		}
	}
	remaining := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.migration), remaining); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted resource is still stored (error %v): finalizers %q, claim %#v",
			err, remaining.Finalizers, remaining.Status.ActiveOperation)
	}
	if writes != 0 {
		t.Fatalf("%d Job writes on a deleting resource, want none", writes)
	}
}

// TestAFinishedJobWhosePodStillRunsTakesTheTTLItsPodEarns is a Job Kubernetes
// marked Failed while its Pod has not stopped, as its deadline does, when a release
// moves the executor. The claim stays while the Pod may write, and the TTL is not
// asked for until the pass that retires it, so it follows what the stopped Pod
// reported rather than the report it had not made yet.
func TestAFinishedJobWhosePodStillRunsTakesTheTTLItsPodEarns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	way := executorRolledOut()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	run.job.Status.Active = 0
	run.job.Status.Failed = 1
	run.job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}
	reconciler, api := run.reconciler(t, way.next)
	reconciler.Client = admittedJobWrites(api, way.next)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if held := readMigration(t, api, run.migration); held.Status.ActiveOperation == nil || held.Status.UnresolvedRun == nil {
		t.Fatalf("the Apply whose Pod still runs did not go unknown with its claim kept: claim=%#v unresolved=%#v",
			held.Status.ActiveOperation, held.Status.UnresolvedRun)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the Job's TTL = %d while its Pod runs, want none until the Pod has reported", *got)
	}

	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	reportingPod(t, pod, run.migration.Status.ActiveOperation, dataplane.MigrationOutcomeFailed)
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
		t.Fatalf("the stopped Pod's claim was kept: %#v", settled.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the Job's TTL = %s once its Pod reported Failed, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
	}
	events := migrationEvents(recorder)
	assertNoShortening(t, events)
	if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
		t.Fatalf("the Events %q say the cleanup was not scheduled, and it was", events)
	}
}
