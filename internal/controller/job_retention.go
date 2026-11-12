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
// collected. A refusal of the cleanup itself fails the short patch as well, and its
// answer is returned for the caller to settle by the rule a harvest's is settled by
// (settleHarvestedJobCleanup): the run has been read or recorded unknown, so a
// Forbidden or Invalid answer leaves the Job in place and names it in a Warning.
// noteShortened is then not called, since no TTL was written.
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
	// applyLogHoldsReportOnGoneNode and applyLogUnreadOnGoneNode are the two
	// readings above for a log that was on a node that is gone
	// (executorLogsOnGoneNodes): it holds the same, and no kubelet is left to
	// serve it.
	applyLogHoldsReportOnGoneNode
	applyLogUnreadOnGoneNode
)

// onGoneNode is reading for a log that was on a node that is gone. A reading
// that promises nothing stays as it is.
func (reading applyLogReading) onGoneNode() applyLogReading {
	switch reading {
	case applyLogHoldsReport:
		return applyLogHoldsReportOnGoneNode
	case applyLogUnread:
		return applyLogUnreadOnGoneNode
	default:
		return reading
	}
}

// nodeGone is a reading for a log that was on a node that is gone.
func (reading applyLogReading) nodeGone() bool {
	return reading == applyLogHoldsReportOnGoneNode || reading == applyLogUnreadOnGoneNode
}

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
// no operation container to ask for a log. Nor is there one when no Pod's executor
// has a log the API server serves (executorLogsLost), because the kubelet lost it
// or the Pod ended before any executor ran, and that holds for a coded failure as
// well: a durable result outlives the log the runner also wrote it to. A log that
// was on a node that is gone (executorLogsOnGoneNodes) holds what it held, and
// the reading says where it was.
func failedApplyLogReading(ctx context.Context, reader client.Reader, job *batchv1.Job, failure error) applyLogReading {
	// A target the run could not open has its sentence whole in status.
	var target *targetFailureError
	if errors.As(failure, &target) {
		return applyLogSaysNothing
	}
	var coded resultCodeError
	if errors.As(failure, &coded) {
		reading := applyLogSaysNothing
		if runner.LeavesReportedError(coded.code) {
			reading = applyLogHoldsReport
		}
		return servedLogReading(ctx, reader, job, reading)
	}
	if job == nil {
		return applyLogSaysNothing
	}
	pods, err := podsOwnedByJob(ctx, reader, job.Namespace, job.Name, job.UID)
	if err != nil || len(pods) == 0 || initBoundaryOf(pods) != "" || executorLogsLost(pods) {
		return applyLogSaysNothing
	}
	if executorLogsOnGoneNodes(pods) {
		return applyLogUnreadOnGoneNode
	}
	return applyLogUnread
}

// servedLogReading is reading once the Job's Pods have been asked whether the
// API server serves the executor's log: a reading that promises a log promises
// nothing when no Pod's executor has one (executorLogsLost), and says the log
// was on a node that is gone when every log there is was on one
// (executorLogsOnGoneNodes). A Job whose Pods are gone, or a list that fails,
// leaves the reading as the result gave it.
func servedLogReading(ctx context.Context, reader client.Reader, job *batchv1.Job, reading applyLogReading) applyLogReading {
	if reading == applyLogSaysNothing || job == nil {
		return reading
	}
	pods, err := podsOwnedByJob(ctx, reader, job.Namespace, job.Name, job.UID)
	switch {
	case err != nil:
		return reading
	case executorLogsLost(pods):
		return applyLogSaysNothing
	case executorLogsOnGoneNodes(pods):
		return reading.onGoneNode()
	default:
		return reading
	}
}

// executorLogUnserved is a Pod whose executor has no log the API server serves.
//
// Either the kubelet ended the executor with ContainerStatusUnknown, which it
// writes for a container it has no status for, one that never started as well
// as one the runtime stopped reporting while it ran, and records no container
// ID with it. Or the Pod failed with no executor status that shows a run: none
// at all, as a Pod the kubelet refused at admission carries, or a waiting one.
// Operation Pods restart nothing (RestartPolicyNever), so a waiting executor
// has no earlier run whose log the kubelet keeps. The kubelet refuses a log
// read of a terminated container with no container ID, of a waiting one with
// no earlier run, and of a container it has no status for
// (validateContainerLogStatus), so `kubectl logs` gets an error for each.
//
// PodGC leaves a waiting executor on a failed Pod as well. When it marks a Pod
// Failed after its node went (DeletionByPodGC), it sets the phase and a
// condition and keeps the statuses the kubelet last reported, so a Pod that was
// pulling its image or waiting for a Secret keeps its executor waiting. The
// executor may have started after that report. No status on the Pod shows that
// it ran, operation Pods restart nothing, and the pointer names the executor's
// container (`-c ptah`), so the status does not say whether it ran, and a log is
// pointed at only where the Pod shows a run.
//
// Only a failed Pod is asked about. One that has not ended can start the
// executor later, whether it reports it waiting or not yet at all, and the
// kubelet marks a Pod Succeeded only once every container in it terminated, so a
// succeeded Pod always reports its executor terminated.
func executorLogUnserved(pod *corev1.Pod) bool {
	failed := pod.Status.Phase == corev1.PodFailed
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == executorContainerName {
			if terminated := status.State.Terminated; terminated != nil {
				return terminated.Reason == "ContainerStatusUnknown"
			}
			return failed && status.State.Waiting != nil
		}
	}
	return failed
}

// executorLogsLost is a list of Pods none of which has an executor log the API
// server serves (executorLogUnserved). It is the one answer every family gives
// before it reads an executor's log or points a person at one. A harvest asks
// it only of a Pod whose executor terminated, so there it is the kubelet's
// ContainerStatusUnknown (lostExecutorLog). An empty list is not such a list:
// Pods that are gone say nothing about the log they had.
func executorLogsLost(pods []*corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for _, pod := range pods {
		if !executorLogUnserved(pod) {
			return false
		}
	}
	return true
}

// executorLogsOnGoneNodes is a list of Pods whose every executor log was on a
// node that is gone: each Pod either has no log the API server serves
// (executorLogUnserved) or carries PodGC's condition (deletedByPodGC), and at
// least one carries it. The log of a run such a Pod shows was written, but the
// API server reaches a log through the kubelet of the Pod's node, and it answers
// a read on a Pod whose Node object is gone with NotFound for the node
// (classifyResultLogError), so `kubectl logs` gets an error. Only the node's own
// disk, while it lasts, or a log shipper that read it there, still has it. An
// empty list is not such a list: Pods that are gone say nothing about the log
// they had.
func executorLogsOnGoneNodes(pods []*corev1.Pod) bool {
	gone := false
	for _, pod := range pods {
		switch {
		case executorLogUnserved(pod):
		case deletedByPodGC(pod):
			gone = true
		default:
			return false
		}
	}
	return gone
}

// deletedByPodGC is a Pod PodGC marked Failed because its Node object is gone:
// it carries the DisruptionTarget condition with podGCDeletionReason. The
// condition's status is not read: the disruption controller resets a stale
// DisruptionTarget only on a Pod that has not ended
// (nonTerminatingPodHasStaleDisruptionCondition), and either way the phase is
// PodGC's.
func deletedByPodGC(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget && condition.Reason == podGCDeletionReason {
			return true
		}
	}
	return false
}

// failedApplyLogPointer says where an Apply's Pod log is and how long it stays. A
// Job that carries a TTL keeps the log for that time, so the pointer stays true of
// a Job a previous release harvested, of an Apply no approval names, and of a Job a
// guard held to the short TTL. stays is the caller's word that admission refused
// the Job's cleanup TTL in this pass: nothing collects the Job then, so the log
// stays until someone deletes the Job, which is when a person needs the pointer
// most. Why the Job stays is the Warning that names it. A later pass can still
// ask again and be admitted, as an execution component change does for the Job
// of an uncertain schema Apply it retires (cleanupRetiredApplyJob); its
// RetiredApplyLogKept Event then says the time the TTL gives the log, and the
// pointer recorded before is not rewritten. A Job with no TTL that the caller
// does not say stays, such as one still running whose cleanup a later pass asks
// for, has no time to promise. The pointer promises only what the reading allows, and is
// empty when there is nothing to read.
//
// A log that was on a node that is gone has no command that reads it, so the
// pointer names none: it says where the log was and where else it can be read,
// whatever the Job's TTL, since no TTL keeps a log no kubelet serves.
func failedApplyLogPointer(job *batchv1.Job, reading applyLogReading, stays bool) string {
	subject := applyLogSubject(reading)
	switch {
	case job == nil || subject == "":
		return ""
	case reading.nodeGone():
		return fmt.Sprintf("%s was in the Pod log of Job %s on a node that is gone, so it can be read only from that node or from a log shipper",
			subject, job.Name)
	case job.Spec.TTLSecondsAfterFinished != nil:
		return fmt.Sprintf("%s is in the Pod log for %s after the Job finishes: kubectl logs -n %s job/%s -c %s",
			subject, retentionWords(*job.Spec.TTLSecondsAfterFinished), job.Namespace, job.Name, executorContainerName)
	case stays:
		return fmt.Sprintf("%s is in the Pod log until the Job is deleted: kubectl logs -n %s job/%s -c %s",
			subject, job.Namespace, job.Name, executorContainerName)
	default:
		return ""
	}
}

// applyLogSubject names what the reading says the Pod log holds, and is empty
// when it holds nothing to read.
func applyLogSubject(reading applyLogReading) string {
	switch reading {
	case applyLogHoldsReport, applyLogHoldsReportOnGoneNode:
		return "What Ptah reported"
	case applyLogUnread, applyLogUnreadOnGoneNode:
		return "Anything the run wrote"
	default:
		return ""
	}
}

// withLogPointer appends the pointer to a sentence, when there is one.
func withLogPointer(sentence string, job *batchv1.Job, reading applyLogReading, stays bool) string {
	if pointer := failedApplyLogPointer(job, reading, stays); pointer != "" {
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
