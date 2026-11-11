package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A PtahMigration deleted under an Apply nobody read records the run Unknown
// and lets the resource go on the next pass. Its MigrationRunUncertain Event
// said the Pod log was there for 24 hours, with the kubectl logs command, but
// the garbage collector deletes the Job and its Pod with the resource, so the
// command found nothing once the Event could be read (#980). These rows hold
// the Event to what happens to the Job: it says the log goes when the Job goes
// with the resource, and points at the log only when the Job outlives it.

const (
	deletedInFlight = "the PtahMigration was deleted while a dispatched Apply was in flight"
	goesWith        = ", and the Job and its Pod are deleted with the PtahMigration"
)

func TestADeletedMigrationsLogPointerFollowsItsJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	owned := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: "ptah-apply-orders-1",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(migration, operatorv1alpha1.GroupVersion.WithKind("PtahMigration")),
			},
		},
		Spec: batchv1.JobSpec{TTLSecondsAfterFinished: new(jobclaim.FailedApplyRetentionTTLSeconds)},
	}
	pointer := "Anything the run wrote is in the Pod log for 24 hours after the Job finishes: " +
		"kubectl logs -n " + migration.Namespace + " job/" + owned.Name + " -c " + executorContainerName
	deleting := func(finalizers ...string) *operatorv1alpha1.PtahMigration {
		deleted := migration.DeepCopy()
		deleted.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
		deleted.Finalizers = append([]string{migrationOperationFinalizer}, finalizers...)
		return deleted
	}
	orphaned := owned.DeepCopy()
	orphaned.OwnerReferences = nil
	untimed := owned.DeepCopy()
	untimed.Spec.TTLSecondsAfterFinished = nil

	for _, test := range []struct {
		name      string
		migration *operatorv1alpha1.PtahMigration
		job       *batchv1.Job
		reading   applyLogReading
		want      string
	}{
		{
			name:      "a resource that is not being deleted points at the log",
			migration: migration, job: owned, reading: applyLogUnread, want: pointer,
		},
		{
			name:      "a deleted resource's Job goes with it",
			migration: deleting(), job: owned, reading: applyLogUnread,
			want: "Anything the run wrote is in the Pod log of Job " + owned.Name + goesWith,
		},
		{
			name:      "a Job that goes with the resource goes whatever TTL it carries",
			migration: deleting(), job: untimed, reading: applyLogUnread,
			want: "Anything the run wrote is in the Pod log of Job " + untimed.Name + goesWith,
		},
		{
			name:      "a log with nothing to read is not named",
			migration: deleting(), job: owned, reading: applyLogSaysNothing, want: "",
		},
		{
			name:      "a deletion still orphaning its dependents leaves the Job its TTL",
			migration: deleting(metav1.FinalizerOrphanDependents), job: owned, reading: applyLogUnread, want: pointer,
		},
		{
			name:      "a Job the deletion has orphaned stays for its TTL",
			migration: deleting(), job: orphaned, reading: applyLogUnread, want: pointer,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := migrationApplyLogPointer(test.migration, test.job, test.reading); got != test.want {
				t.Fatalf("migrationApplyLogPointer() = %q, want %q", got, test.want)
			}
		})
	}
}

// deleteMigrationAfterItsApply stands up an Apply whose Job finished and whose
// Pod stopped with no result, lets prepare change what the deletion meets, deletes
// the resource, and runs the pass that records the run and the pass that lets the
// resource go. Job writes go through the manager's own admission, so a TTL the
// rows read is one admission admits on a deleting resource's Job.
func deleteMigrationAfterItsApply(
	t *testing.T,
	prepare func(t *testing.T, api client.Client, run dispatchedRun),
) (client.Client, dispatchedRun, []string) {
	t.Helper()

	ctx := context.Background()
	builder := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: builder, recorded: true})
	reconciler, api := run.reconciler(t, builder)
	reconciler.Client = admittedJobWrites(api, builder)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	stopMigrationWorkload(t, api, run, "")
	if prepare != nil {
		prepare(t, api, run)
	}
	if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	return api, run, migrationEvents(recorder)
}

// uncertainRunEvent is the one MigrationRunUncertain Event, which has to name the
// deletion: a pass that recorded the run another way is not the row's subject.
func uncertainRunEvent(t *testing.T, events []string) string {
	t.Helper()

	const prefix = "Warning MigrationRunUncertain "
	var found []string
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("MigrationRunUncertain Events = %q, want exactly one, among %q", found, events)
	}
	if !strings.HasPrefix(found[0], prefix+deletedInFlight) {
		t.Fatalf("MigrationRunUncertain = %q, want the deletion's sentence first", found[0])
	}
	return found[0]
}

func TestADeletedMigrationsUncertainRunSaysItsLogGoesWithIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, run, events := deleteMigrationAfterItsApply(t, nil)

	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the deletion did not complete, so the garbage collector has not been handed the Job: error %v, finalizers %#v",
			err, gone.Finalizers)
	}
	event := uncertainRunEvent(t, events)
	if want := deletedInFlight + ". Anything the run wrote is in the Pod log of Job " + run.job.Name + goesWith; !strings.Contains(event, want) {
		t.Fatalf("MigrationRunUncertain = %q, want it to carry %q", event, want)
	}
	for _, stale := range []string{"kubectl logs", "for 24 hours"} {
		if strings.Contains(event, stale) {
			t.Fatalf("MigrationRunUncertain = %q points at a log the deletion takes with it (%q)", event, stale)
		}
	}
}

// A deletion with --cascade=orphan carries the orphan finalizer until the garbage
// collector has taken the owner reference off the Job, so a pass that runs first
// still finds the Job owned. That Job outlives the resource, and the TTL this pass
// gives it is how long its log stays.
func TestAMigrationDeletedWhileItsJobIsOrphanedPointsAtTheLog(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, run, events := deleteMigrationAfterItsApply(t, func(t *testing.T, api client.Client, run dispatchedRun) {
		t.Helper()
		orphaning := readMigration(t, api, run.migration)
		orphaning.Finalizers = append(orphaning.Finalizers, metav1.FinalizerOrphanDependents)
		if err := api.Update(ctx, orphaning); err != nil {
			t.Fatal(err)
		}
	})

	held := readMigration(t, api, run.migration)
	if held.DeletionTimestamp == nil || slices.Contains(held.Finalizers, migrationOperationFinalizer) ||
		!slices.Contains(held.Finalizers, metav1.FinalizerOrphanDependents) {
		t.Fatalf("the resource is not waiting on the orphan finalizer alone: deletion %v, finalizers %#v",
			held.DeletionTimestamp, held.Finalizers)
	}
	if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
		t.Fatalf("the Job's TTL = %s, want %d: the pointer promises it", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
	}
	event := uncertainRunEvent(t, events)
	want := deletedInFlight + ". Anything the run wrote is in the Pod log for 24 hours after the Job finishes: kubectl logs -n " +
		run.job.Namespace + " job/" + run.job.Name + " -c " + executorContainerName
	if !strings.Contains(event, want) || strings.Contains(event, goesWith) {
		t.Fatalf("MigrationRunUncertain = %q, want it to point at the log with %q", event, want)
	}
}

// Once the garbage collector has orphaned the Job it takes the orphan finalizer
// off, so the pass meets a deleting resource with only its own finalizer and a Job
// it no longer controls. That Job is not deleted with the resource, and admission
// refuses its TTL, so the Event promises neither.
func TestAMigrationDeletedAfterItsJobWasOrphanedSaysNothingOfTheLog(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api, run, events := deleteMigrationAfterItsApply(t, func(t *testing.T, api client.Client, run dispatchedRun) {
		t.Helper()
		job := &batchv1.Job{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
			t.Fatal(err)
		}
		job.OwnerReferences = nil
		if err := api.Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	})

	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the deletion did not complete: error %v, finalizers %#v", err, gone.Finalizers)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the orphaned Job's TTL = %s, want none: admission refuses a Job no resource controls", ttlText(got))
	}
	if !slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
		t.Fatalf("the Events %q do not say the orphaned Job stays", events)
	}
	event := uncertainRunEvent(t, events)
	for _, promise := range []string{goesWith, "kubectl logs"} {
		if strings.Contains(event, promise) {
			t.Fatalf("MigrationRunUncertain = %q promises %q of a Job that outlives the resource with no TTL", event, promise)
		}
	}
}
