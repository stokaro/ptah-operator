package planview_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planview"
)

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
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, statements, "v1")
	lab.current(t, plan)
	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

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
	lab := newLab(t, "storefront")
	plan, document := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.current(t, plan)
	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

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
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.applied(t, plan)
	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Applied)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

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
// in the SQL below it. The kinds are what the plan object records and what
// this reading of the SQL finds, together: a plugin and a manager of
// different builds can disagree, and the reader is never shown fewer kinds
// than either raised.
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
			want:       "Privileges:     Ownership\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := newLab(t, "storefront")
			plan := lab.storeDocument(t, planDocument(t, test.statements), "v1",
				func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.PrivilegeChanges = test.recorded })
			lab.current(t, plan)
			view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
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
