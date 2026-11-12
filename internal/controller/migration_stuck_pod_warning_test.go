package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A deleted PtahMigration waits for its Apply until nothing it dispatched can
// still write (dispatchedApplyMayStillWrite), and renews the database Lease
// while it waits, so a Pod on a node that is gone holds the resource with no
// bound, as it holds a deleted PtahSchema. The deletion names the Pod in a
// PodStuckTerminating Warning, with the same backoff: past the Job's stop bound
// while the claim's Job stands, and past the Pod's own stop bound
// (podStopBound) once a person deleted the Job or another Job took its name,
// since the wait lists the Pods by the recorded UID on their owner reference.
// The fake client runs no Job controller and no kubelet, so each row writes
// what a supported cluster shows past the Job's deadline.
func TestADeletedMigrationWaitingOnAStuckPodNamesIt(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// gone deletes the claim's Job, and replace then puts another Job
		// under its name.
		gone, replace bool
		// unrecorded clears the UID the claim recorded, as a create whose
		// answer was lost after it persisted leaves the claim, so the wait
		// knows the Job only by the name it reserved.
		unrecorded bool
	}{
		{name: "under the claim's Job"},
		{name: "under a claim that recorded no Job UID", unrecorded: true},
		{name: "after the Job was deleted", gone: true},
		{name: "under a Job that took its name", gone: true, replace: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := unstartedMigrationApplyRun(t)
			if err := run.api.Delete(ctx, readMigration(t, run.api, run.migration)); err != nil {
				t.Fatal(err)
			}
			claim := reconcileMigration(t, run.reconciler, run.api, run.migration).Status.ActiveOperation
			if claim == nil || claim.JobUID != run.job.UID {
				t.Fatalf("the deletion did not wait for its running Apply: claim %#v", claim)
			}
			holder := leaseHolder(migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest))
			if holder == "" {
				t.Fatal("the Apply holds no database Lease, so the row cannot show the deletion kept it")
			}
			if row.unrecorded {
				stored := readMigration(t, run.api, run.migration)
				stored.Status.ActiveOperation.JobUID = ""
				if err := run.api.Status().Update(ctx, stored); err != nil {
					t.Fatal(err)
				}
			}
			job, _ := jobPodStuckOnAGoneNode(t, run.api, run.job, run.pod)
			bound := jobStopBound(job)
			pod := &corev1.Pod{}
			if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}
			if row.gone {
				// The Pod started ten seconds after its Job, so the Job
				// controller deleted it at the Job's deadline, before its own,
				// and the end of its deletion grace is its bound.
				started := metav1.NewTime(job.Status.StartTime.Add(10 * time.Second))
				pod.Status.StartTime = &started
				if err := run.api.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if err := run.api.Delete(ctx, job.DeepCopy()); err != nil {
					t.Fatal(err)
				}
				bound = podStopBound(pod)
				if want := pod.DeletionTimestamp.Add(jobStopReportSlack); !bound.Equal(want) {
					t.Fatalf("the Pod's stop bound is %s, want the end of its deletion grace and the slack, %s", bound, want)
				}
			}
			if row.replace {
				later := job.DeepCopy()
				later.ResourceVersion, later.UID = "", "later-job-uid"
				later.Status = batchv1.JobStatus{}
				if err := run.api.Create(ctx, later); err != nil {
					t.Fatal(err)
				}
			}
			want := fmt.Sprintf("%sDeletion waits for Pod %s of Job %s on node node-gone: the Pod was to be gone by %s and has not stopped. "+
				"The database Lease stays held while the Pod may still run. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				stuckPodEvent, pod.Name, job.Name, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
			stuckPodEvents(run.recorder)

			// The Pod is stuck terminating from the end of its grace, thirty
			// seconds before either bound, so the first step separates the
			// bound from the stuck Pod alone. The steps after it are the
			// backoff: thirty seconds, then a minute more.
			for _, step := range []struct {
				at    time.Duration
				named bool
			}{
				{at: -time.Second},
				{at: 0, named: true},
				{at: 0},
				{at: 29 * time.Second},
				{at: 30 * time.Second, named: true},
				{at: 89 * time.Second},
				{at: 90 * time.Second, named: true},
			} {
				at := bound.Add(step.at)
				if !podStuckTerminating(pod, at) {
					t.Fatalf("the Pod is not stuck terminating %s from the bound, so the step measures nothing", step.at)
				}
				run.reconciler.Clock = func() time.Time { return at }
				waiting := reconcileMigration(t, run.reconciler, run.api, run.migration)
				events := stuckPodEvents(run.recorder)
				switch {
				case !step.named && len(events) != 0:
					t.Fatalf("a pass %s from the bound recorded %q, want nothing", step.at, events)
				case step.named && (len(events) != 1 || events[0] != want):
					t.Fatalf("a pass %s from the bound recorded %q, want one %q", step.at, events, want)
				}
				if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != claim.ID {
					t.Fatalf("the deletion stopped waiting %s from the bound while the Pod may still run: claim %#v",
						step.at, waiting.Status.ActiveOperation)
				}
				if row.unrecorded && waiting.Status.ActiveOperation.JobUID != "" {
					t.Fatalf("the claim recorded Job UID %q %s from the bound, so the row no longer measures a claim that recorded none",
						waiting.Status.ActiveOperation.JobUID, step.at)
				}
				lease := migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest)
				if held := leaseHolder(lease); held != holder {
					t.Fatalf("the database Lease is held by %q %s from the bound, want the Apply's %q", held, step.at, holder)
				}
			}
		})
	}
}

// A deleted PtahMigration names no Pod of its Apply that is not stuck: one
// still running that nothing deleted, past the Job's stop bound, and one
// deleted that has stopped. The second is no longer waited for at all.
func TestADeletedMigrationNamesNoPodThatIsNotStuck(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		pod  func(t *testing.T, run unstartedMigration)
		// waits is a Pod the deletion still waits for.
		waits bool
	}{
		{name: "a running Pod nothing deleted", waits: true, pod: func(t *testing.T, run unstartedMigration) {
			firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			job := &batchv1.Job{}
			if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.job), job); err != nil {
				t.Fatal(err)
			}
			started := metav1.NewTime(run.reconciler.now().Add(-time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second))
			job.Status = batchv1.JobStatus{Active: 1, StartTime: &started}
			if err := run.api.Status().Update(context.Background(), job); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a deleted Pod that failed", pod: func(t *testing.T, run unstartedMigration) {
			jobPodStuckOnAGoneNode(t, run.api, run.job, run.pod)
			stored := &corev1.Pod{}
			if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.pod), stored); err != nil {
				t.Fatal(err)
			}
			stored.Status.Phase = corev1.PodFailed
			if err := run.api.Status().Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := unstartedMigrationApplyRun(t)
			if err := run.api.Delete(context.Background(), readMigration(t, run.api, run.migration)); err != nil {
				t.Fatal(err)
			}
			if waiting := reconcileMigration(t, run.reconciler, run.api, run.migration); waiting.Status.ActiveOperation == nil {
				t.Fatal("the deletion did not wait for its running Apply")
			}
			row.pod(t, run)
			job := &batchv1.Job{}
			if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.job), job); err != nil {
				t.Fatal(err)
			}
			past := jobStopBound(job).Add(time.Minute)
			if !past.After(run.reconciler.now()) {
				t.Fatalf("the Job's stop bound %s is not a time the row can pass", past)
			}
			run.reconciler.Clock = func() time.Time { return past }
			stuckPodEvents(run.recorder)
			waiting := reconcileMigration(t, run.reconciler, run.api, run.migration)
			if events := stuckPodEvents(run.recorder); len(events) != 0 {
				t.Fatalf("a pass past the Job's stop bound recorded %q for a Pod that is not stuck", events)
			}
			if row.waits && waiting.Status.ActiveOperation == nil {
				t.Fatal("the deletion stopped waiting for a Job that has not finished, so the row read no wait")
			}
		})
	}
}
