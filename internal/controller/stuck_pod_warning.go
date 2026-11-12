package controller

import (
	"fmt"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// An Apply's Job is waited for until it stops, and the database Lease is
// renewed while it runs: by a live PtahSchema's claim, by a deleted PtahSchema,
// and by a deleted PtahMigration. Since Kubernetes 1.31 a Job past its deadline
// is marked Failed only once its Pods have terminated, and a Pod on a node that
// is gone stays Terminating until the node object goes or somebody
// force-deletes the Pod. Holding the Lease is right, since such a Pod may still
// be running SQL on a node that is only cut off, but nothing else says why the
// Apply or the deletion does not end. A PodStuckTerminating Warning says it: it
// names each such Pod, its node, and the delete that ends the wait. A read
// that holds the lock, a Plan or the read that proves an Apply, is waited for
// only until jobStopBound, where its claim lets go and says so in a
// ReadJobOverdue Warning instead (#997).
//
// It warns only once the Job is past jobStopBound, its own deadline plus the
// grace its Pods are given and the slack, when the Job controller has already
// deleted every Pod it was going to stop. Before that the Job may still stop
// by itself, and nothing is listed. A Pod deleted later, or with a longer grace
// than its template's, is named only once its own grace has run out
// (podStuckTerminating).
//
// A wait for the Pods of a Job that is gone, deleted or replaced under its
// name, has no Job to be past the bound of. The Pods still name the Job's UID
// in their owner reference, and a Pod on a node that is gone still holds the
// wait, so each is named once it is past its own stop bound (podStopBound), as
// a read whose recorded Job is gone stops waiting for its Pods then. The
// deletion of a PtahSchema waits so on the Apply a pending observation names,
// after the claim was retired, and the deletion of a PtahMigration on the Pods
// of the Job its claim recorded.
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

// What waits for a stuck Pod, in the words the PodStuckTerminating Warning
// starts with: the deletion of either kind, or a live PtahSchema's Apply.
const (
	deletionWaits = "Deletion waits"
	applyWaits    = "The Apply waits"
)

// stuckPodWarningText is what the PodStuckTerminating Warning says about pod,
// one of the Pods of Job jobName, which waiter waits for.
func stuckPodWarningText(waiter, jobName string, pod *corev1.Pod) string {
	node := ""
	if pod.Spec.NodeName != "" {
		node = " on node " + pod.Spec.NodeName
	}
	return fmt.Sprintf("%s for Pod %s of Job %s%s: the Pod was to be gone by %s and has not stopped. "+
		"The database Lease stays held while the Pod may still run. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		waiter, pod.Name, jobName, node, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
}

// stuckApplyPods returns the pods a wait names at now: each one stuck
// terminating (podStuckTerminating) and past the bound of the wait. job is the
// Job the pods belong to, and its stop bound is that bound. A Job that is gone
// is nil, and each Pod's own stop bound (podStopBound) is its bound instead.
func stuckApplyPods(job *batchv1.Job, pods []*corev1.Pod, now time.Time) []*corev1.Pod {
	if job != nil && jobStopOverdue(job, now) == nil {
		return nil
	}
	var stuck []*corev1.Pod
	for _, pod := range pods {
		if podStuckTerminating(pod, now) && (job != nil || !now.Before(podStopBound(pod))) {
			stuck = append(stuck, pod)
		}
	}
	return stuck
}
