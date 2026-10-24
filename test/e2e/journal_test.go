package e2e

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestApprovedApplyJournaledReadsThePlansLifeInOrder(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)
	entry := func(kind ptahv1alpha1.JournalEntryType, plan string, minute int, outcome ptahv1alpha1.JournalOutcome) ptahv1alpha1.JournalEntry {
		return ptahv1alpha1.JournalEntry{Type: kind, Plan: plan, At: metav1.NewTime(start.Add(time.Duration(minute) * time.Minute)), Outcome: outcome}
	}
	apply := func(plan string, minute int, outcome ptahv1alpha1.JournalOutcome, started int) ptahv1alpha1.JournalEntry {
		finished := entry(ptahv1alpha1.JournalApplyFinished, plan, minute, outcome)
		began := metav1.NewTime(start.Add(time.Duration(started) * time.Minute))
		finished.StartedAt = &began
		return finished
	}
	life := []ptahv1alpha1.JournalEntry{
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 1, ""),
		entry(ptahv1alpha1.JournalApprovalAccepted, "plan-a", 2, ""),
		apply("plan-a", 4, ptahv1alpha1.JournalSucceeded, 3),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 5, ptahv1alpha1.JournalInSync),
	}
	with := func(entries ...ptahv1alpha1.JournalEntry) *ptahv1alpha1.LifecycleJournal {
		return &ptahv1alpha1.LifecycleJournal{Entries: entries}
	}
	if err := approvedApplyJournaled(with(life...), "plan-a"); err != nil {
		t.Fatalf("a plan's whole life was refused: %v", err)
	}
	earlier := append([]ptahv1alpha1.JournalEntry{entry(ptahv1alpha1.JournalPlanPublished, "plan-old", 0, "")}, life...)
	if err := approvedApplyJournaled(with(earlier...), "plan-a"); err != nil {
		t.Fatalf("another plan's entry before it was refused: %v", err)
	}
	approval := func(name string, minute int) ptahv1alpha1.JournalEntry {
		accepted := entry(ptahv1alpha1.JournalApprovalAccepted, "plan-a", minute, "")
		accepted.Approval = name
		return accepted
	}
	// The unsupported-runner row: the first approval is spent on an Apply the
	// runner refused, whose outcome nobody can prove, and a fresh approval of
	// the same plan applies it.
	respent := []ptahv1alpha1.JournalEntry{
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 1, ""),
		approval("old", 2),
		apply("plan-a", 4, ptahv1alpha1.JournalUnknown, 3),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 5, ptahv1alpha1.JournalDriftRemains),
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 6, ""),
		approval("current", 7),
		apply("plan-a", 9, ptahv1alpha1.JournalSucceeded, 8),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 10, ptahv1alpha1.JournalInSync),
	}
	if err := approvedApplyJournaled(with(respent...), "plan-a"); err != nil {
		t.Fatalf("a plan applied under a fresh approval after an unproven Apply was refused: %v", err)
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		want    string
	}{
		{"no journal", nil, "keeps no journal"},
		{"no proof", with(life[:3]...), "stops before Verified"},
		{"another plan's proof", with(append(life[:3:3], entry(ptahv1alpha1.JournalVerified, "plan-b", 5, ptahv1alpha1.JournalInSync))...), "stops before Verified"},
		{"out of order", with(life[0], life[2], life[1], life[3]), "stops before"},
		{"an Apply of unknown outcome", with(life[0], life[1], apply("plan-a", 4, ptahv1alpha1.JournalUnknown, 3), life[3]), `as "Unknown"`},
		{"a proof that found drift", with(life[0], life[1], life[2], entry(ptahv1alpha1.JournalVerified, "plan-a", 5, ptahv1alpha1.JournalDriftRemains)), `as "DriftRemains"`},
		{"an Apply that ended before it began", with(life[0], life[1], apply("plan-a", 4, ptahv1alpha1.JournalSucceeded, 6), life[3]), "did not start before"},
		{"an unproven Apply under the last approval, retried without one", with(respent[0], respent[1], respent[2], respent[6], respent[7]), `as "Unknown"`},
		{"an approval of a plan never published", with(life[1], life[2], life[3]), "never published"},
		{"no approval", with(life[0], life[2], life[3]), "stops before ApprovalAccepted"},
		{"a fresh approval nothing applied", with(append(respent[:6:6], respent[7])...), "stops before ApplyFinished"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := approvedApplyJournaled(test.journal, "plan-a")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("approvedApplyJournaled() = %v, want an error naming %q", err, test.want)
			}
		})
	}
}
