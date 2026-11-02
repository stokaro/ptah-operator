package schemaview

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Format is how a reader asked to see a schema.
type Format string

const (
	// Text is the default: where the schema stands, what the last observation
	// found, and which plan is waiting.
	Text Format = "text"
	// JSON is the same view as a document, for a script.
	JSON Format = "json"
)

// Formats are the accepted values, in the order the help lists them.
var Formats = []Format{Text, JSON}

// Render writes one view in the requested format.
func Render(out io.Writer, view View, format Format) error {
	switch format {
	case Text:
		return renderText(out, view)
	case JSON:
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(view)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

// renderText says where the schema stands and what holds it, then what the
// observation found, then what is waiting for a decision and who decided,
// and last every condition the status carries.
//
// No SQL appears here, and no row of any table. The reference-data line is
// counts: how many declared rows the database is missing, how many it holds
// with a different value, and how many it holds that the declaration no longer
// has. Which rows those are is in the plan, and reading a data plan is reading
// values.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := statusview.FieldWriter(&text, "", statusview.LabelWidth)
	field("Schema", view.Namespace+"/"+view.Schema)
	field("Phase", view.Phase)
	if view.ObservedGeneration != 0 && view.ObservedGeneration != view.Generation {
		field("Status for", fmt.Sprintf("generation %d; the spec is at generation %d",
			view.ObservedGeneration, view.Generation))
	}
	for _, condition := range statusview.Leading(view.Conditions) {
		field(statusview.Clean(condition.Type), statusview.Summary(condition))
	}
	if view.Suspended {
		field("Suspend", "requested")
	}
	if policy := view.Policy; policy != nil {
		field("Policy", policyLine(*policy))
		field("Protected tables", strings.Join(policy.ProtectedTables, ", "))
	}
	for _, line := range view.Source.Fields() {
		field(line.Name, line.Value)
	}
	renderObservation(field, &text, view.Observation)
	if applied := view.Applied; applied != nil {
		name := applied.PlanName
		if name == "" {
			name = applied.PlanFingerprint
		}
		field("Applied", name+" at "+statusview.Time(applied.CompletedAt))
	}
	renderPlan(&text, view.Plan, statusview.Find(view.Conditions, "ApprovalRequired"))
	if statusview.WriteListHeading(&text, "Approvals", "ptahschemaapprovals", view.Namespace,
		view.Approvals, view.ApprovalsUnreadable) {
		for _, approval := range view.Approvals.Items {
			statusview.WriteApproval(&text, approval, "no condition written yet")
		}
	}
	if len(view.Conditions) > 0 {
		text.WriteString("\nConditions:\n")
		statusview.WriteTable(&text, "  ", view.Conditions, view.Generation)
	}
	text.WriteString("\n")
	if view.Plan != nil {
		fmt.Fprintf(&text, "kubectl ptah plan %s -n %s prints the SQL this plan holds.\n", view.Schema, view.Namespace)
	}
	fmt.Fprintf(&text, "kubectl events -n %s --for ptahschema/%s prints its Events.\n", view.Namespace, view.Schema)
	_, err := io.WriteString(out, text.String())
	return err
}

// policyLine names the policy fields by their spec names, so a reader who
// wants another value knows which field to edit.
func policyLine(policy PolicyView) string {
	var parts []string
	if policy.Apply != "" {
		parts = append(parts, "apply "+policy.Apply)
	}
	parts = append(parts, fmt.Sprintf("allowDestructive %t", policy.AllowDestructive))
	if policy.DriftSeverity != "" {
		parts = append(parts, "driftSeverity "+policy.DriftSeverity)
	}
	return strings.Join(parts, ", ")
}

// renderObservation writes what the last drift read found, and says plainly
// when there has not been one. A schema with no observation is not a converged
// schema, and printing nothing would read like one.
func renderObservation(field func(name, value string), text *strings.Builder, observation *ObservationView) {
	if observation == nil {
		field("Observed", "not yet")
		return
	}
	when := "unknown"
	if observation.ObservedAt != nil {
		when = statusview.Time(*observation.ObservedAt)
	}
	field("Observed", when)
	if !observation.Drift {
		field("Drift", "none")
		return
	}
	if observation.FindingCount == 0 && len(observation.Findings) == 0 {
		// A grant, a default privilege, a view or a trigger: the report found
		// the difference and has no category to count it in. A severity rated
		// over no category says nothing, so none is printed.
		field("Drift", "found, in no category the report counts; the plan says what changes")
		return
	}
	field("Drift", fmt.Sprintf("%d in %d categories, highest %s",
		observation.FindingCount, len(observation.Findings), observation.HighestSeverity))
	if rows := observation.ReferenceData; rows != nil {
		field("Reference data", fmt.Sprintf("%d to insert, %d to update, %d to delete",
			rows.Inserts, rows.Updates, rows.Deletes))
	}
	for _, finding := range observation.Findings {
		fmt.Fprintf(text, "  %-34s %6d  %s\n", finding.Category, finding.Count, finding.Severity)
	}
}

// renderPlan names the plan and what it would do, and gives the controller's
// verdict on it: ApprovalRequired says, in either status, whether the plan
// waits for an approval, runs without one, or is refused by the policy, and
// its message is the one that says what to do. The SQL is what `kubectl ptah
// plan` prints, and saying so here is cheaper than a reader discovering that
// this command declines to.
func renderPlan(text *strings.Builder, plan *PlanView, approvalRequired *ConditionView) {
	if plan == nil {
		text.WriteString("\nNo plan is published.\n")
		return
	}
	fmt.Fprintf(text, "\nPlan %s, %s, stored %s\n",
		plan.Name, countedStatements(plan.StatementCount), statusview.Time(plan.CreatedAt))
	if plan.Destructive {
		text.WriteString("  destructive\n")
	}
	if len(plan.PrivilegeChanges) > 0 {
		fmt.Fprintf(text, "  changes privileges: %s\n", strings.Join(plan.PrivilegeChanges, ", "))
	}
	switch {
	case plan.Approved:
		approved := plan.ApprovalName
		if plan.ApprovedAt != nil {
			approved += " at " + statusview.Time(*plan.ApprovedAt)
		}
		if plan.ApprovalApprover != "" {
			approved += " by " + statusview.Clean(plan.ApprovalApprover)
		}
		fmt.Fprintf(text, "  approved by %s\n", approved)
	default:
		text.WriteString("  not approved\n")
	}
	if approvalRequired != nil {
		fmt.Fprintf(text, "  ApprovalRequired: %s\n", statusview.Summary(*approvalRequired))
	}
}

func countedStatements(count int32) string {
	if count == 1 {
		return "1 statement"
	}
	return fmt.Sprintf("%d statements", count)
}
