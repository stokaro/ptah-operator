package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const applyPodGoneEvent = "Warning ApplyPodGone "

// An Apply whose Pod is gone from the API server with nothing to say its
// executor stopped holds the database Lease, renewing it, until the end of the
// term the claim took the Lease for, and hands it back at that instant
// (migrationApplyPodGone). A forced delete removes the Pod object and leaves
// its container running on a node the API server cannot reach, and so does a
// Job deleted with its Pods. A Job that finished with no Pod failed had none
// to lose, and hands the Lease back at once, as before.
//
// Each row runs through the deletion of the resource, which waits for its
// Apply (reconcileMigrationDeletion), and through the live harvest, which
// records the run Unknown (finishUncertainMigrationApply) and keeps the claim
// for the passes after it (reconcileUnaccountedMigrationApply). The fake client
// runs no Job controller, so each row writes what a supported cluster leaves.
func TestAnApplyWhosePodIsGoneHoldsTheLeaseToItsTerm(t *testing.T) {
	t.Parallel()

	for _, state := range []struct {
		name string
		end  func(t *testing.T, run unstartedMigration)
		// held is a state the claim waits on until its Lease term ends.
		held bool
	}{
		{name: "a Pod force-deleted past its Job's deadline", held: true, end: podForceDeletedPastTheDeadline},
		{name: "a Job deleted with its Pod", held: true, end: jobDeletedWithItsPod},
		{name: "a Job whose Pod the API server refused", end: func(t *testing.T, run unstartedMigration) {
			refusedPodCreation(t, run.api, run.job, run.pod, metav1.NewTime(run.reconciler.now()))
			pastTheDeadlineWithoutAPod(t, run.api, run.job)
		}},
		{name: "a completed Job whose Pod is gone", end: completedJobWhosePodIsGone},
	} {
		for _, path := range []struct {
			name    string
			deleted bool
		}{
			{name: "deleted", deleted: true},
			{name: "live"},
		} {
			t.Run(state.name+", "+path.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()

				run := unstartedMigrationApplyRun(t)
				state.end(t, run)
				claim := readMigration(t, run.api, run.migration).Status.ActiveOperation
				if claim == nil || claim.JobUID != run.job.UID || claim.LeaseDurationSeconds < 1 {
					t.Fatalf("the row needs the Apply's claim with its Job and Lease term: %#v", claim)
				}
				// The term is read from the claim's own fields here rather than
				// through migrationApplyLeaseTermEnd, so a bound computed some
				// other way fails the steps around it.
				term := claim.StartedAt.Add(time.Duration(claim.LeaseDurationSeconds) * time.Second)
				if !term.After(claim.ExecutionNotAfter.Time) {
					t.Fatalf("the Lease term ends at %s, not after the execution window, %s", term, claim.ExecutionNotAfter)
				}
				holder := leaseHolder(migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest))
				if holder == "" {
					t.Fatal("the Apply holds no database Lease, so the row cannot show it is kept")
				}
				if path.deleted {
					if err := run.api.Delete(ctx, readMigration(t, run.api, run.migration)); err != nil {
						t.Fatal(err)
					}
				}
				clock := movableMigrationClock(run.reconciler, run.api)
				first := term.Add(-3 * time.Minute)
				if !first.After(claim.StartedAt.Add(terminalPodGrace)) {
					t.Fatalf("the first pass at %s is inside the harvest's Pod grace", first)
				}
				applyPodGoneEvents(run.recorder)

				if !state.held {
					clock.now = first
					reconcileMigrationOrGone(t, run)
					handedBack(t, run, path.deleted, "at once")
					if events := applyPodGoneEvents(run.recorder); len(events) != 0 {
						t.Fatalf("an Apply whose Job had no Pod to lose recorded %q", events)
					}
					return
				}

				waiter := applyWaits
				if path.deleted {
					waiter = deletionWaits
				}
				want := fmt.Sprintf("%s%s for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
					"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
					"still runs on a node the API server cannot reach, so the database Lease stays held until the term "+
					"the Apply took it for ends.",
					applyPodGoneEvent, waiter, run.job.Name, run.job.UID, term.UTC().Format(time.RFC3339))
				for _, step := range []struct {
					at    time.Time
					named bool
				}{
					{at: first, named: true},
					{at: first},
					{at: first.Add(29 * time.Second)},
					{at: first.Add(30 * time.Second), named: true},
					{at: term.Add(-time.Second), named: true},
				} {
					clock.now = step.at
					waiting := reconcileMigration(t, run.reconciler, run.api, run.migration)
					if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != claim.ID {
						t.Fatalf("the claim went %s before its Lease term while the Pod's executor may run: %#v",
							term.Sub(step.at), waiting.Status.ActiveOperation)
					}
					lease := migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest)
					if held := leaseHolder(lease); held != holder {
						t.Fatalf("the database Lease is held by %q %s before the term, want the Apply's %q", held, term.Sub(step.at), holder)
					}
					if renewed := lease.Spec.RenewTime; renewed == nil || !renewed.Time.Equal(step.at.UTC().Truncate(time.Microsecond)) {
						t.Fatalf("the database Lease was last renewed at %v, want the pass %s before the term", renewed, term.Sub(step.at))
					}
					events := applyPodGoneEvents(run.recorder)
					switch {
					case !step.named && len(events) != 0:
						t.Fatalf("a pass %s before the term recorded %q, want nothing", term.Sub(step.at), events)
					case step.named && (len(events) != 1 || events[0] != want):
						t.Fatalf("a pass %s before the term recorded %q, want one %q", term.Sub(step.at), events, want)
					}
				}
				if !path.deleted && readMigration(t, run.api, run.migration).Status.UnresolvedRun == nil {
					t.Fatal("the live harvest kept its claim without recording the run Unknown")
				}

				clock.now = term
				reconcileMigrationOrGone(t, run)
				handedBack(t, run, path.deleted, "at the end of its Lease term")
				if events := applyPodGoneEvents(run.recorder); len(events) != 0 {
					t.Fatalf("the pass at the end of the term recorded %q", events)
				}
			})
		}
	}
}

// podForceDeletedPastTheDeadline is what a forced delete leaves of an Apply
// Pod on a node that is gone, once the Job controller has accounted for it:
// the Pod object is gone, and the Job failed on its deadline with that Pod
// counted failed. Before the deadline the job-tracking finalizer keeps the
// deleted Pod standing (migrationApplyPodGone).
func podForceDeletedPastTheDeadline(t *testing.T, run unstartedMigration) {
	t.Helper()

	firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	if err := run.api.Delete(context.Background(), run.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	podGoneJobStatus(t, run, batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded},
	}})
}

// jobDeletedWithItsPod is a person deleting the Apply's Job while its Pod ran,
// and then the Pod with --force: the garbage collector deletes the Pod, the Job
// controller takes the finalizer off a Pod whose Job is gone, and the forced
// delete removes it.
func jobDeletedWithItsPod(t *testing.T, run unstartedMigration) {
	t.Helper()

	firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	for _, object := range []client.Object{run.job.DeepCopy(), run.pod.DeepCopy()} {
		if err := run.api.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
}

// completedJobWhosePodIsGone is a Job that completed, with its Pod counted
// succeeded, whose Pod object was deleted after the kubelet reported it.
func completedJobWhosePodIsGone(t *testing.T, run unstartedMigration) {
	t.Helper()

	if err := run.api.Delete(context.Background(), run.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	podGoneJobStatus(t, run, batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonCompletionsReached},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonCompletionsReached},
	}})
}

func podGoneJobStatus(t *testing.T, run unstartedMigration, status batchv1.JobStatus) {
	t.Helper()

	job := &batchv1.Job{}
	if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	status.StartTime = job.Status.StartTime
	job.Status = status
	if err := run.api.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

// toTheApplyLeaseTerm moves the reconciler, and the Locker it renews the
// database Lease with, to the end of the Lease term of the Apply claim the
// stored migration holds. An Apply whose Job or Pods are gone with nothing to
// say its executor stopped keeps the claim until then (migrationApplyPodGone),
// so a row about what the claim's release records runs that pass there.
func toTheApplyLeaseTerm(t *testing.T, reconciler *MigrationReconciler, api client.Client, migration *operatorv1alpha1.PtahMigration) {
	t.Helper()

	claim := readMigration(t, api, migration).Status.ActiveOperation
	if claim == nil || claim.LeaseDurationSeconds < 1 {
		t.Fatalf("no Apply claim with a Lease term to move to: %#v", claim)
	}
	term := claim.StartedAt.Add(time.Duration(claim.LeaseDurationSeconds) * time.Second)
	if !term.After(reconciler.now()) {
		t.Fatalf("the clock reads %s, at or past the Lease term %s, so moving to it proves nothing", reconciler.now(), term)
	}
	movableMigrationClock(reconciler, api).now = term
}

// reconcileMigrationOrGone runs one pass, which may be the one that lets a
// deleted resource go.
func reconcileMigrationOrGone(t *testing.T, run unstartedMigration) {
	t.Helper()

	if _, err := run.reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

// handedBack fails unless the pass just run dropped the claim and handed the
// database Lease back, and, on a deleted resource, unless the passes after it
// let the resource go.
func handedBack(t *testing.T, run unstartedMigration, deleted bool, when string) {
	t.Helper()

	stored := &operatorv1alpha1.PtahMigration{}
	err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.migration), stored)
	switch {
	case apierrors.IsNotFound(err) && deleted:
	case err != nil:
		t.Fatal(err)
	case stored.Status.ActiveOperation != nil:
		t.Fatalf("the claim stayed %s: %#v", when, stored.Status.ActiveOperation)
	case !deleted && stored.Status.UnresolvedRun == nil:
		t.Fatalf("the claim went %s without recording the run Unknown", when)
	}
	assertDatabaseHandedBack(t, run.reconciler, run.api)
	if !deleted {
		return
	}
	for range 3 {
		if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.migration), stored); apierrors.IsNotFound(err) {
			return
		}
		reconcileMigrationOrGone(t, run)
	}
	t.Fatalf("the deletion did not complete %s: finalizers %q", when, stored.Finalizers)
}

// applyPodGoneEvents is every ApplyPodGone Event recorded since the last call.
func applyPodGoneEvents(recorder *record.FakeRecorder) []string {
	var found []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, applyPodGoneEvent) {
				found = append(found, event)
			}
		default:
			return found
		}
	}
}
