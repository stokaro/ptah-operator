package e2e

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// applyLeaseTerm is the end of the term an Apply claim took the realm Lease
// for, read from what the claim persisted: its startedAt plus its
// leaseDurationSeconds. A PtahMigration whose Apply Job and Pod are gone, with
// nothing to say the executor stopped, keeps the claim and the Lease until then
// (ApplyPodGone).
func applyLeaseTerm(claim *ptahv1alpha1.MigrationOperationStatus) (time.Time, error) {
	switch {
	case claim == nil:
		return time.Time{}, errors.New("there is no claim to read a Lease term from")
	case claim.StartedAt.IsZero():
		return time.Time{}, errors.New("the claim records no startedAt")
	case claim.LeaseDurationSeconds < 1:
		return time.Time{}, fmt.Errorf("the claim records a Lease duration of %d seconds", claim.LeaseDurationSeconds)
	}
	return claim.StartedAt.Add(time.Duration(claim.LeaseDurationSeconds) * time.Second), nil
}

// applyPodGoneWarnings returns the ApplyPodGone Warnings recorded against this
// exact migration that say its deletion waits for the claim's Job, by name and
// UID, until term. A migration of the same name from an earlier row, a Normal
// Event, another reason, another Job under the same name, a live migration's
// words and another end of the wait do not count.
func applyPodGoneWarnings(events []corev1.Event, migration *ptahv1alpha1.PtahMigration,
	jobName string, jobUID types.UID, term time.Time,
) []corev1.Event {
	says := fmt.Sprintf("Deletion waits for Job %s, UID %s, until %s: ", jobName, jobUID, term.UTC().Format(time.RFC3339))
	var warnings []corev1.Event
	for _, event := range events {
		reference := event.InvolvedObject
		if event.Type == corev1.EventTypeWarning && event.Reason == "ApplyPodGone" &&
			reference.Kind == "PtahMigration" && reference.Name == migration.Name && reference.UID == migration.UID &&
			strings.HasPrefix(event.Message, says) {
			warnings = append(warnings, event)
		}
	}
	return warnings
}

// applyPodGoneWarnedInTerm holds the Warnings applyPodGoneWarnings found to
// having been recorded inside the claim's term, by the Events' own times: at or
// after the claim started, and before the term ended. A Warning dated at the
// term or after it would be one the manager recorded while it was handing the
// Lease back.
func applyPodGoneWarnedInTerm(warnings []corev1.Event, startedAt, term time.Time) error {
	if len(warnings) == 0 {
		return errors.New("no ApplyPodGone Warning names the Job and the end of its term")
	}
	for _, warning := range warnings {
		first, last := eventFirstSeen(&warning), eventLastSeen(&warning)
		switch {
		case first.IsZero() || last.IsZero():
			return fmt.Errorf("the ApplyPodGone Warning %s carries no time of its own", warning.Name)
		case first.Before(startedAt.Truncate(time.Second)):
			return fmt.Errorf("the ApplyPodGone Warning %s was first recorded at %s, before the claim started at %s",
				warning.Name, first.UTC().Format(time.RFC3339), startedAt.UTC().Format(time.RFC3339))
		case !last.Before(term):
			return fmt.Errorf("the ApplyPodGone Warning %s was last recorded at %s, not before the term ended at %s",
				warning.Name, last.UTC().Format(time.RFC3339), term.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// applyPodGoneReading is what the deletion row prints of the ApplyPodGone
// Warning it matched, with the migration and the claim it matched it against.
type applyPodGoneReading struct {
	MigrationName string                                `json:"migrationName"`
	MigrationUID  types.UID                             `json:"migrationUID"`
	Claim         ptahv1alpha1.MigrationOperationStatus `json:"claim"`
	Event         corev1.Event                          `json:"event"`
}

// eventFirstSeen and eventLastSeen are when an Event was first and last
// recorded, as the recorder wrote them: the series' times on an events.k8s.io
// Event, the timestamps on a core one.
func eventFirstSeen(event *corev1.Event) time.Time {
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.FirstTimestamp.Time
}

func eventLastSeen(event *corev1.Event) time.Time {
	switch {
	case event.Series != nil && !event.Series.LastObservedTime.IsZero():
		return event.Series.LastObservedTime.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	}
	return event.LastTimestamp.Time
}
