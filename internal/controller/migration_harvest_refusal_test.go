package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The harvest of a finished migration Apply asks for the Job's cleanup TTL before
// the pass records the run the Job's Pod reported. A refused TTL write used to be
// the pass's error, so a policy that refused the TTL left the migration in
// Applying with its Lease held, and the run it had made was never recorded. A
// refusal is now settled as everywhere else a claim asks for a TTL
// (settleHarvestedJobCleanup), while the stored claim still names the Job.
//
// The Job comes from the real builder, and every Job write passes the real
// controller-write webhook.

// migrationApplyFinished stands up an Apply the claim dispatched and recorded,
// whose Pod reported that it applied the plan and stopped, with the Lease the
// claim holds.
func migrationApplyFinished(t *testing.T) (
	*operatorv1alpha1.PtahMigration, *batchv1.Job, *MigrationReconciler, client.WithWatch, *record.FakeRecorder,
) {
	t.Helper()
	return migrationApplyEnded(t, runner.Result{
		ChildExitCode: 0,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomeApplied,
			Planned:         []int64{3},
			Applied:         []int64{3},
		},
	})
}

// migrationApplyEnded is migrationApplyFinished for a Pod that reported result,
// which the fixture completes with the identity of the claim and its target.
func migrationApplyEnded(t *testing.T, result runner.Result) (
	*operatorv1alpha1.PtahMigration, *batchv1.Job, *MigrationReconciler, client.WithWatch, *record.FakeRecorder,
) {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	builder := workloadBuilderForMigrations()
	job, pod := builtTerminalMigrationWorkload(t, builder, migration, plan)
	result.ProtocolVersion = runner.ProtocolVersion
	result.Operation = runner.OperationMigrationApply
	result.OperationID = operation.ID
	result.CoordinationDigest = operation.CoordinationDigest
	result.TargetIdentityDigest = migration.Status.History.TargetIdentityDigest
	frame := migrationFrame(t, result)
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	reconciler.Jobs = builder
	reconciler.Client = admittedJobWrites(api, builder)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	holdMigrationApplyLease(t, reconciler, api, migration)
	return migration, job, reconciler, api, recorder
}

// TestAMigrationApplyHarvestSettlesTheAnswerToItsCleanup is the harvest of a
// finished Apply, whose run the pass records once the Job's TTL is asked for. A
// refusal, Forbidden from the controller-write webhook or Invalid from a
// ValidatingAdmissionPolicy, says admission will not collect the Job however
// often it is asked, so the pass records the run, and a Warning names the Job. A
// Job deleted since the pass read it needs no TTL. Any other failure may pass, so
// the pass fails with the claim kept, and the next one asks again.
func TestAMigrationApplyHarvestSettlesTheAnswerToItsCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		answer error
		// settled is whether the pass the write failed in recorded the run.
		settled bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name:    "refused",
			answer:  apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal")),
			settled: true, stays: "injected refusal",
		},
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true, stays: "injected policy refusal",
		},
		{name: "deleted since it was read", answer: apierrors.NewNotFound(batchv1.Resource("jobs"), ""), settled: true},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			migration, job, reconciler, api, recorder := migrationApplyFinished(t)
			reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), row.answer)

			_, err := reconciler.Reconcile(ctx, migrationRequest(migration))
			stored := readMigration(t, api, migration)
			if row.settled {
				if err != nil {
					t.Fatalf("Reconcile(): %v, want the answer not to fail the pass", err)
				}
				assertMigrationApplyRecorded(t, stored, job)
				if got := storedTTL(t, api, job); got != nil {
					t.Fatalf("the Job's TTL = %s, want none after the write was answered %v", ttlText(got), row.answer)
				}
				events := migrationEvents(recorder)
				if row.stays == "" {
					if cleanupDeferredFor(events, job.Name, "") {
						t.Fatalf("a Job that is gone is named as staying: %q", events)
					}
					return
				}
				if !cleanupDeferredFor(events, job.Name, row.stays) {
					t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, job.Name, row.stays)
				}
				return
			}
			if !errors.Is(err, row.answer) {
				t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
			}
			if !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, migration.Status.ActiveOperation) ||
				stored.Status.LastRun != nil {
				t.Fatalf("a lost TTL write let the pass act: operation=%#v lastRun=%#v",
					stored.Status.ActiveOperation, stored.Status.LastRun)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() after the lost write: %v", err)
			}
			if got := storedTTL(t, api, job); got == nil || *got != jobclaim.CleanupTTLSeconds {
				t.Fatalf("the Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
			}
			assertMigrationApplyRecorded(t, readMigration(t, api, migration), job)
			if events := migrationEvents(recorder); cleanupDeferredFor(events, job.Name, "") {
				t.Fatalf("a lost TTL write named the Job as staying: %q", events)
			}
		})
	}
}

// TestAMigrationApplyHarvestRefusedAfterItsClaimMovedFailsThePass is the harvest
// of a finished Apply while a second manager, holding the same stale view, has
// already recorded the run and claimed the read that follows it: the
// controller-write webhook reads the claim as stored and refuses the TTL of a Job
// the claim no longer names. The refusal says nothing settled here, so the pass
// fails rather than record the run again, and no Warning names the Job.
func TestAMigrationApplyHarvestRefusedAfterItsClaimMovedFailsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	migration, job, reconciler, api, recorder := migrationApplyFinished(t)
	next := &operatorv1alpha1.MigrationOperationStatus{
		Type: operatorv1alpha1.MigrationOperationHistory, ID: "history-claimed-by-the-other-manager",
		JobName: "history-claimed-by-the-other-manager", Attempt: 1,
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
	}
	var movedTo string
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob && movedTo == "" {
				stored := readMigration(t, api, migration)
				stored.Status.ActiveOperation = next.DeepCopy()
				if err := api.Status().Update(ctx, stored); err != nil {
					t.Fatalf("move the stored claim: %v", err)
				}
				movedTo = stored.ResourceVersion
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})

	_, err := reconciler.Reconcile(ctx, migrationRequest(migration))
	if movedTo == "" {
		t.Fatal("the pass asked for no TTL, so nothing moved the claim")
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "Job is not the exact active migration operation instance") {
		t.Fatalf("Reconcile() error = %v, want the webhook's refusal of a Job the stored claim no longer names", err)
	}
	stored := readMigration(t, api, migration)
	if stored.ResourceVersion != movedTo || !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, next) ||
		stored.Status.LastRun != nil {
		t.Fatalf("the pass wrote over the claim that moved: resourceVersion=%s (moved at %s) operation=%#v lastRun=%#v",
			stored.ResourceVersion, movedTo, stored.Status.ActiveOperation, stored.Status.LastRun)
	}
	if got := storedTTL(t, api, job); got != nil {
		t.Fatalf("the Job's TTL = %s, want none", ttlText(got))
	}
	if events := migrationEvents(recorder); cleanupDeferredFor(events, job.Name, "") {
		t.Fatalf("a refusal for a claim that moved named the Job as staying: %q", events)
	}
}

// TestAMigrationRetryHarvestSettlesARefusedCleanup is the harvest of a History
// read whose Job failed at its deadline without a result, so the claim retries
// under a new Job. A refusal of the old Job's TTL lets the claim move to its next
// attempt, and a Warning names the Job left behind. A lost write fails the pass
// with the claim kept.
func TestAMigrationRetryHarvestSettlesARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		answer  error
		settled bool
	}{
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true,
		},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true, createdWithoutTTL: true})
			reconciler, api := run.reconciler(t, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			finishMigrationJob(t, api, run, batchv1.JobFailed)
			reconciler.Client = refusingJobTTL(admittedJobWrites(api, current), row.answer)

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			claimed := run.migration.Status.ActiveOperation
			operation := readMigration(t, api, run.migration).Status.ActiveOperation
			events := migrationEvents(recorder)
			if !row.settled {
				if !errors.Is(err, row.answer) {
					t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
				}
				if !equality.Semantic.DeepEqual(operation, claimed) {
					t.Fatalf("a lost TTL write let the claim retry: operation=%#v", operation)
				}
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a lost TTL write named the Job as staying: %q", events)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(): %v, want the refusal not to fail the pass", err)
			}
			if operation == nil || operation.Attempt != claimed.Attempt+1 || operation.JobName == run.job.Name {
				t.Fatalf("operation = %#v, want the claim retried at attempt %d under a new Job", operation, claimed.Attempt+1)
			}
			if !cleanupDeferredFor(events, run.job.Name, "injected policy refusal") {
				t.Fatalf("Events = %q, want a Warning that Job %q stays", events, run.job.Name)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
		})
	}
}

// TestAMigrationStaleHarvestSettlesARefusedCleanup is the harvest of a finished
// History read whose resource was edited while the Job ran, so the claim is
// discarded as stale without its result. A refusal of the Job's TTL lets the
// claim go, and a Warning names the Job left behind. A lost write fails the pass
// with the claim kept.
func TestAMigrationStaleHarvestSettlesARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		answer  error
		settled bool
	}{
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true,
		},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true, createdWithoutTTL: true})
			reconciler, api := run.reconciler(t, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			finishMigrationJob(t, api, run, batchv1.JobComplete)
			edited := readMigration(t, api, run.migration)
			edited.Generation++
			if err := api.Update(ctx, edited); err != nil {
				t.Fatal(err)
			}
			reconciler.Client = refusingJobTTL(admittedJobWrites(api, current), row.answer)

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			claimed := run.migration.Status.ActiveOperation
			operation := readMigration(t, api, run.migration).Status.ActiveOperation
			events := migrationEvents(recorder)
			if !row.settled {
				if !errors.Is(err, row.answer) {
					t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
				}
				if !equality.Semantic.DeepEqual(operation, claimed) {
					t.Fatalf("a lost TTL write let the stale claim go: operation=%#v", operation)
				}
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a lost TTL write named the Job as staying: %q", events)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(): %v, want the refusal not to fail the pass", err)
			}
			if operation != nil {
				t.Fatalf("the stale claim was not discarded: operation=%#v", operation)
			}
			if !cleanupDeferredFor(events, run.job.Name, "injected policy refusal") {
				t.Fatalf("Events = %q, want a Warning that Job %q stays", events, run.job.Name)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
		})
	}
}

// TestAMigrationReadHarvestSettlesARefusedCleanup is the harvest of a finished
// History read whose Pod reported the database's revision table, which the pass
// consumes once the Job's TTL is asked for. A refusal of that TTL lets the pass
// record the history and retire the claim, and a Warning names the Job. A lost
// write fails the pass with the claim kept and nothing recorded.
func TestAMigrationReadHarvestSettlesARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		answer  error
		settled bool
	}{
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true,
		},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true, createdWithoutTTL: true})
			reconciler, api := run.reconciler(t, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			finishMigrationJob(t, api, run, batchv1.JobComplete)
			operation := run.migration.Status.ActiveOperation
			report := pendingMigrationHistory()
			reconciler.Logs = staticLogs{content: migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})}
			reconciler.Client = refusingJobTTL(admittedJobWrites(api, current), row.answer)

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			stored := readMigration(t, api, run.migration)
			events := migrationEvents(recorder)
			if !row.settled {
				if !errors.Is(err, row.answer) {
					t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
				}
				if !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, operation) || stored.Status.History != nil {
					t.Fatalf("a lost TTL write let the pass act: operation=%#v history=%#v",
						stored.Status.ActiveOperation, stored.Status.History)
				}
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a lost TTL write named the Job as staying: %q", events)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(): %v, want the refusal not to fail the pass", err)
			}
			if stored.Status.History == nil || stored.Status.History.CurrentVersion != report.CurrentVersion ||
				(stored.Status.ActiveOperation != nil && stored.Status.ActiveOperation.ID == operation.ID) {
				t.Fatalf("the read's result was not consumed: phase=%q operation=%#v history=%#v",
					stored.Status.Phase, stored.Status.ActiveOperation, stored.Status.History)
			}
			if !cleanupDeferredFor(events, run.job.Name, "injected policy refusal") {
				t.Fatalf("Events = %q, want a Warning that Job %q stays", events, run.job.Name)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
		})
	}
}

// assertMigrationApplyRecorded fails unless the Apply's run was recorded as
// applied, naming its Job, and its claim retired.
func assertMigrationApplyRecorded(t *testing.T, stored *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
	t.Helper()
	if stored.Status.LastRun == nil || stored.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied ||
		stored.Status.LastRun.JobUID != job.UID {
		t.Fatalf("the finished Apply was not recorded: phase=%q lastRun=%#v", stored.Status.Phase, stored.Status.LastRun)
	}
	if stored.Status.ActiveOperation != nil {
		t.Fatalf("the finished Apply's claim was retained: %#v", stored.Status.ActiveOperation)
	}
}
