package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// externalView is the view the security_invoker row adds to the external
// schema.
const externalView = "e2e_widget_names"

// viewInvokerChange is what one step of the security_invoker row plans: how the
// one statement opens, whether it may name security_invoker at all, and the
// effect Ptah records for the view, with the option's value before the
// statement and after it. A nil value is an absent one: the view does not exist
// at that point.
type viewInvokerChange struct {
	opens          string
	unstated       bool
	action         string
	before, after  *bool
	describeChange string
}

var (
	// viewCreatedWithOwnerRights is the view created with no option, which
	// reads with its owner's rights. Ptah records every property of a new view
	// as changed, from absent to its value.
	viewCreatedWithOwnerRights = viewInvokerChange{
		opens: `CREATE VIEW "` + externalView + `" AS`, unstated: true, action: "create", after: boolValue(false),
		describeChange: "a view created with its owner's rights",
	}
	// viewInvokerTurnedOn is the existing view switched to its readers' rights.
	viewInvokerTurnedOn = viewInvokerChange{
		opens:  `ALTER VIEW "` + externalView + `" SET (security_invoker=true)`,
		action: "alter", before: boolValue(false), after: boolValue(true),
		describeChange: "security_invoker turned on",
	}
	// viewBodyKeptInvoker is a new body for the view that keeps its readers'
	// rights. CREATE OR REPLACE VIEW has to restate every option the view
	// keeps, so its SQL states security_invoker=true; only the effect says the
	// value did not move.
	viewBodyKeptInvoker = viewInvokerChange{
		opens:  `CREATE OR REPLACE VIEW "` + externalView + `" WITH (security_invoker=true) AS`,
		action: "replace", before: boolValue(true), after: boolValue(true),
		describeChange: "a body change that keeps security_invoker on",
	}
)

func boolValue(value bool) *bool { return &value }

// viewEffectPlan is the part of a native plan document the row reads: each
// statement's SQL and the effects Ptah recorded for it. It is decoded here and
// not by the operator's own reader, so the row does not share that reader's
// assumptions about the record.
type viewEffectPlan struct {
	Statements []struct {
		SQL     string `json:"sql"`
		Effects []struct {
			Version int `json:"version"`
			Object  struct {
				Kind   string `json:"kind"`
				Schema string `json:"schema"`
				Name   string `json:"name"`
			} `json:"object"`
			Action     string `json:"action"`
			Properties []struct {
				Name    string `json:"name"`
				Before  *bool  `json:"before"`
				After   *bool  `json:"after"`
				Changed bool   `json:"changed"`
			} `json:"properties"`
		} `json:"effects"`
	} `json:"statements"`
}

// viewInvokerPlanned is nil when the plan document is the one statement the
// step is for: it opens as want says, and names security_invoker nowhere when
// want says so, and its one effect, at version 1, is the row's view in public with
// want's action and one security_invoker whose values are want's, changed
// exactly when they differ. A plan that reached the same privilege verdict by
// planning something else is refused, which is what lets the row's verdict
// stand for the effect it was read from.
func viewInvokerPlanned(document []byte, want viewInvokerChange) error {
	var plan viewEffectPlan
	if err := json.Unmarshal(document, &plan); err != nil {
		return fmt.Errorf("the plan document does not parse: %w", err)
	}
	if len(plan.Statements) != 1 {
		return fmt.Errorf("the plan holds %d statements, want the one of %s", len(plan.Statements), want.describeChange)
	}
	statement := plan.Statements[0]
	opening, _, _ := strings.Cut(statement.SQL, "\n")
	switch {
	case !strings.HasPrefix(statement.SQL, want.opens):
		return fmt.Errorf("the statement opens %q, want %q for %s", opening, want.opens, want.describeChange)
	case want.unstated && strings.Contains(statement.SQL, "security_invoker"):
		return fmt.Errorf("the statement %q names security_invoker, which %s does not", opening, want.describeChange)
	case len(statement.Effects) != 1:
		return fmt.Errorf("the statement records %d effects, want the view's one", len(statement.Effects))
	}
	effect := statement.Effects[0]
	if effect.Version != 1 || effect.Object.Kind != "view" || effect.Object.Schema != "public" ||
		effect.Object.Name != externalView || effect.Action != want.action {
		return fmt.Errorf("the effect is version %d, %s %s.%s, action %q; want version 1, view public.%s, action %q",
			effect.Version, effect.Object.Kind, effect.Object.Schema, effect.Object.Name, effect.Action, externalView, want.action)
	}
	found := 0
	for _, property := range effect.Properties {
		if property.Name != "security_invoker" {
			continue
		}
		found++
		same := sameOptionalBool(want.before, want.after)
		if !sameOptionalBool(property.Before, want.before) || !sameOptionalBool(property.After, want.after) || property.Changed == same {
			return fmt.Errorf("the effect records security_invoker %s to %s, changed %t; want %s to %s, changed %t",
				optionalBoolText(property.Before), optionalBoolText(property.After), property.Changed,
				optionalBoolText(want.before), optionalBoolText(want.after), !same)
		}
	}
	if found != 1 {
		return fmt.Errorf("the effect records security_invoker %d times, want once", found)
	}
	return nil
}

func sameOptionalBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func optionalBoolText(value *bool) string {
	if value == nil {
		return "absent"
	}
	return fmt.Sprint(*value)
}

// unattendedApplyJournaled is nil when the journal holds the life of a plan
// applied with nobody's approval: published with no approval required, no
// approval of it accepted, one Apply of it that consumed no approval and
// succeeded, after its publication, and the proof joined to that Apply
// finding the database in sync.
func unattendedApplyJournaled(journal *ptahv1alpha1.LifecycleJournal, plan string) error {
	if journal == nil {
		return errors.New("the schema keeps no journal")
	}
	published := -1
	var applies []int
	for index, entry := range journal.Entries {
		if entry.Plan != plan {
			continue
		}
		switch entry.Type {
		case ptahv1alpha1.JournalPlanPublished:
			if entry.ApprovalRequired {
				return fmt.Errorf("the journal publishes plan %s as requiring an approval: %s", plan, journalOf(journal, plan))
			}
			if published < 0 {
				published = index
			}
		case ptahv1alpha1.JournalApprovalAccepted:
			return fmt.Errorf("the journal accepts approval %q of plan %s: %s", entry.Approval, plan, journalOf(journal, plan))
		case ptahv1alpha1.JournalApplyFinished:
			applies = append(applies, index)
		}
	}
	if published < 0 {
		return fmt.Errorf("the journal never publishes plan %s: %s", plan, journalOf(journal, plan))
	}
	if len(applies) != 1 {
		return fmt.Errorf("the journal holds %d %s entries of plan %s, want one: %s",
			len(applies), ptahv1alpha1.JournalApplyFinished, plan, journalOf(journal, plan))
	}
	applied := journal.Entries[applies[0]]
	switch {
	case applies[0] < published:
		return fmt.Errorf("the journal applies plan %s before it publishes it: %s", plan, journalOf(journal, plan))
	case applied.Outcome != ptahv1alpha1.JournalSucceeded:
		return fmt.Errorf("the journal records the Apply of plan %s as %q, not %q", plan, applied.Outcome, ptahv1alpha1.JournalSucceeded)
	case applied.Approval != "" || applied.ApprovalUID != "" || applied.Approver != "":
		return fmt.Errorf("the journal records the Apply of plan %s under approval %q", plan, applied.Approval)
	}
	return proofJournaledFor(journal, applied, ptahv1alpha1.JournalInSync)
}

// approvalsOfPlan counts the approvals that name the plan.
func approvalsOfPlan(approvals []ptahv1alpha1.PtahSchemaApproval, plan types.UID) int {
	count := 0
	for _, approval := range approvals {
		if approval.Spec.PlanRef.UID == plan {
			count++
		}
	}
	return count
}
