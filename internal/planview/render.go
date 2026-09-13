package planview

import (
	"fmt"
	"io"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
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

// Render writes one view in the requested format.
//
// Every format is written from the document the store verified. The JSON one
// is that document unchanged rather than a re-serialization of the decoded
// plan, so what a reader saves still matches the content digest above it.
func Render(out io.Writer, view View, format Format) error {
	switch format {
	case Text:
		return renderText(out, view)
	case SQL:
		_, err := io.WriteString(out, dataplane.StatementsSQL(view.Plan))
		return err
	case JSON:
		document := view.Document
		if len(document) == 0 || document[len(document)-1] != '\n' {
			document = append(append([]byte(nil), document...), '\n')
		}
		_, err := out.Write(document)
		return err
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

// renderText says what the plan is before it says what it does.
//
// The chunk names are deliberately absent. They are how the plan is stored, not
// what it says, and a reader who needs them is diagnosing the store rather than
// reading a plan.
func renderText(out io.Writer, view View) error {
	var text strings.Builder
	field := func(name, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&text, "%-16s%s\n", name+":", value)
	}
	field("Schema", view.Namespace+"/"+view.Schema)
	field("Plan", string(view.Selection)+" ("+view.PlanName+")")
	field("Fingerprint", view.Fingerprint)
	field("Content digest", view.ContentDigest)
	field("Dialect", view.Dialect)
	field("Statements", fmt.Sprintf("%d", view.StatementCount))
	field("Destructive", fmt.Sprintf("%t", view.Destructive))
	if !view.CreatedAt.IsZero() {
		field("Stored", view.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if view.CompletedAt != nil && !view.CompletedAt.IsZero() {
		field("Applied", view.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	text.WriteString("\n")
	text.WriteString(dataplane.StatementsSQL(view.Plan))
	_, err := io.WriteString(out, text.String())
	return err
}
