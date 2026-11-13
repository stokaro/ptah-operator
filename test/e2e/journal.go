package e2e

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"

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
// The plan is published again only once something carried it out or another
// plan replaced it: an Apply of unknown outcome spends its approval, and the
// operator puts the same plan back for a fresh one. It is never published
// again between an Apply that succeeded and the proof after it, where status
// still names it and every write names it again.
func approvedApplyJournaled(journal *ptahv1alpha1.LifecycleJournal, plan string) error {
	if journal == nil {
		return fmt.Errorf("the schema keeps no journal")
	}
	waiting, applied := false, false
	for _, entry := range journal.Entries {
		mine := entry.Plan == plan
		switch {
		case entry.Type == ptahv1alpha1.JournalPlanPublished && mine:
			if waiting || applied {
				return fmt.Errorf("the journal publishes plan %s again with nothing carried out or replaced since: %s", plan, journalOf(journal, plan))
			}
			waiting = true
		case entry.Type == ptahv1alpha1.JournalPlanPublished:
			waiting, applied = false, false
		case entry.Type == ptahv1alpha1.JournalApplyFinished && mine:
			waiting, applied = false, entry.Outcome == ptahv1alpha1.JournalSucceeded
		case entry.Type == ptahv1alpha1.JournalVerified && mine:
			applied = false
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

// approvalJournaledOnce is the decision in the journal: exactly one
// ApprovalAccepted carrying the approval's UID, under its name and on its plan.
// The name alone cannot say so, since a later decision may take it, and an entry
// that lost the UID on its way through the API server would read the same.
func approvalJournaledOnce(journal *ptahv1alpha1.LifecycleJournal, name, uid, plan string) error {
	if journal == nil {
		return fmt.Errorf("the resource keeps no journal")
	}
	count := 0
	for _, entry := range journal.Entries {
		if entry.Type != ptahv1alpha1.JournalApprovalAccepted || string(entry.ApprovalUID) != uid {
			continue
		}
		count++
		if entry.Approval != name || entry.Plan != plan {
			return fmt.Errorf("the entry of approval UID %s names approval %q on plan %q, want %q on %q", uid, entry.Approval, entry.Plan, name, plan)
		}
	}
	if count != 1 {
		return fmt.Errorf("the journal holds %d %s entries for approval UID %s, want one", count, ptahv1alpha1.JournalApprovalAccepted, uid)
	}
	return nil
}

// everyApprovalJournaledByUID holds the approvals journaled for a plan to the
// identity no later approval can take. A fresh cluster's manager wrote each of
// them, so an entry without a UID is the field lost on its way through the API
// server.
func everyApprovalJournaledByUID(journal *ptahv1alpha1.LifecycleJournal, plan string) error {
	if journal == nil {
		return fmt.Errorf("the resource keeps no journal")
	}
	examined := 0
	for _, entry := range journal.Entries {
		if entry.Type != ptahv1alpha1.JournalApprovalAccepted || entry.Plan != plan {
			continue
		}
		examined++
		if entry.ApprovalUID == "" {
			return fmt.Errorf("the journal records approval %q of plan %s with no UID: %s", entry.Approval, plan, journalOf(journal, plan))
		}
	}
	if examined == 0 {
		return fmt.Errorf("the journal records no approval of plan %s: %s", plan, journalOf(journal, plan))
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

// journaledApply is what an Apply ran as, read from the objects that ran it: the
// plan, the approval it consumed and the Job. stopped is where the run said it
// stopped, nil for an Apply that named no statement.
type journaledApply struct {
	plan     *ptahv1alpha1.PtahSchemaPlan
	approval *ptahv1alpha1.PtahSchemaApproval
	job      *batchv1.Job
	outcome  ptahv1alpha1.JournalOutcome
	stopped  *ptahv1alpha1.SchemaApplyFailureStatus
}

// applyJournaledAs is nil when the journal holds exactly one ApplyFinished entry
// of the plan, and that entry names the claim, the plan's UID and fingerprint,
// the approval's name, UID and approver, the Job's name and UID, and where the
// run stopped, each as the object says it. It returns the entry, whose operation
// ID is what the proof after it is joined by.
//
// The entry is found by the plan's name, which every release journaled, so an
// entry that lost one of the fields under test is refused for that field rather
// than missed.
func applyJournaledAs(journal *ptahv1alpha1.LifecycleJournal, want journaledApply) (ptahv1alpha1.JournalEntry, error) {
	if journal == nil {
		return ptahv1alpha1.JournalEntry{}, fmt.Errorf("the schema keeps no journal")
	}
	plan := want.plan.Name
	var found []ptahv1alpha1.JournalEntry
	for _, entry := range journal.Entries {
		if entry.Type == ptahv1alpha1.JournalApplyFinished && entry.Plan == plan {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		return ptahv1alpha1.JournalEntry{}, fmt.Errorf("the journal holds %d %s entries of plan %s, want one: %s",
			len(found), ptahv1alpha1.JournalApplyFinished, plan, journalOf(journal, plan))
	}
	entry := found[0]
	if !journalOperationPattern.MatchString(entry.OperationID) {
		return entry, fmt.Errorf("the %s entry of plan %s names operation %q, not an Apply claim's ID", entry.Type, plan, entry.OperationID)
	}
	for _, field := range []struct {
		name      string
		got, want string
	}{
		{"outcome", string(entry.Outcome), string(want.outcome)},
		{"planUID", string(entry.PlanUID), string(want.plan.UID)},
		{"planFingerprint", entry.PlanFingerprint, want.plan.Spec.Fingerprint},
		{"approval", entry.Approval, want.approval.Name},
		{"approvalUID", string(entry.ApprovalUID), string(want.approval.UID)},
		{"approver", entry.Approver, want.approval.Spec.Approver.Username},
		{"jobName", entry.JobName, want.job.Name},
		{"jobUID", string(entry.JobUID), string(want.job.UID)},
	} {
		if field.got != field.want {
			return entry, fmt.Errorf("the %s entry of plan %s records %s %q, the object says %q", entry.Type, plan, field.name, field.got, field.want)
		}
	}
	if !equality.Semantic.DeepEqual(entry.ApplyFailure, want.stopped) {
		return entry, fmt.Errorf("the %s entry of plan %s records applyFailure %s, want %s",
			entry.Type, plan, applyFailureText(entry.ApplyFailure), applyFailureText(want.stopped))
	}
	return entry, nil
}

// journalOperationPattern is the form the API gives an entry's operationID.
var journalOperationPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// proofAfter is the first Verified entry of the Apply's plan after the Apply's
// own entry, and false when there is none yet. The Apply's entry is found by
// its operation ID and its type.
func proofAfter(journal *ptahv1alpha1.LifecycleJournal, applied ptahv1alpha1.JournalEntry) (ptahv1alpha1.JournalEntry, bool) {
	if journal == nil || applied.OperationID == "" {
		return ptahv1alpha1.JournalEntry{}, false
	}
	at := slices.IndexFunc(journal.Entries, func(entry ptahv1alpha1.JournalEntry) bool {
		return entry.Type == ptahv1alpha1.JournalApplyFinished && entry.OperationID == applied.OperationID
	})
	if at < 0 {
		return ptahv1alpha1.JournalEntry{}, false
	}
	for _, entry := range journal.Entries[at+1:] {
		if entry.Type == ptahv1alpha1.JournalVerified && entry.Plan == applied.Plan {
			return entry, true
		}
	}
	return ptahv1alpha1.JournalEntry{}, false
}

// proofJournaledFor is nil when the proof after the Apply is journaled under the
// Apply's own operation ID, which is how a reader joins the two. outcome is the
// verdict the row expects, or empty where the row does not claim one.
func proofJournaledFor(journal *ptahv1alpha1.LifecycleJournal, applied ptahv1alpha1.JournalEntry, outcome ptahv1alpha1.JournalOutcome) error {
	if journal == nil {
		return fmt.Errorf("the schema keeps no journal")
	}
	proved, found := proofAfter(journal, applied)
	if !found {
		return fmt.Errorf("the journal of plan %s stops before the %s entry after operation %q: %s",
			applied.Plan, ptahv1alpha1.JournalVerified, applied.OperationID, journalOf(journal, applied.Plan))
	}
	if proved.OperationID != applied.OperationID {
		return fmt.Errorf("the %s entry after the Apply of plan %s names operation %q, the Apply %q",
			proved.Type, applied.Plan, proved.OperationID, applied.OperationID)
	}
	if outcome != "" && proved.Outcome != outcome {
		return fmt.Errorf("the journal records the proof of plan %s as %q, not %q", applied.Plan, proved.Outcome, outcome)
	}
	return nil
}

func applyFailureText(failure *ptahv1alpha1.SchemaApplyFailureStatus) string {
	if failure == nil {
		return "none"
	}
	return fmt.Sprintf("statement %d of %d, SQLSTATE %q, error %d", failure.Statement, failure.Statements, failure.SQLState, failure.ErrorNumber)
}
