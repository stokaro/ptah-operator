package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	// applyLogSaysNothing is a run whose sentence is whole in status, one whose
	// operation container never started, or one with no Pod to read a log from,
	// so a person has nothing to read there.
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
// it may have written anything, unless the Job has no Pod, as one admission refused
// until its deadline has, or an init container ended the Pod first: then there is
// no operation container to ask for a log. Nor is there one when the kubelet ended
// every Pod's executor with ContainerStatusUnknown (executorStatusLost).
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
	pods, err := podsOwnedByJob(ctx, reader, job.Namespace, job.Name, job.UID)
	if err != nil || len(pods) == 0 || initBoundaryOf(pods) != "" || everyExecutorStatusLost(pods) {
		return applyLogSaysNothing
	}
	return applyLogUnread
}

// executorStatusLost is a Pod whose executor the kubelet ended with
// ContainerStatusUnknown. The kubelet writes that termination for a container
// it has no status for, one that never started as well as one the runtime
// stopped reporting while it ran, and records no container ID with it. It
// refuses a log read of a terminated container with no container ID
// (validateContainerLogStatus), so `kubectl logs` gets 400 for this one.
func executorStatusLost(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == executorContainerName {
			terminated := status.State.Terminated
			return terminated != nil && terminated.Reason == "ContainerStatusUnknown"
		}
	}
	return false
}

// everyExecutorStatusLost is a list of Pods none of which has an executor log
// the API server serves, because the kubelet lost every one.
func everyExecutorStatusLost(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if !executorStatusLost(pod) {
			return false
		}
	}
	return true
}

// failedApplyLogPointer says where an Apply's Pod log is and how long it stays, from
// the TTL the Job carries, so it stays true of a Job a previous release harvested,
// of an Apply no approval names, and of a Job a guard held to the short TTL. It
// promises only what the reading allows, and is empty when there is nothing to read
// or no time to promise.
func failedApplyLogPointer(job *batchv1.Job, reading applyLogReading) string {
	subject := applyLogSubject(reading)
	if job == nil || job.Spec.TTLSecondsAfterFinished == nil || subject == "" {
		return ""
	}
	return fmt.Sprintf("%s is in the Pod log for %s after the Job finishes: kubectl logs -n %s job/%s -c %s",
		subject, retentionWords(*job.Spec.TTLSecondsAfterFinished), job.Namespace, job.Name, executorContainerName)
}

// applyLogSubject names what the reading says the Pod log holds, and is empty
// when it holds nothing to read.
func applyLogSubject(reading applyLogReading) string {
	switch reading {
	case applyLogHoldsReport:
		return "What Ptah reported"
	case applyLogUnread:
		return "Anything the run wrote"
	default:
		return ""
	}
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
