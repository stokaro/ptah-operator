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

// A PtahSchema's waits for a stuck Apply Pod (stuck_pod_warning.go).

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
	r.warnAboutStuckPods(schema, waiter, job.Name, stuckApplyPods(job, pods, now), now)
	return nil
}

// warnStuckApplyPods is warnStuckPods for the Apply Job a pending observation
// names, over the Pods the wait already listed for it by the Job's UID. The Job
// is read only when one of them is stuck, directly as the claim's wait reads
// its Job. A Job that is gone, or is not the one the record names, leaves the
// Pods their own bounds (stuckApplyPods).
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
	switch err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: pending.ApplyJobName}, job); {
	case apierrors.IsNotFound(err):
		job = nil
	case err != nil:
		return err
	case job.UID != pending.ApplyJobUID:
		job = nil
	}
	r.warnAboutStuckPods(schema, deletionWaits, pending.ApplyJobName, stuckApplyPods(job, pods, now), now)
	return nil
}

// warnAboutStuckPods records the Warning about each of the stuck Pods of Job
// jobName, at most as often as stuckPodWarnings allows.
func (r *SchemaReconciler) warnAboutStuckPods(
	schema *operatorv1alpha1.PtahSchema,
	waiter, jobName string,
	stuck []*corev1.Pod,
	now time.Time,
) {
	warnings := stuckPodWarningsOf(&r.stuckPods)
	for _, pod := range stuck {
		if warnings.due(pod.UID, now) {
			r.event(schema, corev1.EventTypeWarning, "PodStuckTerminating", "%s", stuckPodWarningText(waiter, jobName, pod))
		}
	}
}
