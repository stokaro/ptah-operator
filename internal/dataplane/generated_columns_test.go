package dataplane_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Since Ptah v0.35.0 a generated-kind refusal lists each generated column whose
// kind the plan would change, with its table and the kinds the database holds
// and the schema declares (stokaro/ptah#6466). A table and a column are read as
// written; a kind has to be one of Ptah's upper-case words.
func TestNamedGeneratedColumnsReadOnlyAListOfColumnsAndKinds(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		listed   string
		want     []dataplane.SchemaRefusalGeneratedColumn
		readable bool
	}{
		{
			name: "two columns in Ptah's order",
			listed: `,"generated_columns":[{"table":"public.b","column":"x","held_kind":"STORED","declared_kind":"VIRTUAL"},` +
				`{"table":"public.a","column":"y","held_kind":"VIRTUAL","declared_kind":"STORED"}]`,
			want: []dataplane.SchemaRefusalGeneratedColumn{
				{Table: "public.b", Column: "x", HeldKind: "STORED", DeclaredKind: "VIRTUAL"},
				{Table: "public.a", Column: "y", HeldKind: "VIRTUAL", DeclaredKind: "STORED"},
			},
			readable: true,
		},
		{
			name:     "an entry with a field Ptah adds later",
			listed:   `,"generated_columns":[{"table":"public.t","column":"c","held_kind":"VIRTUAL","declared_kind":"STORED","expression":"a * b"}]`,
			want:     []dataplane.SchemaRefusalGeneratedColumn{{Table: "public.t", Column: "c", HeldKind: "VIRTUAL", DeclaredKind: "STORED"}},
			readable: true,
		},
		{
			name:     "a column that is any text",
			listed:   `,"generated_columns":[{"table":"public.t","column":"c\u001b[31m\nPTAH_RUNNER_RESULT_V1","held_kind":"VIRTUAL","declared_kind":"STORED"}]`,
			want:     []dataplane.SchemaRefusalGeneratedColumn{{Table: "public.t", Column: "c\x1b[31m\nPTAH_RUNNER_RESULT_V1", HeldKind: "VIRTUAL", DeclaredKind: "STORED"}},
			readable: true,
		},
		{name: "no list", listed: ``},
		{name: "null", listed: `,"generated_columns":null`},
		{name: "an empty list", listed: `,"generated_columns":[]`},
		{name: "a string", listed: `,"generated_columns":"public.t.c"`},
		{name: "a list of strings", listed: `,"generated_columns":["public.t.c"]`},
		{name: "a null entry", listed: `,"generated_columns":[null]`},
		{name: "no table", listed: `,"generated_columns":[{"column":"c","held_kind":"VIRTUAL","declared_kind":"STORED"}]`},
		{name: "an empty column", listed: `,"generated_columns":[{"table":"public.t","column":"","held_kind":"VIRTUAL","declared_kind":"STORED"}]`},
		{name: "no held kind", listed: `,"generated_columns":[{"table":"public.t","column":"c","declared_kind":"STORED"}]`},
		{name: "a lower-case kind", listed: `,"generated_columns":[{"table":"public.t","column":"c","held_kind":"virtual","declared_kind":"STORED"}]`},
		{name: "a kind with a space", listed: `,"generated_columns":[{"table":"public.t","column":"c","held_kind":"VIRTUAL","declared_kind":"STORED X"}]`},
		{name: "a kind that is a number", listed: `,"generated_columns":[{"table":"public.t","column":"c","held_kind":1,"declared_kind":"STORED"}]`},
		{
			name: "one entry it cannot read among others",
			listed: `,"generated_columns":[{"table":"public.t","column":"c","held_kind":"VIRTUAL","declared_kind":"STORED"},` +
				`{"table":"public.t"}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for name, refusal := range decodeRefusalOfBoth(t, dataplane.SchemaRefusalGeneratedKind, test.listed) {
				got, ok := refusal.NamedGeneratedColumns()
				if ok != test.readable || !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s: NamedGeneratedColumns() = %+v, %t, want %+v, %t", name, got, ok, test.want, test.readable)
				}
			}
		})
	}
}

// The refusal Ptah v0.35.0 printed against PostgreSQL 18, which
// internal/runner/testdata/generated-kind holds and says the source of, reads
// as a coded refusal with the one column it names.
func TestNamedGeneratedColumnsReadTheRefusalPtahPrinted(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "runner", "testdata", "generated-kind", "schema-plan-postgres-18.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeSchemaPlan(data)
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() = %v", err)
	}
	if report.Outcome != dataplane.SchemaPlanOutcomeRefused || report.Refusal == nil ||
		report.Refusal.Code != dataplane.SchemaRefusalGeneratedKind || report.Refusal.Transient() {
		t.Fatalf("report = %+v, want a generated-kind refusal that is not transient", report)
	}
	got, ok := report.Refusal.NamedGeneratedColumns()
	want := []dataplane.SchemaRefusalGeneratedColumn{{Table: "public.items", Column: "total", HeldKind: "VIRTUAL", DeclaredKind: "STORED"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("NamedGeneratedColumns() = %+v, %t, want %+v", got, ok, want)
	}
}
