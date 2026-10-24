package e2e

import (
	"fmt"
	"strings"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// approvedApplyJournaled is nil when the journal holds the life of the plan an
// approval applied: the plan published, the approval that applied it
// accepted, the Apply under that approval succeeded, and the proof after it
// found the database in sync. The approval that applied it is the plan's last
// one: an earlier approval of the same plan may have been spent on an Apply
// whose outcome nobody could prove, which a fresh approval follows. An entry
// of another plan in between is not part of it, and an Apply that ended before
// it began is a record nobody wrote from a run.
//
// The plan is published once until a proof settles it or another plan
// replaces it. A publication again before that is the plan its Apply left in
// status, recorded by a later write that only named it again.
func approvedApplyJournaled(journal *ptahv1alpha1.LifecycleJournal, plan string) error {
	if journal == nil {
		return fmt.Errorf("the schema keeps no journal")
	}
	open := false
	for _, entry := range journal.Entries {
		switch {
		case entry.Type == ptahv1alpha1.JournalPlanPublished && entry.Plan == plan:
			if open {
				return fmt.Errorf("the journal publishes plan %s again before a proof settled it: %s", plan, journalOf(journal, plan))
			}
			open = true
		case entry.Type == ptahv1alpha1.JournalPlanPublished,
			entry.Type == ptahv1alpha1.JournalVerified && entry.Plan == plan:
			open = false
		}
	}
	approved := -1
	for index, entry := range journal.Entries {
		if entry.Plan == plan && entry.Type == ptahv1alpha1.JournalApprovalAccepted {
			approved = index
		}
	}
	if approved < 0 {
		return fmt.Errorf("the journal of plan %s stops before %s: %s", plan, ptahv1alpha1.JournalApprovalAccepted, journalOf(journal, plan))
	}
	published := false
	for _, entry := range journal.Entries[:approved] {
		published = published || (entry.Plan == plan && entry.Type == ptahv1alpha1.JournalPlanPublished)
	}
	if !published {
		return fmt.Errorf("the journal of plan %s accepts an approval of a plan it never published: %s", plan, journalOf(journal, plan))
	}
	steps := []struct {
		entry   ptahv1alpha1.JournalEntryType
		outcome ptahv1alpha1.JournalOutcome
	}{
		{ptahv1alpha1.JournalApplyFinished, ptahv1alpha1.JournalSucceeded},
		{ptahv1alpha1.JournalVerified, ptahv1alpha1.JournalInSync},
	}
	next := 0
	for _, entry := range journal.Entries[approved+1:] {
		if next == len(steps) || entry.Plan != plan || entry.Type != steps[next].entry {
			continue
		}
		if entry.Outcome != steps[next].outcome {
			return fmt.Errorf("the journal records %s of %s as %q, not %q: %s", entry.Type, plan, entry.Outcome, steps[next].outcome, journalOf(journal, plan))
		}
		if entry.Type == ptahv1alpha1.JournalApplyFinished && (entry.StartedAt == nil || entry.StartedAt.After(entry.At.Time)) {
			return fmt.Errorf("the journal records an Apply of %s that did not start before it ended", plan)
		}
		next++
	}
	if next < len(steps) {
		return fmt.Errorf("the journal of plan %s stops before %s: %s", plan, steps[next].entry, journalOf(journal, plan))
	}
	return nil
}

// journalOf is the plan's entries as a failure prints them, in order, so a
// refusal says what the journal held rather than only what it lacked.
func journalOf(journal *ptahv1alpha1.LifecycleJournal, plan string) string {
	var described []string
	for _, entry := range journal.Entries {
		if entry.Plan != plan {
			continue
		}
		text := string(entry.Type)
		if entry.Approval != "" {
			text += " " + entry.Approval
		}
		if entry.Outcome != "" {
			text += " " + string(entry.Outcome)
		}
		described = append(described, text+" at "+entry.At.UTC().Format("15:04:05"))
	}
	return fmt.Sprintf("%d entries, %d of the plan [%s]", len(journal.Entries), len(described), strings.Join(described, "; "))
}
