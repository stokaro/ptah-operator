package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// TestARetryOverAForeignJobMovesOnAndLeavesTheJobAlone is a read-only migration
// claim over a Job under its reserved name that is not the claim's: one the claim
// did not record (replaced), or one this resource does not own exactly
// (disowned), running or finished. The claim moves to a new attempt under a new
// name in one pass and asks nothing of that Job. It used to ask for the Job's
// cleanup TTL first, which the controller-write webhook refuses on a Job the claim
// does not name by UID or own, so every pass failed before the retry was written
// and the read never ran again.
//
// Every Job here comes from the real builder, and every Job write passes the real
// controller-write webhook.
func TestARetryOverAForeignJobMovesOnAndLeavesTheJobAlone(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		replaced bool
		finished bool
		// why is the reason the retry gives. It is written out rather than
		// taken from retriedMigrationJobReason, so a row fails if the words a
		// reader sees change.
		why string
	}{
		{name: "a replaced Job that is still running", replaced: true, why: "the active Job was replaced"},
		{name: "a replaced Job that has finished", replaced: true, finished: true, why: "the active Job was replaced"},
		{name: "a disowned Job that is still running", why: "the active Job is not owned by this migration"},
		{name: "a disowned Job that has finished", finished: true, why: "the active Job is not owned by this migration"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			current := workloadBuilderForMigrations()
			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory,
				runShape{dispatcher: current, recorded: true})
			if row.replaced {
				run.migration.Status.ActiveOperation.JobUID = types.UID("the-recorded-job-uid")
			} else {
				run.job.OwnerReferences[0].UID = types.UID("another-owner-uid")
			}
			reconciler, api := run.reconciler(t, current)
			if row.finished {
				finishMigrationJob(t, api, run, batchv1.JobComplete)
			}
			var jobWrites []string
			reconciler.Client = writesToJob(admittedJobWrites(api, current), run.job, &jobWrites)
			claimed := run.migration.Status.ActiveOperation

			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}

			actual := readMigration(t, api, run.migration)
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
				t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
			}
			if operation.JobUID != "" || operation.JobName == claimed.JobName {
				t.Fatalf("the retried claim names Job %q (UID %q), want a new name and no UID",
					operation.JobName, operation.JobUID)
			}
			progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || !strings.Contains(progressing.Message, row.why) {
				t.Fatalf("Progressing = %#v, want it to say %q", progressing, row.why)
			}
			if len(jobWrites) != 0 {
				t.Fatalf("the claim wrote %q to a Job that is not its own, want nothing", jobWrites)
			}
			if got, want := ttlText(storedTTL(t, api, run.job)), ttlText(run.job.Spec.TTLSecondsAfterFinished); got != want {
				t.Fatalf("the foreign Job's TTL = %s, want the %s it was created with", got, want)
			}
			if actual.Status.LastRun != nil {
				t.Fatalf("a read-only claim settled a run from a Job that is not its own: %#v", actual.Status.LastRun)
			}
		})
	}
}

// writesToJob records each write writer is asked to make to job, whatever
// admission answers. A refused write stores nothing, so the stored TTL alone
// cannot tell a claim that left the Job alone from one admission stopped.
func writesToJob(writer client.WithWatch, job *batchv1.Job, writes *[]string) client.WithWatch {
	key := client.ObjectKeyFromObject(job)
	named := func(object client.Object) bool {
		_, isJob := object.(*batchv1.Job)
		return isJob && client.ObjectKeyFromObject(object) == key
	}
	return interceptor.NewClient(writer, interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if named(object) {
				*writes = append(*writes, "patch")
			}
			return writer.Patch(ctx, object, patch, options...)
		},
		Update: func(ctx context.Context, writer client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if named(object) {
				*writes = append(*writes, "update")
			}
			return writer.Update(ctx, object, options...)
		},
		Delete: func(ctx context.Context, writer client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if named(object) {
				*writes = append(*writes, "delete")
			}
			return writer.Delete(ctx, object, options...)
		},
	})
}
