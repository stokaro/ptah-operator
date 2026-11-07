package e2e

import (
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"

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
	var reported []string
	for line := range strings.Lines(string(log)) {
		if strings.HasPrefix(line, reportedErrorPrefix) {
			reported = append(reported, strings.TrimSpace(line))
		}
	}
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

// failedApplyPointerSays holds a message to where the log is and for how long,
// for the Job that ran. Naming another Job, or no time, leads nowhere.
func failedApplyPointerSays(message, namespace, job string) error {
	want := "for 24 hours after the Job finishes: kubectl logs -n " + namespace + " job/" + job + " -c ptah"
	if !strings.Contains(message, want) {
		return fmt.Errorf("the message does not say %q: %q", want, message)
	}
	return nil
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
