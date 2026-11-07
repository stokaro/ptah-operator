package e2e

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/internal/jobclaim"
)

// What the rows make a database refuse, and how PostgreSQL words it. The line in
// the Pod log has to carry the answer, and nothing the statement said: the index it
// names is in the statement, so that is hidden too.
const (
	refusedStatementAnswer    = `could not create unique index "[in the statement]"`
	refusedStatementSQLState  = "23505"
	refusedStatementStatement = "CREATE UNIQUE INDEX"
	refusedStatementIndex     = "e2e_refused_one_per_customer"
	reportedErrorPrefix       = "ptah-runner: ptah reported: "
)

// failedApplyLogSays holds a Pod log to the claim: one line from the failed run,
// with the database's answer in it and not the statement Ptah appends after it. A
// log with the answer and the statement fails, and so does one with neither.
func failedApplyLogSays(log []byte) error {
	reported := reportedErrors(log)
	switch {
	case len(reported) != 1:
		return fmt.Errorf("the Pod log holds %d reported errors, want 1", len(reported))
	case !strings.Contains(reported[0], refusedStatementAnswer):
		return fmt.Errorf("the reported error does not say %q: %q", refusedStatementAnswer, reported[0])
	case strings.Contains(string(log), refusedStatementStatement):
		return fmt.Errorf("the Pod log carries the statement %q", refusedStatementStatement)
	case strings.Contains(string(log), refusedStatementIndex):
		return fmt.Errorf("the Pod log carries the name the statement declares, %q", refusedStatementIndex)
	}
	return nil
}

// failedApplyLogReports holds a Pod log to there being what the long TTL keeps it for:
// the one line from the run. A row that does not know the database's words reads this.
func failedApplyLogReports(log []byte) error {
	if reported := reportedErrors(log); len(reported) != 1 {
		return fmt.Errorf("the Pod log holds %d reported errors, want 1", len(reported))
	}
	return nil
}

func reportedErrors(log []byte) []string {
	var reported []string
	for line := range strings.Lines(string(log)) {
		if strings.HasPrefix(line, reportedErrorPrefix) {
			reported = append(reported, strings.TrimSpace(line))
		}
	}
	return reported
}

// failedApplyPointerSays holds a message to where the log is and for how long,
// for the Job that ran. Naming another Job, or no time, leads nowhere.
func failedApplyPointerSays(message, namespace, job string) error {
	want := "for 24 hours after the Job finishes: kubectl logs -n " + namespace + " job/" + job + " -c ptah"
	if !strings.Contains(message, want) {
		return fmt.Errorf("the message does not say %q: %q", want, message)
	}
	return nil
}

// printedLogCommandPattern is the one command a row runs from a message: the log
// command the manager prints, last in the message, where a person copies it from.
var printedLogCommandPattern = regexp.MustCompile(
	`(?:^|\s)kubectl (logs -n [a-z0-9](?:[-a-z0-9]*[a-z0-9])? job/[a-z0-9](?:[-a-z0-9]*[a-z0-9])? -c ptah)$`)

// printedLogCommand reads the arguments of the kubectl command a message prints. They
// come from the message and nowhere else, so a row that runs them runs what a person
// would. A status is not a place a test takes any other command from, so anything but
// this one command, whole, is refused rather than run.
func printedLogCommand(message string) ([]string, error) {
	if count := strings.Count(message, "kubectl "); count != 1 {
		return nil, fmt.Errorf("the message prints %d kubectl commands, want 1: %q", count, message)
	}
	match := printedLogCommandPattern.FindStringSubmatch(message)
	if match == nil {
		return nil, fmt.Errorf("the message does not end in the log command: %q", message)
	}
	return strings.Fields(match[1]), nil
}

// readPrintedLog runs the command a message prints and returns what it wrote to
// standard output. kubectl is the cluster's, which adds the kubeconfig and nothing else.
func readPrintedLog(message string, kubectl func(arguments ...string) (stdout, stderr []byte, err error)) ([]byte, error) {
	arguments, err := printedLogCommand(message)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := kubectl(arguments...)
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(arguments, " "), err, stderr)
	}
	return stdout, nil
}

// jobFinishedAt is the instant a finished Job's cleanup TTL counts from, read as the
// TTL controller reads it (jobFinishTime in pkg/controller/ttlafterfinished): the
// transition of a Complete or Failed condition that is true. A Job that met its
// success criteria or is about to fail has not started that timer.
func jobFinishedAt(job *batchv1.Job) (time.Time, error) {
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) &&
			condition.Status == corev1.ConditionTrue {
			if condition.LastTransitionTime.IsZero() {
				return time.Time{}, fmt.Errorf("Job %s finished at no recorded time", job.Name)
			}
			return condition.LastTransitionTime.Time, nil
		}
	}
	return time.Time{}, fmt.Errorf("Job %s has not finished", job.Name)
}

// shortTTLDeadline is when the TTL controller would have collected a finished Job
// that kept the five minutes every other finished Job keeps.
func shortTTLDeadline(job *batchv1.Job) (time.Time, error) {
	finished, err := jobFinishedAt(job)
	if err != nil {
		return time.Time{}, err
	}
	return finished.Add(time.Duration(jobclaim.CleanupTTLSeconds) * time.Second), nil
}

// failedApplyJobOutlivedShortTTL holds a reading of a Job kept for its log to the
// claim a row can measure without waiting 24 hours: the Job that ran, still there and
// still holding the long TTL, read no earlier than the deadline the short TTL gives it
// by its own finish time. readAt is taken before the read, so the read is no earlier
// than that.
func failedApplyJobOutlivedShortTTL(kept, read *batchv1.Job, readAt time.Time) error {
	deadline, err := shortTTLDeadline(read)
	switch {
	case err != nil:
		return err
	case read.UID != kept.UID:
		return fmt.Errorf("Job %s is UID %s now, want %s", read.Name, read.UID, kept.UID)
	case read.DeletionTimestamp != nil:
		return fmt.Errorf("Job %s is being deleted", read.Name)
	case readAt.Before(deadline):
		return fmt.Errorf("Job %s was read at %s, before its short TTL would end at %s", read.Name, readAt, deadline)
	}
	return failedApplyJobRetained(read)
}

// statusKeepsTheAnswerOut refuses a status or an Event that repeats what the
// database said. The pointer belongs there, and the answer does not.
func statusKeepsTheAnswerOut(what, document string) error {
	if document == "" {
		return errors.New(what + " is empty, so it proves nothing")
	}
	for _, text := range []string{"could not create", "unique index", refusedStatementIndex, refusedStatementStatement} {
		if strings.Contains(document, text) {
			return fmt.Errorf("%s carries %q: %s", what, text, document)
		}
	}
	return nil
}

// failedApplyJobRetained holds a Job to the long TTL. A Job that says nothing,
// or the five minutes every other Job keeps, loses the log.
func failedApplyJobRetained(job *batchv1.Job) error {
	if ttl := job.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != jobclaim.FailedApplyRetentionTTLSeconds {
		return fmt.Errorf("Job %s keeps %v seconds, want %d", job.Name, ttl, jobclaim.FailedApplyRetentionTTLSeconds)
	}
	return nil
}

// finishedJobKeepsShortTTL holds a Job to the five minutes. The long TTL on a run no
// approval names and whose report does not stop its resource is the Job per cycle the
// rule exists to prevent, and a Job with no TTL is never collected.
func finishedJobKeepsShortTTL(job *batchv1.Job) error {
	if ttl := job.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != jobclaim.CleanupTTLSeconds {
		return fmt.Errorf("Job %s keeps %v seconds, want %d", job.Name, ttl, jobclaim.CleanupTTLSeconds)
	}
	return nil
}

// cleanupScheduled reads whether a Job carries one of the two cleanup TTLs. An
// Apply the successor reads reported success or an unknown outcome, so either
// is a cleanup scheduled, and any other value is neither.
func cleanupScheduled(ttl int64) bool {
	return ttl == int64(jobclaim.CleanupTTLSeconds) || ttl == int64(jobclaim.FailedApplyRetentionTTLSeconds)
}
