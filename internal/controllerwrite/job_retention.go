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
	executorContainer           = "ptah"
)

// validateCleanupTTLChoice holds a Job's cleanup TTL to one of the two values
// and the long one to its reason. Every finished Job keeps CleanupTTLSeconds. A
// Job keeps FailedApplyRetentionTTLSeconds only when it is an Apply Job whose run
// failed or whose outcome is unknown, because its Pod log is the one place that
// holds what the database answered, and validateCleanupTTLApproved then holds it to
// an Apply a person approved.
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

// validateCleanupTTLApproved holds the long TTL to an Apply a person approved. Under
// `apply: Always` the same failure repeats every cycle, so a long TTL would keep one
// Job per cycle and could exhaust a namespace's Job quota, while each run writes a
// fresh log anyway. Only the claim can say an approval authorized this run.
func validateCleanupTTLApproved(seconds int32, approved bool) error {
	if seconds == failedApplyRetentionSeconds && !approved {
		return denyf("Job cleanup TTL %d is for the Job of an Apply a person approved, and no approval authorized this run; it keeps %d seconds",
			seconds, cleanupTTLSeconds)
	}
	return nil
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
// in its termination summary that the run succeeded. A Job can start more than
// one Pod, and no one Pod's report speaks for another, so more than one is not
// a success. It is read through the API server: the cache may not have the Pod.
func (v *Validator) applyReportedSuccess(ctx context.Context, job *batchv1.Job, ownerKind string) (bool, error) {
	pods := &corev1.PodList{}
	if err := v.Reader.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return false, err
	}
	owned := make([]*corev1.Pod, 0, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		for _, reference := range pod.OwnerReferences {
			if reference.UID == job.UID && reference.Controller != nil && *reference.Controller {
				owned = append(owned, pod)
				break
			}
		}
	}
	if len(owned) != 1 {
		return false, nil
	}
	operation := runner.OperationApply
	if ownerKind == "PtahMigration" {
		operation = runner.OperationMigrationApply
	}
	// The label holds a hash of the ID, and the summary the ID itself.
	return podReportedSuccess(owned[0], operation, job.Annotations[workload.AnnotationOperationID]), nil
}

// podReportedSuccess reads the summary the executor container left. Only a
// summary that reads, for this operation, can say the run succeeded.
func podReportedSuccess(pod *corev1.Pod, operation runner.Operation, operationID string) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != executorContainer || status.State.Terminated == nil {
			continue
		}
		summary, err := runner.ParseSummaryFor(status.State.Terminated.Message, operation, operationID)
		if err != nil {
			return false
		}
		return summaryReportsSuccess(summary)
	}
	return false
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
