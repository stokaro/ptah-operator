package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A deleted PtahSchema waits for the Job of a claim that holds the database
// lock, and renews the Lease while it waits. Since Kubernetes 1.31 a Job past
// its deadline is marked Failed only once its Pods have terminated, and a Pod
// on a node that is gone stays Terminating until the node object goes or
// somebody force-deletes the Pod. Holding the Lease is right, since such a Pod
// may still be running SQL on a node that is only cut off, but nothing else
// says why the resource stays. warnStuckPods says it: a PodStuckTerminating
// Warning names each such Pod, its node, and the delete that ends the wait.
//
// It warns only once the Job is past jobStopBound, its own deadline plus the
// grace its Pods are given and the slack, when the Job controller has already
// deleted every Pod it was going to stop. Before that the Job may still stop
// by itself, and nothing is listed.
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

// podStuckTerminating is a Pod the API server is deleting and that has not
// stopped: the kubelet has not reported it Succeeded or Failed.
func podStuckTerminating(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp != nil &&
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
	warnings := stuckPodWarningsOf(&r.stuckPods)
	for _, pod := range pods {
		if podStuckTerminating(pod) && warnings.due(pod.UID, now) {
			r.event(schema, corev1.EventTypeWarning, "PodStuckTerminating", "%s", stuckPodWarningText(job, pod))
		}
	}
	return nil
}
