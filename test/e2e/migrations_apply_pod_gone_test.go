package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// applyPodGoneDeletionReading is the reading the deletion row printed on a
// kind run: the ApplyPodGone Warning it matched, with the migration and the
// claim it matched it against.
func applyPodGoneDeletionReading(t *testing.T) applyPodGoneReading {
	t.Helper()
	return applyPodGoneReadingFrom(t, "apply-pod-gone-deletion.json")
}

// applyPodGoneLiveReading is the reading the unresolved-alert row printed on a
// kind run, of a live migration whose Apply Job it deleted.
func applyPodGoneLiveReading(t *testing.T) applyPodGoneReading {
	t.Helper()
	return applyPodGoneReadingFrom(t, "apply-pod-gone-live.json")
}

func applyPodGoneReadingFrom(t *testing.T, name string) applyPodGoneReading {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	var reading applyPodGoneReading
	if err := json.Unmarshal(body, &reading); err != nil {
		t.Fatal(err)
	}
	if reading.MigrationUID == "" || reading.Claim.JobUID == "" || reading.Event.Reason != "ApplyPodGone" {
		t.Fatalf("the reading lacks what the rows below read: %+v", reading)
	}
	return reading
}

func applyPodGoneMigration(reading applyPodGoneReading) *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Name, migration.UID = reading.MigrationName, reading.MigrationUID
	return migration
}

func TestApplyLeaseTermIsTheClaimsStartPlusItsLeaseDuration(t *testing.T) {
	t.Parallel()

	reading := applyPodGoneDeletionReading(t)
	term, err := applyLeaseTerm(&reading.Claim)
	if err != nil {
		t.Fatal(err)
	}
	if want := reading.Claim.StartedAt.Add(time.Duration(reading.Claim.LeaseDurationSeconds) * time.Second); !term.Equal(want) {
		t.Fatalf("applyLeaseTerm() = %s, want %s", term, want)
	}
	if notAfter := reading.Claim.ExecutionNotAfter; notAfter == nil || !term.After(notAfter.Time) {
		t.Fatalf("the term %s is not after the claim's execution deadline %v", term, notAfter)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationOperationStatus){
		"no startedAt":      func(claim *ptahv1alpha1.MigrationOperationStatus) { claim.StartedAt = metav1.Time{} },
		"no Lease duration": func(claim *ptahv1alpha1.MigrationOperationStatus) { claim.LeaseDurationSeconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			claim := reading.Claim.DeepCopy()
			mutate(claim)
			if term, err := applyLeaseTerm(claim); err == nil {
				t.Fatalf("applyLeaseTerm() = %s, want a refusal", term)
			}
		})
	}
	if term, err := applyLeaseTerm(nil); err == nil {
		t.Fatalf("applyLeaseTerm(nil) = %s, want a refusal", term)
	}
}

func TestApplyPodGoneWarningsCountOnlyTheClaimsWarningToItsTerm(t *testing.T) {
	t.Parallel()

	reading := applyPodGoneDeletionReading(t)
	term, err := applyLeaseTerm(&reading.Claim)
	if err != nil {
		t.Fatal(err)
	}
	type inputs struct {
		waiter string
		jobUID types.UID
		term   time.Time
	}
	t.Run("the reading", func(t *testing.T) {
		t.Parallel()
		found := applyPodGoneWarnings([]corev1.Event{reading.Event}, applyPodGoneMigration(reading), applyPodGoneDeletionWaits, reading.Claim.JobName, reading.Claim.JobUID, term)
		if len(found) != 1 {
			t.Fatalf("applyPodGoneWarnings() = %v, want the one Warning", found)
		}
	})
	// The live row's own reading counts for the waiter it names, and not for
	// a deletion's.
	t.Run("the live reading", func(t *testing.T) {
		t.Parallel()
		live := applyPodGoneLiveReading(t)
		liveTerm, err := applyLeaseTerm(&live.Claim)
		if err != nil {
			t.Fatal(err)
		}
		migration := applyPodGoneMigration(live)
		if found := applyPodGoneWarnings([]corev1.Event{live.Event}, migration, applyPodGoneApplyWaits, live.Claim.JobName, live.Claim.JobUID, liveTerm); len(found) != 1 {
			t.Fatalf("applyPodGoneWarnings() = %v, want the one Warning", found)
		}
		if found := applyPodGoneWarnings([]corev1.Event{live.Event}, migration, applyPodGoneDeletionWaits, live.Claim.JobName, live.Claim.JobUID, liveTerm); len(found) != 0 {
			t.Fatalf("applyPodGoneWarnings() counted %v for a deletion", found)
		}
		if err := applyPodGoneWarnedInTerm([]corev1.Event{live.Event}, live.Claim.StartedAt.Time, liveTerm); err != nil {
			t.Fatalf("applyPodGoneWarnedInTerm() refused the reading: %v", err)
		}
	})
	for _, row := range []struct {
		name   string
		event  func(*corev1.Event)
		inputs func(*inputs)
	}{
		{name: "a migration of the same name and another UID", event: func(event *corev1.Event) { event.InvolvedObject.UID = "other-uid" }},
		{name: "another migration", event: func(event *corev1.Event) { event.InvolvedObject.Name = "other" }},
		{name: "a PtahSchema", event: func(event *corev1.Event) { event.InvolvedObject.Kind = "PtahSchema" }},
		{name: "a Normal Event", event: func(event *corev1.Event) { event.Type = corev1.EventTypeNormal }},
		{name: "a Pod stuck terminating", event: func(event *corev1.Event) { event.Reason = "PodStuckTerminating" }},
		{name: "a live migration's words", event: func(event *corev1.Event) {
			event.Message = "The Apply waits for" + event.Message[len("Deletion waits for"):]
		}},
		// The same Warning read by a row that waits on a live migration.
		{name: "a deletion's words for a live migration", inputs: func(in *inputs) { in.waiter = applyPodGoneApplyWaits }},
		// client-go folds Warnings of one reason with different messages into
		// this. A hold repeats one message, so a combined one is not its own.
		{name: "a combination of similar Warnings", event: func(event *corev1.Event) {
			event.Message = "(combined from similar events): " + event.Message
		}},
		// The Job a later attempt created under the claim's name.
		{name: "another Job under the same name", inputs: func(in *inputs) { in.jobUID = "replacement-job-uid" }},
		// The bound the issue proposed, short of the claim's term.
		{name: "the execution deadline and 30 seconds", inputs: func(in *inputs) {
			in.term = reading.Claim.ExecutionNotAfter.Add(30 * time.Second)
		}},
		{name: "the term one second early", inputs: func(in *inputs) { in.term = in.term.Add(-time.Second) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			event := *reading.Event.DeepCopy()
			if row.event != nil {
				row.event(&event)
			}
			in := inputs{waiter: applyPodGoneDeletionWaits, jobUID: reading.Claim.JobUID, term: term}
			if row.inputs != nil {
				row.inputs(&in)
			}
			if found := applyPodGoneWarnings([]corev1.Event{event}, applyPodGoneMigration(reading), in.waiter, reading.Claim.JobName, in.jobUID, in.term); len(found) != 0 {
				t.Fatalf("applyPodGoneWarnings() counted %v", found)
			}
		})
	}
}

func TestApplyPodGoneWarnedInTermDatesTheWarningByItsOwnTimes(t *testing.T) {
	t.Parallel()

	reading := applyPodGoneDeletionReading(t)
	term, err := applyLeaseTerm(&reading.Claim)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := reading.Claim.StartedAt.Time
	if err := applyPodGoneWarnedInTerm([]corev1.Event{reading.Event}, startedAt, term); err != nil {
		t.Fatalf("applyPodGoneWarnedInTerm() refused the reading: %v", err)
	}
	if err := applyPodGoneWarnedInTerm(nil, startedAt, term); err == nil {
		t.Fatal("applyPodGoneWarnedInTerm() accepted no Warning")
	}
	for name, mutate := range map[string]func(*corev1.Event){
		"no times of its own": func(event *corev1.Event) {
			event.FirstTimestamp, event.LastTimestamp, event.EventTime, event.Series = metav1.Time{}, metav1.Time{}, metav1.MicroTime{}, nil
		},
		// A zero last time is before every term, so only the check for a
		// missing time refuses it.
		"a first time and no last time": func(event *corev1.Event) {
			event.EventTime, event.Series = metav1.MicroTime{}, nil
			event.LastTimestamp = metav1.Time{}
		},
		"last recorded at the term": func(event *corev1.Event) {
			event.EventTime, event.Series = metav1.MicroTime{}, nil
			event.LastTimestamp = metav1.NewTime(term)
		},
		"last recorded after the term in a series": func(event *corev1.Event) {
			event.EventTime = metav1.NewMicroTime(term.Add(-time.Minute))
			event.Series = &corev1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(term.Add(time.Second))}
		},
		"first recorded before the claim started": func(event *corev1.Event) {
			event.EventTime = metav1.MicroTime{}
			event.FirstTimestamp = metav1.NewTime(startedAt.Add(-time.Minute))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			event := *reading.Event.DeepCopy()
			mutate(&event)
			if err := applyPodGoneWarnedInTerm([]corev1.Event{event}, startedAt, term); err == nil {
				t.Fatalf("applyPodGoneWarnedInTerm() accepted %+v", event)
			}
		})
	}
}

// applyPodRemovalReading is a Pod history a kind cluster stored, read from
// testdata/e2e/readings, with the Pod's UID and its Job's.
func applyPodRemovalReading(t *testing.T, name string) (events []watchEvent[*corev1.Pod], podUID, jobUID types.UID) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Object == nil || len(events[0].Object.OwnerReferences) != 1 {
		t.Fatalf("the reading %s lacks what the rows below read", name)
	}
	first := events[0].Object
	return events, first.UID, first.OwnerReferences[0].UID
}

func clonePodEvents(events []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
	clone := make([]watchEvent[*corev1.Pod], 0, len(events))
	for _, event := range events {
		clone = append(clone, watchEvent[*corev1.Pod]{Type: event.Type, Object: event.Object.DeepCopy()})
	}
	return clone
}

func TestApplyPodGoneUnseenRefusesAPodStoppedUnderItsJob(t *testing.T) {
	t.Parallel()

	background, podUID, jobUID := applyPodRemovalReading(t, "apply-pod-removal-background.json")
	for name, events := range map[string][]watchEvent[*corev1.Pod]{
		// What a Job deleted with its Pods leaves: the kubelet stores the
		// Pod Failed, its executor terminated, before it lets the Pod go.
		"a Job deleted with its Pods": background,
		// A watch that resumed past every change still delivers the Pod's
		// removal, and the removal carries the last state.
		"only the Pod's creation and removal": {background[0], background[len(background)-1]},
		"a Pod that succeeded under its Job": func() []watchEvent[*corev1.Pod] {
			events := clonePodEvents(background)
			for _, event := range events {
				if event.Object.Status.Phase == corev1.PodFailed {
					event.Object.Status.Phase = corev1.PodSucceeded
				}
			}
			return events
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := applyPodGoneUnseen(events, podUID, jobUID); err == nil {
				t.Fatal("applyPodGoneUnseen() accepted a history that reads the Pod stopped under its Job")
			}
		})
	}
}

func TestApplyPodGoneUnseenAcceptsAPodOrphanedBeforeItStopped(t *testing.T) {
	t.Parallel()

	orphaned, podUID, jobUID := applyPodRemovalReading(t, "apply-pod-removal-orphaned.json")
	if err := applyPodGoneUnseen(orphaned, podUID, jobUID); err != nil {
		t.Fatalf("applyPodGoneUnseen() refused the orphaned history: %v", err)
	}
	// Another Job's Pods, stopped under their own Job, are not this one's.
	background, _, _ := applyPodRemovalReading(t, "apply-pod-removal-background.json")
	if err := applyPodGoneUnseen(slices.Concat(background, orphaned), podUID, jobUID); err != nil {
		t.Fatalf("applyPodGoneUnseen() refused the orphaned history beside another Job's: %v", err)
	}

	gone := orphaned[len(orphaned)-1]
	if gone.Type != watch.Deleted || controlledByJob(gone.Object.OwnerReferences, jobUID) {
		t.Fatalf("the orphaned reading does not end with the removal of a Pod outside its Job: %+v", gone)
	}
	for name, read := range map[string]func() ([]watchEvent[*corev1.Pod], types.UID, types.UID){
		"read against another Job": func() ([]watchEvent[*corev1.Pod], types.UID, types.UID) {
			return orphaned, podUID, "4b7f8f5a-0000-4000-8000-000000000000"
		},
		"read against another Pod of the Job": func() ([]watchEvent[*corev1.Pod], types.UID, types.UID) {
			return orphaned, "4b7f8f5a-0000-4000-8000-000000000001", jobUID
		},
		"a second Pod under the Job": func() ([]watchEvent[*corev1.Pod], types.UID, types.UID) {
			sibling := watchEvent[*corev1.Pod]{Type: watch.Added, Object: orphaned[0].Object.DeepCopy()}
			sibling.Object.Name, sibling.Object.UID = sibling.Object.Name+"-2", "4b7f8f5a-0000-4000-8000-000000000002"
			return append(clonePodEvents(orphaned), sibling), podUID, jobUID
		},
		"no removal": func() ([]watchEvent[*corev1.Pod], types.UID, types.UID) {
			return orphaned[:len(orphaned)-1], podUID, jobUID
		},
		// A Pod the GC did not orphan before it was deleted.
		"the Job's reference kept to the end": func() ([]watchEvent[*corev1.Pod], types.UID, types.UID) {
			events := clonePodEvents(orphaned)
			for _, event := range events {
				event.Object.OwnerReferences = orphaned[0].Object.OwnerReferences
			}
			return events, podUID, jobUID
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, pod, job := read()
			if err := applyPodGoneUnseen(events, pod, job); err == nil {
				t.Fatal("applyPodGoneUnseen() accepted the history")
			}
		})
	}
}
