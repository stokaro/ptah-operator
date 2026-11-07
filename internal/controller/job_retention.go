package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// scheduleJobCleanup sets the TTL Kubernetes collects a finished Job by. The
// timer starts when the Job finishes, so a Job that is still running keeps its
// deadline. A Job that already carries a TTL keeps it: no release changes one.
func scheduleJobCleanup(ctx context.Context, writer client.Client, job *batchv1.Job, seconds int32) error {
	if job == nil || job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	before := job.DeepCopy()
	job.Spec.TTLSecondsAfterFinished = ptr(seconds)
	if err := writer.Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		job.Spec.TTLSecondsAfterFinished = nil
		return fmt.Errorf("schedule completed Job cleanup: %w", err)
	}
	return nil
}

// retainFailedApplyJob schedules the cleanup of an Apply Job whose run did not
// succeed, in both families. keepLog is whether the run keeps
// jobclaim.FailedApplyRetentionTTLSeconds: for a schema an approval, and for a
// migration jobclaim.MigrationRunKeepsLog.
//
// That TTL is for a log a person comes back to: the Pod log is the only place that
// holds what the database answered, and the run does not come around again by itself.
// Every other run keeps the five minutes every other finished Job keeps. Under
// `apply: Always` a failure that does repeat writes a fresh log each cycle, so a long
// TTL would hold one Job per cycle and could exhaust a namespace's Job quota.
//
// A guard that refuses the long TTL still gets a cleanup, and says so through
// noteShortened and the log. A guard from the release before this one is installed
// for a moment during an upgrade, and the webhook refuses the long TTL on a run that
// reported success, which reaches here when the failure is the manager's own rather
// than the run's. Either way the claim has to settle, and the Job has to be
// collected. A refusal of the cleanup itself fails the short patch as well, and is
// returned.
func retainFailedApplyJob(
	ctx context.Context,
	writer client.Client,
	job *batchv1.Job,
	keepLog bool,
	noteShortened func(message string),
) error {
	if !keepLog {
		return scheduleJobCleanup(ctx, writer, job, jobCleanupTTLSeconds)
	}
	refused := scheduleJobCleanup(ctx, writer, job, failedApplyTTLSeconds)
	if refused == nil || (!apierrors.IsForbidden(refused) && !apierrors.IsInvalid(refused)) {
		return refused
	}
	if err := scheduleJobCleanup(ctx, writer, job, jobCleanupTTLSeconds); err != nil {
		return err
	}
	message := fmt.Sprintf("Admission refused the %s Job %s is due, so its Pod log stays for %s after the Job finishes: %s",
		retentionWords(failedApplyTTLSeconds), job.Name, retentionWords(jobCleanupTTLSeconds), bounded(refused.Error(), 256))
	log.FromContext(ctx).Info("a failed Apply's Job keeps the short cleanup TTL", "job", job.Name, "reason", refused.Error())
	noteShortened(message)
	return nil
}

// applyLogReading is what can be said of the Pod log of an Apply that did not
// succeed. The zero value promises nothing.
type applyLogReading int

const (
	// applyLogSaysNothing is a run whose sentence is whole in status, or one whose
	// operation container never started, so a person has nothing to read there.
	applyLogSaysNothing applyLogReading = iota
	// applyLogHoldsReport is a run the runner wrote Ptah's sentence for.
	applyLogHoldsReport
	// applyLogUnread is a run whose result nobody read, so the log may hold
	// anything or nothing.
	applyLogUnread
)

// resultCodeError is a failure read from a result. It keeps the code, because the
// code is what says whether the runner wrote Ptah's sentence to the Pod log.
type resultCodeError struct {
	code    string
	message string
}

func (e resultCodeError) Error() string { return e.message }

// failedApplyLogReading reads what a schema Apply's failure says of its log, or a
// migration's when no report of the run was read. A failure with no result behind
// it may have written anything, unless an init container ended the Pod first: then
// there is no operation container to ask for a log.
func failedApplyLogReading(ctx context.Context, reader client.Reader, job *batchv1.Job, failure error) applyLogReading {
	// A target the run could not open has its sentence whole in status.
	var target *targetFailureError
	if errors.As(failure, &target) {
		return applyLogSaysNothing
	}
	var coded resultCodeError
	if errors.As(failure, &coded) {
		if runner.LeavesReportedError(coded.code) {
			return applyLogHoldsReport
		}
		return applyLogSaysNothing
	}
	if job == nil {
		return applyLogSaysNothing
	}
	boundary, err := failedInitBoundary(ctx, reader, job)
	if err != nil || boundary != "" {
		return applyLogSaysNothing
	}
	return applyLogUnread
}

// failedApplyLogPointer says where an Apply's Pod log is and how long it stays, from
// the TTL the Job carries, so it stays true of a Job a previous release harvested,
// of an Apply no approval names, and of a Job a guard held to the short TTL. It
// promises only what the reading allows, and is empty when there is nothing to read
// or no time to promise.
func failedApplyLogPointer(job *batchv1.Job, reading applyLogReading) string {
	if job == nil || job.Spec.TTLSecondsAfterFinished == nil {
		return ""
	}
	var subject string
	switch reading {
	case applyLogHoldsReport:
		subject = "What Ptah reported"
	case applyLogUnread:
		subject = "Anything the run wrote"
	default:
		return ""
	}
	return fmt.Sprintf("%s is in the Pod log for %s after the Job finishes: kubectl logs -n %s job/%s -c %s",
		subject, retentionWords(*job.Spec.TTLSecondsAfterFinished), job.Namespace, job.Name, executorContainerName)
}

// withLogPointer appends the pointer to a sentence, when there is one.
func withLogPointer(sentence string, job *batchv1.Job, reading applyLogReading) string {
	if pointer := failedApplyLogPointer(job, reading); pointer != "" {
		return sentence + ". " + pointer
	}
	return sentence
}

// retentionWords names a TTL as a person reads it.
func retentionWords(seconds int32) string {
	switch {
	case seconds > 0 && seconds%3600 == 0:
		return fmt.Sprintf("%d hours", seconds/3600)
	case seconds > 0 && seconds%60 == 0:
		return fmt.Sprintf("%d minutes", seconds/60)
	default:
		return fmt.Sprintf("%d seconds", seconds)
	}
}
