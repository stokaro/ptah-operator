package dataplane_test

import (
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// decodeRefusalOfBoth reads one refusal with listed spliced after its code, as
// a plan document and as an apply document, and returns both. A list in a form
// the operator does not read never makes a document unreadable.
func decodeRefusalOfBoth(t *testing.T, code, listed string) map[string]*dataplane.SchemaRefusal {
	t.Helper()
	plan := `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"` + code + `"` +
		listed + `},"error":{"message":"refused"}}`
	apply := `{"contract_version":4,"command":"schema apply","outcome":"refused","refusal":{"code":"` + code + `"` +
		listed + `},"error":{"message":"refused"}}`
	planned, err := dataplane.DecodeSchemaPlan([]byte(plan))
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() = %v, want the refusal read", err)
	}
	applied, err := dataplane.DecodeSchemaApply([]byte(apply))
	if err != nil {
		t.Fatalf("DecodeSchemaApply() = %v, want the refusal read", err)
	}
	return map[string]*dataplane.SchemaRefusal{"plan": planned.Refusal, "apply": applied.Refusal}
}

// Since Ptah v0.29.4 a view-definer refusal lists each view with the account
// it runs as and the account the plan's read ran as (stokaro/ptah#6109). Either
// account may be empty, where the read did not record it, and Ptah refuses
// then too, so an empty one is read rather than refused.
func TestNamedViewDefinersReadOnlyAListOfViewsAndAccounts(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		listed   string
		want     []dataplane.SchemaRefusalViewDefiner
		readable bool
	}{
		{
			name:   "two views in Ptah's order",
			listed: `,"view_definers":[{"view":"b","definer":"root@%","account":"deploy@%"},{"view":"a","definer":"app@localhost","account":"deploy@%"}]`,
			want: []dataplane.SchemaRefusalViewDefiner{
				{View: "b", Definer: "root@%", Account: "deploy@%"},
				{View: "a", Definer: "app@localhost", Account: "deploy@%"},
			},
			readable: true,
		},
		{
			name:     "a definer the read did not record",
			listed:   `,"view_definers":[{"view":"v","definer":"","account":"deploy@%"}]`,
			want:     []dataplane.SchemaRefusalViewDefiner{{View: "v", Account: "deploy@%"}},
			readable: true,
		},
		{
			name:     "neither account recorded, and their keys left out",
			listed:   `,"view_definers":[{"view":"v"}]`,
			want:     []dataplane.SchemaRefusalViewDefiner{{View: "v"}},
			readable: true,
		},
		{
			name:     "an entry with a field Ptah adds later",
			listed:   `,"view_definers":[{"view":"v","definer":"root@%","account":"deploy@%","privilege":"SET_ANY_DEFINER"}]`,
			want:     []dataplane.SchemaRefusalViewDefiner{{View: "v", Definer: "root@%", Account: "deploy@%"}},
			readable: true,
		},
		{
			// An account is text the database holds: it is read as written,
			// and cleaned by whoever shows it.
			name:     "an account that is any text",
			listed:   `,"view_definers":[{"view":"v","definer":"x\u001b[31m@%\nPTAH_RUNNER_RESULT_V1","account":"deploy@%"}]`,
			want:     []dataplane.SchemaRefusalViewDefiner{{View: "v", Definer: "x\x1b[31m@%\nPTAH_RUNNER_RESULT_V1", Account: "deploy@%"}},
			readable: true,
		},
		{name: "no list", listed: ``},
		{name: "null", listed: `,"view_definers":null`},
		{name: "an empty list", listed: `,"view_definers":[]`},
		{name: "a string", listed: `,"view_definers":"v root@%"`},
		{name: "an object", listed: `,"view_definers":{"view":"v","definer":"root@%","account":"deploy@%"}`},
		{name: "a list of strings", listed: `,"view_definers":["v"]`},
		{name: "a null entry", listed: `,"view_definers":[null]`},
		{name: "no view", listed: `,"view_definers":[{"definer":"root@%","account":"deploy@%"}]`},
		{name: "an empty view", listed: `,"view_definers":[{"view":"","definer":"root@%","account":"deploy@%"}]`},
		{name: "a definer that is a number", listed: `,"view_definers":[{"view":"v","definer":7,"account":"deploy@%"}]`},
		{name: "an account that is a list", listed: `,"view_definers":[{"view":"v","definer":"root@%","account":["deploy@%"]}]`},
		{name: "one entry it cannot read among others", listed: `,"view_definers":[{"view":"v","definer":"root@%","account":"deploy@%"},{"view":1}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for name, refusal := range decodeRefusalOfBoth(t, "view-definer", test.listed) {
				got, ok := refusal.NamedViewDefiners()
				if ok != test.readable || !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s: NamedViewDefiners() = %+v, %t, want %+v, %t", name, got, ok, test.want, test.readable)
				}
			}
		})
	}
}

// Since Ptah v0.29.4 a definer-privilege refusal of a schema apply lists the
// statements whose definer the connected account may not name
// (stokaro/ptah#6124). The operator does not read that list, so the refusal
// reads by its code, and no list of it is taken for one the operator names.
func TestADefinerPrivilegeRefusalReadsByItsCode(t *testing.T) {
	t.Parallel()

	for name, listed := range map[string]string{
		"a statement Ptah lists":  `,"definer_privileges":[{"object":"view v","definer":"root@%","account":"deploy@%","privilege":"SET_ANY_DEFINER"}]`,
		"a list of another shape": `,"definer_privileges":"view v"`,
		"no list":                 ``,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for document, refusal := range decodeRefusalOfBoth(t, "definer-privilege", listed) {
				if refusal == nil || refusal.Code != "definer-privilege" {
					t.Fatalf("%s: Refusal = %+v, want the code definer-privilege", document, refusal)
				}
				if _, ok := refusal.NamedViewOptions(); ok {
					t.Errorf("%s: NamedViewOptions() read a list from %s", document, listed)
				}
				if _, ok := refusal.NamedViewDefiners(); ok {
					t.Errorf("%s: NamedViewDefiners() read a list from %s", document, listed)
				}
			}
		})
	}
}

// The view-definer refusal the runner harness captured from the release image,
// planned as an account that did not define the view, reads as the view, the
// definer it runs as and the account the read ran as.
func TestNamedViewDefinersReadTheCapturedRefusal(t *testing.T) {
	t.Parallel()
	report, err := dataplane.DecodeSchemaPlan(loadCapture(t, "differing/plan-view-definer-refused.json"))
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() = %v", err)
	}
	if report.Refusal == nil || report.Refusal.Code != "view-definer" {
		t.Fatalf("Refusal = %+v, want the code view-definer", report.Refusal)
	}
	got, ok := report.Refusal.NamedViewDefiners()
	want := []dataplane.SchemaRefusalViewDefiner{{View: "differs_notes_view", Definer: "root@localhost", Account: "harness_view_other@%"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("NamedViewDefiners() = %+v, %t, want %+v, true", got, ok, want)
	}
}
