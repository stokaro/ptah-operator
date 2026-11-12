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
// renewed while it runs: by the claim of a live resource of either kind, and by
// the deletion of either kind. Since Kubernetes 1.31 a Job past its deadline
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
// a read whose recorded Job is gone stops waiting for its Pods then. A
// PtahSchema waits so on the Apply a pending observation names, after the claim
// was retired: a live one before its proof reads the database, and a deleted
// one before it lets the database go. The deletion of a PtahMigration waits so
// on the Pods of the Job its claim recorded.
//
// The Warning goes through the Event recorder, whose spam filter gives each
// object a burst of 25 Events of a type and then one every five minutes, so a
// pass every five seconds would spend that burst in two minutes and starve every
// other Warning about the schema. The Warning about one Pod is repeated twice as
// long after each time, from thirty seconds to five minutes, as the result
// collector's ResourceQuota Warning is.
//
// Each wait keeps its own backoff for a Pod. One Pod can hold several waits in
// turn: a live Apply's, then the deletion that follows it, or the proof that
// follows an Apply that ended while the Pod ran. The first Warning of the next
// wait starts with other words and says something new, so it is due at once
// rather than when the last wait's next one would have been.
const (
	stuckPodWarningFirst = 30 * time.Second
	stuckPodWarningMax   = 5 * time.Minute
	// maxStuckPodWarnings bounds the table. A Pod past it is warned about on
	// every pass, which the recorder's spam filter then bounds.
	maxStuckPodWarnings = 1024
)

// stuckPodWarnings remembers, per wait and object, when the next Warning about
// it is due, and how long the last wait was. It is kept in memory: a manager
// restart warns again at once, and authorizes nothing.
type stuckPodWarnings struct {
	mu   sync.Mutex
	next map[stuckPodWarningKey]stuckPodWarning
}

// stuckPodWarningKey is what one backoff is kept for: the wait that warns, in
// the words its Warning starts with, and the object it warns about.
type stuckPodWarningKey struct {
	waiter string
	uid    types.UID
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

// dueFor reports whether the Warning waiter records about the object uid is due
// at now, and when it is, records when the next one is.
func (w *stuckPodWarnings) dueFor(waiter string, uid types.UID, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.next == nil {
		w.next = map[stuckPodWarningKey]stuckPodWarning{}
	}
	key := stuckPodWarningKey{waiter: waiter, uid: uid}
	last, seen := w.next[key]
	if seen && now.Before(last.due) {
		return false
	}
	if !seen && len(w.next) >= maxStuckPodWarnings {
		// A Pod still stuck is looked at every pass, which moves its entry
		// as soon as it is due. One overdue by more than the longest wait
		// is a Pod nothing looks at any more.
		for stale, entry := range w.next {
			if now.Sub(entry.due) > stuckPodWarningMax {
				delete(w.next, stale)
			}
		}
	}
	if seen || len(w.next) < maxStuckPodWarnings {
		waited := min(max(2*last.waited, stuckPodWarningFirst), stuckPodWarningMax)
		w.next[key] = stuckPodWarning{due: now.Add(waited), waited: waited}
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
// starts with: the deletion of either kind, a live resource's Apply, or a live
// PtahSchema's proof of an Apply that ended while its Pod could still run.
const (
	deletionWaits = "Deletion waits"
	applyWaits    = "The Apply waits"
	proofWaits    = "The Apply's proof waits"
)

// stuckPodWarningText is what the PodStuckTerminating Warning says about pod,
// one of the Pods of the Job jobName with the UID jobUID, which waiter waits
// for.
//
// The UID is named with the name because a wait can outlast the Job: one
// deleted by hand, or replaced by another Job under its name, still holds the
// wait through Pods that name its UID on their owner reference, and the name
// alone would point `kubectl get job` at the replacement.
func stuckPodWarningText(waiter, jobName string, jobUID types.UID, pod *corev1.Pod) string {
	node := ""
	if pod.Spec.NodeName != "" {
		node = " on node " + pod.Spec.NodeName
	}
	return fmt.Sprintf("%s for Pod %s of Job %s (UID %s)%s: the Pod was to be gone by %s and has not stopped. "+
		"The database Lease stays held while the Pod may still run. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		waiter, pod.Name, jobName, jobUID, node, pod.DeletionTimestamp.UTC().Format(time.RFC3339), pod.Namespace, pod.Name)
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
