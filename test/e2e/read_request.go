package e2e

import (
	"fmt"
	"time"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRequestBound is how long a read on request may take, from the request
// to the history the read recorded: a pass to answer it, a pass to claim the
// read, and the Jobs of Resolve, Verify and the history read.
const readRequestBound = 4 * time.Minute

// historyReadOnRequest is a migration whose history was read because a person
// asked: the status answered exactly that request, the history was read after
// it and within readRequestBound, before the read the interval had set, and it
// found the database at version. A reading the interval took, one older than
// the request, and one of another request all fail it.
func historyReadOnRequest(migration *ptahv1alpha1.PtahMigration, value string, requested, intervalRead time.Time, version int64) bool {
	history := migration.Status.History
	if migration.Status.LastReadRequest != value || history == nil || history.ObservedAt.IsZero() {
		return false
	}
	read := history.ObservedAt.Time
	return read.After(requested) && read.Before(requested.Add(readRequestBound)) && read.Before(intervalRead) &&
		history.CurrentVersion == version
}

// observedOnRequest is nil when exactly one Observe Job ran for a read
// request: created no earlier than the second the request names, within
// readRequestBound of it, and before the read the interval had set. The time
// is the Job's own, not when a poll noticed it.
func observedOnRequest(jobs []observedJob, requested, intervalRead time.Time) error {
	if len(jobs) != 1 {
		return fmt.Errorf("%d Observe Jobs followed the read request, not one", len(jobs))
	}
	created, err := time.Parse(time.RFC3339, jobs[0].Created)
	if err != nil {
		return fmt.Errorf("Observe Job %s records no creation time: %w", jobs[0].Name, err)
	}
	switch {
	case created.Before(requested):
		return fmt.Errorf("Observe Job %s was created at %s, before the request at %s", jobs[0].Name, created, requested)
	case !created.Before(requested.Add(readRequestBound)):
		return fmt.Errorf("Observe Job %s was created at %s, more than %s after the request", jobs[0].Name, created, readRequestBound)
	case !created.Before(intervalRead):
		return fmt.Errorf("Observe Job %s was created at %s, when the interval would have read anyway", jobs[0].Name, created)
	}
	return nil
}
