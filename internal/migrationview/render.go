package migrationview

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Format is how a reader asked to see a migration.
type Format string

const (
	// Text is the default: the state, the history, the sequence, the last run.
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

// renderText says where the migration stands before it says what is pending.
//
// No SQL appears here, and no row of any table: a migration plan records
// versions and checksums, and the statements live in the artifact.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := func(name, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&text, "%-16s%s\n", name+":", value)
	}
	field("Migration", view.Namespace+"/"+view.Migration)
	field("Phase", view.Phase)
	if view.Suspended {
		field("Suspended", "true")
	}
	if view.Artifact != nil {
		field("Artifact", view.Artifact.Reference)
	}
	if history := view.History; history != nil {
		field("History read", history.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"))
		field("Current version", fmt.Sprintf("%d", history.CurrentVersion))
		if history.CheckpointVersion > 0 {
			field("Checkpoint", fmt.Sprintf("%d", history.CheckpointVersion))
		}
		field("Applied", fmt.Sprintf("%d", history.AppliedCount))
		field("Pending", fmt.Sprintf("%d", history.PendingCount))
		if history.Dirty {
			field("Dirty", "true")
		}
		if len(history.ModifiedVersions) > 0 {
			field("Modified", joinVersions(history.ModifiedVersions))
		}
	}
	if run := view.LastRun; run != nil {
		finished := ""
		if run.FinishedAt != nil {
			finished = " at " + run.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		field("Last run", run.Outcome+finished)
		if len(run.AppliedVersions) > 0 {
			field("Run applied", joinVersions(run.AppliedVersions))
		}
		field("Run message", run.Message)
	}
	for _, condition := range view.Conditions {
		if condition.Status == "True" || condition.Type == "Ready" {
			field(condition.Type, condition.Status+" ("+condition.Reason+")")
		}
	}
	if view.Plan == nil {
		text.WriteString("\nNo plan is published.\n")
		_, err := io.WriteString(out, text.String())
		return err
	}
	fmt.Fprintf(&text, "\nPlan %s, %s from version %d:\n",
		view.Plan.Name, countedMigrations(len(view.Plan.Migrations)), view.Plan.CurrentVersion)
	for _, planned := range view.Plan.Migrations {
		description := planned.Description
		if description == "" {
			description = "(no description)"
		}
		marks := ""
		if planned.Checkpoint {
			marks += " [checkpoint]"
		}
		if planned.TransactionMode != "" {
			marks += " [tx " + planned.TransactionMode + "]"
		}
		fmt.Fprintf(&text, "  %-20d %s%s\n", planned.Version, description, marks)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func countedMigrations(count int) string {
	if count == 1 {
		return "1 migration"
	}
	return fmt.Sprintf("%d migrations", count)
}

func joinVersions(versions []int64) string {
	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, fmt.Sprintf("%d", version))
	}
	return strings.Join(parts, ", ")
}
