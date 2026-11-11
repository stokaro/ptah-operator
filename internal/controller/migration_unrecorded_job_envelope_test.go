package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A claim that never recorded its Job's UID records it before its run is settled,
// so admission admits the Job's cleanup TTL. It records only a Job that is the one
// the claim fixes without a rebuild (validateMigrationJobEnvelope). Owning the Job is
// not enough: the controller-write webhook holds only the manager's own creates to
// the claim, and recording the UID of a Job the claim refused would make that Job
// the claim's after all.

// outsideItsClaim is the same way in, over a Job that alter puts outside what the
// claim fixes, on top of whatever the way already changed about it.
func (way unknownWhileRunning) outsideItsClaim(alter func(*batchv1.Job)) unknownWhileRunning {
	own := way.shape.alter
	way.shape.alter = func(job *batchv1.Job) {
		if own != nil {
			own(job)
		}
		alter(job)
	}
	return way
}

// countingMigrationWrites counts, in front of writer, the status writes that record
// a Job UID on the claim and the patches that reach a Job, including the ones
// admission would refuse.
func countingMigrationWrites(writer client.WithWatch, recordings, jobPatches *int) client.WithWatch {
	return interceptor.NewClient(writer, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, writer client.Client, subResource string, object client.Object, patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			if migration, ok := object.(*operatorv1alpha1.PtahMigration); ok &&
				migration.Status.ActiveOperation != nil && migration.Status.ActiveOperation.JobUID != "" {
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

// cleanupDeferredEvents is the MigrationJobCleanupDeferred Warnings that name job.
func cleanupDeferredEvents(events []string, job string) []string {
	var named []string
	for _, event := range events {
		if strings.HasPrefix(event, corev1.EventTypeWarning+" MigrationJobCleanupDeferred ") &&
			strings.Contains(event, `"`+job+`"`) {
			named = append(named, event)
		}
	}
	return named
}

const notTheClaimsJob = "the Job is not the one the claim fixes"

// TestAnUnrecordedClaimNeverRecordsARunningJobOutsideWhatItFixes is a claim that
// never recorded its Job's UID over a running Job this resource owns that fails
// what the claim fixes: one under another epoch, or one carrying another claim's
// labels. The pass that records the run unknown keeps the claim while the Job runs
// and records no UID. The pass that retires the claim once the Job has stopped
// records none either, asks for no TTL, and a Warning names the Job then, as the
// last pass that can.
func TestAnUnrecordedClaimNeverRecordsARunningJobOutsideWhatItFixes(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		way   func() unknownWhileRunning
		alter func(*batchv1.Job)
		why   string
	}{
		{
			name: "an executor rollout over a Job under another epoch", way: executorRolledOut,
			alter: underAnotherEpoch, why: "the Job runs under another execution epoch than its claim",
		},
		{
			name: "an executor rollout over a Job carrying another claim's labels", way: executorRolledOut,
			alter: underAnotherClaim, why: "the Job's labels are not its claim's",
		},
		{
			name: "a release that changed the Job, under another epoch", way: jobRebuiltDifferently,
			alter: underAnotherEpoch, why: "the Job runs under another execution epoch than its claim",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := row.way().unrecorded().outsideItsClaim(row.alter).reach(t)
			if uid := readMigration(t, api, run.migration).Status.ActiveOperation.JobUID; uid != "" {
				t.Fatalf("the claim kept over the running Job records Job UID %q, a Job outside what it fixes", uid)
			}
			if named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name); len(named) != 0 {
				t.Fatalf("the pass that kept the claim named the Job as staying: %q", named)
			}

			retiring := record.NewFakeRecorder(64)
			reconciler.Recorder = retiring
			var recordings, jobPatches int
			reconciler.Client = countingMigrationWrites(reconciler.Client.(client.WithWatch), &recordings, &jobPatches)
			stopMigrationWorkload(t, api, run, "")
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept: %#v", settled.Status.ActiveOperation)
			}
			if recordings != 0 {
				t.Fatalf("%d status writes recorded the UID of a Job outside what the claim fixes, want none", recordings)
			}
			if jobPatches != 0 {
				t.Fatalf("%d writes reached a Job the claim never recorded, want none", jobPatches)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job outside its claim has TTL %d, want none", *got)
			}
			named := cleanupDeferredEvents(migrationEvents(retiring), run.job.Name)
			if len(named) != 1 || !strings.Contains(named[0], notTheClaimsJob) || !strings.Contains(named[0], row.why) {
				t.Fatalf("Events naming Job %q = %q, want one Warning that it stays because %q: %q",
					run.job.Name, named, notTheClaimsJob, row.why)
			}
		})
	}
}

// TestAnUnrecordedClaimNamesAFinishedJobOutsideWhatItFixesAsItGoes is the same claim
// over a Job that had stopped before the pass that records the run unknown. Nothing
// can write, so the claim goes in that pass, and that pass names the Job: it records
// no UID and asks for no TTL admission would refuse. A release that moved the
// executor reads the name itself, and a release that changed the Job hands on the
// Job it refused.
func TestAnUnrecordedClaimNamesAFinishedJobOutsideWhatItFixesAsItGoes(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		way  func() unknownWhileRunning
	}{
		{name: "an executor rollout", way: executorRolledOut},
		{name: "a release that changed the Job", way: jobRebuiltDifferently},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			way := row.way().unrecorded().outsideItsClaim(underAnotherEpoch)
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
			reconciler, api := run.reconciler(t, way.next)
			var recordings, jobPatches int
			reconciler.Client = countingMigrationWrites(admittedJobWrites(api, way.next), &recordings, &jobPatches)
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
			if recordings != 0 {
				t.Fatalf("%d status writes recorded the UID of a Job outside what the claim fixes, want none", recordings)
			}
			if jobPatches != 0 {
				t.Fatalf("%d writes reached a Job the claim never recorded, want none", jobPatches)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job outside its claim has TTL %d, want none", *got)
			}
			named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name)
			if len(named) != 1 || !strings.Contains(named[0], notTheClaimsJob) {
				t.Fatalf("Events naming Job %q = %q, want one Warning that it stays because %q", run.job.Name, named, notTheClaimsJob)
			}
		})
	}
}

// TestAFailedWriteOfAnAdoptedUIDFailsThePass is the write that records the Job a
// supervising pass adopts, failing. The pass fails, so the claim is retried with no
// UID recorded, and the next pass records it.
func TestAFailedWriteOfAnAdoptedUIDFailsThePass(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationApply, operatorv1alpha1.MigrationOperationHistory,
	} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := runDispatchedBy(t, operation, runShape{dispatcher: current})
			reconciler, api := run.reconciler(t, current)
			failures := 0
			reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
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

			_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			if failures != 1 {
				t.Fatalf("%d writes of the adopted UID reached the injected failure, want one", failures)
			}
			if err == nil {
				t.Fatal("Reconcile() returned no error, want the failed write of the adopted UID to fail the pass")
			}
			if uid := readMigration(t, api, run.migration).Status.ActiveOperation.JobUID; uid != "" {
				t.Fatalf("the claim records Job UID %q after its write failed", uid)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v on the pass after the failure", err)
			}
			assertAdopted(t, api, run)
		})
	}
}

// TestATTLWriteThatMeetsANewerJobKeepsTheClaim is the TTL patch of a stopped Job
// answered with a conflict in the pass that records its run unknown, as when the Job
// controller wrote the Job's status between the read and the patch. Admission admits
// the TTL only while the claim names the Job, so the claim stays, and the database
// with it, for the pass that retires it, which asks again. A deleting resource does
// not wait for it: the garbage collector takes the Job with its owner. Nor does a
// claim over a later Job under its name, whose TTL admission refuses whatever the
// next pass reads: the claim goes, and a Warning names the Job.
func TestATTLWriteThatMeetsANewerJobKeepsTheClaim(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name     string
		way      func() unknownWhileRunning
		deleting bool
		// replaced is a claim that recorded another Job's UID, so the Job under
		// its name is a later one.
		replaced bool
	}{
		{name: "a claim that recorded the Job", way: executorRolledOut},
		{name: "a claim that never recorded the Job", way: func() unknownWhileRunning { return executorRolledOut().unrecorded() }},
		{name: "a deleting resource", way: executorRolledOut, deleting: true},
		{
			name: "a later Job under the claim's name", replaced: true,
			way: func() unknownWhileRunning {
				return unknownWhileRunning{shape: runShape{dispatcher: current, recorded: true}, next: current}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			way := row.way()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, way.shape)
			if row.replaced {
				run.migration.Status.ActiveOperation.JobUID = "the-recorded-job-uid"
			}
			reconciler, api := run.reconciler(t, way.next)
			conflicts := 0
			reconciler.Client = interceptor.NewClient(admittedJobWrites(api, way.next), interceptor.Funcs{
				Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if _, isJob := object.(*batchv1.Job); isJob && conflicts == 0 {
						conflicts++
						return apierrors.NewConflict(batchv1.Resource("jobs"), object.GetName(),
							errors.New("the object has been modified; please apply your changes to the latest version"))
					}
					return writer.Patch(ctx, object, patch, options...)
				},
			})
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			stopMigrationWorkload(t, api, run, "")
			if row.deleting {
				if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
					t.Fatal(err)
				}
			}

			result, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if conflicts != 1 {
				t.Fatalf("%d TTL writes met the injected conflict, want one", conflicts)
			}
			first := readMigration(t, api, run.migration)
			if first.Status.UnresolvedRun == nil || first.Status.UnresolvedRun.OperationID != run.migration.Status.ActiveOperation.ID {
				t.Fatalf("unresolved run = %#v, want the stopped Apply's run recorded", first.Status.UnresolvedRun)
			}
			if row.deleting || row.replaced {
				if first.Status.ActiveOperation != nil {
					t.Fatalf("the claim was kept for a TTL nothing would admit or need: %#v", first.Status.ActiveOperation)
				}
				named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name)
				if len(named) != 1 || !strings.Contains(named[0], "the object has been modified") {
					t.Fatalf("Events naming Job %q = %q, want one Warning that the conflict left it with no TTL", run.job.Name, named)
				}
				return
			}
			if first.Status.ActiveOperation == nil || first.Status.ActiveOperation.JobUID != run.job.UID {
				t.Fatalf("the claim after the conflict = %#v, want it kept and naming Job %q", first.Status.ActiveOperation, run.job.UID)
			}
			assertDatabaseStillHeld(t, reconciler, api)
			if named := cleanupDeferredEvents(migrationEvents(recorder), run.job.Name); len(named) != 0 {
				t.Fatalf("a conflict named the Job as staying: %q", named)
			}
			// Nothing about the stopped Job will change again to wake the resource,
			// so the pass asks for the next one itself, and soon: the claim holds
			// the database until then.
			if result.RequeueAfter <= 0 || result.RequeueAfter > 5*time.Second {
				t.Fatalf("the pass that kept the claim asks for the next one after %s, want within 5s", result.RequeueAfter)
			}
			reconciler.Recorder = record.NewFakeRecorder(64)

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v on the pass after the conflict", err)
			}
			if settled := readMigration(t, api, run.migration); settled.Status.ActiveOperation != nil {
				t.Fatalf("the stopped Job's claim was kept after its TTL: %#v", settled.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
				t.Fatalf("the stopped Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
			}
		})
	}
}

// TestARecordedRunNamesOnlyItsOwnJob holds nameRecordedMigrationRun to the records
// it may fill: the claim's own run, and only where it names no Job yet.
func TestARecordedRunNamesOnlyItsOwnJob(t *testing.T) {
	t.Parallel()

	const operationID = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	const otherID = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: workloadBuilderForMigrations()})
	job := run.job
	dispatched := dispatcherRecord(job)
	if dispatched == nil {
		t.Fatal("the built Job carries no manager record, so the row cannot tell one from none")
	}
	earlier := &operatorv1alpha1.ManagerRecord{
		ControllerImage: "example.invalid/manager@sha256:" + strings.Repeat("e", 64), ControllerRevision: "earlier",
		RunnerImage: "example.invalid/operator@sha256:" + strings.Repeat("e", 64),
	}
	for _, row := range []struct {
		name string
		// id, jobName, jobUID and by are what both records hold before.
		id, jobName string
		jobUID      types.UID
		by          *operatorv1alpha1.ManagerRecord
		// named is whether the Job is written into them.
		named bool
		// wantBy is the manager the records name after.
		wantBy *operatorv1alpha1.ManagerRecord
	}{
		{name: "the claim's run, naming no Job", id: operationID, named: true, wantBy: dispatched},
		{name: "the claim's run, naming its dispatcher", id: operationID, by: earlier, named: true, wantBy: earlier},
		{name: "another run", id: otherID},
		{name: "the claim's run, naming a Job", id: operationID, jobName: "an-earlier-job", jobUID: "an-earlier-uid"},
		{name: "the claim's run, naming a Job's UID alone", id: operationID, jobUID: "an-earlier-uid"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration := &operatorv1alpha1.PtahMigration{Status: operatorv1alpha1.PtahMigrationStatus{
				LastRun: &operatorv1alpha1.MigrationRunStatus{
					Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: row.id,
					JobName: row.jobName, JobUID: row.jobUID, DispatchedBy: row.by.DeepCopy(), StartedAt: metav1.Now(),
				},
				UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
					Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: row.id,
					JobName: row.jobName, JobUID: row.jobUID, DispatchedBy: row.by.DeepCopy(),
				},
			}}
			nameRecordedMigrationRun(migration, &operatorv1alpha1.MigrationOperationStatus{ID: operationID}, job)

			wantName, wantUID, wantBy := row.jobName, row.jobUID, row.by
			if row.named {
				wantName, wantUID, wantBy = job.Name, job.UID, row.wantBy
			}
			last, unresolved := migration.Status.LastRun, migration.Status.UnresolvedRun
			for _, got := range []struct {
				record string
				name   string
				uid    types.UID
				by     *operatorv1alpha1.ManagerRecord
			}{
				{"status.lastRun", last.JobName, last.JobUID, last.DispatchedBy},
				{"status.unresolvedRun", unresolved.JobName, unresolved.JobUID, unresolved.DispatchedBy},
			} {
				if got.name != wantName || got.uid != wantUID {
					t.Errorf("%s names Job %q (UID %q), want %q (UID %q)", got.record, got.name, got.uid, wantName, wantUID)
				}
				if (got.by == nil) != (wantBy == nil) || (got.by != nil && *got.by != *wantBy) {
					t.Errorf("%s dispatchedBy = %#v, want %#v", got.record, got.by, wantBy)
				}
			}
		})
	}
}
