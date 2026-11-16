package e2e

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

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

// applyLeaseTermMargin is how far short of the claim's term a hold stops
// looking. The manager hands the Lease back on its first pass at the term,
// passes a few seconds apart, so a look in the last seconds could read the
// release that follows rather than the hold.
const applyLeaseTermMargin = 15 * time.Second

// applyLeaseTermSlack is how long after the claim's term a row allows the
// manager to let the claim go: a pass at the term, which the hold requeues
// every five seconds, then the status writes that retire the claim and hand
// the Lease back. The term itself is read from the claim, so the slack bounds
// what follows it and nothing before.
//
// In the uncertain row it also covers one reading cycle. That resource's next
// reading came due while the claim held the term, so it runs Resolve, Verify
// and History the moment the claim retires, and the row settles only on the
// refusal that reading leaves: 35 to 38 seconds after the term on CI.
const applyLeaseTermSlack = 2 * time.Minute

// The words an ApplyPodGone Warning opens with, by who waits: a deleted
// migration's finalizer, or a live migration's Apply.
const (
	applyPodGoneDeletionWaits = "Deletion waits"
	applyPodGoneApplyWaits    = "The Apply waits"
)

// applyPodGoneWarnings returns the ApplyPodGone Warnings recorded against this
// exact migration that say waiter waits for the claim's Job, by name and UID,
// until term. A migration of the same name from an earlier row, a Normal
// Event, another reason, another Job under the same name, the other waiter's
// words and another end of the wait do not count.
func applyPodGoneWarnings(events []corev1.Event, migration *ptahv1alpha1.PtahMigration,
	waiter, jobName string, jobUID types.UID, term time.Time,
) []corev1.Event {
	says := fmt.Sprintf("%s for Job %s, UID %s, until %s: ", waiter, jobName, jobUID, term.UTC().Format(time.RFC3339))
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
// Lease back. Only Warnings listed once the claim has let go can show one, so
// a row holds the set it lists then, as well as the set its hold found.
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

// applyPodGoneReading is what a row that holds a claim to its term prints of
// the ApplyPodGone Warning it matched, with the migration and the claim it
// matched it against.
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

// applyPodGoneUnseen is nil where a Pod history shows an Apply's Pod removed
// with no state a manager pass could have read as its Job's executor
// stopping. The events are every Pod the history holds; podUID is the Pod the
// Job ran and jobUID the Job.
//
// A state that reads Succeeded or Failed while it names the Job as its
// controller is the kubelet's word that the executor stopped, and a migration
// hands its realm Lease back at once on that, whatever the Pod does next
// (podStopUnconfirmed in internal/controller). A Job deleted with its Pods
// always leaves one: the kubelet records a Pod's terminal phase before it
// lets the Pod go. A state that names no such Job is not read as the Job's at
// all, since the manager finds a Job's Pods by that controller reference.
//
// The history has to name the Job's Pod under the Job at least once, which is
// what ties it to the two UIDs: read against another Job, every state would
// pass. It has to hold no other Pod of the Job, since the manager reads all of
// them, and it has to hold the Pod's removal, since a state after the last one
// read could still be the one that says it stopped.
func applyPodGoneUnseen(events []watchEvent[*corev1.Pod], podUID, jobUID types.UID) error {
	owned, gone := 0, false
	for _, event := range events {
		pod := event.Object
		if pod == nil {
			continue
		}
		underJob := controlledByJob(pod.OwnerReferences, jobUID)
		switch {
		case underJob && pod.UID != podUID:
			return fmt.Errorf("another Pod, %s with UID %s, names the Job as its controller", pod.Name, pod.UID)
		case underJob && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed):
			return fmt.Errorf("the Pod was stored %s at resourceVersion %s while it still named the Job as its controller",
				pod.Status.Phase, pod.ResourceVersion)
		case underJob:
			owned++
		}
		if pod.UID == podUID && event.Type == watch.Deleted {
			gone = true
		}
	}
	switch {
	case owned == 0:
		return errors.New("no state of the Pod named the Job as its controller")
	case !gone:
		return errors.New("the Pod's removal is not in the history")
	}
	return nil
}
