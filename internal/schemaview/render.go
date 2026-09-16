package schemaview

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
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

// renderText says where the schema stands, then what the observation found,
// then what is waiting for a decision.
//
// No SQL appears here, and no row of any table. The reference-data line is
// counts: how many declared rows the database is missing, how many it holds
// with a different value, and how many it holds that the declaration no longer
// has. Which rows those are is in the plan, and reading a data plan is reading
// values.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := func(name, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&text, "%-18s%s\n", name+":", value)
	}
	field("Schema", view.Namespace+"/"+view.Schema)
	field("Phase", view.Phase)
	if view.Suspended {
		field("Suspended", "true")
	}
	if source := view.Source; source != nil {
		field("Artifact", source.Reference)
		if source.Verified {
			field("Verified", "true")
		}
	}
	renderObservation(&text, view.Observation)
	for _, condition := range view.Conditions {
		if condition.Status == "True" || condition.Type == "Ready" {
			field(condition.Type, condition.Status+" ("+condition.Reason+")")
		}
	}
	if applied := view.Applied; applied != nil {
		name := applied.PlanName
		if name == "" {
			name = applied.PlanFingerprint
		}
		field("Applied", name+" at "+applied.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	renderPlan(&text, view.Plan)
	_, err := io.WriteString(out, text.String())
	return err
}

// renderObservation writes what the last drift read found, and says plainly
// when there has not been one. A schema with no observation is not a converged
// schema, and printing nothing would read like one.
func renderObservation(text *strings.Builder, observation *ObservationView) {
	if observation == nil {
		fmt.Fprintf(text, "%-18s%s\n", "Observed:", "not yet")
		return
	}
	when := "unknown"
	if observation.ObservedAt != nil {
		when = observation.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	fmt.Fprintf(text, "%-18s%s\n", "Observed:", when)
	if !observation.Drift {
		fmt.Fprintf(text, "%-18s%s\n", "Drift:", "none")
		return
	}
	fmt.Fprintf(text, "%-18s%s\n", "Drift:",
		fmt.Sprintf("%d in %d categories, highest %s",
			observation.FindingCount, len(observation.Findings), observation.HighestSeverity))
	if rows := observation.ReferenceData; rows != nil {
		fmt.Fprintf(text, "%-18s%s\n", "Reference data:",
			fmt.Sprintf("%d to insert, %d to update, %d to delete",
				rows.Inserts, rows.Updates, rows.Deletes))
	}
	if observation.Truncated {
		fmt.Fprintf(text, "%-18s%s\n", "Findings:", "truncated; the count above covers the whole report")
	}
	for _, finding := range observation.Findings {
		fmt.Fprintf(text, "  %-34s %6d  %s\n", finding.Category, finding.Count, finding.Severity)
	}
}

// renderPlan names the plan and what it would do. The SQL is what
// `kubectl ptah plan` prints, and saying so here is cheaper than a reader
// discovering that this command declines to.
func renderPlan(text *strings.Builder, plan *PlanView) {
	if plan == nil {
		text.WriteString("\nNo plan is published.\n")
		return
	}
	fmt.Fprintf(text, "\nPlan %s, %s, stored %s\n",
		plan.Name, countedStatements(plan.StatementCount),
		plan.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if plan.Destructive {
		text.WriteString("  destructive\n")
	}
	switch {
	case plan.Approved:
		approved := plan.ApprovalName
		if plan.ApprovedAt != nil {
			approved += " at " + plan.ApprovedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if plan.ApprovalApprover != "" {
			approved += " by " + plan.ApprovalApprover
		}
		fmt.Fprintf(text, "  approved by %s\n", approved)
	default:
		text.WriteString("  not approved\n")
	}
	text.WriteString("\nkubectl ptah plan prints the SQL this plan holds.\n")
}

func countedStatements(count int32) string {
	if count == 1 {
		return "1 statement"
	}
	return fmt.Sprintf("%d statements", count)
}
