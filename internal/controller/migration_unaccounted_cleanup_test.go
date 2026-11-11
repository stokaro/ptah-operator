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
				// Nothing was reported, so the run keeps its log for 24 hours: that
				// TTL is asked first, and the five-minute fallback after it.
				if failures != 2 {
					t.Fatalf("%d TTL writes reached the refusal, want the long one and its fallback", failures)
				}
				events := migrationEvents(recorder)
				// No TTL was written, so nothing says the log stays five minutes.
				assertNoShortening(t, events)
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
	// The Job has no TTL yet and is not one that stays: the pass that retires the
	// claim once it stops gives it one, so the record promises no time for its log.
	if strings.Contains(held.Status.LastRun.Message, "kubectl logs") {
		t.Fatalf("LastRun.Message = %q points at the log of a running Job the record cannot time", held.Status.LastRun.Message)
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

// TestAClaimThatNeverRecordedItsJobRecordsItBeforeTheRunIsSettled is a claim that
// never recorded its Job's UID, as when the manager that created the Job stopped
// before it recorded the create. Admission admits a cleanup TTL only on the Job a
// claim names by UID, so the pass that records the run unknown records the UID
// first, the step adopting the Job takes, and the claim it keeps while the Job runs
// names the Job. The pass that retires the claim once the Job has stopped then
// schedules the TTL by the rule a recorded claim's Job follows.
//
// A release that moved the executor and a lost lease read no Job a claim did not
// record, and a release that changed the Job reads it and cannot confirm it. Each
// records the UID all the same.
func TestAClaimThatNeverRecordedItsJobRecordsItBeforeTheRunIsSettled(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		way  func() unknownWhileRunning
	}{
		{name: "an executor rollout", way: executorRolledOut},
		{name: "a release that changed the Job", way: jobRebuiltDifferently},
		{name: "a lost lease", way: leaseLost},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := row.way().unrecorded().reach(t)
			if kept := readMigration(t, api, run.migration).Status.ActiveOperation; kept.JobUID != run.job.UID {
				t.Fatalf("the claim kept over the running Job names Job UID %q, want %q", kept.JobUID, run.job.UID)
			}
			stopMigrationWorkload(t, api, run, "")
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
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

// leaseLost is a database lock that changed hands under the running Apply. The
// release and the binding hold, and the claim records that its lease was not
// continuous.
func leaseLost() unknownWhileRunning {
	current := workloadBuilderForMigrations()
	return unknownWhileRunning{
		shape: runShape{dispatcher: current, recorded: true, claim: func(operation *operatorv1alpha1.MigrationOperationStatus) {
			operation.LeaseContinuityLost = true
		}},
		next: current, says: "the database lock epoch changed",
	}
}

// TestAClaimThatNeverRecordedAFinishedJobSchedulesItsCleanupBeforeItGoes is the
// same claim over a Job that had finished before the release that moved the
// executor reconciled it. Nothing can write, so the claim goes in the pass that
// records the run unknown, and that pass is the only one that can schedule the
// TTL: it records the UID, schedules the TTL, and the run names the Pod log the
// TTL keeps.
func TestAClaimThatNeverRecordedAFinishedJobSchedulesItsCleanupBeforeItGoes(t *testing.T) {
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
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
		t.Fatalf("the finished Job's TTL = %s once its claim went, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
	}
	const names = "Anything the run wrote is in the Pod log for 24 hours"
	if !strings.Contains(settled.Status.LastRun.Message, names) {
		t.Fatalf("status.lastRun.message = %q, want it to say %q", settled.Status.LastRun.Message, names)
	}
	events := migrationEvents(recorder)
	assertNoShortening(t, events)
	if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
		t.Fatalf("the Events %q say the cleanup was not scheduled, and it was", events)
	}
}

// TestAFailedWriteOfTheUIDRecordsNoRun is the write that records an unrecorded
// claim's Job failing in the pass that would record the run unknown. The pass
// fails before it records anything else, so the retry finds the claim as it was
// and records the UID, the run and the TTL in order.
func TestAFailedWriteOfTheUIDRecordsNoRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	way := executorRolledOut().unrecorded()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	reconciler, api := run.reconciler(t, way.next)
	failures := 0
	reconciler.Client = interceptor.NewClient(admittedJobWrites(api, way.next), interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, writer client.Client, subResource string, object client.Object, patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok && failures == 0 &&
				migration.Status.ActiveOperation != nil && migration.Status.ActiveOperation.JobUID != "" {
				failures++
				return apierrors.NewConflict(operatorv1alpha1.GroupVersion.WithResource("ptahmigrations").GroupResource(),
					migration.Name, errors.New("the object has been modified"))
			}
			return writer.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	})
	stopMigrationWorkload(t, api, run, "")

	_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
	if failures == 0 {
		t.Fatal("no write of the UID reached the injected failure, so this row proved nothing")
	}
	if err == nil {
		t.Fatal("Reconcile() returned no error, want the failed write of the UID to fail the pass")
	}
	current := readMigration(t, api, run.migration)
	if current.Status.ActiveOperation == nil || current.Status.ActiveOperation.JobUID != "" ||
		current.Status.LastRun != nil || current.Status.UnresolvedRun != nil {
		t.Fatalf("the failed pass recorded something: claim=%#v lastRun=%#v unresolved=%#v",
			current.Status.ActiveOperation, current.Status.LastRun, current.Status.UnresolvedRun)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v on the pass after the failure", err)
	}
	settled := readMigration(t, api, run.migration)
	if settled.Status.ActiveOperation != nil || settled.Status.UnresolvedRun == nil {
		t.Fatalf("the retried pass did not record the run and let the claim go: claim=%#v unresolved=%#v",
			settled.Status.ActiveOperation, settled.Status.UnresolvedRun)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
		t.Fatalf("the finished Job's TTL = %s after the retried pass, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
	}
}

// TestAnUnrecordedClaimRecordsOnlyAJobItsResourceOwns is a Job another
// PtahMigration owns standing under the name an unrecorded claim reserved. It is
// not the Job this claim's dispatch created, so the claim records no UID, and the
// Job gets no write while it runs or after it stops.
func TestAnUnrecordedClaimRecordsOnlyAJobItsResourceOwns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	way := executorRolledOut().unrecorded()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	run.job.OwnerReferences[0].UID = "another-migration-uid"
	reconciler, api := run.reconciler(t, way.next)
	writes := 0
	reconciler.Client = interceptor.NewClient(admittedJobWrites(api, way.next), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob {
				writes++
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	held := readMigration(t, api, run.migration)
	if held.Status.ActiveOperation == nil || held.Status.UnresolvedRun == nil {
		t.Fatalf("the Apply did not go unknown with its claim kept while the Job under its name runs: claim=%#v unresolved=%#v",
			held.Status.ActiveOperation, held.Status.UnresolvedRun)
	}
	if uid := held.Status.ActiveOperation.JobUID; uid != "" {
		t.Fatalf("the claim records Job UID %q, the UID of a Job another resource owns", uid)
	}
	stopMigrationWorkload(t, api, run, "")
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
		t.Fatalf("the stopped claim was kept: %#v", settled.Status.ActiveOperation)
	}
	if writes != 0 {
		t.Fatalf("%d writes reached a Job another resource owns, want none", writes)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the other resource's Job TTL = %d, want none", *got)
	}
	if events := migrationEvents(recorder); slices.ContainsFunc(events, func(event string) bool {
		return strings.Contains(event, "MigrationJobCleanupDeferred")
	}) {
		t.Fatalf("the Events %q report a cleanup for a Job the claim never ran", events)
	}
}

// TestAnUnreadNameKeepsAnUnrecordedClaimUntilItRecordsItsJob is a read of the name
// an unrecorded claim reserved that fails in the pass that records the run unknown,
// over a Job that has finished. That read is what would have recorded the UID, and
// nothing then says whether the name holds a Job that may still write, so the claim
// and the database stay, as for any read that cannot say. The next pass reads the
// stopped Job, records its UID and schedules its TTL before the claim goes. A write
// of the UID that fails there keeps the claim for the pass after it, because the
// claim is what lets the TTL be scheduled at all. The run recorded before the name
// could be read names no Job, and names the Job once the claim records it, so the
// record says which Job holds the log the TTL keeps.
func TestAnUnreadNameKeepsAnUnrecordedClaimUntilItRecordsItsJob(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// failRecord is whether the write that records the UID fails once.
		failRecord bool
	}{
		{name: "the next pass records it"},
		{name: "the write that records it fails once", failRecord: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			way := executorRolledOut().unrecorded()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
			reconciler, api := run.reconciler(t, way.next)
			reconciler.Client = admittedJobWrites(api, way.next)
			stopMigrationWorkload(t, api, run, "")
			readFailures := 0
			reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
				Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if _, isJob := object.(*batchv1.Job); isJob && readFailures == 0 {
						readFailures++
						return apierrors.NewServiceUnavailable("the API server is restarting")
					}
					return reader.Get(ctx, key, object, options...)
				},
			})

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if readFailures == 0 {
				t.Fatal("no read of the reserved name reached the injected failure, so this row proved nothing")
			}
			held := readMigration(t, api, run.migration)
			if held.Status.ActiveOperation == nil || held.Status.UnresolvedRun == nil ||
				held.Status.LastRun == nil || !strings.Contains(held.Status.LastRun.Message, way.says) {
				t.Fatalf("the Apply did not go unknown with its claim kept over an unread name: claim=%#v unresolved=%#v lastRun=%#v",
					held.Status.ActiveOperation, held.Status.UnresolvedRun, held.Status.LastRun)
			}
			if uid := held.Status.ActiveOperation.JobUID; uid != "" {
				t.Fatalf("the claim records Job UID %q from a read that failed", uid)
			}
			if last, unresolved := held.Status.LastRun, held.Status.UnresolvedRun; last.JobName != "" || last.JobUID != "" ||
				unresolved.JobName != "" || unresolved.JobUID != "" {
				t.Fatalf("the run recorded over an unread name names a Job: lastRun=%#v unresolved=%#v", last, unresolved)
			}
			assertDatabaseStillHeld(t, reconciler, api)
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %d before its claim recorded it, want none", *got)
			}

			recordFailures := 0
			if row.failRecord {
				reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
					SubResourcePatch: func(
						ctx context.Context, writer client.Client, subResource string, object client.Object, patch client.Patch,
						options ...client.SubResourcePatchOption,
					) error {
						if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok && recordFailures == 0 &&
							migration.Status.ActiveOperation != nil && migration.Status.ActiveOperation.JobUID != "" {
							recordFailures++
							return apierrors.NewConflict(operatorv1alpha1.GroupVersion.WithResource("ptahmigrations").GroupResource(),
								migration.Name, errors.New("the object has been modified"))
						}
						return writer.SubResource(subResource).Patch(ctx, object, patch, options...)
					},
				})
				_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
				if recordFailures == 0 {
					t.Fatal("no write of the UID reached the injected failure, so this row proved nothing")
				}
				if err == nil {
					t.Fatal("Reconcile() returned no error, want the failed write of the UID to fail the pass so it is retried")
				}
				if current := readMigration(t, api, run.migration); current.Status.ActiveOperation == nil {
					t.Fatal("a failed write of the UID let the claim go")
				}
				if got := storedTTL(t, api, run.job); got != nil {
					t.Fatalf("the Job's TTL = %d after the failed write of the UID, want none yet", *got)
				}
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v on the pass that records the Job", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
				t.Fatalf("the stopped Job's TTL = %s once its claim went, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
			}
			settled := readMigration(t, api, run.migration)
			last, unresolved := settled.Status.LastRun, settled.Status.UnresolvedRun
			if last == nil || unresolved == nil || last.OperationID != held.Status.LastRun.OperationID {
				t.Fatalf("the unknown run's records went with its claim: lastRun=%#v unresolved=%#v", last, unresolved)
			}
			if last.JobName != run.job.Name || last.JobUID != run.job.UID ||
				unresolved.JobName != run.job.Name || unresolved.JobUID != run.job.UID {
				t.Fatalf("the unknown run does not name Job %q (UID %q), which keeps its log: lastRun=%#v unresolved=%#v",
					run.job.Name, run.job.UID, last, unresolved)
			}
			if last.DispatchedBy == nil || unresolved.DispatchedBy == nil {
				t.Fatalf("the unknown run names its Job but not the manager that built it: lastRun=%#v unresolved=%#v", last, unresolved)
			}
			if !unresolvedRunCopyMatches(settled, unresolved) {
				t.Fatalf("the unresolved-run copy %q is not the record %#v",
					settled.Annotations[operatorv1alpha1.UnresolvedRunAnnotation], unresolved)
			}
		})
	}
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
