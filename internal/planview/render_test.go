package planview_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planview"
	"github.com/stokaro/ptah-operator/pkg/planread"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

// loadForDisplay reads a plan a newer Ptah could have written, the way the plan
// command does.
func loadForDisplay(t *testing.T, statements []string) planread.Plan {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	lab.SetCurrent(t, lab.PublishDocument(t, planreadtest.DocumentWithUnreadFields(t, statements), "v1"))
	plan, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	return plan
}

// The reader is told which fields went unread, by name, and is told that this
// plugin does not approve the plan. The statements read as they always do.
func TestRenderTextNamesUnreadFieldsAndPrintsNoValue(t *testing.T) {
	t.Parallel()
	view := loadForDisplay(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"})

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.Text); err != nil {
		t.Fatalf("Render(text) error = %v", err)
	}

	for _, want := range []string{
		"Unread fields:  annotations, statements[].inputs (",
		"does not approve this plan",
		"Statement 1 of 1: safe\nCREATE TABLE customers (id BIGINT PRIMARY KEY);\n",
	} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("the text output does not carry %q:\n%s", want, rendered.String())
		}
	}
	if strings.Contains(rendered.String(), planreadtest.UnreadValue) {
		t.Fatalf("the text output prints a value it did not read:\n%s", rendered.String())
	}
}

// A plan with nothing unread has no such line, so the line says something when
// it is there.
func TestRenderTextHasNoUnreadLineForAPlanItReadsWhole(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.SetCurrent(t, plan)

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, load(t, lab, planread.Current), planview.Text); err != nil {
		t.Fatalf("Render(text) error = %v", err)
	}
	if strings.Contains(rendered.String(), "Unread") {
		t.Fatalf("the text output speaks of unread fields on a plan it read whole:\n%s", rendered.String())
	}
}

// The SQL format is the statements and nothing else, for a script.
func TestRenderSQLOfAPlanWithUnreadFieldsIsItsStatements(t *testing.T) {
	t.Parallel()
	view := loadForDisplay(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"})

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.SQL); err != nil {
		t.Fatalf("Render(sql) error = %v", err)
	}
	if want := "CREATE TABLE customers (id BIGINT PRIMARY KEY);\n"; rendered.String() != want {
		t.Fatalf("the SQL output is %q, want %q", rendered.String(), want)
	}
}

// The JSON format is the stored document or nothing, and the stored document
// holds the values of the fields this plugin did not read. It writes nothing:
// an empty line a script took for a document would be worse than the refusal.
func TestRenderJSONRefusesAPlanWithUnreadFields(t *testing.T) {
	t.Parallel()
	view := loadForDisplay(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"})

	var rendered bytes.Buffer
	err := planview.Render(&rendered, view, planview.JSON)

	if err == nil || !strings.Contains(err.Error(), "annotations, statements[].inputs") {
		t.Fatalf("Render(json) error = %v, want a refusal that names the fields", err)
	}
	if rendered.Len() != 0 || strings.Contains(err.Error(), planreadtest.UnreadValue) {
		t.Fatalf("Render(json) wrote %q and said %v", rendered.String(), err)
	}
}

// A raised severity still says whose each rating is when the plan has unread
// fields, though the plan comes with no document to read Ptah's rating from.
func TestRenderTextSaysWhoRatedAStatementOfAPlanWithUnreadFields(t *testing.T) {
	t.Parallel()
	view := loadForDisplay(t, []string{`ALTER TABLE "customers" ALTER COLUMN "email" DROP DEFAULT`})

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.Text); err != nil {
		t.Fatalf("Render(text) error = %v", err)
	}
	if want := "destructive as this plugin reads it; Ptah rated it safe"; !strings.Contains(rendered.String(), want) {
		t.Fatalf("the text output does not carry %q:\n%s", want, rendered.String())
	}
}

// load reads a schema's plan the way the plugin does before rendering it.
func load(t *testing.T, lab *planreadtest.Lab, selection planread.Selection) planread.Plan {
	t.Helper()
	plan, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", selection)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return plan
}

// The SQL a reader pipes somewhere else is the SQL the plan holds: the same
// statements, in the same order, with nothing re-split on a semicolon. A
// function body, a string literal and a comment all carry semicolons that are
// not statement boundaries.
func TestRenderSQLKeepsTheStatementsThePlanHolds(t *testing.T) {
	t.Parallel()
	statements := []string{
		"CREATE TABLE customers (id BIGINT PRIMARY KEY)",
		"CREATE FUNCTION touch() RETURNS trigger AS $$ BEGIN NEW.seen := now(); RETURN NEW; END; $$ LANGUAGE plpgsql",
		"COMMENT ON TABLE customers IS 'one; two; three'",
		"-- a comment with a semicolon;\nALTER TABLE customers ADD COLUMN email TEXT",
	}
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, statements, "v1")
	lab.SetCurrent(t, plan)
	view := load(t, lab, planread.Current)

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.SQL); err != nil {
		t.Fatalf("Render(sql) error = %v", err)
	}

	// Each statement comes out whole, in order, terminated once. Written as
	// equality because the semicolons inside these statements are exactly what
	// a renderer that re-split would cut on, and a looser assertion would not
	// see the difference.
	var want strings.Builder
	for _, statement := range statements {
		want.WriteString(statement)
		want.WriteString(";\n")
	}
	if rendered.String() != want.String() {
		t.Fatalf("the rendered SQL is\n%s\nand the plan holds\n%s", rendered.String(), want.String())
	}
}

// The JSON a reader saves is the document the store verified, so it still
// matches the content digest printed beside it.
func TestRenderJSONIsTheStoredDocument(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	plan, document := lab.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.SetCurrent(t, plan)
	view := load(t, lab, planread.Current)

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.JSON); err != nil {
		t.Fatalf("Render(json) error = %v", err)
	}

	// One trailing newline is the only difference a terminal needs.
	if got := bytes.TrimSuffix(rendered.Bytes(), []byte("\n")); !bytes.Equal(got, document) {
		t.Fatalf("the JSON output is %d bytes and the stored document is %d", len(got), len(document))
	}
}

// The default output says what the plan is before it says what it does, and
// says nothing about how it is stored.
func TestRenderTextNamesThePlanAndNotItsChunks(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.SetApplied(t, plan)
	view := load(t, lab, planread.Applied)

	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.Text); err != nil {
		t.Fatalf("Render(text) error = %v", err)
	}

	text := rendered.String()
	for _, want := range []string{
		"application/storefront",
		"applied (" + plan.Name + ")",
		plan.Spec.Fingerprint,
		plan.Spec.ContentDigest,
		"postgresql",
		"CREATE TABLE customers",
		"Applied:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the text output does not carry %q:\n%s", want, text)
		}
	}
	// The ConfigMaps are how the plan is stored, not what it says.
	if strings.Contains(text, plan.Spec.Chunks[0].Name) {
		t.Fatalf("the text output names a chunk ConfigMap:\n%s", text)
	}
}

// The text output says which kinds of authority a plan changes, the way it
// says whether the plan is destructive, so a reviewer knows what to look for
// in the SQL below it. The recorded kinds are what held the plan; a kind only
// this plugin reads in the SQL is shown apart and marked, because a plugin and
// a manager of different builds can disagree and the manager did not wait for
// it.
func TestRenderTextNamesThePrivilegeChanges(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		statements []string
		recorded   []operatorv1alpha1.PrivilegeChange
		want       string
	}{
		{
			name:       "no privilege change",
			statements: []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"},
			want:       "Privileges:     none\n",
		},
		{
			name: "found in the SQL",
			statements: []string{
				`GRANT SELECT ON TABLE "public"."customers" TO PUBLIC`,
				`CREATE OR REPLACE FUNCTION "public"."tenant"() RETURNS text AS $$ SELECT 'a' $$ LANGUAGE sql SECURITY DEFINER`,
			},
			recorded: []operatorv1alpha1.PrivilegeChange{
				operatorv1alpha1.PrivilegeChangeGrant, operatorv1alpha1.PrivilegeChangeSecurityDefiner,
				operatorv1alpha1.PrivilegeChangeFunctionReplacement,
			},
			want: "Privileges:     Grant, SecurityDefiner, FunctionReplacement\n",
		},
		{
			name:       "recorded by a plan object this build reads differently",
			statements: []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"},
			recorded:   []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeOwnership},
			want:       "Privileges:     Ownership\n",
		},
		{
			name:       "found in the SQL and not recorded",
			statements: []string{`ALTER TABLE "public"."customers" OWNER TO "app"`},
			want:       "Privileges:     Ownership (read by this plugin, not recorded)\n",
		},
		{
			name: "some recorded, some not",
			statements: []string{
				`ALTER TABLE "public"."customers" OWNER TO "app"`,
				`GRANT SELECT ON TABLE "public"."customers" TO PUBLIC`,
			},
			recorded: []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeGrant},
			want:     "Privileges:     Grant; Ownership (read by this plugin, not recorded)\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := planreadtest.NewLab(t, "storefront")
			plan := lab.PublishDocument(t, planreadtest.Document(t, test.statements), "v1",
				func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.PrivilegeChanges = test.recorded })
			lab.SetCurrent(t, plan)
			view := load(t, lab, planread.Current)
			var rendered bytes.Buffer
			if err := planview.Render(&rendered, view, planview.Text); err != nil {
				t.Fatalf("Render(text) error = %v", err)
			}
			if !strings.Contains(rendered.String(), "\n"+test.want) {
				t.Fatalf("the text output does not carry %q:\n%s", test.want, rendered.String())
			}
		})
	}
}
