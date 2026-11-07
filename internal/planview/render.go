// Package planview prints a plan for `kubectl ptah plan`.
//
// Reading the plan is not this package's job: pkg/planread resolves it,
// reconstructs it and verifies it, and only a plan that held every check
// reaches Render.
//
// A plan a newer Ptah wrote can carry fields this plugin does not read. Render
// never sees one, since planread.Load refuses it. RenderDisplay prints what
// planread.LoadForDisplay read of it, as text only, with the fields named and no
// value of theirs. The other formats are for a program, which would take the
// statements for the whole plan.
package planview

import (
	"fmt"
	"io"
	"strings"

	"github.com/stokaro/ptah-operator/internal/statusview"
	"github.com/stokaro/ptah-operator/pkg/planread"
)

// Format is how a reader asked to see a plan.
type Format string

const (
	// Text is the default: what the plan is, then each statement it holds
	// under a line that gives its severity and the reason Ptah gave.
	Text Format = "text"
	// SQL is the statements alone, for piping somewhere else. A plan this plugin
	// does not read whole has no such format, because a pipe would run it.
	SQL Format = "sql"
	// JSON is the stored document, byte for byte. A plan this plugin does not
	// read whole has no such format, because those bytes hold what it did not
	// read.
	JSON Format = "json"
)

// Formats are the accepted values, in the order the help lists them.
var Formats = []Format{Text, SQL, JSON}

// Render writes one plan read whole in the requested format.
//
// Every format is written from the document the store verified. The JSON one
// is that document unchanged rather than a re-serialization of the decoded
// plan, so what a reader saves still matches the content digest above it.
func Render(out io.Writer, plan planread.Plan, format Format) error {
	switch format {
	case Text:
		return RenderDisplay(out, plan.Display())
	case SQL:
		_, err := io.WriteString(out, plan.SQL())
		return err
	case JSON:
		document := plan.Document
		if len(document) == 0 || document[len(document)-1] != '\n' {
			document = append(append([]byte(nil), document...), '\n')
		}
		_, err := out.Write(document)
		return err
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

// privilegesLine names the kinds the plan records, which are what held it for
// an approval, and then any kind this plugin reads in the SQL that the record
// lacks, marked as such: a reader told the plan waited for a kind nobody
// recorded would be told something that did not happen.
func privilegesLine(plan planread.DisplayPlan) string {
	var parts []string
	if len(plan.PrivilegeChanges) > 0 {
		parts = append(parts, strings.Join(plan.PrivilegeChanges, ", "))
	}
	if len(plan.UnrecordedPrivilegeChanges) > 0 {
		parts = append(parts, strings.Join(plan.UnrecordedPrivilegeChanges, ", ")+
			" (read by this plugin, not recorded)")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// RenderDisplay writes a plan as text, saying what it is before what it does.
//
// The chunk names are deliberately absent. They are how the plan is stored, not
// what it says, and a reader who needs them is diagnosing the store rather than
// reading a plan.
//
// The statements come out as `-o sql` prints them, one at a time, each under a
// line that is not SQL: its place in the plan, its severity, and the reason
// Ptah gave. Those lines are for review, which is why `-o sql`,
// the format a script reads or a reviewer diffs, carries none of them.
func RenderDisplay(out io.Writer, plan planread.DisplayPlan) error {
	var text strings.Builder
	field := func(name, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&text, "%-16s%s\n", name+":", value)
	}
	field("Schema", plan.Namespace+"/"+plan.Schema)
	field("Plan", string(plan.Selection)+" ("+plan.PlanName+")")
	field("Fingerprint", plan.Fingerprint)
	field("Content digest", plan.ContentDigest)
	field("Dialect", plan.Dialect)
	field("Statements", fmt.Sprintf("%d", len(plan.Statements)))
	field("Destructive", fmt.Sprintf("%t", plan.Destructive))
	field("Privileges", privilegesLine(plan))
	if len(plan.UnreadFields) > 0 {
		field("Unread fields", strings.Join(plan.UnreadFields, ", ")+
			" (a newer Ptah wrote them and this plugin does not read them; "+
			"read this plan with a plugin at the operator's version before you approve it)")
	}
	if !plan.CreatedAt.IsZero() {
		field("Stored", plan.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if plan.CompletedAt != nil && !plan.CompletedAt.IsZero() {
		field("Applied", plan.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	text.WriteString("\n")
	writeStatements(&text, plan)
	_, err := io.WriteString(out, text.String())
	return err
}

// writeStatements writes each statement under its review line. A statement is
// rendered by the same code as `-o sql`, so the SQL under the lines is the SQL
// that format prints, and a statement it skips is skipped here too.
func writeStatements(text *strings.Builder, plan planread.DisplayPlan) {
	written := 0
	for index, statement := range plan.Statements {
		sql := planread.Plan{Statements: []planread.Statement{statement}}.SQL()
		if sql == "" {
			continue
		}
		if written > 0 {
			text.WriteString("\n")
		}
		written++
		fmt.Fprintf(text, "Statement %d of %d: %s\n", index+1, len(plan.Statements), classification(statement))
		text.WriteString(sql)
	}
}

// classification is a statement's severity and the reason Ptah gave for it.
//
// The severity is the one this plugin decoded, by the rules the operator
// decodes a plan with: where those rules read a statement as destructive that
// Ptah rated lower -- a DROP of a default or of a NOT NULL constraint, say --
// the severity is raised, and Ptah's reason still explains the lower rating.
// Printed side by side without a word, "destructive" and "does not remove
// data" would contradict each other, so a raised severity says whose each one
// is. It says "this plugin" rather than "the operator" for the reason the
// privileges line does: the plugin and the manager are separate builds.
func classification(statement planread.Statement) string {
	severity := statusview.Clean(strings.TrimSpace(statement.Severity))
	reason := statusview.Clean(strings.TrimSpace(statement.Reason))
	stored := statusview.Clean(strings.TrimSpace(statement.StoredSeverity))
	if stored != "" && !strings.EqualFold(stored, severity) {
		raised := severity + " as this plugin reads it; Ptah rated it " + stored
		if reason != "" {
			raised += " (" + reason + ")"
		}
		return raised
	}
	if reason != "" {
		return severity + " (" + reason + ")"
	}
	return severity
}
