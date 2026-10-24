package e2e

import (
	"fmt"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// approvedApplyJournaled is nil when the journal holds the life of the plan an
// approval applied, in order: published, its approval accepted, the Apply that
// succeeded, and the proof that found the database in sync. An entry of
// another plan in between is not part of it, and an Apply that ended before it
// began is a record nobody wrote from a run.
func approvedApplyJournaled(journal *ptahv1alpha1.LifecycleJournal, plan string) error {
	if journal == nil {
		return fmt.Errorf("the schema keeps no journal")
	}
	steps := []struct {
		entry   ptahv1alpha1.JournalEntryType
		outcome ptahv1alpha1.JournalOutcome
	}{
		{ptahv1alpha1.JournalPlanPublished, ""},
		{ptahv1alpha1.JournalApprovalAccepted, ""},
		{ptahv1alpha1.JournalApplyFinished, ptahv1alpha1.JournalSucceeded},
		{ptahv1alpha1.JournalVerified, ptahv1alpha1.JournalInSync},
	}
	next := 0
	for _, entry := range journal.Entries {
		if next == len(steps) || entry.Plan != plan || entry.Type != steps[next].entry {
			continue
		}
		if entry.Outcome != steps[next].outcome {
			return fmt.Errorf("the journal records %s of %s as %q, not %q", entry.Type, plan, entry.Outcome, steps[next].outcome)
		}
		if entry.Type == ptahv1alpha1.JournalApplyFinished && (entry.StartedAt == nil || entry.StartedAt.After(entry.At.Time)) {
			return fmt.Errorf("the journal records an Apply of %s that did not start before it ended", plan)
		}
		next++
	}
	if next < len(steps) {
		return fmt.Errorf("the journal of plan %s stops before %s: %d entries", plan, steps[next].entry, len(journal.Entries))
	}
	return nil
}
