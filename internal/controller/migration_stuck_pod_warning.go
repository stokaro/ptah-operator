package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahMigration's waits for an Apply Pod that is stuck or gone
// (stuck_pod_warning.go).

// warnStuckMigrationApplyPods is warnStuckPods for the Apply a deleted
// PtahMigration waits for (dispatchedApplyMayStillWrite), by the Job identity
// that wait asks about: the one the claim recorded, or the Job under its name
// where it recorded none. job is the Job the deletion read under the claim's
// name, or nil where none stands that is the claim's: dispatchedMigrationApplyJob
// already refuses a Job whose UID is not the one the claim recorded, so job is
// never another Job. A Job the claim names that is gone leaves its Pods their
// own bounds, and its Pods are listed on every pass then, as the wait itself
// lists them.
func (r *MigrationReconciler) warnStuckMigrationApplyPods(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
) error {
	jobName, jobUID := operation.JobName, operation.JobUID
	if jobUID == "" && job != nil {
		jobName, jobUID = job.Name, job.UID
	}
	now := r.now()
	if job != nil && jobStopOverdue(job, now) == nil {
		return nil
	}
	pods, err := podsOwnedByJob(ctx, r.directReader(), migration.Namespace, jobName, jobUID)
	if err != nil {
		return err
	}
	warnings := stuckPodWarningsOf(&r.stuckPods)
	for _, pod := range stuckApplyPods(job, pods, now) {
		if warnings.due(pod.UID, now) {
			r.event(migration, corev1.EventTypeWarning, "PodStuckTerminating", "%s",
				stuckPodWarningText(deletionWaits, jobName, pod))
		}
	}
	return nil
}

// warnMigrationApplyPodGone records an ApplyPodGone Warning about Job jobName,
// whose Pods are gone with nothing to say that its executor stopped, and which
// the claim waits for until (migrationApplyPodGone). It is repeated with the
// backoff a PodStuckTerminating Warning has, by the Job's UID.
func (r *MigrationReconciler) warnMigrationApplyPodGone(
	migration *operatorv1alpha1.PtahMigration,
	jobName string,
	jobUID types.UID,
	until time.Time,
) {
	now := r.now()
	if !stuckPodWarningsOf(&r.goneApplyPods).due(jobUID, now) {
		return
	}
	waiter := applyWaits
	if migration.DeletionTimestamp != nil {
		waiter = deletionWaits
	}
	r.event(migration, corev1.EventTypeWarning, "ApplyPodGone",
		"%s for Job %s until %s: the Job has no Pod left, and no Pod reported that its executor stopped. "+
			"A Pod removed by a forced delete, or with its Job, can still be running on a node the API server cannot reach, "+
			"so the database Lease stays held until the term the Apply took it for ends.",
		waiter, jobName, until.UTC().Format(time.RFC3339))
}
