package controllerwrite

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	failedApplyRetentionSeconds = jobclaim.FailedApplyRetentionTTLSeconds
	applyOperationLabel         = "apply"
)

// validateCleanupTTLChoice holds a Job's cleanup TTL to one of the two values
// and the long one to its reason. Every finished Job keeps CleanupTTLSeconds. A
// Job keeps FailedApplyRetentionTTLSeconds only when it is an Apply Job whose run
// failed or whose outcome is unknown, because its Pod log is the one place that
// holds what the database answered. validateCleanupTTLApproved then holds a schema's
// to an Apply a person approved, and validateMigrationRunKeepsLog a migration's to
// that or a run that stops its resource.
//
// The Job alone cannot say which Apply this is. A runner that delivered its
// frame exits zero, so a failed Apply usually leaves a Complete Job, and the
// resource records the outcome only after this write. The Pod's termination
// summary does say, so that is what refuses a run that reported success. A Job
// that says nothing about success is not refused: no summary, a summary that
// does not read, and a Job Kubernetes marked failed are what an unknown outcome
// looks like.
func (v *Validator) validateCleanupTTLChoice(ctx context.Context, oldJob *batchv1.Job, seconds int32, ownerKind string) error {
	switch seconds {
	case cleanupTTLSeconds:
		return nil
	case failedApplyRetentionSeconds:
	default:
		return denyf("Job cleanup TTL %d is neither the %d seconds every finished Job keeps nor the %d seconds "+
			"the Job of an Apply that failed keeps", seconds, cleanupTTLSeconds, failedApplyRetentionSeconds)
	}
	if operation := oldJob.Labels[workload.LabelOperation]; operation != applyOperationLabel {
		return denyf("Job cleanup TTL %d is for the Job of an Apply that failed, and this is a %q Job; it keeps %d seconds",
			seconds, operation, cleanupTTLSeconds)
	}
	if !jobSucceeded(oldJob) {
		return nil
	}
	reported, err := v.applyReportedSuccess(ctx, oldJob, ownerKind)
	if err != nil {
		return internalf("read the Pods of Apply Job %s/%s: %v", oldJob.Namespace, oldJob.Name, err)
	}
	if reported {
		return denyf("Job cleanup TTL %d is for the Job of an Apply that failed, and this run reported success; it keeps %d seconds",
			seconds, cleanupTTLSeconds)
	}
	return nil
}

// validateCleanupTTLApproved holds a schema's long TTL to an Apply a person approved.
// Under `apply: Always` a failure repeats, and an uncertain Apply goes to a proof that
// plans again and may Apply again, so a long TTL would keep one Job per cycle and
// could exhaust a namespace's Job quota, while each run writes a fresh log anyway.
// Only the claim can say an approval authorized this run.
func validateCleanupTTLApproved(seconds int32, approved bool) error {
	if seconds == failedApplyRetentionSeconds && !approved {
		return denyf("Job cleanup TTL %d is for the Job of an Apply a person approved, and no approval authorized this run; it keeps %d seconds",
			seconds, cleanupTTLSeconds)
	}
	return nil
}

// validateMigrationRunKeepsLog holds a migration's long TTL to
// jobclaim.MigrationRunKeepsLog: the claim names an approval, or the run's own Pod
// reports an outcome that stops the resource.
//
// It reads the report whatever the Job's condition says, unlike the success check in
// validateCleanupTTLChoice: a run that reported Failed may leave either condition, and
// under `apply: Always` it does not earn the long TTL. It reads neither
// spec.policy.apply nor the resource's phase or unresolved run, because an edit moves
// the first and the manager writes the others after this. Silence reads as unknown,
// as it does for the manager, which blocks the resource on such a run.
func (v *Validator) validateMigrationRunKeepsLog(
	ctx context.Context,
	job *batchv1.Job,
	seconds int32,
	operation *operatorv1alpha1.MigrationOperationStatus,
) error {
	if seconds != failedApplyRetentionSeconds || operation.ApprovalRef != nil {
		return nil
	}
	pods, err := v.jobNamespacePods(ctx, job)
	if err != nil {
		return internalf("read the Pods of Apply Job %s/%s: %v", job.Namespace, job.Name, err)
	}
	outcome := jobclaim.ReportedMigrationRun(pods, job.UID, operation.ID)
	if jobclaim.MigrationRunBlocks(outcome) {
		return nil
	}
	return denyf("Job cleanup TTL %d is for an Apply a person approved or a migration run that stops its resource, "+
		"and no approval authorized this run and it reported %s; it keeps %d seconds", seconds, outcome, cleanupTTLSeconds)
}

// schemaApplyApproved says whether the schema's Apply was authorized by an approval:
// the plan its claim holds, or the one its pending observation kept once a binding
// change retired the claim.
func schemaApplyApproved(schema *operatorv1alpha1.PtahSchema) bool {
	if schema.Status.ActiveOperation != nil {
		return schema.Status.Plan != nil && schema.Status.Plan.Approval != nil
	}
	return schema.Status.PendingObservation != nil && schema.Status.PendingObservation.Plan.Approval != nil
}

// applyReportedSuccess reports whether the one Pod of a complete Apply Job says
// in its termination summary that the run succeeded.
func (v *Validator) applyReportedSuccess(ctx context.Context, job *batchv1.Job, ownerKind string) (bool, error) {
	pods, err := v.jobNamespacePods(ctx, job)
	if err != nil {
		return false, err
	}
	operation := runner.OperationApply
	if ownerKind == "PtahMigration" {
		operation = runner.OperationMigrationApply
	}
	// The label holds a hash of the ID, and the summary the ID itself.
	summary, read := jobclaim.ApplySummary(pods, job.UID, operation, job.Annotations[workload.AnnotationOperationID])
	return read && summaryReportsSuccess(summary), nil
}

// jobNamespacePods lists what jobclaim.ApplySummary matches by owner, the way the
// manager does: removing the job-name label from a Pod must not silence its report.
// It is read through the API server: the cache may not have the Pod.
func (v *Validator) jobNamespacePods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := v.Reader.List(ctx, pods, client.InNamespace(job.Namespace)); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// summaryReportsSuccess is the same reading the frame gets: no error code, and
// for a migration run an outcome that moved the database as planned or found
// nothing to do.
func summaryReportsSuccess(summary runner.Summary) bool {
	if summary.ErrorCode != "" || summary.Uncertain {
		return false
	}
	if summary.Migration == nil {
		return true
	}
	return summary.Migration.Outcome == dataplane.MigrationOutcomeApplied ||
		summary.Migration.Outcome == dataplane.MigrationOutcomeUpToDate
}
