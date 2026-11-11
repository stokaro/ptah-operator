package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A read-only migration claim that lets go of the Job under its reserved name,
// because the resource was suspended or an execution component changed, used to
// leave that Job with no cleanup TTL: admission admits the TTL only while a claim
// names the Job by UID, so once the claim was gone nothing could schedule it. The
// claim now settles what it owes the Job first, by the rule a claim that refused
// its Job keeps (letGoOfReadOnlyMigrationJob).
//
// Every Job here comes from the real builder, and every Job write passes the real
// controller-write webhook.

// retiredJobWait is what Progressing says while a claim an execution-binding
// change retires waits for its Job to stop.
func retiredJobWait(run dispatchedRun) string {
	return "Waiting for Job " + run.job.Name +
		" to stop before the History operation is retired: an execution component changed"
}

// suspendMigration sets spec.suspend on the stored resource.
func suspendMigration(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()
	migration := readMigration(t, api, run.migration)
	migration.Spec.Suspend = true
	if err := api.Update(context.Background(), migration); err != nil {
		t.Fatal(err)
	}
}

// assertHeldOverItsJob fails unless the claim is the one the run dispatched,
// naming its Job by UID whether or not it had recorded it, and the Job has no TTL
// yet.
func assertHeldOverItsJob(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()
	kept := run.migration.Status.ActiveOperation.DeepCopy()
	kept.JobUID = run.job.UID
	if held := readMigration(t, api, run.migration).Status.ActiveOperation; !equality.Semantic.DeepEqual(held, kept) {
		t.Fatalf("the claim did not stay over its running Job, naming it by UID %q: operation=%#v", run.job.UID, held)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the running Job's TTL = %d, want none while it runs", *got)
	}
}

// notWaited is what the Warning says of a running Job a suspended claim leaves.
const notWaited = "a suspended claim does not wait for its Job to stop, " +
	"and admission admits a cleanup only on a Job that has finished"

// TestASuspendedReadOnlyClaimNamesTheRunningJobItLeaves is a read-only claim over
// a running Job admission would collect once it stopped, when the resource is
// suspended: one the claim supervises, one whose UID it never recorded, and one it
// was already waiting for after refusing it. Suspension is the stop button, so
// the claim is discarded in that pass without waiting for the Job. Admission
// admits a TTL only on a finished Job a claim names, so nothing can collect the
// Job once the claim is gone: a Warning names it, nothing is asked of it, and the
// claim records no UID it would never use.
func TestASuspendedReadOnlyClaimNamesTheRunningJobItLeaves(t *testing.T) {
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

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, row.shape)
			reconciler, api := run.reconciler(t, current)
			admitted := admittedJobWrites(api, current)
			reconciler.Client = admitted
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			if row.refused {
				if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() before the suspension: %v", err)
				}
				assertWaitSaidOnce(t, reconciler, api, run, refusedJobWait(run))
			}

			suspendMigration(t, api, run)
			var recordings, jobPatches int
			reconciler.Client = countingMigrationWrites(admitted, &recordings, &jobPatches)
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() after the suspension: %v", err)
			}
			settled := readMigration(t, api, run.migration)
			if settled.Status.ActiveOperation != nil {
				t.Fatalf("the suspended claim waited for its running Job: %#v", settled.Status.ActiveOperation)
			}
			if settled.Status.LastRun != nil {
				t.Fatalf("a suspended read-only claim settled a run: %#v", settled.Status.LastRun)
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
			if events := migrationEvents(recorder); !cleanupDeferredFor(events, run.job.Name, notWaited) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, notWaited)
			}
		})
	}
}

// TestASuspendedReadOnlyClaimGoesAtOnceWhereItOwesItsJobNothingItCanGive is a
// read-only claim over a Job that is not running when the resource is suspended:
// the Job has finished, already has its TTL, is not the claim's, or is one
// admission would refuse a TTL. The claim is discarded in that pass. A finished
// Job gets its TTL first, the claim recording its UID where it never did, a Job
// admission would refuse is named by a Warning, and nothing is asked of a Job
// that is not the claim's.
func TestASuspendedReadOnlyClaimGoesAtOnceWhereItOwesItsJobNothingItCanGive(t *testing.T) {
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

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, row.shape)
			if row.collected {
				run.job.Spec.TTLSecondsAfterFinished = ptr(jobclaim.CleanupTTLSeconds)
			}
			if row.replaced {
				run.migration.Status.ActiveOperation.JobUID = types.UID("the-recorded-job-uid")
			}
			run.migration.Spec.Suspend = true
			reconciler, api := run.reconciler(t, current)
			if row.finished {
				finishMigrationJob(t, api, run, batchv1.JobComplete)
			}
			var recordings, jobPatches int
			reconciler.Client = countingMigrationWrites(admittedJobWrites(api, current), &recordings, &jobPatches)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			if operation := readMigration(t, api, run.migration).Status.ActiveOperation; operation != nil {
				t.Fatalf("the suspended claim stayed: %#v", operation)
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
				if cleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a Job the claim owes nothing more is named as staying: %q", events)
				}
				return
			}
			if !cleanupDeferredFor(events, run.job.Name, row.stays) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
			}
		})
	}
}

// TestASuspendedReadOnlyClaimKeepsAFinishedJobWhoseTTLWriteIsLost is a suspended
// resource whose read-only claim asks for the finished Job's TTL, and the write
// fails with an answer that may pass. Discarding the claim then would leave the
// Job with no TTL for good, since no claim would name it again, so the pass fails
// with the claim kept, and the next pass asks again and lets go.
func TestASuspendedReadOnlyClaimKeepsAFinishedJobWhoseTTLWriteIsLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
	run.migration.Spec.Suspend = true
	reconciler, api := run.reconciler(t, current)
	finishMigrationJob(t, api, run, batchv1.JobComplete)
	lost := errors.New("connection reset by peer")
	reconciler.Client = refusingJobTTL(admittedJobWrites(api, current), lost)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); !errors.Is(err, lost) {
		t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
	}
	if kept := readMigration(t, api, run.migration).Status.ActiveOperation; !equality.Semantic.DeepEqual(
		kept, run.migration.Status.ActiveOperation) {
		t.Fatalf("a lost TTL write let the suspended claim go: operation=%#v", kept)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() after the lost write: %v", err)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the finished Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
	}
	if operation := readMigration(t, api, run.migration).Status.ActiveOperation; operation != nil {
		t.Fatalf("the suspended claim stayed once its Job had its TTL: %#v", operation)
	}
	if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
		t.Fatalf("a lost TTL write named the Job as staying: %q", events)
	}
}

// TestARotationCollectsTheReadOnlyJobItsClaimLeaves is a read-only claim over a
// running Job when an execution component changes. The rotation waits, under the
// binding status still records, for the Job to stop, and says why in Progressing;
// the pass that sees it stopped gives it its TTL and installs the new binding with
// the claim retired. A Job admission would refuse a TTL does not hold the
// rotation, and a Warning names it. Neither does the Job of a suspended resource:
// the rotation lets go of it at once, as the suspension would. A read of the Job
// that fails fails the pass, with the claim and the binding where they were.
func TestARotationCollectsTheReadOnlyJobItsClaimLeaves(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	next := current
	next.ExecutorImage = "example.invalid/ptah@sha256:" + strings.Repeat("9", 64)

	t.Run("a Job admission would collect", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
		reconciler, api := run.reconciler(t, next)
		reconciler.Client = admittedJobWrites(api, next)
		recorder := record.NewFakeRecorder(64)
		reconciler.Recorder = recorder

		result, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
		if err != nil {
			t.Fatalf("Reconcile(): %v", err)
		}
		if result.RequeueAfter <= 0 {
			t.Fatalf("the rotation waiting for the Job asked for no later pass: %#v", result)
		}
		assertHeldOverItsJob(t, api, run)
		if binding := readMigration(t, api, run.migration).Status.ExecutionBinding; binding.ExecutorImage != current.ExecutorImage {
			t.Fatalf("the binding moved to %q while the claim's Job ran, want it held at %q",
				binding.ExecutorImage, current.ExecutorImage)
		}
		assertWaitSaidOnce(t, reconciler, api, run, retiredJobWait(run))

		finishMigrationJob(t, api, run, batchv1.JobComplete)
		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() after the Job stopped: %v", err)
		}
		if got := storedTTL(t, api, run.job); got == nil || *got != 300 {
			t.Fatalf("the stopped Job's TTL = %s, want the 300 seconds a harvested read-only Job gets", ttlText(got))
		}
		rotated := readMigration(t, api, run.migration)
		if rotated.Status.ActiveOperation != nil {
			t.Fatalf("the retired claim outlived the rotation: %#v", rotated.Status.ActiveOperation)
		}
		if rotated.Status.ExecutionBinding.ExecutorImage != next.ExecutorImage {
			t.Fatalf("the binding is %q after the Job stopped, want %q",
				rotated.Status.ExecutionBinding.ExecutorImage, next.ExecutorImage)
		}
		if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
			t.Fatalf("the Job that got its TTL is named as staying: %q", events)
		}
	})

	t.Run("a Job admission would refuse", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory,
			runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true})
		reconciler, api := run.reconciler(t, next)
		reconciler.Client = admittedJobWrites(api, next)
		recorder := record.NewFakeRecorder(64)
		reconciler.Recorder = recorder

		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile(): %v", err)
		}
		rotated := readMigration(t, api, run.migration)
		if rotated.Status.ActiveOperation != nil || rotated.Status.ExecutionBinding.ExecutorImage != next.ExecutorImage {
			t.Fatalf("the rotation waited for a Job admission would not collect: claim=%#v binding=%q",
				rotated.Status.ActiveOperation, rotated.Status.ExecutionBinding.ExecutorImage)
		}
		const stays = "the Job runs under another execution epoch than its claim"
		if events := migrationEvents(recorder); !cleanupDeferredFor(events, run.job.Name, stays) {
			t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, stays)
		}
	})

	// The rotation runs before the pass that discards a suspended claim, so a
	// rotation that went on waiting would hold the suspension until the Job
	// stopped, and kubectl ptah suspend --wait would time out.
	t.Run("a suspension while the rotation waits", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
		reconciler, api := run.reconciler(t, next)
		admitted := admittedJobWrites(api, next)
		reconciler.Client = admitted
		recorder := record.NewFakeRecorder(64)
		reconciler.Recorder = recorder

		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() before the suspension: %v", err)
		}
		assertWaitSaidOnce(t, reconciler, api, run, retiredJobWait(run))

		suspendMigration(t, api, run)
		var recordings, jobPatches int
		reconciler.Client = countingMigrationWrites(admitted, &recordings, &jobPatches)
		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() after the suspension: %v", err)
		}
		rotated := readMigration(t, api, run.migration)
		if rotated.Status.ActiveOperation != nil || rotated.Status.ExecutionBinding.ExecutorImage != next.ExecutorImage {
			t.Fatalf("the rotation went on waiting for the running Job of a suspended resource: claim=%#v binding=%q",
				rotated.Status.ActiveOperation, rotated.Status.ExecutionBinding.ExecutorImage)
		}
		if jobPatches != 0 {
			t.Fatalf("%d writes reached the running Job, want none", jobPatches)
		}
		if got := storedTTL(t, api, run.job); got != nil {
			t.Fatalf("the running Job's TTL = %d, want none", *got)
		}
		if events := migrationEvents(recorder); !cleanupDeferredFor(events, run.job.Name, notWaited) {
			t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, notWaited)
		}
	})

	t.Run("a read of the Job that fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
		reconciler, api := run.reconciler(t, next)
		reconciler.Client = admittedJobWrites(api, next)
		lost := errors.New("connection reset by peer")
		reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
			Get: func(
				ctx context.Context,
				reader client.WithWatch,
				key client.ObjectKey,
				object client.Object,
				options ...client.GetOption,
			) error {
				if _, isJob := object.(*batchv1.Job); isJob && key.Name == run.job.Name {
					return lost
				}
				return reader.Get(ctx, key, object, options...)
			},
		})

		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); !errors.Is(err, lost) {
			t.Fatalf("Reconcile() error = %v, want the failed read of the Job, so the next pass reads it again", err)
		}
		kept := readMigration(t, api, run.migration)
		if !equality.Semantic.DeepEqual(kept.Status.ActiveOperation, run.migration.Status.ActiveOperation) ||
			kept.Status.ExecutionBinding.ExecutorImage != current.ExecutorImage {
			t.Fatalf("a failed read of the Job retired its claim: claim=%#v binding=%q",
				kept.Status.ActiveOperation, kept.Status.ExecutionBinding.ExecutorImage)
		}
	})
}

// TestDeletingAMigrationRecordsAnApplyJobItNeverRecordedBeforeItsCleanup is a
// deleted PtahMigration whose Apply claim never recorded its Job's UID, over a Job
// that has stopped. The claim records the UID before the run is settled, as every
// other pass that settles an Apply nobody read does, so admission admits the Job's
// cleanup TTL and nothing says the Job stays.
func TestDeletingAMigrationRecordsAnApplyJobItNeverRecordedBeforeItsCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: current})
	reconciler, api := run.reconciler(t, current)
	reconciler.Client = admittedJobWrites(api, current)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	stopMigrationWorkload(t, api, run, "")
	if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	settled := readMigration(t, api, run.migration)
	if settled.Status.ActiveOperation != nil || settled.Status.LastRun == nil ||
		settled.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown ||
		settled.Status.LastRun.JobUID != run.job.UID {
		t.Fatalf("the deleted Apply did not go unknown naming Job %q: claim=%#v lastRun=%#v",
			run.job.UID, settled.Status.ActiveOperation, settled.Status.LastRun)
	}
	if got := storedTTL(t, api, run.job); got == nil {
		t.Fatal("the stopped Job of the deleted Apply has no TTL, want the one its run keeps its log by")
	}
	if named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name); len(named) != 0 {
		t.Fatalf("the Job that got its TTL is named as staying: %q", named)
	}
}

// TestAnApplyJobGoneBeforeItsTTLIsNotNamedAsStaying is the TTL write of a stopped
// Apply's Job answered NotFound in the pass that records the run unknown: the Job
// was deleted between the read and the patch. Nothing stays, so no Warning says
// it does, and the run is recorded as it would be with the TTL set.
func TestAnApplyJobGoneBeforeItsTTLIsNotNamedAsStaying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	way := jobRebuiltDifferently()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
	reconciler, api := run.reconciler(t, way.next)
	stopMigrationWorkload(t, api, run, "")
	gone := 0
	reconciler.Client = interceptor.NewClient(admittedJobWrites(api, way.next), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob {
				gone++
				return apierrors.NewNotFound(batchv1.SchemeGroupVersion.WithResource("jobs").GroupResource(), object.GetName())
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if gone != 1 {
		t.Fatalf("%d TTL writes reached the Job, want the one answered NotFound", gone)
	}
	settled := readMigration(t, api, run.migration)
	if settled.Status.ActiveOperation != nil || settled.Status.LastRun == nil ||
		!strings.Contains(settled.Status.LastRun.Message, way.says) {
		t.Fatalf("the stopped Apply did not go unknown with its claim let go: claim=%#v lastRun=%#v",
			settled.Status.ActiveOperation, settled.Status.LastRun)
	}
	if named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name); len(named) != 0 {
		t.Fatalf("a Job deleted before its TTL is named as staying: %q", named)
	}
}
