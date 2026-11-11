package controller

import (
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// readOnlyJobRelease is what a read-only claim does with the Job under its
// reserved name before it lets go of it, in either family: when it retries past a
// Job it cannot confirm, when its resource is suspended, and when an execution
// binding change retires a migration's claim.
//
// The claim owes that Job the cleanup TTL a harvested read-only Job gets, and the
// claim is what makes the TTL possible. The controller-write webhook admits a
// read-only Job's cleanup TTL only once the Job has finished, Complete or Failed
// as jobTerminal reads it, and only on the Job the active claim names by UID, held
// to what the claim fixes without a rebuild. Once the claim lets go, no claim
// names the Job, and its cleanup cannot be scheduled again: the Job and its Pod
// stay until the resource is deleted, and hold a place in a count/jobs.batch
// quota until then.
type readOnlyJobRelease int

const (
	// releaseReadOnlyJobAsItIs lets go of a Job the claim owes nothing it can
	// give: none stands under the name, the Job already carries a TTL that
	// collects it, or the Job is not the claim's, because it replaced the Job
	// the claim recorded or the resource does not own it exactly.
	releaseReadOnlyJobAsItIs readOnlyJobRelease = iota
	// leaveReadOnlyJobUncollected lets go of a Job whose TTL admission would
	// refuse, and a Warning names it: one an unrecorded claim may not record,
	// one outside what the claim fixes, and one that has not finished by
	// jobStopBound. Waiting for it would hold the claim for nothing. A claim of a
	// suspended resource leaves a running Job the same way.
	leaveReadOnlyJobUncollected
	// recordReadOnlyJob records the UID of the Job a claim never recorded, where
	// the resource owns it exactly and it is the one the claim fixes, and asks
	// again: admission admits the TTL only on the Job a claim names by UID.
	recordReadOnlyJob
	// waitForReadOnlyJob keeps the claim over a running Job admission would
	// collect until it stops, as a Job the claim supervised would.
	waitForReadOnlyJob
	// collectReadOnlyJob asks for the TTL of a finished Job admission would
	// collect, and lets go once the answer is settled (settleJobCleanupAnswer).
	collectReadOnlyJob
)

// releaseReadOnlyJob decides what the claim does with job, given the verdict
// about it (mutationlifecycle.VerdictFor), envelope, the family's answer to
// whether the Job is what the claim fixes without a rebuild, now, and whether the
// claim may wait for the Job to stop. It returns the reason a Warning gives for
// leaving the Job uncollected.
//
// mayWait is false for a claim of a suspended resource, whichever pass lets go
// of it: suspension is the stop button. Such a claim settles at once what it can.
// A finished Job gets its TTL, and a Warning names a running one, which admission
// will not let any claim collect once this one is gone. It records no UID for a
// Job it cannot collect anyway. A PtahSchema claim that serves post-apply proof
// is the exception (schemaClaimMayWait), since that work outranks suspension.
//
// The wait ends by jobStopBound (stopWaitingForJob).
func releaseReadOnlyJob(
	job *batchv1.Job,
	verdict mutationlifecycle.JobVerdict,
	envelope error,
	now time.Time,
	mayWait bool,
) (readOnlyJobRelease, error) {
	switch {
	case job == nil || job.Spec.TTLSecondsAfterFinished != nil || verdict == mutationlifecycle.VerdictRetry:
		return releaseReadOnlyJobAsItIs, nil
	case verdict == mutationlifecycle.VerdictAdopt && envelope != nil:
		return leaveReadOnlyJobUncollected, unrecordedJobRefusal(envelope)
	case envelope != nil:
		return leaveReadOnlyJobUncollected,
			fmt.Errorf("admission admits a cleanup only on a Job its claim fixes: %w", envelope)
	case !mayWait && !jobTerminal(job):
		return leaveReadOnlyJobUncollected, errSuspendedClaimDoesNotWait
	case verdict == mutationlifecycle.VerdictAdopt:
		return recordReadOnlyJob, nil
	case jobTerminal(job):
		return collectReadOnlyJob, nil
	}
	if stop := stopWaitingForJob(job, now, mayWait); stop != nil {
		return leaveReadOnlyJobUncollected, stop
	}
	return waitForReadOnlyJob, nil
}

// errSuspendedClaimDoesNotWait is why a claim of a suspended resource leaves the
// running Job under its name without a cleanup.
var errSuspendedClaimDoesNotWait = errors.New(
	"a suspended claim does not wait for its Job to stop, and admission admits a cleanup only on a Job that has finished")

// stopWaitingForJob is why a read-only claim over a running Job it owes a cleanup
// stops waiting for that Job and lets go of it, or nil while it may wait on. It is
// the rule every such wait keeps in either family, releaseReadOnlyJob's and the
// two a PtahSchema keeps over the Lease its claim holds: the wait of a claim an
// execution-binding change retired (cleanupRetiredReadOnlyJob) and the wait of a
// claim whose Lease epoch moved (recoverLeaseContinuity). A claim that may not
// wait, one of a suspended resource, does not wait at all, and no claim waits past
// the Job's stop bound (jobStopOverdue).
func stopWaitingForJob(job *batchv1.Job, now time.Time, mayWait bool) error {
	if !mayWait {
		return errSuspendedClaimDoesNotWait
	}
	return jobStopOverdue(job, now)
}

// jobStopOverdue is why a claim stops waiting for a running Job it owes a
// cleanup, or nil while the Job may still stop by jobStopBound. Since Kubernetes
// 1.31 a Job past its deadline carries only FailureTarget until every Pod has
// terminated, and a Pod on a node that is gone may never terminate, so the Job
// would never finish, and a claim waiting for it would wait forever.
func jobStopOverdue(job *batchv1.Job, now time.Time) error {
	stopBy := jobStopBound(job)
	if now.Before(stopBy) {
		return nil
	}
	return fmt.Errorf(
		"the Job did not stop by its own deadline and the grace its Pods are given, %s, "+
			"and admission admits a cleanup only on a Job that has finished",
		stopBy.UTC().Format(time.RFC3339))
}

// unrecordedJobRefusal is why a claim that never recorded its Job's UID does not
// record the Job under its reserved name: the Job fails what the claim fixes
// without a rebuild, the match admission holds the Job's cleanup TTL to.
func unrecordedJobRefusal(envelope error) error {
	return fmt.Errorf("its claim never recorded the Job's UID, and the Job is not the one the claim fixes: %w", envelope)
}

// settleJobCleanupAnswer settles the answer to a Job's cleanup TTL write in either
// family. A TTL that was set, or a Job deleted since it was read, needs nothing
// more. A refusal says admission will not collect the Job however often it is
// asked: Forbidden from the controller-write webhook, Invalid from a
// ValidatingAdmissionPolicy such as the controller-object guard. refused is told,
// to name the Job, and the caller goes on. Any other failure may pass, so it is
// returned, and the pass fails and asks again.
func settleJobCleanupAnswer(err error, refused func(error)) error {
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
		refused(err)
		return nil
	default:
		return err
	}
}

// settleHarvestedJobCleanup settles the answer to the cleanup TTL write a pass
// makes on a finished Job before it acts on what that Job did, in either family:
// a harvest's (harvestJob, harvestMigrationJob), and the retention of an Apply
// that failed or whose outcome is unknown (retainFailedApplyJob), including the
// Apply a retirement record names. The answer says nothing about what the Job's
// result proves, which the pass read from the Job's own Pods or records as
// unknown, so it is settled by the rule settleJobCleanupAnswer keeps, with one
// more condition on a refusal. Admission reads the Job's owner as stored and
// refuses the TTL of a Job the stored record no longer names (the active claim,
// or the retirement record), so a refusal can also mean that the record this
// pass read has moved since, as under two managers, and a pass that went on would
// act for a record that is no longer there. A refusal is settled only where
// claimed reports that the stored record still names the Job. Otherwise the pass
// fails with the refusal, and the next pass reads the record again.
func settleHarvestedJobCleanup(err error, claimed func() (bool, error), refused func(error)) error {
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return settleJobCleanupAnswer(err, refused)
	}
	stillClaimed, readErr := claimed()
	if readErr != nil {
		return fmt.Errorf("read the record that names the Job again after its cleanup was refused (%w): %w", err, readErr)
	}
	if !stillClaimed {
		return fmt.Errorf("the record that names the Job changed after this pass read it, "+
			"so the refusal of the Job's cleanup is read again with it: %w", err)
	}
	refused(err)
	return nil
}

// jobStopReportSlack is how long past the moment a Job's Pods must have stopped
// the claim still waits: for the kubelet to report the stopped Pod and the Job
// controller to write the Job's terminal condition.
const jobStopReportSlack = 30 * time.Second

// jobStopBound is when a Job that has not finished has had every chance
// to: its own deadline, counted from its start as the Job controller counts it,
// plus the termination grace its Pods are given and jobStopReportSlack. A Job the
// Job controller has not started yet counts from its creation, which comes no
// later. A Job with no deadline, which no release of this operator builds, gets
// no time beyond the grace and the slack. A FailureTarget or SuccessCriteriaMet
// condition says the Job controller is already terminating the Job's Pods, so
// where one stands, the bound is its time plus the same grace and slack, if that
// comes first.
//
// Neither condition is terminal for this purpose. Admission admits the TTL only on
// a Job that is Complete or Failed, the same test jobTerminal makes.
func jobStopBound(job *batchv1.Job) time.Time {
	grace := time.Duration(corev1.DefaultTerminationGracePeriodSeconds) * time.Second
	if seconds := job.Spec.Template.Spec.TerminationGracePeriodSeconds; seconds != nil {
		grace = time.Duration(*seconds) * time.Second
	}
	start := job.CreationTimestamp.Time
	if job.Status.StartTime != nil {
		start = job.Status.StartTime.Time
	}
	var deadline time.Duration
	if job.Spec.ActiveDeadlineSeconds != nil {
		deadline = time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second
	}
	bound := start.Add(deadline + grace + jobStopReportSlack)
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue || condition.LastTransitionTime.IsZero() ||
			(condition.Type != batchv1.JobFailureTarget && condition.Type != batchv1.JobSuccessCriteriaMet) {
			continue
		}
		if stopping := condition.LastTransitionTime.Add(grace + jobStopReportSlack); stopping.Before(bound) {
			bound = stopping
		}
	}
	return bound
}
