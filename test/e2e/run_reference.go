package e2e

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// finishedRunOf is the one entry of the type given, a schema's ApplyFinished
// or a migration's RunFinished, that ran under the approval with the UID
// given, or under none when the UID is empty, and it succeeded. Its operation
// ID is the claim's, which the operator hands Ptah as the run reference, so it
// is what joins the run to the rows the database's own log keeps of it.
//
// A RunFinished entry also says what authorized the run, and that has to be
// the approval when there is one and the policy when there is none.
func finishedRunOf(journal *ptahv1alpha1.LifecycleJournal, entryType ptahv1alpha1.JournalEntryType,
	approvalUID types.UID) (ptahv1alpha1.JournalEntry, error) {
	if journal == nil {
		return ptahv1alpha1.JournalEntry{}, errors.New("the resource keeps no journal")
	}
	under := "no approval"
	if approvalUID != "" {
		under = "approval UID " + string(approvalUID)
	}
	var found []ptahv1alpha1.JournalEntry
	var described []string
	for _, entry := range journal.Entries {
		if entry.Type != entryType {
			continue
		}
		described = append(described, fmt.Sprintf("approval UID %q %s at %s", entry.ApprovalUID, entry.Outcome, entry.At.UTC().Format("15:04:05")))
		if entry.ApprovalUID == approvalUID {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		return ptahv1alpha1.JournalEntry{}, fmt.Errorf("the journal holds %d %s entries under %s, want one: of %d entries, [%s]",
			len(found), entryType, under, len(journal.Entries), strings.Join(described, "; "))
	}
	entry := found[0]
	authorization := ptahv1alpha1.MigrationRunAuthorizedByPolicy
	if approvalUID != "" {
		authorization = ptahv1alpha1.MigrationRunAuthorizedByApproval
	}
	switch {
	case entry.Outcome != ptahv1alpha1.JournalSucceeded:
		return entry, fmt.Errorf("the %s entry under %s records outcome %q, want %q", entryType, under, entry.Outcome, ptahv1alpha1.JournalSucceeded)
	case entryType == ptahv1alpha1.JournalRunFinished && entry.Authorization != authorization:
		return entry, fmt.Errorf("the %s entry under %s records authorization %q, want %q", entryType, under, entry.Authorization, authorization)
	case !journalOperationPattern.MatchString(entry.OperationID):
		return entry, fmt.Errorf("the %s entry under %s names operation %q, not a claim's ID", entryType, under, entry.OperationID)
	}
	return entry, nil
}
