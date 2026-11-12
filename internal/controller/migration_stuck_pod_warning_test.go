package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
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
			// The Job is named by the UID the claim recorded, which a Job that
			// took the name does not have.
			want := fmt.Sprintf("%sDeletion waits for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
				"The database Lease stays held while the Pod may still run. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				stuckPodEvent, pod.Name, run.job.Name, run.job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
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

// A live PtahMigration supervises its Apply until the Job stops, and renews the
// database Lease for as long, so a Pod on a node that is gone holds it with no
// bound, as it holds a live PtahSchema. The claim names the Pod past the Job's
// stop bound in the same Warning, worded for an Apply, and keeps the claim and
// the Lease.
func TestALiveMigrationApplyWaitingOnAStuckPodNamesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run := unstartedMigrationApplyRun(t)
	claim := readMigration(t, run.api, run.migration).Status.ActiveOperation
	if claim == nil || claim.JobUID != run.job.UID {
		t.Fatalf("the run is not an Apply claim that recorded its Job: %#v", claim)
	}
	holder := leaseHolder(migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest))
	if holder == "" {
		t.Fatal("the Apply holds no database Lease, so the row cannot show it kept one")
	}
	job, _ := jobPodStuckOnAGoneNode(t, run.api, run.job, run.pod)
	bound := jobStopBound(job)
	pod := &corev1.Pod{}
	if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%sThe Apply waits for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
		"The database Lease stays held while the Pod may still run. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		stuckPodEvent, pod.Name, job.Name, job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
	stuckPodEvents(run.recorder)

	for _, step := range []struct {
		at    time.Duration
		named bool
	}{
		{at: -time.Second},
		{at: 0, named: true},
		{at: 0},
		{at: 29 * time.Second},
		{at: 30 * time.Second, named: true},
	} {
		at := bound.Add(step.at)
		if !podStuckTerminating(pod, at) {
			t.Fatalf("the Pod is not stuck terminating %s from the Job's stop bound, so the step measures nothing", step.at)
		}
		run.reconciler.Clock = func() time.Time { return at }
		supervising := reconcileMigration(t, run.reconciler, run.api, run.migration)
		events := stuckPodEvents(run.recorder)
		switch {
		case !step.named && len(events) != 0:
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want nothing", step.at, events)
		case step.named && (len(events) != 1 || events[0] != want):
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want one %q", step.at, events, want)
		}
		if supervising.Status.ActiveOperation == nil || supervising.Status.ActiveOperation.ID != claim.ID {
			t.Fatalf("the Apply stopped supervising its Job %s from the Job's stop bound while the Pod may still run: %#v",
				step.at, supervising.Status.ActiveOperation)
		}
		lease := migrationDatabaseLease(t, run.api, run.reconciler.LockNamespace, claim.CoordinationDigest)
		if held := leaseHolder(lease); held != holder {
			t.Fatalf("the database Lease is held by %q %s from the Job's stop bound, want the Apply's %q", held, step.at, holder)
		}
	}
}

// A live migration read is supervised by the same branch while its Job runs,
// and is never named in that Warning: a read's Job is let go of at its stop
// bound once the Job controller marks it to fail (readJobStopOverdue), and
// ReadJobOverdue names its Pod then. The row's Job is past its stop bound
// without that mark, as a Job controller that has not acted yet leaves it, so
// the read is still supervised over a Pod stuck terminating.
func TestALiveMigrationReadNamesNoStuckPodAsAnApplys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	builder := workloadBuilderForMigrations()
	dispatched := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: builder, recorded: true})
	reconciler, api := dispatched.reconciler(t, builder)
	recorder := record.NewFakeRecorder(256)
	reconciler.Recorder = recorder
	job, _ := jobPodStuckOnAGoneNode(t, api, dispatched.job, dispatched.pod)
	// The Job controller has not marked the deadline: the Job carries no
	// FailureTarget, and its stop bound is its deadline from its start.
	job.Status.Conditions = nil
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(dispatched.pod), pod); err != nil {
		t.Fatal(err)
	}
	past := jobStopBound(job).Add(time.Minute)
	if !podStuckTerminating(pod, past) || readJobStopOverdue(job, past) {
		t.Fatalf("at %s the Pod is stuck: %t, and the read is let go of: %t; want a stuck Pod under a read still supervised",
			past, podStuckTerminating(pod, past), readJobStopOverdue(job, past))
	}
	reconciler.Clock = func() time.Time { return past }
	stuckPodEvents(recorder)
	supervising := reconcileMigration(t, reconciler, api, dispatched.migration)
	if events := stuckPodEvents(recorder); len(events) != 0 {
		t.Fatalf("a read past its Job's stop bound recorded %q, want no PodStuckTerminating Warning", events)
	}
	if supervising.Status.ActiveOperation == nil || supervising.Status.ActiveOperation.JobUID != dispatched.job.UID {
		t.Fatalf("the read stopped supervising its Job, so the row read no wait: %#v", supervising.Status.ActiveOperation)
	}
}

// heldUnknownMigrationApply is an Apply whose run was recorded Unknown while
// its Job ran, an execution component having changed, so the claim is kept,
// the run is recorded, and the database Lease is renewed until the Job stops
// (reconcileUnaccountedMigrationApply).
func heldUnknownMigrationApply(t *testing.T) (
	dispatchedRun, *MigrationReconciler, client.WithWatch, *record.FakeRecorder, *operatorv1alpha1.MigrationOperationStatus, string,
) {
	t.Helper()

	run, reconciler, api, recorder := executorRolledOut().reach(t)
	held := readMigration(t, api, run.migration)
	claim := held.Status.ActiveOperation
	if claim == nil || claim.JobUID != run.job.UID || held.Status.UnresolvedRun == nil ||
		held.Status.UnresolvedRun.OperationID != claim.ID {
		t.Fatalf("the run is not an Apply recorded Unknown over its kept claim: claim %#v, unresolved %#v",
			claim, held.Status.UnresolvedRun)
	}
	holder := leaseHolder(migrationDatabaseLease(t, api, reconciler.LockNamespace, claim.CoordinationDigest))
	if holder == "" {
		t.Fatal("the kept claim holds no database Lease, so the row cannot show it kept one")
	}
	return run, reconciler, api, recorder, claim, holder
}

// A PtahMigration that recorded its Apply's run Unknown while the Job ran keeps
// the claim, and renews the database Lease, until the Job stops. That wait comes
// ahead of deletion, so it is the one a live and a deleted migration both meet,
// and a Pod on a node that is gone holds it with no bound. Past the Job's stop
// bound the claim names the Pod in the Warning every other Apply wait records,
// in the words of the resource that waits.
func TestAMigrationApplyRecordedUnknownWaitingOnAStuckPodNamesIt(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		deleted bool
		waiter  string
	}{
		{name: "on a live migration", waiter: "The Apply waits"},
		{name: "on a deleted migration", deleted: true, waiter: "Deletion waits"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder, claim, holder := heldUnknownMigrationApply(t)
			job, _ := jobPodStuckOnAGoneNode(t, api, run.job, run.pod)
			if row.deleted {
				if err := api.Delete(ctx, readMigration(t, api, run.migration)); err != nil {
					t.Fatal(err)
				}
				if readMigration(t, api, run.migration).DeletionTimestamp == nil {
					t.Fatal("the migration is not being deleted, so the row reads no deletion")
				}
			}
			bound := jobStopBound(job)
			pod := &corev1.Pod{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%s%s for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
				"The database Lease stays held while the Pod may still run. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				stuckPodEvent, row.waiter, pod.Name, job.Name, job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339),
				pod.Namespace, pod.Name)
			stuckPodEvents(recorder)

			for _, step := range []struct {
				at    time.Duration
				named bool
			}{
				{at: -time.Second},
				{at: 0, named: true},
				{at: 0},
				{at: 29 * time.Second},
				{at: 30 * time.Second, named: true},
			} {
				at := bound.Add(step.at)
				if !podStuckTerminating(pod, at) {
					t.Fatalf("the Pod is not stuck terminating %s from the Job's stop bound, so the step measures nothing", step.at)
				}
				reconciler.Clock = func() time.Time { return at }
				waiting := reconcileMigration(t, reconciler, api, run.migration)
				events := stuckPodEvents(recorder)
				switch {
				case !step.named && len(events) != 0:
					t.Fatalf("a pass %s from the Job's stop bound recorded %q, want nothing", step.at, events)
				case step.named && (len(events) != 1 || events[0] != want):
					t.Fatalf("a pass %s from the Job's stop bound recorded %q, want one %q", step.at, events, want)
				}
				if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != claim.ID ||
					waiting.Status.UnresolvedRun == nil || waiting.Status.UnresolvedRun.OperationID != claim.ID {
					t.Fatalf("the claim went %s from the Job's stop bound while the Pod may still run: claim %#v, unresolved %#v",
						step.at, waiting.Status.ActiveOperation, waiting.Status.UnresolvedRun)
				}
				lease := migrationDatabaseLease(t, api, reconciler.LockNamespace, claim.CoordinationDigest)
				if held := leaseHolder(lease); held != holder {
					t.Fatalf("the database Lease is held by %q %s from the Job's stop bound, want the claim's %q", held, step.at, holder)
				}
			}
		})
	}
}

// The same wait over a Job that stands, has not reached its stop bound, and
// owns a Pod someone deleted early on a node that is gone. That Pod is past its
// own deletion grace, and the Job can still stop it by its deadline, so it is
// not named yet: a standing Job's Pods are named only once the Job is past its
// stop bound, as on a deletion.
func TestAMigrationApplyRecordedUnknownNamesNoPodBeforeItsJobsStopBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, recorder, claim, _ := heldUnknownMigrationApply(t)
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = "node-gone"
	pod.Finalizers = append(pod.Finalizers, "example.invalid/kubelet-confirmation")
	if err := api.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	runningExecutorPod(pod)
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	// The Job started as the Pod was deleted, so its own deadline is still
	// ahead.
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	active := int32(1)
	job.Status = batchv1.JobStatus{StartTime: pod.DeletionTimestamp.DeepCopy(), Active: active}
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	at := pod.DeletionTimestamp.Add(time.Minute)
	if !podStuckTerminating(pod, at) || jobStopOverdue(job, at) != nil {
		t.Fatalf("at %s the Pod is stuck: %t, and the Job is past its stop bound %s: %t; want a stuck Pod under a Job that can still stop it",
			at, podStuckTerminating(pod, at), jobStopBound(job), jobStopOverdue(job, at) != nil)
	}
	reconciler.Clock = func() time.Time { return at }
	stuckPodEvents(recorder)
	waiting := reconcileMigration(t, reconciler, api, run.migration)
	if events := stuckPodEvents(recorder); len(events) != 0 {
		t.Fatalf("a pass before the Job's stop bound recorded %q, want no PodStuckTerminating Warning", events)
	}
	if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != claim.ID {
		t.Fatalf("the claim went while its Job runs, so the row read no wait: %#v", waiting.Status.ActiveOperation)
	}
}
