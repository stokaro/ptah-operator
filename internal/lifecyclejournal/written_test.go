package lifecyclejournal_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// Record says how many of the newest entries a write wrote, so the write can
// read exactly those back from what the API server stored. The rows hold the
// count to the entries the journal gained or changed, and Newest to returning
// them.
func TestRecordCountsTheNewestEntriesTheWriteWrote(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	waiting := published("plan-waiting", -40*day)
	waiting.ApprovalRequired = true
	full := &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour)}
	for index := range lifecyclejournal.MaxEntries {
		full.Entries = append(full.Entries, failure("Observe", fmt.Sprintf("Reason%02d", index), time.Duration(index-lifecyclejournal.MaxEntries)*time.Second))
	}
	folded := failure("Observe", "ObservationFailed", -2*time.Minute)
	folded.LastAt, folded.Count = ptr(at(-time.Minute)), 2

	for _, test := range []struct {
		name    string
		journal *operatorv1alpha1.LifecycleJournal
		entries []operatorv1alpha1.JournalEntry
		want    []operatorv1alpha1.JournalEntry
	}{
		{
			name:    "a write that records nothing",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: []operatorv1alpha1.JournalEntry{published("plan-a", -time.Hour)}},
		},
		{
			name:    "the first write of a journal",
			entries: []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), failure("Plan", "PlanFailed", 0)},
			want:    []operatorv1alpha1.JournalEntry{published("plan-a", -time.Minute), failure("Plan", "PlanFailed", 0)},
		},
		{
			name: "entries added after stored ones",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: []operatorv1alpha1.JournalEntry{
				published("plan-a", -time.Hour), failure("Observe", "ObservationFailed", -30*time.Minute),
			}},
			entries: []operatorv1alpha1.JournalEntry{published("plan-b", 0)},
			want:    []operatorv1alpha1.JournalEntry{published("plan-b", 0)},
		},
		{
			// The stored entry the attempt folds into is the write's own: its
			// count moved.
			name: "a failure folded into the newest stored entry",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: []operatorv1alpha1.JournalEntry{
				published("plan-a", -time.Hour), failure("Observe", "ObservationFailed", -2*time.Minute),
			}},
			entries: []operatorv1alpha1.JournalEntry{failure("Observe", "ObservationFailed", -time.Minute)},
			want:    []operatorv1alpha1.JournalEntry{folded},
		},
		{
			name: "a fold and an entry after it",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: []operatorv1alpha1.JournalEntry{
				published("plan-a", -time.Hour), failure("Observe", "ObservationFailed", -2*time.Minute),
			}},
			entries: []operatorv1alpha1.JournalEntry{failure("Observe", "ObservationFailed", -time.Minute), published("plan-b", 0)},
			want:    []operatorv1alpha1.JournalEntry{folded, published("plan-b", 0)},
		},
		{
			// Two attempts of one write fold into one entry, which is written once.
			name:    "a failure folded into an entry the same write added",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-time.Hour), Entries: []operatorv1alpha1.JournalEntry{published("plan-a", -time.Hour)}},
			entries: []operatorv1alpha1.JournalEntry{
				failure("Observe", "ObservationFailed", -2*time.Minute), failure("Observe", "ObservationFailed", -time.Minute),
			},
			want: []operatorv1alpha1.JournalEntry{folded},
		},
		{
			// The count bound drops the oldest stored entry, never the new one.
			name:    "an entry added to a full journal",
			journal: full,
			entries: []operatorv1alpha1.JournalEntry{published("plan-b", 0)},
			want:    []operatorv1alpha1.JournalEntry{published("plan-b", 0)},
		},
		{
			// The age bound can drop an entry this write added, dated a month
			// before the newest one, while it keeps the older plan nothing has
			// carried out. That plan is stored but is not the write's own.
			name: "an added entry the age bound drops behind a kept plan",
			journal: &operatorv1alpha1.LifecycleJournal{CoveredSince: at(-50 * day), Entries: []operatorv1alpha1.JournalEntry{
				waiting,
			}},
			entries: []operatorv1alpha1.JournalEntry{failure("Observe", "ObservationFailed", -35*day), failure("Plan", "PlanFailed", 0)},
			want:    []operatorv1alpha1.JournalEntry{failure("Plan", "PlanFailed", 0)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			journal, written := lifecyclejournal.Record(test.journal, test.entries, now)
			if written != len(test.want) {
				t.Fatalf("Record counted %d written entries of %#v, want %d", written, journal.Entries, len(test.want))
			}
			if got := lifecyclejournal.Newest(journal, written); !reflect.DeepEqual(got, test.want) && (len(got) != 0 || len(test.want) != 0) {
				t.Fatalf("Newest = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNewestReturnsNoMoreThanTheJournalHolds(t *testing.T) {
	t.Parallel()
	journal := &operatorv1alpha1.LifecycleJournal{Entries: []operatorv1alpha1.JournalEntry{published("plan-a", 0)}}
	for _, test := range []struct {
		name    string
		journal *operatorv1alpha1.LifecycleJournal
		count   int
		want    int
	}{
		{"no journal", nil, 2, 0},
		{"no count", journal, 0, 0},
		{"a count past the journal", journal, 3, 1},
	} {
		if got := lifecyclejournal.Newest(test.journal, test.count); len(got) != test.want {
			t.Errorf("%s: Newest returned %d entries, want %d", test.name, len(got), test.want)
		}
	}
}
