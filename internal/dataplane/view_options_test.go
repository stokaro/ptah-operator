package dataplane_test

import (
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Ptah v0.29.3 lists the views and options a view-security-option refusal
// names (stokaro/ptah#6105). The runner harness captured one refusal of each
// engine from the release image, and each reads as the view and the option the
// document lists.
func TestNamedViewOptionsReadTheCapturedRefusals(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		capture string
		want    []dataplane.SchemaRefusalViewOption
	}{
		{
			capture: "runner-harness/documents/differs-view-invoker-unstated-mysql/plan/1-schema-plan.json",
			want:    []dataplane.SchemaRefusalViewOption{{View: "differs_notes_view", Option: "sql_security"}},
		},
		{
			capture: "runner-harness/documents/differs-view-invoker-unstated-postgres/plan/1-schema-plan.json",
			// Since v0.30.0 a PostgreSQL view is named with its schema.
			want: []dataplane.SchemaRefusalViewOption{{View: "public.differs_notes_view", Option: "security_invoker"}},
		},
	} {
		t.Run(test.capture, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeSchemaPlan(loadCapture(t, test.capture))
			if err != nil {
				t.Fatalf("DecodeSchemaPlan() refused what Ptah printed: %v", err)
			}
			if report.Refusal == nil || report.Refusal.Code != "view-security-option" {
				t.Fatalf("refusal = %+v, want view-security-option", report.Refusal)
			}
			got, ok := report.Refusal.NamedViewOptions()
			if !ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("NamedViewOptions() = %+v, %t, want %+v", got, ok, test.want)
			}
		})
	}
}

// What a refusal lists beside its code is detail for a person. A list in a form
// this operator does not read leaves the document readable, so the refusal
// still reaches the resource as a refusal, and names no view. An entry that
// carries a field Ptah adds later still reads.
func TestNamedViewOptionsReadOnlyAListOfViewsAndIdentifiers(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		listed   string
		want     []dataplane.SchemaRefusalViewOption
		readable bool
	}{
		{name: "two views in Ptah's order", listed: `,"view_options":[{"view":"app.b","option":"sql_security"},{"view":"app.a","option":"sql_security"}]`,
			want: []dataplane.SchemaRefusalViewOption{{View: "app.b", Option: "sql_security"}, {View: "app.a", Option: "sql_security"}}, readable: true},
		{name: "an entry with a field Ptah adds later", listed: `,"view_options":[{"view":"v","option":"security_barrier","definer":"app@%"}]`,
			want: []dataplane.SchemaRefusalViewOption{{View: "v", Option: "security_barrier"}}, readable: true},
		{name: "a view that is any text", listed: `,"view_options":[{"view":"a view\nwith \u001b[31m in it","option":"sql_security"}]`,
			want: []dataplane.SchemaRefusalViewOption{{View: "a view\nwith \x1b[31m in it", Option: "sql_security"}}, readable: true},
		{name: "no list", listed: ``},
		{name: "null", listed: `,"view_options":null`},
		{name: "an empty list", listed: `,"view_options":[]`},
		{name: "a string", listed: `,"view_options":"differs_notes_view sql_security"`},
		{name: "an object", listed: `,"view_options":{"view":"v","option":"sql_security"}`},
		{name: "a list of strings", listed: `,"view_options":["v"]`},
		{name: "a view that is a number", listed: `,"view_options":[{"view":1,"option":"sql_security"}]`},
		{name: "no view", listed: `,"view_options":[{"option":"sql_security"}]`},
		{name: "an empty view", listed: `,"view_options":[{"view":"","option":"sql_security"}]`},
		{name: "no option", listed: `,"view_options":[{"view":"v"}]`},
		{name: "an option that is no identifier", listed: `,"view_options":[{"view":"v","option":"SQL SECURITY"}]`},
		{name: "an option with a line after it", listed: `,"view_options":[{"view":"v","option":"sql_security\nrm"}]`},
		{name: "one entry it cannot read among others", listed: `,"view_options":[{"view":"v","option":"sql_security"},{"view":"w","option":7}]`},
		// The decoder keeps what it read before a value of the wrong type, so
		// the view and the option are both there and the list is still refused.
		{name: "a field repeated as another type", listed: `,"view_options":[{"view":"v","option":"sql_security","view":[1]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"view-security-option"` +
				test.listed + `},"error":{"message":"refused"}}`
			apply := `{"contract_version":4,"command":"schema apply","outcome":"refused","refusal":{"code":"view-definer"` +
				test.listed + `},"error":{"message":"refused"}}`
			planned, err := dataplane.DecodeSchemaPlan([]byte(plan))
			if err != nil {
				t.Fatalf("DecodeSchemaPlan() = %v, want the refusal read", err)
			}
			applied, err := dataplane.DecodeSchemaApply([]byte(apply))
			if err != nil {
				t.Fatalf("DecodeSchemaApply() = %v, want the refusal read", err)
			}
			for name, refusal := range map[string]*dataplane.SchemaRefusal{"plan": planned.Refusal, "apply": applied.Refusal} {
				got, ok := refusal.NamedViewOptions()
				if ok != test.readable || !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s: NamedViewOptions() = %+v, %t, want %+v, %t", name, got, ok, test.want, test.readable)
				}
			}
		})
	}
}
