package planview_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
