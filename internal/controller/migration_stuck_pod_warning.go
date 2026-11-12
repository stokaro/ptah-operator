package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

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

// warnMigrationApplyPodGone records an ApplyPodGone Warning about the hold
// dispatchedApplyMayStillWrite returned, where it returned one: nothing says
// the executor of the claim's Job stopped, and the claim waits until its Lease
// term ends. Either the API server has no Pod of that Job left
// (migrationApplyPodGone), or a Pod of it stands with a terminal phase that is
// not the kubelet's (podStopUnconfirmed), which the Warning names. It is
// repeated with the backoff a PodStuckTerminating Warning has, by the Job's
// UID, so a Pod that stands and then goes does not start the backoff again.
//
// The UID is named because the name is not enough: another Job can hold the
// name the claim reserved, with Pods of its own.
func (r *MigrationReconciler) warnMigrationApplyPodGone(migration *operatorv1alpha1.PtahMigration, hold *applyPodGoneHold) {
	if hold == nil {
		return
	}
	now := r.now()
	if !stuckPodWarningsOf(&r.goneApplyPods).due(hold.jobUID, now) {
		return
	}
	waiter := applyWaits
	if migration.DeletionTimestamp != nil {
		waiter = deletionWaits
	}
	until := hold.until.UTC().Format(time.RFC3339)
	switch {
	case hold.stop == stopUnconfirmedRunning:
		r.event(migration, corev1.EventTypeWarning, "ApplyPodGone",
			"%s for Job %s, UID %s, until %s: Pod %s of the Job reads %s while its executor container reads running. "+
				"The kubelet does not write that phase before the container stops, so nothing says the executor stopped. "+
				"PodGC writes it on a Pod whose Node object is gone, or on a terminating Pod on a node tainted "+
				"node.kubernetes.io/out-of-service, while the container can still run on a node the API "+
				"server cannot reach, so the database Lease stays held until the term the Apply took it for ends.",
			waiter, hold.jobName, hold.jobUID, until, hold.podName, hold.podPhase)
		return
	case hold.stop == stopUnconfirmedByPodGC:
		r.event(migration, corev1.EventTypeWarning, "ApplyPodGone",
			"%s for Job %s, UID %s, until %s: Pod %s of the Job reads %s, which PodGC wrote when the Pod's Node object "+
				"went, and its executor container never read terminated, so nothing says the executor stopped. "+
				"The container can have started since its kubelet last reported, on a node the API server cannot reach, "+
				"so the database Lease stays held until the term the Apply took it for ends.",
			waiter, hold.jobName, hold.jobUID, until, hold.podName, hold.podPhase)
		return
	}
	r.event(migration, corev1.EventTypeWarning, "ApplyPodGone",
		"%s for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
			"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
			"still runs on a node the API server cannot reach, so the database Lease stays held until the term "+
			"the Apply took it for ends.",
		waiter, hold.jobName, hold.jobUID, until)
}
