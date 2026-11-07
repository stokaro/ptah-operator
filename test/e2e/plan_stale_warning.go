package e2e

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// planStaleWarnings returns the PlanStale Warnings recorded against this exact
// schema. A schema of the same name from an earlier row, a Normal Event and
// another reason do not count.
func planStaleWarnings(events []corev1.Event, schema *ptahv1alpha1.PtahSchema) []corev1.Event {
	var warnings []corev1.Event
	for _, event := range events {
		reference := event.InvolvedObject
		if event.Type == corev1.EventTypeWarning && event.Reason == "PlanStale" && reference.Kind == "PtahSchema" &&
			reference.Name == schema.Name && reference.UID == schema.UID {
			warnings = append(warnings, event)
		}
	}
	return warnings
}

// planWarnedReplaced holds the Warnings of a schema to the one replacement that
// happened: a single Event, with no repeat count, that names both plans.
func planWarnedReplaced(warnings []corev1.Event, replaced, replacement string) error {
	if len(warnings) != 1 {
		return fmt.Errorf("%d PlanStale Warnings, want exactly one", len(warnings))
	}
	warning := warnings[0]
	if warning.Count > 1 {
		return fmt.Errorf("the PlanStale Warning was recorded %d times, want once", warning.Count)
	}
	if !strings.Contains(warning.Message, replaced) || !strings.Contains(warning.Message, replacement) {
		return fmt.Errorf("the PlanStale Warning %q does not name plans %s and %s", warning.Message, replaced, replacement)
	}
	return nil
}

// planReplacedOnRecord holds a journal to the replacement it has to keep when the
// Warning is lost: the newest PlanPublished entry names the new plan and the plan
// it replaced, and says the new plan asks for an approval.
func planReplacedOnRecord(journal *ptahv1alpha1.LifecycleJournal, replaced, replacement string) error {
	if journal == nil {
		return fmt.Errorf("the schema keeps no journal")
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := journal.Entries[index]
		if entry.Type != ptahv1alpha1.JournalPlanPublished {
			continue
		}
		if entry.Plan != replacement || entry.Replaced != replaced || !entry.ApprovalRequired {
			return fmt.Errorf("the newest PlanPublished entry names plan %q replacing %q, approvalRequired %t; want plan %s replacing %s, approvalRequired true",
				entry.Plan, entry.Replaced, entry.ApprovalRequired, replacement, replaced)
		}
		return nil
	}
	return fmt.Errorf("the journal holds no PlanPublished entry among %d", len(journal.Entries))
}

// readKeptPlan is a schema that read its database after since and is waiting on
// the same plan again: the same object, under the same fingerprint.
func readKeptPlan(schema *ptahv1alpha1.PtahSchema, plan *ptahv1alpha1.CurrentPlanStatus, since metav1.Time) bool {
	current := schema.Status.Plan
	observed := schema.Status.Target.LastObservedAt
	return planAwaitingApproval(schema) && current.Name == plan.Name && current.UID == plan.UID &&
		current.Fingerprint == plan.Fingerprint && observed != nil && observed.After(since.Time)
}

// planReplacedAwaitingApproval is a schema waiting on another plan than the one
// named.
func planReplacedAwaitingApproval(schema *ptahv1alpha1.PtahSchema, replaced string) bool {
	return planAwaitingApproval(schema) && schema.Status.Plan.Name != replaced
}
