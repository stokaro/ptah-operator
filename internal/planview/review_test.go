package planview_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/planview"
	"github.com/stokaro/ptah-operator/pkg/planread"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

// The native plan Ptah wrote for the exclusion row: one safe statement and
// one destructive one, each with Ptah's reason. The text view puts each
// statement under a line that says where it sits and how it was rated.
func TestRenderTextLabelsEachStatementWithItsSeverityAndReason(t *testing.T) {
	t.Parallel()

	view := publishNative(t, "postgresql-schema-exclusion-wide-plan.json")
	text := renderPlan(t, view, planview.Text)

	want := "\nStatement 1 of 2: safe (does not remove data or tighten constraints)\n" +
		"-- Add/modify columns for table: e2e_widgets\n" +
		"ALTER TABLE \"e2e_widgets\" ADD COLUMN \"fault_token\" text;\n" +
		"\n" +
		"Statement 2 of 2: destructive (DROP TABLE removes the table and all rows)\n" +
		"-- WARNING: This will delete all data!\n" +
		"DROP TABLE IF EXISTS \"e2e_excluded_policy_keep\" CASCADE;\n"
	if !strings.HasSuffix(text, want) {
		t.Fatalf("the text output does not end with the labeled statements:\n%s\nwant the end to be:\n%s", text, want)
	}
	if !strings.Contains(text, "Destructive:    true\n") {
		t.Fatalf("the text output lost the plan's destructive line:\n%s", text)
	}
}

// The review lines are the only thing the text adds. Take them out, and the
// separators between statements, and what is left is byte for byte what
// `-o sql` prints -- which carries no review line at all.
func TestRenderTextHoldsExactlyTheSQLTheSQLFormatPrints(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		view func(t *testing.T) planread.Plan
	}{
		{name: "native plan", view: func(t *testing.T) planread.Plan {
			return publishNative(t, "postgresql-schema-exclusion-wide-plan.json")
		}},
		{name: "statements carrying semicolons", view: func(t *testing.T) planread.Plan {
			return publishStatements(t, []string{
				"CREATE FUNCTION touch() RETURNS trigger AS $$ BEGIN NEW.seen := now(); RETURN NEW; END; $$ LANGUAGE plpgsql",
				"COMMENT ON TABLE customers IS 'one; two; three';",
				"-- a comment with a semicolon;\nALTER TABLE customers ADD COLUMN email TEXT",
			})
		}},
		{name: "statements spanning chunks", view: func(t *testing.T) planread.Plan {
			return publishStatements(t, planreadtest.SpanningStatements())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			view := test.view(t)
			text := renderPlan(t, view, planview.Text)
			sql := renderPlan(t, view, planview.SQL)

			if strings.Contains(sql, "Statement 1 of") {
				t.Fatalf("-o sql carries a review line:\n%s", sql)
			}
			header, body, found := strings.Cut(text, "\n\n")
			if !found || !strings.Contains(header, "Statements:") {
				t.Fatalf("the text output has no header before its statements:\n%s", text)
			}
			var expected, joined strings.Builder
			for index, statement := range view.Statements {
				one := planread.Plan{Statements: []planread.Statement{statement}}.SQL()
				if index > 0 {
					expected.WriteString("\n")
				}
				fmt.Fprintf(&expected, "Statement %d of %d: ", index+1, len(view.Statements))
				expected.WriteString(statement.Severity)
				if statement.Reason != "" {
					expected.WriteString(" (" + statement.Reason + ")")
				}
				expected.WriteString("\n" + one)
				joined.WriteString(one)
			}
			if body != expected.String() {
				t.Fatalf("the statements in the text output are\n%s\nand want\n%s", body, expected.String())
			}
			if joined.String() != sql {
				t.Fatalf("the SQL under the review lines is\n%s\nand -o sql prints\n%s", joined.String(), sql)
			}
		})
	}
}

// The operator raises a statement it reads as destructive that Ptah rated
// lower, and Ptah's reason still explains the lower rating. The review line
// says whose each one is; a severity that differs only in case was not
// raised and says nothing of the kind.
func TestRenderTextSaysWhoseSeverityARaisedStatementCarries(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		severity string
		reason   string
		want     string
	}{
		{
			name:     "raised by this plugin's reading",
			severity: "safe", reason: "does not remove data or tighten constraints",
			want: "Statement 1 of 1: destructive as this plugin reads it; Ptah rated it safe" +
				" (does not remove data or tighten constraints)\n",
		},
		{
			name:     "raised, with no reason",
			severity: "warning",
			want:     "Statement 1 of 1: destructive as this plugin reads it; Ptah rated it warning\n",
		},
		{
			name:     "rated destructive by Ptah",
			severity: "destructive", reason: "DROP NOT NULL widens the column",
			want: "Statement 1 of 1: destructive (DROP NOT NULL widens the column)\n",
		},
		{
			name:     "rated destructive in another case",
			severity: "Destructive", reason: "DROP NOT NULL widens the column",
			want: "Statement 1 of 1: destructive (DROP NOT NULL widens the column)\n",
		},
		{
			// A reason is Ptah's text, and a line break in it would read as
			// the start of the statement.
			name:     "reason with a line break and an escape sequence",
			severity: "destructive", reason: "drops\nthe column\x1b[2J",
			want: "Statement 1 of 1: destructive (drops the column\\x1b[2J)\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			statement := `ALTER TABLE "customers" ALTER COLUMN "email" DROP NOT NULL`
			document := documentOf(t, dataplane.PlanStatement{SQL: statement, Severity: test.severity, Reason: test.reason})
			lab := planreadtest.NewLab(t, "storefront")
			plan := lab.PublishDocument(t, document, "v1", func(spec *operatorv1alpha1.PtahSchemaPlanSpec) {
				spec.Destructive = true
			})
			lab.SetCurrent(t, plan)
			text := renderPlan(t, load(t, lab, planread.Current), planview.Text)
			if !strings.Contains(text, "\n"+test.want+statement+";\n") {
				t.Fatalf("the text output does not carry %q above the statement:\n%s", test.want, text)
			}
		})
	}
}

func renderPlan(t *testing.T, view planread.Plan, format planview.Format) string {
	t.Helper()
	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, format); err != nil {
		t.Fatalf("Render(%s) error = %v", format, err)
	}
	return rendered.String()
}

// publishNative stores a plan document Ptah wrote, as it wrote it.
func publishNative(t *testing.T, name string) planread.Plan {
	t.Helper()
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Destructive bool              `json:"destructive"`
		Statements  []json.RawMessage `json:"statements"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	lab := planreadtest.NewLab(t, "storefront")
	plan := lab.PublishDocument(t, document, "v1", func(spec *operatorv1alpha1.PtahSchemaPlanSpec) {
		spec.Destructive = decoded.Destructive
		spec.StatementCount = int32(len(decoded.Statements))
	})
	lab.SetCurrent(t, plan)
	return load(t, lab, planread.Current)
}

func publishStatements(t *testing.T, statements []string) planread.Plan {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, statements, "v1")
	lab.SetCurrent(t, plan)
	return load(t, lab, planread.Current)
}

// documentOf writes a plan document with the statements given, severity and
// reason included, the way an executor writes one.
func documentOf(t *testing.T, statements ...dataplane.PlanStatement) []byte {
	t.Helper()
	document, err := json.Marshal(dataplane.PlanFile{
		FormatVersion:   dataplane.PlanFormatVersion,
		Name:            "plan",
		Dialect:         "postgres",
		FromFingerprint: "sha256:from",
		ToFingerprint:   "sha256:to",
		Statements:      statements,
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}
