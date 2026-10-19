// Package planview prints a plan for `kubectl ptah plan`.
//
// Reading the plan is not this package's job: pkg/planread resolves it,
// reconstructs it and verifies it, and only a plan that held every check
// reaches Render.
package planview

import (
	"fmt"
	"io"
	"strings"

	"github.com/stokaro/ptah-operator/pkg/planread"
)

// Format is how a reader asked to see a plan.
type Format string

const (
	// Text is the default: what the plan is, then the SQL it holds.
	Text Format = "text"
	// SQL is the statements alone, for piping somewhere else.
	SQL Format = "sql"
	// JSON is the stored document, byte for byte.
	JSON Format = "json"
)

// Formats are the accepted values, in the order the help lists them.
var Formats = []Format{Text, SQL, JSON}

// Render writes one plan in the requested format.
//
// Every format is written from the document the store verified. The JSON one
// is that document unchanged rather than a re-serialization of the decoded
// plan, so what a reader saves still matches the content digest above it.
func Render(out io.Writer, plan planread.Plan, format Format) error {
	switch format {
	case Text:
		return renderText(out, plan)
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
func privilegesLine(plan planread.Plan) string {
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

// renderText says what the plan is before it says what it does.
//
// The chunk names are deliberately absent. They are how the plan is stored, not
// what it says, and a reader who needs them is diagnosing the store rather than
// reading a plan.
func renderText(out io.Writer, plan planread.Plan) error {
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
	if !plan.CreatedAt.IsZero() {
		field("Stored", plan.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if plan.CompletedAt != nil && !plan.CompletedAt.IsZero() {
		field("Applied", plan.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	text.WriteString("\n")
	text.WriteString(plan.SQL())
	_, err := io.WriteString(out, text.String())
	return err
}
