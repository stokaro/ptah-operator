package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahMigration's waits for an Apply Pod that is stuck or gone
// (stuck_pod_warning.go).

// warnStuckMigrationApplyPods is warnStuckPods for the Apply a PtahMigration
// waits for, in the words of waiter: a deleted one's wait
// (dispatchedApplyMayStillWrite), or a live one's supervision of its running
// Job. It asks by the Job identity that wait asks about: the one the claim
// recorded, or the Job under its name where it recorded none. job is the Job
// the pass read under the claim's name, or nil where none stands that is the
// claim's: dispatchedMigrationApplyJob already refuses a Job whose UID is not
// the one the claim recorded, so job is never another Job. A Job the claim
// names that is gone leaves its Pods their own bounds, and its Pods are listed
// on every pass then, as the wait itself lists them. The Warning names the
// Job's UID, since another Job can hold the name by then.
func (r *MigrationReconciler) warnStuckMigrationApplyPods(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
	waiter string,
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
		if warnings.dueFor(waiter, pod.UID, now) {
			r.event(migration, corev1.EventTypeWarning, "PodStuckTerminating", "%s",
				stuckPodWarningText(waiter, jobName, jobUID, pod))
		}
	}
	return nil
}

// warnStuckUnaccountedMigrationApplyPods is warnStuckMigrationApplyPods for a
// claim kept after its run was recorded Unknown (reconcileUnaccountedMigrationApply).
// That wait comes ahead of deletion, so it speaks for a live migration and for a
// deleted one, in the words each waits in. It reads the claim's Job as a
// deletion does (dispatchedMigrationApplyJob), since the gate it follows reads
// the Job for itself. A read or a list that fails warns about nothing and is not
// returned: an error requeue backs off, and this wait is what renews the Lease.
func (r *MigrationReconciler) warnStuckUnaccountedMigrationApplyPods(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) {
	operation := migration.Status.ActiveOperation
	waiter := applyWaits
	if migration.DeletionTimestamp != nil {
		waiter = deletionWaits
	}
	job, err := r.dispatchedMigrationApplyJob(ctx, migration, operation)
	if err == nil {
		err = r.warnStuckMigrationApplyPods(ctx, migration, operation, job, waiter)
	}
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("could not look for a stuck Apply Pod of a run recorded Unknown", "error", err.Error())
	}
}

// warnMigrationApplyPodGone records an ApplyPodGone Warning about the hold
// dispatchedApplyMayStillWrite returned, where it returned one: the API server
// has no Pod of the claim's Job left, nothing says its executor stopped, and the
// claim waits until its Lease term ends (migrationApplyPodGone). It is repeated
// with the backoff a PodStuckTerminating Warning has, kept for each wait and
// Job UID, so a deletion says so at once although its live Apply said so a
// moment before.
//
// The UID is named because the name is not enough: another Job can hold the
// name the claim reserved, with Pods of its own.
func (r *MigrationReconciler) warnMigrationApplyPodGone(migration *operatorv1alpha1.PtahMigration, hold *applyPodGoneHold) {
	if hold == nil {
		return
	}
	waiter := applyWaits
	if migration.DeletionTimestamp != nil {
		waiter = deletionWaits
	}
	if !stuckPodWarningsOf(&r.goneApplyPods).dueFor(waiter, hold.jobUID, r.now()) {
		return
	}
	r.event(migration, corev1.EventTypeWarning, "ApplyPodGone",
		"%s for Job %s, UID %s, until %s: the API server has no Pod of the Job with that UID, "+
			"and nothing says its executor stopped. A Pod can be gone from the API server while its container "+
			"still runs on a node the API server cannot reach, so the database Lease stays held until the term "+
			"the Apply took it for ends.",
		waiter, hold.jobName, hold.jobUID, hold.until.UTC().Format(time.RFC3339))
}
