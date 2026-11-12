package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A deleted PtahSchema waits for its Apply's Job, and a Pod on a node that is
// gone keeps that Job from finishing (#994). The fake client runs no Job
// controller and no kubelet: each row writes what a supported cluster shows past
// the Job's deadline, a Job carrying FailureTarget whose Pod the Job controller
// deleted and no kubelet has reported stopped.

const stuckPodEvent = "Warning PodStuckTerminating "

// stuckOnAGoneNode leaves the run's Pod deleted and still Running on a node
// that does not answer, and the Job past its deadline: FailureTarget, with the
// Pod counted as terminating. The finalizer keeps the deleted Pod readable, as
// the kubelet's missing confirmation does on a cluster.
//
// The Job controller deletes the Pod when it marks the deadline, and the API
// server sets the Pod's deletionTimestamp to the end of its grace. The fake
// client stamps the delete's own instant, by the wall clock, and keeps the
// field immutable, so the Job is dated to that stamp instead: it failed the
// template's grace before the Pod was to be gone. It returns the Job as stored
// and when it failed.
func stuckOnAGoneNode(t *testing.T, run unstartedSchema) (*batchv1.Job, time.Time) {
	t.Helper()

	return jobPodStuckOnAGoneNode(t, run.api, run.job, run.pod)
}

// jobPodStuckOnAGoneNode is stuckOnAGoneNode for any Job and its Pod.
func jobPodStuckOnAGoneNode(t *testing.T, api client.Client, dispatched *batchv1.Job, dispatchedPod *corev1.Pod) (*batchv1.Job, time.Time) {
	t.Helper()

	ctx := context.Background()
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(dispatchedPod), pod); err != nil {
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
	if err := api.Get(ctx, client.ObjectKeyFromObject(dispatchedPod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.DeletionTimestamp == nil {
		t.Fatal("the Pod is not being deleted, so it is not the Pod the deadline leaves")
	}
	runningExecutorPod(pod)
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(dispatched), job); err != nil {
		t.Fatal(err)
	}
	failed := pod.DeletionTimestamp.Add(-time.Duration(*job.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second)
	started := metav1.NewTime(failed.Add(-time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second))
	terminating := int32(1)
	job.Status = batchv1.JobStatus{StartTime: &started, Terminating: &terminating, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
		LastTransitionTime: metav1.NewTime(failed),
	}}}
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job, failed
}

// stuckPodEvents is every PodStuckTerminating Event recorded since the last
// call.
func stuckPodEvents(recorder *record.FakeRecorder) []string {
	var found []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, stuckPodEvent) {
				found = append(found, event)
			}
		default:
			return found
		}
	}
}

func TestADeletedSchemaWaitingOnAStuckPodNamesIt(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	job, failed := stuckOnAGoneNode(t, run)
	bound := jobStopBound(job)
	if !bound.After(failed) {
		t.Fatalf("the Job's stop bound %s is not after its FailureTarget %s, so no pass can come before it", bound, failed)
	}
	pod := &corev1.Pod{}
	if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%sDeletion waits for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
		"The database Lease stays held while the Pod may still run. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		stuckPodEvent, pod.Name, job.Name, job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
	stuckPodEvents(run.recorder)

	// Each step is a pass at an offset from the Job's stop bound, and whether
	// it names the Pod: not before the bound, then after thirty seconds and
	// after a minute more.
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
		run.reconciler.Clock = func() time.Time { return at }
		run.reconcileOnce(t)
		events := stuckPodEvents(run.recorder)
		switch {
		case !step.named && len(events) != 0:
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want nothing", step.at, events)
		case step.named && (len(events) != 1 || events[0] != want):
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want one %q", step.at, events, want)
		}
		if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.ActiveOperation == nil {
			t.Fatalf("the deletion stopped waiting %s from the Job's stop bound while the Pod may still run", step.at)
		}
	}
}

// A live schema supervises its Apply until the Job stops, and renews the
// database Lease for as long, so a Pod on a node that is gone holds it with no
// bound, as it holds a deleted schema. The claim names the Pod past the Job's
// stop bound in the same Warning, worded for an Apply, and keeps the claim and
// the Lease.
func TestALiveApplyWaitingOnAStuckPodNamesIt(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	claim := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation
	if claim == nil || claim.Type != operatorv1alpha1.OperationApply || claim.JobUID != run.job.UID {
		t.Fatalf("the run is not an Apply claim that recorded its Job: %#v", claim)
	}
	holder := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace))
	if holder == "" {
		t.Fatal("the Apply holds no database Lease, so the row cannot show it kept one")
	}
	job, _ := stuckOnAGoneNode(t, run)
	bound := jobStopBound(job)
	pod := &corev1.Pod{}
	if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.pod), pod); err != nil {
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
		run.reconcileOnce(t)
		events := stuckPodEvents(run.recorder)
		switch {
		case !step.named && len(events) != 0:
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want nothing", step.at, events)
		case step.named && (len(events) != 1 || events[0] != want):
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want one %q", step.at, events, want)
		}
		supervising := safetyGetSchema(t, run.api, run.schema)
		if supervising.Status.ActiveOperation == nil || supervising.Status.ActiveOperation.ID != claim.ID {
			t.Fatalf("the Apply stopped supervising its Job %s from the Job's stop bound while the Pod may still run: %#v",
				step.at, supervising.Status.ActiveOperation)
		}
		if held := leaseHolder(testDatabaseLease(t, run.api, run.reconciler.LockNamespace)); held != holder {
			t.Fatalf("the database Lease is held by %q %s from the Job's stop bound, want the Apply's %q", held, step.at, holder)
		}
	}
}

// The Pod a live Apply warned about is the Pod the deletion that follows waits
// for. The deletion's first Warning says something the Apply's did not, that
// the deletion now waits, so it is due at once, not when the Apply's next one
// would have been: each wait keeps its own backoff for the Pod. The deletion's
// backoff then runs from its own first Warning.
func TestADeletionAfterALiveApplysWarningNamesItsPodAtOnce(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
	job, _ := stuckOnAGoneNode(t, run)
	bound := jobStopBound(job)
	pod := &corev1.Pod{}
	if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	text := func(waiter string) string {
		return fmt.Sprintf("%s%s for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
			"The database Lease stays held while the Pod may still run. "+
			"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
			stuckPodEvent, waiter, pod.Name, job.Name, job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
	}
	stuckPodEvents(run.recorder)

	for _, step := range []struct {
		at time.Duration
		// deleted is a schema deleted before this pass.
		deleted bool
		want    string
	}{
		{at: 0, want: text("The Apply waits")},
		{at: time.Second, deleted: true, want: text("Deletion waits")},
		{at: 2 * time.Second},
		// The Apply's next Warning would be due here, and the deletion's
		// is not yet.
		{at: 30 * time.Second},
		{at: 31 * time.Second, want: text("Deletion waits")},
	} {
		if step.deleted {
			if err := run.api.Delete(context.Background(), safetyGetSchema(t, run.api, run.schema)); err != nil {
				t.Fatal(err)
			}
		}
		at := bound.Add(step.at)
		run.reconciler.Clock = func() time.Time { return at }
		run.reconcileOnce(t)
		switch events := stuckPodEvents(run.recorder); {
		case step.want == "" && len(events) != 0:
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want nothing", step.at, events)
		case step.want != "" && (len(events) != 1 || events[0] != step.want):
			t.Fatalf("a pass %s from the Job's stop bound recorded %q, want one %q", step.at, events, step.want)
		}
		if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.ActiveOperation == nil {
			t.Fatalf("the wait ended %s from the Job's stop bound while the Pod may still run", step.at)
		}
	}
}

// A live Apply past its Job's stop bound names no Pod that nothing deleted. A
// read is never named (TestASchemaReadStopsSupervisingAFailingJobAtItsStopBound).
func TestALiveApplyNamesNoPodNothingDeleted(t *testing.T) {
	t.Parallel()

	run := admittedUnstartedSchemaApplyRun(t)
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
	past := jobStopBound(job).Add(time.Minute)
	if !past.After(run.reconciler.now()) {
		t.Fatalf("the Job's stop bound %s is not a time the row can pass", past)
	}
	run.reconciler.Clock = func() time.Time { return past }
	stuckPodEvents(run.recorder)
	run.reconcileOnce(t)
	if events := stuckPodEvents(run.recorder); len(events) != 0 {
		t.Fatalf("a pass past the Job's stop bound recorded %q for a running Pod nothing deleted", events)
	}
	if supervising := safetyGetSchema(t, run.api, run.schema); supervising.Status.ActiveOperation == nil {
		t.Fatal("the Apply stopped supervising a Job that has not finished, so the row read no wait")
	}
}

// A deletion past the Job's stop bound names no Pod that is not stuck: one
// still running that nothing deleted, and one deleted that has stopped.
func TestADeletedSchemaNamesNoPodThatIsNotStuck(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		pod  func(t *testing.T, run unstartedSchema)
	}{
		{name: "a running Pod nothing deleted", pod: func(t *testing.T, run unstartedSchema) {
			firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			// The Job's deadline has passed and nothing has acted on it.
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
		{name: "a deleted Pod that failed", pod: func(t *testing.T, run unstartedSchema) {
			stuckOnAGoneNode(t, run)
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

			run := admittedUnstartedSchemaApplyRun(t)
			deleteSchemaWhileItsApplyRuns(t, run)
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
			run.reconcileOnce(t)
			if events := stuckPodEvents(run.recorder); len(events) != 0 {
				t.Fatalf("a pass past the Job's stop bound recorded %q for a Pod that is not stuck", events)
			}
			if waiting := safetyGetSchema(t, run.api, run.schema); waiting.Status.ActiveOperation == nil {
				t.Fatal("the deletion stopped waiting for a Job that has not finished, so the row read no wait")
			}
		})
	}
}

// Past the Job's stop bound only a Pod the API server is deleting, whose grace
// has run out and that has not stopped, is named. A Pod still running and not
// deleted is not stuck terminating, a Pod inside its grace is still being
// stopped, and a stopped Pod held by a finalizer waits only on the Job
// controller.
func TestOnlyAPodThatIsDeletedAndHasNotStoppedIsNamed(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		pod  func(pod *corev1.Pod)
		// early is how long before the end of the Pod's grace the Pod is
		// looked at.
		early time.Duration
		named bool
	}{
		{name: "a deleted Pod still running", named: true, pod: func(*corev1.Pod) {}},
		{name: "a deleted Pod still running inside its grace", early: time.Second, pod: func(*corev1.Pod) {}},
		{name: "a deleted Pod still pending", named: true, pod: func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodPending }},
		{name: "a deleted Pod that failed", pod: func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed }},
		{name: "a deleted Pod that succeeded", pod: func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded }},
		{name: "a running Pod nothing deleted", pod: func(pod *corev1.Pod) { pod.DeletionTimestamp = nil }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			goneBy := metav1.NewTime(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &goneBy}}
			runningExecutorPod(pod)
			row.pod(pod)
			if got := podStuckTerminating(pod, goneBy.Add(-row.early)); got != row.named {
				t.Fatalf("podStuckTerminating() = %t, want %t", got, row.named)
			}
		})
	}
}

// A Pod deleted after its Job's stop bound, or with a longer grace than its
// template's, is still being stopped until its own grace runs out, which its
// deletionTimestamp records. It is named from then and not before, whatever the
// Job's bound says.
func TestAPodIsNamedOnlyOnceItsOwnGraceRunsOut(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	deadline, grace := int64(600), int64(30)
	started := metav1.NewTime(now.Add(-time.Hour))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-apply-app-1", UID: "job-uid", CreationTimestamp: started},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &deadline,
			Template:              corev1.PodTemplateSpec{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace}},
		},
		Status: batchv1.JobStatus{StartTime: &started},
	}
	if jobStopOverdue(job, now) == nil {
		t.Fatal("the Job is not past its stop bound, so the row measures nothing past the bound")
	}
	// Deleted a second ago with a grace of 600 seconds.
	goneBy := metav1.NewTime(now.Add(599 * time.Second))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: job.Name + "-abc12", UID: "pod-uid",
		DeletionTimestamp: &goneBy, Finalizers: []string{"example.invalid/kubelet-confirmation"},
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}, Spec: corev1.PodSpec{NodeName: "node-gone"}}
	runningExecutorPod(pod)
	api := fake.NewClientBuilder().WithScheme(jobAndPodScheme(t)).WithObjects(pod).Build()
	recorder := record.NewFakeRecorder(8)
	at := now
	r := &SchemaReconciler{APIReader: api, Recorder: recorder, Clock: func() time.Time { return at }}
	schema := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: "app"}}
	for _, step := range []struct {
		at    time.Time
		named bool
	}{
		{at: now},
		{at: goneBy.Add(-time.Second)},
		{at: goneBy.Time, named: true},
	} {
		at = step.at
		if err := r.warnStuckPods(context.Background(), schema, job, deletionWaits); err != nil {
			t.Fatal(err)
		}
		if events := stuckPodEvents(recorder); len(events) != map[bool]int{true: 1, false: 0}[step.named] {
			t.Fatalf("a look %s before the Pod's grace ran out recorded %q, want a Warning: %t",
				goneBy.Sub(step.at), events, step.named)
		}
	}
}

// The deletion of a schema whose Apply ended while its Pod still ran waits on
// that Pod from the pending observation, with no bound either, so it names a Pod
// stuck terminating there as the claim's wait does: past the Job's stop bound
// while the Job the record names stands. The wait lists the Pods by the Job's
// UID on their owner reference, so a Pod still stands, and still holds the
// wait, after a person deleted its Job or another Job took the name. Nothing is
// past that Job's bound then, and the Pod is named once it is past its own
// (podStopBound): the end of its deletion grace and 30 seconds.
//
// A live schema's proof waits on the same Pod before it reads the database,
// with the database Lease renewed all along, and names it the same way, in
// words of its own. The Warning names the Job by the UID the record holds as
// well as by its name: where another Job took the name, `kubectl get job` on
// the name alone shows that Job.
func TestASchemaWaitingOnItsPendingObservationNamesAStuckPod(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// gone deletes the recorded Job, and replace then puts another Job
		// under its name.
		gone, replace bool
		// live leaves the schema in place, so its proof is what waits.
		live bool
	}{
		{name: "a deletion, under the Job the record names"},
		{name: "a deletion, after the Job was deleted", gone: true},
		{name: "a deletion, under a Job that took its name", gone: true, replace: true},
		{name: "a live proof, under the Job the record names", live: true},
		{name: "a live proof, under a Job that took its name", gone: true, replace: true, live: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := admittedUnstartedSchemaApplyRun(t)
			// The words are written out, so a change to the waiters' constants
			// fails these rows.
			waiter := "The Apply's proof waits"
			if !row.live {
				waiter = "Deletion waits"
				deleteSchemaWhileItsApplyRuns(t, run)
			}
			firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
			// One pass records the lost continuity, and the next ends the Apply.
			run.reconcileOnce(t)
			run.reconcileOnce(t)
			if settled := safetyGetSchema(t, run.api, run.schema); settled.Status.ActiveOperation != nil ||
				settled.Status.PendingObservation == nil || settled.Status.PendingObservation.ApplyJobUID != run.job.UID {
				t.Fatalf("the Apply did not end into a pending observation of its Job: claim %#v, pending %#v",
					settled.Status.ActiveOperation, settled.Status.PendingObservation)
			}
			job, _ := stuckOnAGoneNode(t, run)
			bound := jobStopBound(job)
			pod := &corev1.Pod{}
			if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}
			if row.gone {
				// The Pod started ten seconds after its Job, so the Job
				// controller deleted it at the Job's deadline, before its
				// own, and the end of its deletion grace is its bound.
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
				named := &batchv1.Job{}
				if err := run.api.Get(ctx, client.ObjectKeyFromObject(job), named); err != nil || named.UID == run.job.UID {
					t.Fatalf("the name %s holds Job %q (error %v), want the later Job, or the name alone would still say which Job it is",
						job.Name, named.UID, err)
				}
			}
			want := fmt.Sprintf("%s%s for Pod %s of Job %s (UID %s) on node node-gone: the Pod was to be gone by %s and has not stopped. "+
				"The database Lease stays held while the Pod may still run. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				stuckPodEvent, waiter, pod.Name, run.job.Name, run.job.UID, pod.DeletionTimestamp.UTC().Format(time.RFC3339),
				pod.Namespace, pod.Name)
			stuckPodEvents(run.recorder)

			// The Pod is stuck terminating from the end of its grace, thirty
			// seconds before either bound, so the first step separates the
			// bound from the stuck Pod alone. The steps after it are the
			// backoff.
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
					t.Fatalf("the Pod is not stuck terminating %s from the bound, so the step measures nothing", step.at)
				}
				run.reconciler.Clock = func() time.Time { return at }
				run.reconcileOnce(t)
				events := stuckPodEvents(run.recorder)
				switch {
				case !step.named && len(events) != 0:
					t.Fatalf("a pass %s from the bound recorded %q, want nothing", step.at, events)
				case step.named && (len(events) != 1 || events[0] != want):
					t.Fatalf("a pass %s from the bound recorded %q, want one %q", step.at, events, want)
				}
				waiting := safetyGetSchema(t, run.api, run.schema)
				if waiting.Status.PendingObservation == nil || waiting.Status.ActiveOperation != nil {
					t.Fatalf("the wait ended %s from the bound while the Pod may still run: claim %#v, pending %#v",
						step.at, waiting.Status.ActiveOperation, waiting.Status.PendingObservation)
				}
			}
		})
	}
}

// The Warning about one Pod is due at once, then thirty seconds later, and then
// twice as long after each time, up to five minutes.
func TestAStuckPodWarningBacksOffToFiveMinutes(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	warnings := &stuckPodWarnings{}
	var due []time.Duration
	for at := time.Duration(0); at <= 1100*time.Second; at += time.Second {
		if warnings.due("stuck-pod", start.Add(at)) {
			due = append(due, at)
		}
	}
	want := []time.Duration{0, 30 * time.Second, 90 * time.Second, 210 * time.Second, 450 * time.Second, 750 * time.Second, 1050 * time.Second}
	if !slices.Equal(due, want) {
		t.Fatalf("the Warning was due at %v, want %v", due, want)
	}
}

// Each wait keeps its own backoff for a Pod, so a Warning one wait recorded does
// not delay another wait's first one, and does not move its next.
func TestAStuckPodWarningBacksOffForEachWait(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	warnings := &stuckPodWarnings{}
	for _, step := range []struct {
		waiter string
		at     time.Duration
		due    bool
	}{
		{waiter: applyWaits, at: 0, due: true},
		{waiter: deletionWaits, at: time.Second, due: true},
		{waiter: proofWaits, at: 2 * time.Second, due: true},
		{waiter: applyWaits, at: 29 * time.Second},
		{waiter: applyWaits, at: 30 * time.Second, due: true},
		{waiter: deletionWaits, at: 30 * time.Second},
		{waiter: deletionWaits, at: 31 * time.Second, due: true},
	} {
		if due := warnings.dueFor(step.waiter, "stuck-pod", start.Add(step.at)); due != step.due {
			t.Fatalf("%q about the Pod %s in: due %t, want %t", step.waiter, step.at, due, step.due)
		}
	}
}

// The table holds maxStuckPodWarnings Pods. A Pod past that is warned about on
// every pass rather than never, and an entry nothing has asked about since
// longer than the longest wait makes room.
func TestStuckPodWarningsStayBounded(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	warnings := &stuckPodWarnings{}
	for index := range maxStuckPodWarnings {
		if !warnings.due(types.UID(fmt.Sprintf("pod-%d", index)), start) {
			t.Fatalf("the first Warning about Pod %d was not due", index)
		}
	}
	for _, at := range []time.Duration{0, time.Second} {
		if !warnings.due("one-too-many", start.Add(at)) {
			t.Fatalf("a Pod the full table cannot hold was not warned about %s in", at)
		}
	}
	if held := len(warnings.next); held != maxStuckPodWarnings {
		t.Fatalf("the table holds %d Pods, want its bound %d", held, maxStuckPodWarnings)
	}

	// Every entry was due stuckPodWarningFirst after start. Past that by more
	// than the longest wait, nothing has looked at them.
	later := start.Add(stuckPodWarningFirst + stuckPodWarningMax + time.Second)
	if !warnings.due("arrived-later", later) {
		t.Fatal("the first Warning about a new Pod was not due")
	}
	if warnings.due("arrived-later", later) {
		t.Fatal("a new Pod was warned about twice at once, so the stale entries made no room for it")
	}
	if held := len(warnings.next); held != 1 {
		t.Fatalf("the table holds %d Pods after the stale ones went, want the one that arrived", held)
	}
}
