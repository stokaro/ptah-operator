package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// A PtahMigration deleted under an Apply nobody read records the run Unknown
// and lets the resource go on the next pass. Its MigrationRunUncertain Event
// said the Pod log was there for 24 hours, with the kubectl logs command, but
// the garbage collector deletes the Job and its Pod with the resource, so the
// command found nothing once the Event could be read (#980). These rows hold
// every place that treats a deleting resource's Job as gone to what happens to
// the Job: the Events say the log goes when the Job goes with the resource, and
// a Job that outlives it is pointed at and given its TTL.

const (
	deletedInFlight = "the PtahMigration was deleted while a dispatched Apply was in flight"
	goesWith        = ", and the Job and its Pod are deleted with the PtahMigration"
	discardedGoes   = ". The Job and its Pod log are deleted with the PtahMigration"
)

// deletingMigration is migrationFixture being deleted, carrying its own finalizer
// and finalizers.
func deletingMigration(finalizers ...string) *operatorv1alpha1.PtahMigration {
	deleted := migrationFixture()
	deleted.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	deleted.Finalizers = append([]string{migrationOperationFinalizer}, finalizers...)
	return deleted
}

// controlledJob is a finished Apply Job migrationFixture controls, holding the
// 24-hour TTL.
func controlledJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migrationFixture().Namespace, Name: "ptah-apply-orders-1",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(migrationFixture(), operatorv1alpha1.GroupVersion.WithKind("PtahMigration")),
			},
		},
		Spec: batchv1.JobSpec{TTLSecondsAfterFinished: new(jobclaim.FailedApplyRetentionTTLSeconds)},
	}
}

func TestADeletedMigrationsLogPointerFollowsItsJob(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	owned := controlledJob()
	pointer := "Anything the run wrote is in the Pod log for 24 hours after the Job finishes: " +
		"kubectl logs -n " + migration.Namespace + " job/" + owned.Name + " -c " + executorContainerName
	goes := "Anything the run wrote is in the Pod log of Job " + owned.Name + goesWith
	orphaned := owned.DeepCopy()
	orphaned.OwnerReferences = nil
	untimed := owned.DeepCopy()
	untimed.Spec.TTLSecondsAfterFinished = nil
	going := owned.DeepCopy()
	going.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 11, 12, 0, 1, 0, time.UTC)}
	going.Finalizers = []string{metav1.FinalizerDeleteDependents}

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
			migration: deletingMigration(), job: owned, reading: applyLogUnread, want: goes,
		},
		{
			name:      "a Job that goes with the resource goes whatever TTL it carries",
			migration: deletingMigration(), job: untimed, reading: applyLogUnread, want: goes,
		},
		{
			name:      "a log with nothing to read is not named",
			migration: deletingMigration(), job: owned, reading: applyLogSaysNothing, want: "",
		},
		{
			name:      "a deletion still orphaning its dependents leaves the Job its TTL",
			migration: deletingMigration(metav1.FinalizerOrphanDependents), job: owned, reading: applyLogUnread, want: pointer,
		},
		{
			name:      "a Job the deletion has orphaned stays for its TTL",
			migration: deletingMigration(), job: orphaned, reading: applyLogUnread, want: pointer,
		},
		{
			name:      "a foreground deletion takes the Job with the resource",
			migration: deletingMigration(metav1.FinalizerDeleteDependents), job: owned, reading: applyLogUnread, want: goes,
		},
		{
			name:      "another finalizer only delays the Job going",
			migration: deletingMigration("example.com/backup"), job: owned, reading: applyLogUnread, want: goes,
		},
		{
			name:      "a Job already being deleted may have lost its Pod",
			migration: deletingMigration(metav1.FinalizerDeleteDependents), job: going, reading: applyLogUnread,
			want: "Anything the run wrote was in the Pod log of Job " + going.Name + ", which is being deleted with its Pod",
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

// TestWhatGoesWithADeletedMigration holds the rule the four sites share to the
// cases the log pointer does not reach.
func TestWhatGoesWithADeletedMigration(t *testing.T) {
	t.Parallel()

	going := controlledJob()
	going.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 11, 12, 0, 1, 0, time.UTC)}
	going.Finalizers = []string{metav1.FinalizerDeleteDependents}
	goingOrphan := going.DeepCopy()
	goingOrphan.OwnerReferences = nil

	for _, test := range []struct {
		name      string
		migration *operatorv1alpha1.PtahMigration
		job       *batchv1.Job
		want      bool
	}{
		{name: "no Job", migration: deletingMigration(), job: nil, want: false},
		{name: "a Job being deleted beside a resource that is not", migration: migrationFixture(), job: going, want: false},
		{
			name:      "a Job being deleted under a deletion that orphans",
			migration: deletingMigration(metav1.FinalizerOrphanDependents), job: going, want: true,
		},
		{name: "an orphaned Job someone is deleting", migration: deletingMigration(), job: goingOrphan, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := migrationJobGoesWithResource(test.migration, test.job); got != test.want {
				t.Fatalf("migrationJobGoesWithResource() = %v, want %v", got, test.want)
			}
		})
	}
}

// deletedApply is a PtahMigration deleted after its Apply's Job finished and its
// Pod stopped with no result. Job writes go through the manager's own admission,
// so a TTL a row reads is one admission admits on a deleting resource's Job.
type deletedApply struct {
	// prepare changes what the deletion meets, before the delete.
	prepare func(t *testing.T, api client.Client, run dispatchedRun)
	// writes wraps the client the manager writes Jobs through.
	writes func(client.WithWatch) client.WithWatch
	// between runs after the pass that records the run, before the next one.
	between func(t *testing.T, api client.Client, run dispatchedRun)
}

// settle deletes the resource and runs the pass that records the run and the
// pass after it, which lets the resource go once nothing holds the claim.
func (scenario deletedApply) settle(t *testing.T) (client.Client, dispatchedRun, []string) {
	t.Helper()

	ctx := context.Background()
	builder := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: builder, recorded: true})
	reconciler, api := run.reconciler(t, builder)
	reconciler.Client = admittedJobWrites(api, builder)
	if scenario.writes != nil {
		reconciler.Client = scenario.writes(reconciler.Client.(client.WithWatch))
	}
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	stopMigrationWorkload(t, api, run, "")
	if scenario.prepare != nil {
		scenario.prepare(t, api, run)
	}
	if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if scenario.between != nil {
		scenario.between(t, api, run)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return api, run, migrationEvents(recorder)
}

// uncertainRunEvent is the one MigrationRunUncertain Event, which has to name the
// deletion: a pass that recorded the run another way is not the row's subject.
func uncertainRunEvent(t *testing.T, events []string) string {
	t.Helper()
	event := onlyEvent(t, events, "Warning MigrationRunUncertain ")
	if !strings.HasPrefix(event, "Warning MigrationRunUncertain "+deletedInFlight) {
		t.Fatalf("MigrationRunUncertain = %q, want the deletion's sentence first", event)
	}
	return event
}

// onlyEvent is the one Event with prefix.
func onlyEvent(t *testing.T, events []string, prefix string) string {
	t.Helper()
	var found []string
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("Events with %q = %q, want exactly one, among %q", prefix, found, events)
	}
	return found[0]
}

// migrationGone fails unless the deleted resource is no longer stored.
func migrationGone(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()
	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(run.migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the deletion did not complete: error %v, finalizers %#v, claim %#v",
			err, gone.Finalizers, gone.Status.ActiveOperation)
	}
}

// orphaning is the orphan finalizer the API server puts on a resource deleted
// with --cascade=orphan, before the garbage collector has acted on it.
func orphaning(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()
	migration := readMigration(t, api, run.migration)
	migration.Finalizers = append(migration.Finalizers, metav1.FinalizerOrphanDependents)
	if err := api.Update(context.Background(), migration); err != nil {
		t.Fatal(err)
	}
}

// orphaned is the Job once the garbage collector has taken its owner reference off.
func orphaned(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	job.OwnerReferences = nil
	if err := api.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}

// failingTTLWrites fails the TTL writes refuse picks with failure, and counts them.
func failingTTLWrites(failure error, refuse func(ttl int32, failed int) bool, failed *int) func(client.WithWatch) client.WithWatch {
	return func(writer client.WithWatch) client.WithWatch {
		return interceptor.NewClient(writer, interceptor.Funcs{
			Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if job, isJob := object.(*batchv1.Job); isJob && job.Spec.TTLSecondsAfterFinished != nil &&
					refuse(*job.Spec.TTLSecondsAfterFinished, *failed) {
					*failed++
					return failure
				}
				return writer.Patch(ctx, object, patch, options...)
			},
		})
	}
}

func TestADeletedMigrationsUncertainRunSaysItsLogGoesWithIt(t *testing.T) {
	t.Parallel()

	api, run, events := deletedApply{}.settle(t)

	migrationGone(t, api, run)
	event := uncertainRunEvent(t, events)
	if want := deletedInFlight + ". Anything the run wrote is in the Pod log of Job " + run.job.Name + goesWith; !strings.Contains(event, want) {
		t.Fatalf("MigrationRunUncertain = %q, want it to carry %q", event, want)
	}
	for _, stale := range []string{"kubectl logs", "for 24 hours"} {
		if strings.Contains(event, stale) {
			t.Fatalf("MigrationRunUncertain = %q points at a log the deletion takes with it (%q)", event, stale)
		}
	}
	if discarded := onlyEvent(t, events, "Warning UnresolvedRunDiscarded "); !strings.HasSuffix(discarded, discardedGoes) {
		t.Fatalf("UnresolvedRunDiscarded = %q, want it to end with %q", discarded, discardedGoes)
	}
}

// A deletion with --cascade=orphan carries the orphan finalizer until the garbage
// collector has taken the owner reference off the Job, so a pass that runs first
// still finds the Job owned. That Job outlives the resource, and the TTL this pass
// gives it is how long its log stays.
func TestAMigrationDeletedWhileItsJobIsOrphanedPointsAtTheLog(t *testing.T) {
	t.Parallel()

	api, run, events := deletedApply{prepare: orphaning}.settle(t)

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
	if discarded := onlyEvent(t, events, "Warning UnresolvedRunDiscarded "); strings.Contains(discarded, discardedGoes) {
		t.Fatalf("UnresolvedRunDiscarded = %q says a Job that outlives the resource goes with it", discarded)
	}
}

// Once the garbage collector has orphaned the Job it takes the orphan finalizer
// off, so the pass meets a deleting resource with only its own finalizer and a Job
// it no longer controls. That Job is not deleted with the resource, and admission
// refuses its TTL, so the Event promises neither.
func TestAMigrationDeletedAfterItsJobWasOrphanedSaysNothingOfTheLog(t *testing.T) {
	t.Parallel()

	api, run, events := deletedApply{prepare: orphaned}.settle(t)

	migrationGone(t, api, run)
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
	if discarded := onlyEvent(t, events, "Warning UnresolvedRunDiscarded "); strings.Contains(discarded, discardedGoes) {
		t.Fatalf("UnresolvedRunDiscarded = %q says an orphaned Job goes with the resource", discarded)
	}
}

// The Event that discards the record names the Job the run ran as, and says it
// goes only of that Job, standing.
func TestADiscardedRunSaysOnlyItsOwnStandingJobGoes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		between func(t *testing.T, api client.Client, run dispatchedRun)
	}{
		{name: "the Job is gone", between: func(t *testing.T, api client.Client, run dispatchedRun) {
			t.Helper()
			if err := api.Delete(context.Background(), run.job.DeepCopy()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a later Job holds the name", between: func(t *testing.T, api client.Client, run dispatchedRun) {
			t.Helper()
			ctx := context.Background()
			later := &batchv1.Job{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), later); err != nil {
				t.Fatal(err)
			}
			if err := api.Delete(ctx, later.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			later.ResourceVersion = ""
			later.UID = types.UID("a-later-job-uid")
			if err := api.Create(ctx, later); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api, run, events := deletedApply{between: test.between}.settle(t)

			migrationGone(t, api, run)
			discarded := onlyEvent(t, events, "Warning UnresolvedRunDiscarded ")
			if !strings.Contains(discarded, run.job.Name) {
				t.Fatalf("UnresolvedRunDiscarded = %q, want it to name the Job %s", discarded, run.job.Name)
			}
			if strings.Contains(discarded, discardedGoes) {
				t.Fatalf("UnresolvedRunDiscarded = %q says a Job goes that is not the run's", discarded)
			}
		})
	}
}

// A TTL write that conflicts keeps the claim for the pass that reads the Job again,
// so that pass can give the TTL. A Job that goes with the resource needs no TTL, so
// its resource goes at once; one that outlives it waits that one pass for its TTL.
func TestAConflictedTTLWaitsOnlyForAJobThatOutlivesItsResource(t *testing.T) {
	t.Parallel()

	conflict := apierrors.NewConflict(batchv1.Resource("jobs"), "ptah", errors.New("the object has been modified"))
	first := func(_ int32, failed int) bool { return failed == 0 }

	t.Run("a Job that goes with the resource", func(t *testing.T) {
		t.Parallel()
		failed := 0
		api, run, _ := deletedApply{writes: failingTTLWrites(conflict, first, &failed)}.settle(t)
		if failed == 0 {
			t.Fatal("no TTL write reached the conflict, so this row proved nothing")
		}
		migrationGone(t, api, run)
	})
	t.Run("a Job that outlives the resource", func(t *testing.T) {
		t.Parallel()
		failed := 0
		api, run, events := deletedApply{prepare: orphaning, writes: failingTTLWrites(conflict, first, &failed)}.settle(t)
		if failed == 0 {
			t.Fatal("no TTL write reached the conflict, so this row proved nothing")
		}
		if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
			t.Fatalf("the Job's TTL = %s after the pass that read it again, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
		}
		if held := readMigration(t, api, run.migration); held.Status.ActiveOperation != nil {
			t.Fatalf("the pass that gave the TTL kept the claim: %#v", held.Status.ActiveOperation)
		}
		if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "MigrationJobCleanupDeferred") }) {
			t.Fatalf("the Events %q say the cleanup was not scheduled, and it was", events)
		}
	})
}

// Admission that refuses the 24 hours leaves the Job five minutes, and a Warning
// says the log was cut short. A Job that goes with the resource keeps it for
// neither, so nothing was cut short.
func TestAShortenedLogIsReportedOnlyForAJobThatOutlivesItsResource(t *testing.T) {
	t.Parallel()

	refused := apierrors.NewForbidden(batchv1.Resource("jobs"), "ptah", errors.New("an older guard"))
	long := func(ttl int32, _ int) bool { return ttl == jobclaim.FailedApplyRetentionTTLSeconds }
	for _, test := range []struct {
		name      string
		prepare   func(t *testing.T, api client.Client, run dispatchedRun)
		shortened bool
	}{
		{name: "a Job that goes with the resource"},
		{name: "a Job that outlives the resource", prepare: orphaning, shortened: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			failed := 0
			api, run, events := deletedApply{prepare: test.prepare, writes: failingTTLWrites(refused, long, &failed)}.settle(t)
			if failed == 0 {
				t.Fatal("no 24-hour TTL write reached the refusal, so this row proved nothing")
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
				t.Fatalf("the Job's TTL = %s, want the %d admission admitted", ttlText(got), jobclaim.CleanupTTLSeconds)
			}
			reported := slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "FailedApplyLogShortened") })
			if reported != test.shortened {
				t.Fatalf("FailedApplyLogShortened reported = %v, want %v, among %q", reported, test.shortened, events)
			}
		})
	}
}

// A claim kept because its Job could still write goes in the pass that sees the
// Job stopped, and that pass gives the Job its TTL. On a resource being deleted it
// gives the TTL only to a Job that outlives the resource, and nothing it meets on
// the way holds the finalizer: a failure is reported, and the claim goes.
func TestADeletedMigrationsStoppedJobGetsItsTTLOnlyWhenItOutlivesTheResource(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, api client.Client, run dispatchedRun)
		// fail breaks the reads or writes of the Job after the Pods were listed.
		fail     func(reconciler *MigrationReconciler, api client.WithWatch) *int
		ttl      *int32
		deferred bool
	}{
		{name: "a Job that outlives the resource", prepare: orphaning, ttl: new(jobclaim.FailedApplyRetentionTTLSeconds)},
		{
			name: "a Job that outlives the resource, its TTL write failing", prepare: orphaning, deferred: true,
			fail: func(reconciler *MigrationReconciler, _ client.WithWatch) *int {
				failed := 0
				reconciler.Client = failingTTLWrites(errors.New("connection reset by peer"),
					func(int32, int) bool { return true }, &failed)(reconciler.Client.(client.WithWatch))
				return &failed
			},
		},
		{
			name: "a Job whose read fails", deferred: true,
			fail: func(reconciler *MigrationReconciler, api client.WithWatch) *int {
				listed, failed := false, 0
				reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
					List: func(ctx context.Context, reader client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
						listed = true
						return reader.List(ctx, list, options...)
					},
					Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, isJob := object.(*batchv1.Job); isJob && listed {
							failed++
							return apierrors.NewServiceUnavailable("the API server is restarting")
						}
						return reader.Get(ctx, key, object, options...)
					},
				})
				return &failed
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := executorRolledOut().reach(t)
			stopMigrationWorkload(t, api, run, "")
			if test.prepare != nil {
				test.prepare(t, api, run)
			}
			if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
				t.Fatal(err)
			}
			var failed *int
			if test.fail != nil {
				failed = test.fail(reconciler, api)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v, want nothing on the way to hold a deleting resource", err)
			}
			if failed != nil && *failed == 0 {
				t.Fatal("nothing reached the injected failure, so this row proved nothing")
			}
			if held := readMigration(t, api, run.migration); held.Status.ActiveOperation != nil {
				t.Fatalf("the deleting resource kept the stopped Job's claim: %#v", held.Status.ActiveOperation)
			}
			if got := storedTTL(t, api, run.job); (got == nil) != (test.ttl == nil) || (got != nil && *got != *test.ttl) {
				t.Fatalf("the stopped Job's TTL = %s, want %s", ttlText(got), ttlText(test.ttl))
			}
			events := migrationEvents(recorder)
			deferred := slices.ContainsFunc(events, func(event string) bool {
				return strings.Contains(event, "MigrationJobCleanupDeferred") && strings.Contains(event, run.job.Name)
			})
			if deferred != test.deferred {
				t.Fatalf("MigrationJobCleanupDeferred naming %s = %v, want %v, among %q", run.job.Name, deferred, test.deferred, events)
			}
		})
	}
}
