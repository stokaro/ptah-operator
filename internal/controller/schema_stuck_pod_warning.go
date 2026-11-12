package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A deleted PtahSchema waits for the Job of an Apply, and renews the Lease
// while it waits. Since Kubernetes 1.31 a Job past its deadline is marked
// Failed only once its Pods have terminated, and a Pod on a node that is gone
// stays Terminating until the node object goes or somebody force-deletes the
// Pod. Holding the Lease is right, since such a Pod may still be running SQL on
// a node that is only cut off, but nothing else says why the resource stays.
// warnStuckPods says it: a PodStuckTerminating Warning names each such Pod, its
// node, and the delete that ends the wait. A read that holds the lock, a Plan
// or the read that proves an Apply, is waited for only until jobStopBound,
// where the deletion discards its claim and says so in a ReadJobOverdue
// Warning instead (#997).
//
// It warns only once the Job is past jobStopBound, its own deadline plus the
// grace its Pods are given and the slack, when the Job controller has already
// deleted every Pod it was going to stop. Before that the Job may still stop
// by itself, and nothing is listed. A Pod deleted later, or with a longer grace
// than its template's, is named only once its own grace has run out
// (podStuckTerminating).
//
// The deletion that waits on the Apply a pending observation names, after the
// claim was retired, has no bound either while that Pod stays, so it names the
// Pod the same way (warnStuckApplyPods).
//
// The Warning goes through the Event recorder, whose spam filter gives each
// object a burst of 25 Events of a type and then one every five minutes, so a
// pass every five seconds would spend that burst in two minutes and starve every
// other Warning about the schema. The Warning about one Pod is repeated twice as
// long after each time, from thirty seconds to five minutes, as the result
// collector's ResourceQuota Warning is.
const (
	stuckPodWarningFirst = 30 * time.Second
	stuckPodWarningMax   = 5 * time.Minute
	// maxStuckPodWarnings bounds the table. A Pod past it is warned about on
	// every pass, which the recorder's spam filter then bounds.
	maxStuckPodWarnings = 1024
)

// stuckPodWarnings remembers, per Pod, when the next PodStuckTerminating
// Warning about it is due, and how long the last wait was. It is kept in memory:
// a manager restart warns again at once, and authorizes nothing.
type stuckPodWarnings struct {
	mu   sync.Mutex
	next map[types.UID]stuckPodWarning
}

type stuckPodWarning struct {
	due    time.Time
	waited time.Duration
}

// stuckPodWarningsMade guards making a reconciler's stuckPodWarnings on first
// use, as resultLogFailuresMade does for resultLogFailures.
var stuckPodWarningsMade sync.Mutex

func stuckPodWarningsOf(field **stuckPodWarnings) *stuckPodWarnings {
	stuckPodWarningsMade.Lock()
	defer stuckPodWarningsMade.Unlock()
	if *field == nil {
		*field = &stuckPodWarnings{}
	}
	return *field
}

// due reports whether a Warning about pod is due at now, and when it is,
// records when the next one is.
func (w *stuckPodWarnings) due(pod types.UID, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.next == nil {
		w.next = map[types.UID]stuckPodWarning{}
	}
	last, seen := w.next[pod]
	if seen && now.Before(last.due) {
		return false
	}
	if !seen && len(w.next) >= maxStuckPodWarnings {
		// A Pod still stuck is looked at every pass, which moves its entry
		// as soon as it is due. One overdue by more than the longest wait
		// is a Pod nothing looks at any more.
		for uid, entry := range w.next {
			if now.Sub(entry.due) > stuckPodWarningMax {
				delete(w.next, uid)
			}
		}
	}
	if seen || len(w.next) < maxStuckPodWarnings {
		waited := min(max(2*last.waited, stuckPodWarningFirst), stuckPodWarningMax)
		w.next[pod] = stuckPodWarning{due: now.Add(waited), waited: waited}
	}
	return true
}

// podStuckTerminating is a Pod the API server is deleting whose grace period
// has run out by now and that has not stopped: the kubelet has not reported it
// Succeeded or Failed. A Pod's deletionTimestamp is the instant its grace ends,
// so before it the Pod is still being stopped.
func podStuckTerminating(pod *corev1.Pod, now time.Time) bool {
	return pod.DeletionTimestamp != nil && !now.Before(pod.DeletionTimestamp.Time) &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// stuckPodWarningText is what the PodStuckTerminating Warning says about pod,
// one of job's Pods.
func stuckPodWarningText(job *batchv1.Job, pod *corev1.Pod) string {
	node := ""
	if pod.Spec.NodeName != "" {
		node = " on node " + pod.Spec.NodeName
	}
	return fmt.Sprintf("Deletion waits for Pod %s of Job %s%s: the Pod was to be gone by %s and has not stopped. "+
		"The database Lease stays held while the Pod may still run. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		pod.Name, job.Name, node, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
}

// warnStuckPods records a PodStuckTerminating Warning for each Pod of job that
// is stuck terminating once job is past jobStopBound, at most as often as
// stuckPodWarnings allows. A failed list is logged by the caller and warns
// about nothing: the wait goes on either way.
func (r *SchemaReconciler) warnStuckPods(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job) error {
	now := r.now()
	if jobStopOverdue(job, now) == nil {
		return nil
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, job.Name, job.UID)
	if err != nil {
		return err
	}
	r.warnAboutStuckPods(schema, job, pods, now)
	return nil
}

// warnStuckApplyPods is warnStuckPods for the Apply Job a pending observation
// names, over the Pods the wait already listed for it. The Job is read only
// when one of them is stuck, directly as the claim's wait reads its Job, and a
// Job that is gone, or is not the one the record names, bounds nothing, so
// nothing is said.
func (r *SchemaReconciler) warnStuckApplyPods(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	pods []*corev1.Pod,
) error {
	now := r.now()
	if !slices.ContainsFunc(pods, func(pod *corev1.Pod) bool { return podStuckTerminating(pod, now) }) {
		return nil
	}
	job := &batchv1.Job{}
	if err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: pending.ApplyJobName}, job); err != nil {
		return client.IgnoreNotFound(err)
	}
	if job.UID != pending.ApplyJobUID || jobStopOverdue(job, now) == nil {
		return nil
	}
	r.warnAboutStuckPods(schema, job, pods, now)
	return nil
}

// warnAboutStuckPods records the Warning about each of job's pods that is stuck
// terminating at now, at most as often as stuckPodWarnings allows.
func (r *SchemaReconciler) warnAboutStuckPods(schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, pods []*corev1.Pod, now time.Time) {
	warnings := stuckPodWarningsOf(&r.stuckPods)
	for _, pod := range pods {
		if podStuckTerminating(pod, now) && warnings.due(pod.UID, now) {
			r.event(schema, corev1.EventTypeWarning, "PodStuckTerminating", "%s", stuckPodWarningText(job, pod))
		}
	}
}
