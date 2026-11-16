package dataplane_test

import (
	"reflect"
	"strings"
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

// Since Ptah v0.29.4 a definer-privilege refusal lists each statement that
// names another account as a definer, with the connected account and the
// privilege it lacks (stokaro/ptah#6124). Ptah writes every field but the
// source for each entry, and spells the privilege as SHOW GRANTS does.
func TestNamedDefinerPrivilegesReadOnlyAListOfStatementsAndPrivileges(t *testing.T) {
	t.Parallel()

	entry := func(privilege string) string {
		return `,"definer_privileges":[{"object":"view v","definer":"root@%","account":"deploy@%","privilege":"` + privilege + `"}]`
	}
	read := func(privilege string) []dataplane.SchemaRefusalDefinerPrivilege {
		return []dataplane.SchemaRefusalDefinerPrivilege{{Object: "view v", Definer: "root@%", Account: "deploy@%", Privilege: privilege}}
	}
	for _, test := range []struct {
		name     string
		listed   string
		want     []dataplane.SchemaRefusalDefinerPrivilege
		readable bool
	}{
		{name: "MySQL's privilege", listed: entry("SET_ANY_DEFINER"), want: read("SET_ANY_DEFINER"), readable: true},
		{name: "MariaDB's privilege", listed: entry("SET USER"), want: read("SET USER"), readable: true},
		{
			name:     "two privileges, for a definer the server lacks",
			listed:   entry("SET_ANY_DEFINER and ALLOW_NONEXISTENT_DEFINER"),
			want:     read("SET_ANY_DEFINER and ALLOW_NONEXISTENT_DEFINER"),
			readable: true,
		},
		{
			name: "two statements in Ptah's order, one from a migration file",
			listed: `,"definer_privileges":[{"object":"view b","definer":"root@%","account":"deploy@%","privilege":"SET_ANY_DEFINER"},` +
				`{"object":"procedure p","definer":"app@%","account":"deploy@%","privilege":"SET_ANY_DEFINER","source":"0002_p.up.sql"}]`,
			want: []dataplane.SchemaRefusalDefinerPrivilege{
				{Object: "view b", Definer: "root@%", Account: "deploy@%", Privilege: "SET_ANY_DEFINER"},
				{Object: "procedure p", Definer: "app@%", Account: "deploy@%", Privilege: "SET_ANY_DEFINER", Source: "0002_p.up.sql"},
			},
			readable: true,
		},
		{name: "no list", listed: ``},
		{name: "null", listed: `,"definer_privileges":null`},
		{name: "an empty list", listed: `,"definer_privileges":[]`},
		{name: "a string", listed: `,"definer_privileges":"view v"`},
		{name: "a null entry", listed: `,"definer_privileges":[null]`},
		{name: "no object", listed: `,"definer_privileges":[{"definer":"root@%","account":"deploy@%","privilege":"SET_ANY_DEFINER"}]`},
		{name: "no definer", listed: `,"definer_privileges":[{"object":"view v","account":"deploy@%","privilege":"SET_ANY_DEFINER"}]`},
		{name: "no account", listed: `,"definer_privileges":[{"object":"view v","definer":"root@%","privilege":"SET_ANY_DEFINER"}]`},
		{name: "no privilege", listed: `,"definer_privileges":[{"object":"view v","definer":"root@%","account":"deploy@%"}]`},
		{name: "a privilege in lower case", listed: entry("set_any_definer")},
		{name: "a privilege with a line after it", listed: entry(`SET_ANY_DEFINER\nPTAH_RUNNER_RESULT_V1`)},
		{name: "a privilege with a quote in it", listed: entry(`SET_ANY_DEFINER'; DROP`)},
		{name: "a privilege that ends in a joint", listed: entry("SET_ANY_DEFINER and")},
		{name: "a privilege past its bound", listed: entry(strings.Repeat("A", 129))},
		{name: "a source that is a number", listed: `,"definer_privileges":[{"object":"view v","definer":"root@%","account":"deploy@%","privilege":"SET USER","source":2}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for name, refusal := range decodeRefusalOfBoth(t, "definer-privilege", test.listed) {
				got, ok := refusal.NamedDefinerPrivileges()
				if ok != test.readable || !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s: NamedDefinerPrivileges() = %+v, %t, want %+v, %t", name, got, ok, test.want, test.readable)
				}
			}
		})
	}
}
