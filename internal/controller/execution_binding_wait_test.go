package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// Every Job here comes from the real builder, and the release that reconciles
// it is a real builder too. That builder refuses to build a Job under the
// binding its predecessor recorded, as it refuses any stale binding, so a pass
// that held the Job to what this release would dispatch now, rather than to
// what its claim was dispatched under, settles the run unknown and fails the
// rows that expect it read.

// executorRelease is the release after previous when the two differ in the
// executor image and the Ptah version, the components a rotation waits
// through, and in the manager's own identity. The move crosses v0.34.0, the
// Ptah release from which a migration's OCI pull passes --skip-validation, so
// the Job previous dispatched for v0.3.0 has none and the Jobs next dispatches
// under its own binding have it: a rebuild that took next's arguments rather
// than the recorded binding's differs from the Job it adopts.
func executorRelease(previous workload.Builder) workload.Builder {
	next := previous
	next.ExecutorImage = "example.invalid/ptah@sha256:" + strings.Repeat("e", 64)
	next.PtahVersion = "v0.34.0"
	next.ControllerImage = "example.invalid/manager@sha256:" + strings.Repeat("d", 64)
	next.ControllerRevision = testControllerRevision + "-next"
	next.RunnerImage = "example.invalid/operator@sha256:" + strings.Repeat("d", 64)
	return next
}

// migrationApplyAcrossARotation is a migration Apply the release before next
// dispatched and is still running, reconciled by next through the
// controller-write webhook. Progressing says what claimMigrationApply wrote.
// The claim records its Job's UID when shape says so.
func migrationApplyAcrossARotation(
	t *testing.T,
	shape runShape,
	podChange func(*corev1.Pod),
) (dispatchedRun, *MigrationReconciler, client.WithWatch, workload.Builder) {
	t.Helper()

	previous := workloadBuilderForMigrations()
	next := executorRelease(previous)
	shape.dispatcher = previous
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, shape)
	setMigrationCondition(run.migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApprovedPlan, claimedApplyWords)
	if podChange != nil {
		podChange(run.pod)
	}
	if fetch := migrationPull(run.job); fetch == nil || slices.Contains(fetch.Args, skipValidation) {
		t.Fatalf("the Apply dispatched for Ptah %s pulls with %v, want no %s", previous.PtahVersion, fetch, skipValidation)
	}
	reconciler, api := run.reconciler(t, next)
	reconciler.Client = admittedJobWrites(api, next)
	if configured, err := reconciler.configuredMigrationBinding(); err != nil ||
		executionBindingComponentsEqual(run.migration.Status.ExecutionBinding, configured) ||
		!rotationMayWaitFor(run.migration.Status.ExecutionBinding, configured, run.migration.Status.ActiveOperation.ExecutionBindingID) {
		t.Fatalf("the release does not move the binding in a way a rotation waits through: %#v, %v", configured, err)
	}
	return run, reconciler, api, next
}

// skipValidation is the argument a migration's OCI pull passes from Ptah
// v0.34.0 on.
const skipValidation = "--skip-validation"

// migrationPull is the init container of job that pulls its migration
// artifact, or nil when it pulls none.
func migrationPull(job *batchv1.Job) *corev1.Container {
	for index := range job.Spec.Template.Spec.InitContainers {
		if container := &job.Spec.Template.Spec.InitContainers[index]; container.Name == "fetch-migrations" {
			return container
		}
	}
	return nil
}

// migrationApplyStopped is the Job and its one Pod as the Job controller and
// the kubelet leave them once the executor exited, with the frame logs holds.
func migrationApplyStopped(t *testing.T, reconciler *MigrationReconciler, api client.Client, run dispatchedRun, logs []byte) {
	t.Helper()

	ctx := context.Background()
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active, job.Status.Succeeded = 0, 1
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
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: logs}
}

// migrationApplyResult is what a migration Apply's runner reports.
func migrationApplyResult(migration *operatorv1alpha1.PtahMigration, report *dataplane.MigrationRunReport) runner.Result {
	operation := migration.Status.ActiveOperation
	return runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun:         report,
	}
}

// ofAnotherProtocol is the frame of result as a runner of the next protocol
// writes it: the same report, under a protocol version this manager does not
// read. runner.MarshalFrame refuses to write one, so it is framed by hand.
func ofAnotherProtocol(t *testing.T, result runner.Result) []byte {
	t.Helper()

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["protocolVersion"] = runner.ProtocolVersion + 1
	return foreignRunnerFrame(t, document)
}

// reconcileMigrationUntilRun runs passes until one records a run, and returns
// the migration as that pass stored it. The first pass of a wait writes
// Progressing and ends, so a run is recorded on the second at the latest.
func reconcileMigrationUntilRun(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) *operatorv1alpha1.PtahMigration {
	t.Helper()

	for pass := 0; pass < 3; pass++ {
		if actual := reconcileMigrationTimes(t, reconciler, api, migration, 1); actual.Status.LastRun != nil {
			return actual
		}
	}
	actual := readMigration(t, api, migration)
	t.Fatalf("no pass recorded a run: phase=%q claim=%#v conditions=%#v",
		actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	return nil
}

// reconcileMigrationTimes runs passes passes and returns the migration as
// stored after the last.
func reconcileMigrationTimes(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
	passes int,
) *operatorv1alpha1.PtahMigration {
	t.Helper()

	for pass := 0; pass < passes; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
	}
	return readMigration(t, api, migration)
}

// migrationJobsIn returns every Job in the migration's namespace.
func migrationJobsIn(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) []batchv1.Job {
	t.Helper()

	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(migration.Namespace)); err != nil {
		t.Fatal(err)
	}
	return jobs.Items
}

// claimedApplyWords is Progressing's message as claimMigrationApply writes it
// for the one migration runDispatchedBy's plan holds.
const claimedApplyWords = "Applying 1 planned migration"

// A release that moves the executor and the Ptah version under a running
// migration Apply holds the binding back while the Apply runs. The claim stays
// as it was, under the binding status records, the Job its predecessor built
// is supervised rather than recorded unknown, nothing else is dispatched, and
// Progressing says why the binding has not moved, after what it said: the
// claim's words, or the words a wait for the Lease leaves when it ends.
func TestARotationWaitsForARunningMigrationApply(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		reason  operatorv1alpha1.ConditionReason
		message string
	}{
		{name: "as claimed", reason: operatorv1alpha1.ReasonApprovedPlan, message: claimedApplyWords},
		{name: "after a wait for the Lease", reason: operatorv1alpha1.ReasonOperationInProgress, message: "Apply operation is in progress"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run, reconciler, api, _ := migrationApplyAcrossARotation(t, runShape{recorded: true}, nil)
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(run.migration), run.migration); err != nil {
				t.Fatal(err)
			}
			setMigrationCondition(run.migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, row.reason, row.message)
			if err := api.Status().Update(context.Background(), run.migration); err != nil {
				t.Fatal(err)
			}
			recorded := run.migration.Status.ExecutionBinding.DeepCopy()
			claimed := run.migration.Status.ActiveOperation

			actual := reconcileMigrationTimes(t, reconciler, api, run.migration, 3)
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.JobUID != run.job.UID {
				t.Fatalf("the running Apply's claim did not stay: operation=%#v lastRun=%#v", operation, actual.Status.LastRun)
			}
			if actual.Status.LastRun != nil || actual.Status.UnresolvedRun != nil {
				t.Fatalf("a running Apply was settled: lastRun=%#v unresolved=%#v", actual.Status.LastRun, actual.Status.UnresolvedRun)
			}
			if binding := actual.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the binding moved under a running Apply: %#v, want %#v", binding, recorded)
			}
			progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != string(row.reason) ||
				progressing.Message != row.message+RotationWaitWords {
				t.Fatalf("Progressing = %#v, want it to say the binding waits for the Apply after %q", progressing, row.message)
			}
			if jobs := migrationJobsIn(t, api, run.migration); len(jobs) != 1 || jobs[0].UID != run.job.UID {
				t.Fatalf("Jobs while the rotation waits = %d, want only the running Apply's", len(jobs))
			}
		})
	}
}

// The wait is bounded by the claim's Lease term, not by its execution
// deadline, which falls well before the term ends. A Job still running in
// between holds the binding back. Each row runs at an instant only the right
// bound separates: a second past the execution deadline, and a second before
// the term ends.
func TestARotationWaitsForARunningMigrationApplyUntilItsTermEnds(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		at   func(*operatorv1alpha1.MigrationOperationStatus) time.Time
	}{
		{
			name: "a second past its execution deadline",
			at: func(operation *operatorv1alpha1.MigrationOperationStatus) time.Time {
				return operation.ExecutionNotAfter.Add(time.Second)
			},
		},
		{
			name: "a second before its term ends",
			at: func(operation *operatorv1alpha1.MigrationOperationStatus) time.Time {
				return migrationApplyLeaseTermEnd(operation).Add(-time.Second)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run, reconciler, api, _ := migrationApplyAcrossARotation(t, runShape{recorded: true}, nil)
			recorded := run.migration.Status.ExecutionBinding.DeepCopy()
			claimed := run.migration.Status.ActiveOperation
			termEnd := migrationApplyLeaseTermEnd(claimed)
			if claimed.ExecutionNotAfter == nil || !claimed.ExecutionNotAfter.Add(time.Second).Before(termEnd.Add(-time.Second)) {
				t.Fatalf("the claim's execution deadline %v does not fall before its term ends at %v, so the rows cannot tell the bounds apart",
					claimed.ExecutionNotAfter, termEnd)
			}
			at := row.at(claimed)
			reconciler.Clock = func() time.Time { return at }

			actual := reconcileMigrationTimes(t, reconciler, api, run.migration, 3)
			if operation := actual.Status.ActiveOperation; operation == nil || operation.ID != claimed.ID ||
				operation.JobUID != run.job.UID || actual.Status.LastRun != nil || actual.Status.UnresolvedRun != nil {
				t.Fatalf("the running Apply's claim did not hold at %v: claim=%#v lastRun=%#v unresolved=%#v",
					at, operation, actual.Status.LastRun, actual.Status.UnresolvedRun)
			}
			if binding := actual.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the binding moved at %v under an Apply still inside its term: %#v", at, binding)
			}
		})
	}
}

// The Apply the rotation waited for ends, and its own result settles it under
// the binding that authorized it, as any Apply's does. The binding moves on a
// later pass, once no claim is in flight, and nothing it dispatches after that
// runs under the binding it retired.
func TestAMigrationApplyARotationWaitedForIsSettledByItsOwnResult(t *testing.T) {
	t.Parallel()

	termEnd := func(run dispatchedRun) time.Time {
		return migrationApplyLeaseTermEnd(run.migration.Status.ActiveOperation)
	}
	for _, row := range []struct {
		name    string
		outcome string
		applied []int64
		want    operatorv1alpha1.MigrationRunOutcome
		// at moves the clock before the pass that reads the result. A Job
		// that stopped holds the rotation whenever it is read.
		at func(dispatchedRun) time.Time
		// unrecorded is a claim that lost its create's answer, so it names
		// its Job by the reserved name alone until a pass records the UID.
		unrecorded bool
		// repeatablesAlone is an Apply of a plan that selects repeatable
		// migrations and no version. Its Job carries them in a variable of
		// their own beside an empty versioned sequence, and its rebuild reads
		// them from the same plan.
		repeatablesAlone bool
	}{
		{name: "the run applied", outcome: dataplane.MigrationOutcomeApplied, applied: []int64{3}, want: operatorv1alpha1.MigrationRunOutcomeApplied},
		{name: "the run failed", outcome: dataplane.MigrationOutcomeFailed, want: operatorv1alpha1.MigrationRunOutcomeFailed},
		{
			name: "the run applied and is read after its claim's term", outcome: dataplane.MigrationOutcomeApplied,
			applied: []int64{3}, want: operatorv1alpha1.MigrationRunOutcomeApplied, at: termEnd,
		},
		{
			name: "the run applied under a claim that never recorded its Job", outcome: dataplane.MigrationOutcomeApplied,
			applied: []int64{3}, want: operatorv1alpha1.MigrationRunOutcomeApplied, unrecorded: true,
		},
		{
			name: "the run applied repeatable migrations alone", outcome: dataplane.MigrationOutcomeApplied,
			want: operatorv1alpha1.MigrationRunOutcomeApplied, repeatablesAlone: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			shape := runShape{recorded: !row.unrecorded}
			report := migrationRunReport(row.outcome, row.applied...)
			var appliedRepeatables []string
			if row.repeatablesAlone {
				shape.replan = func(t *testing.T, migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigrationPlan {
					return publishedPlanWith(t, migration, nil, []operatorv1alpha1.PlannedRepeatableMigration{{
						Identity: "names", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA,
					}})
				}
				applied := dataplane.MigrationRepeatable{Identity: "names", Path: "R__names.sql", Checksum: repeatableContentB,
					AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateApplied}
				afterward := syncedHistoryWith(applied)
				report.Planned = nil
				report.PlannedRepeatables = []dataplane.MigrationRepeatableWork{{Identity: "names", Path: "R__names.sql",
					Checksum: repeatableContentB, AppliedChecksum: repeatableContentA, Reason: dataplane.RepeatableReasonChanged}}
				report.AppliedRepeatables = []string{"names"}
				report.Status = &afterward
				appliedRepeatables = report.AppliedRepeatables
			}
			run, reconciler, api, next := migrationApplyAcrossARotation(t, shape, nil)
			recorded := run.migration.Status.ExecutionBinding.DeepCopy()
			claimed := run.migration.Status.ActiveOperation
			if environment := executorEnv(t, run.job); row.repeatablesAlone &&
				(environment[runner.EnvExpectedRepeatableSequence].Value == "" || environment[runner.EnvExpectedMigrationSequence].Value != "[]") {
				t.Fatalf("the Apply of repeatable migrations alone carries sequence %q and repeatable sequence %q",
					environment[runner.EnvExpectedMigrationSequence].Value, environment[runner.EnvExpectedRepeatableSequence].Value)
			}
			if waiting := reconcileMigrationTimes(t, reconciler, api, run.migration, 2); waiting.Status.ActiveOperation == nil ||
				waiting.Status.ActiveOperation.JobUID != run.job.UID || waiting.Status.LastRun != nil {
				t.Fatalf("the waiting claim does not hold its running Job: claim=%#v lastRun=%#v",
					waiting.Status.ActiveOperation, waiting.Status.LastRun)
			}

			migrationApplyStopped(t, reconciler, api, run, migrationFrame(t, migrationApplyResult(run.migration, report)))
			if row.at != nil {
				at := row.at(run)
				reconciler.Clock = func() time.Time { return at }
			}
			settled := reconcileMigrationTimes(t, reconciler, api, run.migration, 1)
			last := settled.Status.LastRun
			if last == nil || last.Outcome != row.want || last.OperationID != claimed.ID || last.JobUID != run.job.UID ||
				!slices.Equal(last.AppliedRepeatables, appliedRepeatables) {
				t.Fatalf("last run = %#v, want %s for the Apply the rotation waited for, with repeatable migrations %v",
					last, row.want, appliedRepeatables)
			}
			if last.ExecutionBindingID != recorded.Epoch {
				t.Fatalf("the run names epoch %q, want the one that authorized it, %q", last.ExecutionBindingID, recorded.Epoch)
			}
			if dispatchedBy := last.DispatchedBy; dispatchedBy == nil ||
				dispatchedBy.ControllerImage != testControllerImage || dispatchedBy.RunnerImage != run.job.Spec.Template.Spec.InitContainers[0].Image {
				t.Fatalf("the run names dispatcher %#v, want the release that built its Job", dispatchedBy)
			}
			if settled.Status.ActiveOperation != nil || settled.Status.UnresolvedRun != nil {
				t.Fatalf("the settled Apply left claim=%#v unresolved=%#v", settled.Status.ActiveOperation, settled.Status.UnresolvedRun)
			}
			// The evidence is written under the binding that authorized the
			// run; the binding moves second.
			if binding := settled.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the pass that settled the run moved the binding: %#v", binding)
			}
			if ttl := storedTTL(t, api, run.job); ttl == nil {
				t.Fatal("the Apply's Job was not given its cleanup TTL under the binding that dispatched it")
			}

			var moved *operatorv1alpha1.PtahMigration
			for pass := 0; pass < 4; pass++ {
				moved = reconcileMigrationTimes(t, reconciler, api, run.migration, 1)
				if claim := moved.Status.ActiveOperation; claim != nil &&
					(claim.ExecutionBindingID == recorded.Epoch || moved.Status.ExecutionBinding.Epoch == recorded.Epoch) {
					t.Fatalf("pass %d after the run was read claimed %s under the binding it retires", pass, claim.Type)
				}
			}
			binding := moved.Status.ExecutionBinding
			if binding == nil || binding.Epoch == recorded.Epoch ||
				binding.ExecutorImage != next.ExecutorImage || binding.PtahVersion != next.PtahVersion {
				t.Fatalf("the binding did not move once no claim was in flight: %#v", binding)
			}
			if moved.Status.LastRun == nil || moved.Status.LastRun.Outcome != row.want || moved.Status.UnresolvedRun != nil {
				t.Fatalf("the rotation rewrote the settled run: lastRun=%#v unresolved=%#v", moved.Status.LastRun, moved.Status.UnresolvedRun)
			}
			later, pulled := 0, 0
			for _, job := range migrationJobsIn(t, api, run.migration) {
				if job.UID == run.job.UID {
					continue
				}
				later++
				if epoch := job.Annotations[workload.AnnotationExecutionBindingID]; epoch != binding.Epoch {
					t.Fatalf("Job %s runs under epoch %q after the rotation, want %q", job.Name, epoch, binding.Epoch)
				}
				if fetch := migrationPull(&job); fetch != nil {
					pulled++
					if !slices.Contains(fetch.Args, skipValidation) {
						t.Fatalf("Job %s pulls under Ptah %s with %q, want %s", job.Name, next.PtahVersion, fetch.Args, skipValidation)
					}
				}
			}
			if later == 0 || pulled == 0 {
				t.Fatalf("%d Jobs were dispatched after the rotation and %d pulled the artifact, so the row proves nothing about what runs under it",
					later, pulled)
			}
		})
	}
}

// During a Helm upgrade the old manager may still lead when the Apply ends,
// and read its result first, as its own Apply under its own binding. The new
// manager then finds no claim in flight: it moves the binding and rewrites
// nothing the old one recorded. Had the new one waited first, the old one's
// read is the same ordinary read. These rows hand leadership over between
// passes; two passes that overlap write status under an optimistic lock
// (patchMigrationStatus), so the second write of the same claim is refused
// and its pass starts again from what the first one stored.
func TestAnOldLeaderThatReadsTheApplyFirstLeavesTheRotationNothingToWait(t *testing.T) {
	t.Parallel()

	for _, newFirst := range []bool{false, true} {
		name := "the old manager reads it before the new one leads"
		if newFirst {
			name = "the new manager waited before the old one read it"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run, reconciler, api, next := migrationApplyAcrossARotation(t, runShape{recorded: true}, nil)
			recorded := run.migration.Status.ExecutionBinding.DeepCopy()
			if newFirst {
				reconcileMigrationTimes(t, reconciler, api, run.migration, 2)
			}
			previous := workloadBuilderForMigrations()
			old := *reconciler
			old.Jobs = previous
			old.Client = admittedJobWrites(api, previous)
			migrationApplyStopped(t, &old, api, run, migrationFrame(t, migrationApplyResult(run.migration,
				migrationRunReport(dataplane.MigrationOutcomeApplied, 3))))

			read := reconcileMigrationTimes(t, &old, api, run.migration, 1)
			if last := read.Status.LastRun; last == nil || last.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied ||
				last.ExecutionBindingID != recorded.Epoch {
				t.Fatalf("the old manager did not read the Apply as its own: %#v", last)
			}
			readRun := read.Status.LastRun.DeepCopy()

			moved := reconcileMigrationTimes(t, reconciler, api, run.migration, 3)
			if binding := moved.Status.ExecutionBinding; binding == nil || binding.Epoch == recorded.Epoch ||
				binding.ExecutorImage != next.ExecutorImage {
				t.Fatalf("the new manager did not move the binding after the old one read the Apply: %#v", binding)
			}
			if last := moved.Status.LastRun; last == nil || last.OperationID != readRun.OperationID ||
				last.Outcome != readRun.Outcome || last.JobUID != readRun.JobUID || moved.Status.UnresolvedRun != nil {
				t.Fatalf("the new manager rewrote the run the old one read: lastRun=%#v unresolved=%#v",
					moved.Status.LastRun, moved.Status.UnresolvedRun)
			}
		})
	}
}

// A rotation stops waiting where the Apply can no longer be read under the
// binding that authorized it, and the Apply is recorded outcome unknown as
// before: its Job is gone, or still running at the end of its claim's term,
// or what the pass reads is not what the claim dispatched under that binding.
func TestAMigrationApplyARotationCannotReadIsUnknown(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		shape runShape
		pod   func(next workload.Builder) func(*corev1.Pod)
		// end is what happens to the run before the pass that settles it.
		end  func(t *testing.T, reconciler *MigrationReconciler, api client.WithWatch, run dispatchedRun)
		says string
	}{
		{
			name: "its Job is gone",
			end: func(t *testing.T, _ *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
				for _, object := range []client.Object{run.pod, run.job} {
					if err := api.Delete(context.Background(), object); err != nil {
						t.Fatal(err)
					}
				}
			},
			says: "an execution component changed while the Apply was dispatched",
		},
		{
			name: "its Job still runs at the end of its claim's term",
			end: func(_ *testing.T, reconciler *MigrationReconciler, _ client.WithWatch, run dispatchedRun) {
				end := migrationApplyLeaseTermEnd(run.migration.Status.ActiveOperation)
				reconciler.Clock = func() time.Time { return end }
			},
			says: "an execution component changed while the Apply was dispatched",
		},
		{
			name: "another Job holds its name",
			end: func(t *testing.T, _ *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
				replaceJobUnderItsName(t, api, run.job)
			},
			says: "an execution component changed while the Apply was dispatched",
		},
		{
			name: "its Job no longer names the resource as its controller",
			end: func(t *testing.T, _ *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
				disownJob(t, api, run.job)
			},
			says: "an execution component changed while the Apply was dispatched",
		},
		{
			name: "its Job carries the Ptah version of the new binding",
			shape: runShape{alter: func(job *batchv1.Job) {
				for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
					annotations[workload.AnnotationPtahVersion] = executorRelease(workloadBuilderForMigrations()).PtahVersion
				}
			}},
			says: "dispatched Apply Job intent changed",
		},
		{
			name: "its Job runs the executor of the new binding",
			shape: runShape{alter: func(job *batchv1.Job) {
				containers := job.Spec.Template.Spec.Containers
				for index := range containers {
					if containers[index].Name == executorContainerName {
						containers[index].Image = "example.invalid/ptah@sha256:" + strings.Repeat("e", 64)
					}
				}
			}},
			says: "dispatched Apply Job intent changed",
		},
		{
			name: "its Pod ran the executor of the new binding",
			pod: func(next workload.Builder) func(*corev1.Pod) {
				return func(pod *corev1.Pod) {
					for index := range pod.Spec.Containers {
						if pod.Spec.Containers[index].Name == executorContainerName {
							pod.Spec.Containers[index].Image = next.ExecutorImage
						}
					}
				}
			},
			end: func(t *testing.T, reconciler *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
				migrationApplyStopped(t, reconciler, api, run, migrationFrame(t, migrationApplyResult(run.migration,
					migrationRunReport(dataplane.MigrationOutcomeApplied, 3))))
			},
			says: "terminal Job pod does not match immutable intent",
		},
		{
			name: "its frame is of another runner protocol",
			end: func(t *testing.T, reconciler *MigrationReconciler, api client.WithWatch, run dispatchedRun) {
				migrationApplyStopped(t, reconciler, api, run, ofAnotherProtocol(t, migrationApplyResult(run.migration,
					migrationRunReport(dataplane.MigrationOutcomeApplied, 3))))
			},
			says: "read the Apply result",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var podChange func(*corev1.Pod)
			if row.pod != nil {
				podChange = row.pod(executorRelease(workloadBuilderForMigrations()))
			}
			shape := row.shape
			shape.recorded = true
			run, reconciler, api, _ := migrationApplyAcrossARotation(t, shape, podChange)
			recorded := run.migration.Status.ExecutionBinding.DeepCopy()
			if row.end != nil {
				row.end(t, reconciler, api, run)
			}

			settled := reconcileMigrationUntilRun(t, reconciler, api, run.migration)
			last := settled.Status.LastRun
			if last == nil || last.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || !strings.Contains(last.Message, row.says) {
				t.Fatalf("last run = %#v, want outcome unknown saying %q", last, row.says)
			}
			if settled.Status.UnresolvedRun == nil || settled.Status.UnresolvedRun.OperationID != run.migration.Status.ActiveOperation.ID {
				t.Fatalf("unresolved run = %#v, want the Apply nobody accounted for", settled.Status.UnresolvedRun)
			}
			if binding := settled.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the pass that recorded the unknown run moved the binding: %#v", binding)
			}
		})
	}
}

// replaceJobUnderItsName puts another Job under the name job's claim reserved:
// the same Job as stored, created again, as a person or another controller
// would, with a UID the claim never recorded.
func replaceJobUnderItsName(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	replacement := stored.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-job-uid"
	if err := api.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil || stored.UID != replacement.UID {
		t.Fatalf("the Job under %s is %q (%v), want the replacement", job.Name, stored.UID, err)
	}
}

// disownJob drops the owner references of the Job as stored, so it no longer
// names any resource as its controller.
func disownJob(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.OwnerReferences) == 0 {
		t.Fatalf("Job %s has no owner to drop", job.Name)
	}
	stored.OwnerReferences = nil
	if err := api.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
}

// schemaApplyAcrossARotation is a schema Apply the release before next
// dispatched and is still running with its one Pod, reconciled by next
// through the controller-write webhook. Applying says what an Apply in flight
// says.
func schemaApplyAcrossARotation(
	t *testing.T,
	alter func(*batchv1.Job),
	podChange func(next workload.Builder, pod *corev1.Pod),
) (dispatchedSchemaRun, *corev1.Pod, workload.Builder) {
	t.Helper()

	previous := workloadBuilderForMigrations()
	next := executorRelease(previous)
	run := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply,
		runShape{dispatcher: previous, recorded: true, alter: alter}, next)
	watched, ok := run.api.(client.WithWatch)
	if !ok {
		t.Fatalf("the fixture's client %T cannot carry the webhook", run.api)
	}
	run.reconciler.Client = admittedJobWrites(watched, next)
	pod := terminalPodForJob(run.job)
	runningExecutorPod(pod)
	if podChange != nil {
		podChange(next, pod)
	}
	if err := run.api.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if configured, err := run.reconciler.configuredExecutionBinding(); err != nil ||
		executionBindingComponentsEqual(run.schema.Status.ExecutionBinding, configured) ||
		!rotationMayWaitFor(run.schema.Status.ExecutionBinding, configured, run.schema.Status.ActiveOperation.ExecutionBindingID) {
		t.Fatalf("the release does not move the binding in a way a rotation waits through: %#v, %v", configured, err)
	}
	if applying := meta.FindStatusCondition(run.schema.Status.Conditions, operatorv1alpha1.ConditionApplying); applying == nil ||
		applying.Status != metav1.ConditionTrue || applying.Reason != string(operatorv1alpha1.ReasonApprovedPlan) {
		t.Fatalf("Applying = %#v, want what the claim wrote", applying)
	}
	return run, pod, next
}

// schemaApplyStopped is schemaApplyAcrossARotation's Job and Pod once the
// executor exited, with frame in the Pod's log.
func schemaApplyStopped(t *testing.T, run dispatchedSchemaRun, pod *corev1.Pod, frame []byte) {
	t.Helper()

	ctx := context.Background()
	job := &batchv1.Job{}
	if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active, job.Status.Succeeded = 0, 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := run.api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Pod{}
	if err := run.api.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Phase = corev1.PodSucceeded
	stored.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := run.api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	run.reconciler.Logs = staticLogs{content: frame}
}

// schemaApplyResult is what a schema Apply's runner reports when it succeeds.
func schemaApplyResult(schema *operatorv1alpha1.PtahSchema) runner.Result {
	return runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply,
		OperationID: schema.Status.ActiveOperation.ID, MutationStarted: true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	}
}

// reconcileSchemaTimes runs passes passes and returns the schema as stored
// after the last.
func reconcileSchemaTimes(t *testing.T, run dispatchedSchemaRun, passes int) *operatorv1alpha1.PtahSchema {
	t.Helper()

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
	for pass := 0; pass < passes; pass++ {
		if _, err := run.reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
	}
	return safetyGetSchema(t, run.api, run.schema)
}

// reconcileSchemaUntilSettled runs passes until one settles the Apply claim,
// and returns the schema as that pass stored it.
func reconcileSchemaUntilSettled(t *testing.T, run dispatchedSchemaRun) *operatorv1alpha1.PtahSchema {
	t.Helper()

	for pass := 0; pass < 3; pass++ {
		if actual := reconcileSchemaTimes(t, run, 1); actual.Status.ActiveOperation == nil {
			return actual
		}
	}
	actual := safetyGetSchema(t, run.api, run.schema)
	t.Fatalf("no pass settled the Apply: phase=%q claim=%#v conditions=%#v",
		actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	return nil
}

// TestARotationWaitsForARunningMigrationApply, for a schema.
func TestARotationWaitsForARunningSchemaApply(t *testing.T) {
	t.Parallel()

	run, _, _ := schemaApplyAcrossARotation(t, nil, nil)
	recorded := run.schema.Status.ExecutionBinding.DeepCopy()
	claimed := run.schema.Status.ActiveOperation

	actual := reconcileSchemaTimes(t, run, 3)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.JobUID != run.job.UID {
		t.Fatalf("the running Apply's claim did not stay: operation=%#v pending=%#v", operation, actual.Status.PendingObservation)
	}
	if actual.Status.PendingObservation != nil {
		t.Fatalf("a running Apply was settled: %#v", actual.Status.PendingObservation)
	}
	if binding := actual.Status.ExecutionBinding; binding == nil || *binding != *recorded {
		t.Fatalf("the binding moved under a running Apply: %#v, want %#v", binding, recorded)
	}
	applying := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying)
	if applying == nil || applying.Status != metav1.ConditionTrue ||
		applying.Message != "Applying the exact current plan"+RotationWaitWords {
		t.Fatalf("Applying = %#v, want it to say the binding waits for the Apply", applying)
	}
	jobs := &batchv1.JobList{}
	if err := run.api.List(context.Background(), jobs, client.InNamespace(run.schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].UID != run.job.UID {
		t.Fatalf("Jobs while the rotation waits = %d, want only the running Apply's", len(jobs.Items))
	}
}

// TestARotationWaitsForARunningMigrationApplyUntilItsTermEnds, for a schema.
func TestARotationWaitsForARunningSchemaApplyUntilItsTermEnds(t *testing.T) {
	t.Parallel()

	termEnd := func(operation *operatorv1alpha1.ActiveOperationStatus) time.Time {
		return operation.StartedAt.Add(time.Duration(operation.LeaseDurationSeconds) * time.Second)
	}
	for _, row := range []struct {
		name string
		at   func(*operatorv1alpha1.ActiveOperationStatus) time.Time
	}{
		{
			name: "a second past its execution deadline",
			at: func(operation *operatorv1alpha1.ActiveOperationStatus) time.Time {
				return operation.ExecutionNotAfter.Add(time.Second)
			},
		},
		{
			name: "a second before its term ends",
			at: func(operation *operatorv1alpha1.ActiveOperationStatus) time.Time {
				return termEnd(operation).Add(-time.Second)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run, _, _ := schemaApplyAcrossARotation(t, nil, nil)
			recorded := run.schema.Status.ExecutionBinding.DeepCopy()
			claimed := run.schema.Status.ActiveOperation
			if claimed.ExecutionNotAfter == nil || !claimed.ExecutionNotAfter.Add(time.Second).Before(termEnd(claimed).Add(-time.Second)) {
				t.Fatalf("the claim's execution deadline %v does not fall before its term ends at %v, so the rows cannot tell the bounds apart",
					claimed.ExecutionNotAfter, termEnd(claimed))
			}
			at := row.at(claimed)
			run.reconciler.Clock = func() time.Time { return at }

			actual := reconcileSchemaTimes(t, run, 3)
			if operation := actual.Status.ActiveOperation; operation == nil || operation.ID != claimed.ID ||
				operation.JobUID != run.job.UID || actual.Status.PendingObservation != nil {
				t.Fatalf("the running Apply's claim did not hold at %v: claim=%#v pending=%#v",
					at, operation, actual.Status.PendingObservation)
			}
			if binding := actual.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the binding moved at %v under an Apply still inside its term: %#v", at, binding)
			}
		})
	}
}

// A schema Apply the rotation waited for is consumed under the binding that
// authorized it: a run that reported success owes its proof, and one that
// reported a refused statement keeps where it stopped, as any Apply does. The
// binding moves on a later pass, and the proof is still owed under the new one.
func TestASchemaApplyARotationWaitedForIsSettledByItsOwnResult(t *testing.T) {
	t.Parallel()

	refused := &dataplane.SchemaApplyFailure{Statement: 1, Statements: 1, SQLState: "23505"}
	for _, row := range []struct {
		name    string
		result  func(*operatorv1alpha1.PtahSchema) runner.Result
		outcome operatorv1alpha1.PendingObservationOutcome
		stopped *operatorv1alpha1.SchemaApplyFailureStatus
		late    bool
	}{
		{name: "the run succeeded", result: schemaApplyResult, outcome: operatorv1alpha1.PendingObservationApplySucceeded},
		{
			name: "the run succeeded and is read after its claim's term", result: schemaApplyResult,
			outcome: operatorv1alpha1.PendingObservationApplySucceeded, late: true,
		},
		{
			name: "the database refused a statement",
			result: func(schema *operatorv1alpha1.PtahSchema) runner.Result {
				result := schemaApplyResult(schema)
				result.ChildExitCode, result.Uncertain = 2, true
				result.Error = &runner.ResultError{
					Code: "apply_outcome_unknown", Message: "ptah sent the statements and returned an error, so how far they got is unknown",
				}
				result.ApplyFailure = refused
				return result
			},
			outcome: operatorv1alpha1.PendingObservationOutcomeUnknown,
			stopped: &operatorv1alpha1.SchemaApplyFailureStatus{Statement: 1, Statements: 1, SQLState: "23505"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run, pod, next := schemaApplyAcrossARotation(t, nil, nil)
			recorded := run.schema.Status.ExecutionBinding.DeepCopy()
			claimed := run.schema.Status.ActiveOperation
			reconcileSchemaTimes(t, run, 1)

			schemaApplyStopped(t, run, pod, safetyRunnerFrame(t, row.result(run.schema)))
			if row.late {
				end := claimed.StartedAt.Add(time.Duration(claimed.LeaseDurationSeconds) * time.Second)
				run.reconciler.Clock = func() time.Time { return end }
			}
			settled := reconcileSchemaUntilSettled(t, run)
			pending := settled.Status.PendingObservation
			if pending == nil || pending.Outcome != row.outcome || pending.ApplyOperationID != claimed.ID ||
				pending.ApplyJobUID != run.job.UID {
				t.Fatalf("pending observation = %#v, want the Apply the rotation waited for, %s", pending, row.outcome)
			}
			if (pending.ApplyFailure == nil) != (row.stopped == nil) ||
				row.stopped != nil && *pending.ApplyFailure != *row.stopped {
				t.Fatalf("the Apply stopped at %#v, want %#v", pending.ApplyFailure, row.stopped)
			}
			if failed := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
				strings.Contains(failed.Message, "execution binding changed") {
				t.Fatalf("ReconciliationFailed = %q, want the run's own account rather than the rotation", failed.Message)
			}
			// The evidence is written under the binding that authorized the
			// run; the binding moves second.
			if binding := settled.Status.ExecutionBinding; binding == nil || *binding != *recorded {
				t.Fatalf("the pass that settled the run moved the binding: %#v", binding)
			}

			var moved *operatorv1alpha1.PtahSchema
			for pass := 0; pass < 4; pass++ {
				moved = reconcileSchemaTimes(t, run, 1)
				if claim := moved.Status.ActiveOperation; claim != nil &&
					(claim.ExecutionBindingID == recorded.Epoch || moved.Status.ExecutionBinding.Epoch == recorded.Epoch) {
					t.Fatalf("pass %d after the run was read claimed %s under the binding it retires", pass, claim.Type)
				}
			}
			binding := moved.Status.ExecutionBinding
			if binding == nil || binding.Epoch == recorded.Epoch ||
				binding.ExecutorImage != next.ExecutorImage || binding.PtahVersion != next.PtahVersion {
				t.Fatalf("the binding did not move once no claim was in flight: %#v", binding)
			}
			owed := moved.Status.PendingObservation
			if owed == nil || owed.ApplyOperationID != claimed.ID || owed.Outcome != row.outcome {
				t.Fatalf("the rotation dropped the proof the Apply owes: %#v", owed)
			}
		})
	}
}

// TestAMigrationApplyARotationCannotReadIsUnknown, for a schema. A rotation
// that stops waiting because the Job is gone or ran past its claim's term
// records the Apply in the write that rotates the binding, as it always has.
// One whose Job, Pod or frame is not the claim's is recorded as any such Apply
// is, and the binding moves on a later pass.
func TestASchemaApplyARotationCannotReadIsUnknown(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		alter func(*batchv1.Job)
		pod   func(next workload.Builder, pod *corev1.Pod)
		end   func(t *testing.T, run dispatchedSchemaRun, pod *corev1.Pod)
		says  string
		// rotated is an ending recorded in the write that rotates the binding.
		rotated bool
	}{
		{
			name: "its Job is gone",
			end: func(t *testing.T, run dispatchedSchemaRun, pod *corev1.Pod) {
				for _, object := range []client.Object{pod, run.job} {
					if err := run.api.Delete(context.Background(), object); err != nil {
						t.Fatal(err)
					}
				}
			},
			says: "execution binding changed after Apply dispatch", rotated: true,
		},
		{
			name: "its Job still runs at the end of its claim's term",
			end: func(_ *testing.T, run dispatchedSchemaRun, _ *corev1.Pod) {
				operation := run.schema.Status.ActiveOperation
				end := operation.StartedAt.Add(time.Duration(operation.LeaseDurationSeconds) * time.Second)
				run.reconciler.Clock = func() time.Time { return end }
			},
			says: "execution binding changed after Apply dispatch", rotated: true,
		},
		{
			name: "another Job holds its name",
			end: func(t *testing.T, run dispatchedSchemaRun, _ *corev1.Pod) {
				replaceJobUnderItsName(t, run.api, run.job)
			},
			says: "execution binding changed after Apply dispatch", rotated: true,
		},
		{
			name: "its Job no longer names the resource as its controller",
			end: func(t *testing.T, run dispatchedSchemaRun, _ *corev1.Pod) {
				disownJob(t, run.api, run.job)
			},
			says: "execution binding changed after Apply dispatch", rotated: true,
		},
		{
			name: "its Job runs the executor of the new binding",
			alter: func(job *batchv1.Job) {
				containers := job.Spec.Template.Spec.Containers
				for index := range containers {
					if containers[index].Name == executorContainerName {
						containers[index].Image = "example.invalid/ptah@sha256:" + strings.Repeat("e", 64)
					}
				}
			},
			says: "dispatched Apply Job intent changed",
		},
		{
			name: "its Pod ran the executor of the new binding",
			pod: func(next workload.Builder, pod *corev1.Pod) {
				for index := range pod.Spec.Containers {
					if pod.Spec.Containers[index].Name == executorContainerName {
						pod.Spec.Containers[index].Image = next.ExecutorImage
					}
				}
			},
			end: func(t *testing.T, run dispatchedSchemaRun, pod *corev1.Pod) {
				schemaApplyStopped(t, run, pod, safetyRunnerFrame(t, schemaApplyResult(run.schema)))
			},
			says: "terminal Job pod does not match immutable intent",
		},
		{
			name: "its frame is of another runner protocol",
			end: func(t *testing.T, run dispatchedSchemaRun, pod *corev1.Pod) {
				schemaApplyStopped(t, run, pod, ofAnotherProtocol(t, schemaApplyResult(run.schema)))
			},
			says: "apply result is uncertain",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run, pod, next := schemaApplyAcrossARotation(t, row.alter, row.pod)
			recorded := run.schema.Status.ExecutionBinding.DeepCopy()
			claimed := run.schema.Status.ActiveOperation
			if row.end != nil {
				row.end(t, run, pod)
			}

			settled := reconcileSchemaUntilSettled(t, run)
			if failed := settledSchemaFailure(t, run.api, run.schema); !strings.Contains(failed.Message, row.says) {
				t.Fatalf("ReconciliationFailed = %q, want it to say %q", failed.Message, row.says)
			}
			if pending := settled.Status.PendingObservation; pending.ApplyOperationID != claimed.ID {
				t.Fatalf("pending observation = %#v, want the Apply of the recorded binding", pending)
			}
			binding := settled.Status.ExecutionBinding
			if moved := binding != nil && binding.Epoch != recorded.Epoch; moved != row.rotated {
				t.Fatalf("the pass that recorded the unknown Apply moved the binding: %t, want %t (%#v)", moved, row.rotated, binding)
			}
			if row.rotated && (binding.ExecutorImage != next.ExecutorImage || binding.PtahVersion != next.PtahVersion) {
				t.Fatalf("the binding moved to %#v, want the configured one", binding)
			}
		})
	}
}

// Suspending a resource while its Apply runs is a spec edit, and the generation
// it bumps is one of the claim's inputs, so the Apply is recorded Unknown
// whatever the rotation does. Suspension still never discards a dispatched
// Apply, so across a rotation the claim holds the binding while its Job runs
// and nothing else is dispatched. Once the Job stops, the run is recorded
// Unknown because its inputs changed, under the binding that authorized it,
// and the binding moves on a later pass. The fake client keeps
// metadata.generation as written, so each row bumps it as the API server does
// on a spec write.
func TestSuspendingAResourceARotationWaitsForRecordsTheApplyUnknown(t *testing.T) {
	t.Parallel()

	t.Run("migration", func(t *testing.T) {
		t.Parallel()

		run, reconciler, api, next := migrationApplyAcrossARotation(t, runShape{recorded: true}, nil)
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(run.migration), run.migration); err != nil {
			t.Fatal(err)
		}
		run.migration.Spec.Suspend = true
		run.migration.Generation++
		if err := api.Update(context.Background(), run.migration); err != nil {
			t.Fatal(err)
		}
		recorded := run.migration.Status.ExecutionBinding.DeepCopy()
		claimed := run.migration.Status.ActiveOperation

		waiting := reconcileMigrationTimes(t, reconciler, api, run.migration, 3)
		if operation := waiting.Status.ActiveOperation; operation == nil || operation.ID != claimed.ID ||
			operation.JobUID != run.job.UID || waiting.Status.LastRun != nil || waiting.Status.UnresolvedRun != nil {
			t.Fatalf("the suspended resource let go of its running Apply: claim=%#v lastRun=%#v unresolved=%#v",
				operation, waiting.Status.LastRun, waiting.Status.UnresolvedRun)
		}
		if binding := waiting.Status.ExecutionBinding; binding == nil || *binding != *recorded {
			t.Fatalf("the binding moved under a running Apply of a suspended resource: %#v", binding)
		}

		migrationApplyStopped(t, reconciler, api, run,
			migrationFrame(t, migrationApplyResult(run.migration, migrationRunReport(dataplane.MigrationOutcomeApplied, 3))))
		settled := reconcileMigrationUntilRun(t, reconciler, api, run.migration)
		last := settled.Status.LastRun
		if last == nil || last.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || last.OperationID != claimed.ID ||
			last.ExecutionBindingID != recorded.Epoch ||
			!strings.HasPrefix(last.Message, "the operation inputs changed while the Job was running") {
			t.Fatalf("last run = %#v, want the suspended resource's Apply recorded unknown for its changed inputs under epoch %q",
				last, recorded.Epoch)
		}
		if unresolved := settled.Status.UnresolvedRun; unresolved == nil || unresolved.OperationID != claimed.ID {
			t.Fatalf("unresolved run = %#v, want the Apply nobody established", unresolved)
		}
		if binding := settled.Status.ExecutionBinding; binding == nil || *binding != *recorded {
			t.Fatalf("the pass that recorded the run moved the binding: %#v", binding)
		}

		moved := reconcileMigrationTimes(t, reconciler, api, run.migration, 3)
		if binding := moved.Status.ExecutionBinding; binding == nil || binding.Epoch == recorded.Epoch ||
			binding.ExecutorImage != next.ExecutorImage {
			t.Fatalf("the binding did not move once no claim was in flight: %#v", binding)
		}
		if moved.Status.ActiveOperation != nil {
			t.Fatalf("a suspended resource claimed %s after the rotation", moved.Status.ActiveOperation.Type)
		}
		if last := moved.Status.LastRun; last == nil || last.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown ||
			last.ExecutionBindingID != recorded.Epoch || moved.Status.UnresolvedRun == nil {
			t.Fatalf("the rotation rewrote the recorded run: lastRun=%#v unresolved=%#v", last, moved.Status.UnresolvedRun)
		}
		if jobs := migrationJobsIn(t, api, run.migration); len(jobs) != 1 || jobs[0].UID != run.job.UID {
			t.Fatalf("Jobs of the suspended resource = %d, want only the Apply's", len(jobs))
		}
	})

	t.Run("schema", func(t *testing.T) {
		t.Parallel()

		run, pod, next := schemaApplyAcrossARotation(t, nil, nil)
		stored := safetyGetSchema(t, run.api, run.schema)
		stored.Spec.Suspend = true
		stored.Generation++
		if err := run.api.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		recorded := run.schema.Status.ExecutionBinding.DeepCopy()
		claimed := run.schema.Status.ActiveOperation

		waiting := reconcileSchemaTimes(t, run, 3)
		if operation := waiting.Status.ActiveOperation; operation == nil || operation.ID != claimed.ID ||
			operation.JobUID != run.job.UID || waiting.Status.PendingObservation != nil {
			t.Fatalf("the suspended resource let go of its running Apply: claim=%#v pending=%#v",
				operation, waiting.Status.PendingObservation)
		}
		if binding := waiting.Status.ExecutionBinding; binding == nil || *binding != *recorded {
			t.Fatalf("the binding moved under a running Apply of a suspended resource: %#v", binding)
		}

		schemaApplyStopped(t, run, pod, safetyRunnerFrame(t, schemaApplyResult(run.schema)))
		settled := reconcileSchemaUntilSettled(t, run)
		pending := settled.Status.PendingObservation
		if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
			pending.ApplyOperationID != claimed.ID || pending.Plan.ExecutionBindingID != recorded.Epoch {
			t.Fatalf("pending observation = %#v, want the suspended resource's Apply recorded unknown under epoch %q",
				pending, recorded.Epoch)
		}
		if binding := settled.Status.ExecutionBinding; binding == nil || *binding != *recorded {
			t.Fatalf("the pass that recorded the run moved the binding: %#v", binding)
		}

		moved := reconcileSchemaTimes(t, run, 3)
		if binding := moved.Status.ExecutionBinding; binding == nil || binding.Epoch == recorded.Epoch ||
			binding.ExecutorImage != next.ExecutorImage {
			t.Fatalf("the binding did not move once no claim was in flight: %#v", binding)
		}
		if moved.Status.ActiveOperation != nil {
			t.Fatalf("a suspended resource claimed %s after the rotation", moved.Status.ActiveOperation.Type)
		}
		if owed := moved.Status.PendingObservation; owed == nil || owed.ApplyOperationID != claimed.ID ||
			owed.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
			t.Fatalf("the rotation dropped or rewrote the reading the Apply owes: %#v", owed)
		}
	})
}
