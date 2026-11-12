package controller

import (
	"context"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahSchema's waits for an Apply Pod that is stuck or gone
// (stuck_pod_warning.go).

// warnStuckPods records a PodStuckTerminating Warning, in the words of waiter,
// for each Pod of job that is stuck terminating once job is past jobStopBound,
// at most as often as stuckPodWarnings allows. The Pods are listed only past
// the bound. A failed list is logged by the caller and warns about nothing: the
// wait goes on either way.
func (r *SchemaReconciler) warnStuckPods(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, waiter string) error {
	now := r.now()
	if jobStopOverdue(job, now) == nil {
		return nil
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, job.Name, job.UID)
	if err != nil {
		return err
	}
	r.warnAboutStuckPods(schema, waiter, job.Name, job.UID, stuckApplyPods(job, pods, now), now)
	return nil
}

// warnStuckApplyPods is warnStuckPods for the Apply Job a pending observation
// names, over the Pods the wait already listed for it by the Job's UID, in the
// words of waiter: a deletion's, or a live proof's. The Job is read only when
// one of them is stuck, directly as the claim's wait reads its Job. A Job that
// is gone, or is not the one the record names, leaves the Pods their own bounds
// (stuckApplyPods), and the Warning names the Job by the UID the record holds.
func (r *SchemaReconciler) warnStuckApplyPods(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	pods []*corev1.Pod,
	waiter string,
) error {
	now := r.now()
	if !slices.ContainsFunc(pods, func(pod *corev1.Pod) bool { return podStuckTerminating(pod, now) }) {
		return nil
	}
	job := &batchv1.Job{}
	switch err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: pending.ApplyJobName}, job); {
	case apierrors.IsNotFound(err):
		job = nil
	case err != nil:
		return err
	case job.UID != pending.ApplyJobUID:
		job = nil
	}
	r.warnAboutStuckPods(schema, waiter, pending.ApplyJobName, pending.ApplyJobUID, stuckApplyPods(job, pods, now), now)
	return nil
}

// warnAboutStuckPods records the Warning about each of the stuck Pods of the Job
// jobName with the UID jobUID, at most as often as stuckPodWarnings allows for
// that Pod and waiter.
func (r *SchemaReconciler) warnAboutStuckPods(
	schema *operatorv1alpha1.PtahSchema,
	waiter, jobName string,
	jobUID types.UID,
	stuck []*corev1.Pod,
	now time.Time,
) {
	warnings := stuckPodWarningsOf(&r.stuckPods)
	for _, pod := range stuck {
		if warnings.dueFor(waiter, pod.UID, now) {
			r.event(schema, corev1.EventTypeWarning, "PodStuckTerminating", "%s", stuckPodWarningText(waiter, jobName, jobUID, pod))
		}
	}
}

// warnApplyPodGone records an ApplyPodGone Warning while a deleted PtahSchema
// waits until observeAfter, its pending observation's ObserveAfter, with no Pod
// of the Apply's Job left: pods is what the wait listed by the Job's UID.
// Nothing says the executor stopped. A Pod force-deleted, or removed by PodGC
// once its Node object went, can still run on a node the API server cannot
// reach, so the deletion holds the database Lease until the Apply's execution
// deadline and its Pod's termination grace have passed
// (finishUncertainApplyWithEvidenceAndBinding), the instant a live schema's
// proof waits for before it reads the database. A live schema shows that
// instant in status.pendingObservation.observeAfter. A deleted one only stays
// Terminating, so this Warning says why it waits and until when. It is repeated
// with the backoff a PodStuckTerminating Warning has, by the Job's UID, which
// it names because another Job can hold the name.
//
// A record that names no Job UID warns about nothing: its create may never
// have landed, so there is no Job to name and no Pod the API server could have
// lost. Nor does one whose Pods still stand, stopped or not: a stuck one is
// what PodStuckTerminating names.
func (r *SchemaReconciler) warnApplyPodGone(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	pods []*corev1.Pod,
	observeAfter time.Time,
) {
	if pending.ApplyJobUID == "" || len(pods) != 0 {
		return
	}
	if !stuckPodWarningsOf(&r.goneApplyPods).due(pending.ApplyJobUID, r.now()) {
		return
	}
	r.event(schema, corev1.EventTypeWarning, "ApplyPodGone",
		"%s for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
			"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
			"still runs on a node the API server cannot reach, so the database Lease stays held until the "+
			"Apply's execution deadline and its Pod's termination grace have passed.",
		deletionWaits, pending.ApplyJobName, pending.ApplyJobUID, observeAfter.UTC().Format(time.RFC3339))
}
