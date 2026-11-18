package dataplane_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Since Ptah v0.30.1 schema apply, its dry run included, refuses an owner the
// server would refuse before any statement runs (stokaro/ptah#6196); schema
// plan does not check owners. The refusal names one statement in an owner
// object, with what its code describes beside the statement, kind and role.
// The documents are shaped as that release reports each code, with the
// statements v0.30.0 planned against PostgreSQL 17. A form the operator does
// not read leaves the document readable and the refusal read by its code.
func TestNamedOwnerReadsWhatEachCodeNames(t *testing.T) {
	t.Parallel()

	const (
		missing  = `ALTER VIEW \"public\".\"my_docs\" OWNER TO \"no_such_role\"`
		transfer = `ALTER VIEW \"public\".\"my_docs\" OWNER TO \"outsider\"`
		restore  = `ALTER VIEW \"public\".\"kept_docs\" OWNER TO \"auditors\"`
		schema   = `ALTER VIEW \"public\".\"my_docs\" OWNER TO \"nocreate\"`
	)
	owner := func(fields string) string { return `,"owner":{` + fields + `}` }
	for _, test := range []struct {
		name, code, listed string
		want               dataplane.SchemaRefusalOwner
		readable           bool
	}{
		{
			name: "a role the server lacks", code: dataplane.SchemaRefusalMissingOwnerRole,
			listed:   owner(`"statement":"` + missing + `","kind":"VIEW","role":"no_such_role"`),
			want:     dataplane.SchemaRefusalOwner{Statement: `ALTER VIEW "public"."my_docs" OWNER TO "no_such_role"`, Kind: "VIEW", Role: "no_such_role"},
			readable: true,
		},
		{
			name: "a role the login may not give the view to", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + transfer + `","kind":"VIEW","role":"outsider","connected":"ptah","privilege":"SET"`),
			want: dataplane.SchemaRefusalOwner{
				Statement: `ALTER VIEW "public"."my_docs" OWNER TO "outsider"`, Kind: "VIEW", Role: "outsider",
				Connected: "ptah", Privilege: "SET",
			},
			readable: true,
		},
		{
			name: "the owner a rebuild gives back, before PostgreSQL 16", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + restore + `","kind":"VIEW","role":"auditors","connected":"ptah","privilege":"MEMBER","holds":true`),
			want: dataplane.SchemaRefusalOwner{
				Statement: `ALTER VIEW "public"."kept_docs" OWNER TO "auditors"`, Kind: "VIEW", Role: "auditors",
				Connected: "ptah", Privilege: "MEMBER", Holds: true,
			},
			readable: true,
		},
		{
			name: "an owner with no CREATE on the schema", code: dataplane.SchemaRefusalOwnerSchemaPrivilege,
			listed:   owner(`"statement":"` + schema + `","kind":"VIEW","role":"nocreate","schema":"public"`),
			want:     dataplane.SchemaRefusalOwner{Statement: `ALTER VIEW "public"."my_docs" OWNER TO "nocreate"`, Kind: "VIEW", Role: "nocreate", Schema: "public"},
			readable: true,
		},
		{
			name: "a field Ptah adds later", code: dataplane.SchemaRefusalMissingOwnerRole,
			listed:   owner(`"statement":"` + missing + `","kind":"VIEW","role":"no_such_role","object":"public.my_docs"`),
			want:     dataplane.SchemaRefusalOwner{Statement: `ALTER VIEW "public"."my_docs" OWNER TO "no_such_role"`, Kind: "VIEW", Role: "no_such_role"},
			readable: true,
		},
		{name: "no owner", code: dataplane.SchemaRefusalOwnerTransfer},
		{name: "null", code: dataplane.SchemaRefusalOwnerTransfer, listed: `,"owner":null`},
		{name: "a string", code: dataplane.SchemaRefusalMissingOwnerRole, listed: `,"owner":"no_such_role"`},
		{name: "a list", code: dataplane.SchemaRefusalMissingOwnerRole, listed: `,"owner":[{"statement":"` + missing + `","kind":"VIEW","role":"no_such_role"}]`},
		{name: "no statement", code: dataplane.SchemaRefusalMissingOwnerRole, listed: owner(`"kind":"VIEW","role":"no_such_role"`)},
		{name: "no kind", code: dataplane.SchemaRefusalMissingOwnerRole, listed: owner(`"statement":"` + missing + `","role":"no_such_role"`)},
		{name: "no role", code: dataplane.SchemaRefusalMissingOwnerRole, listed: owner(`"statement":"` + missing + `","kind":"VIEW"`)},
		{name: "a role that is a number", code: dataplane.SchemaRefusalMissingOwnerRole, listed: owner(`"statement":"` + missing + `","kind":"VIEW","role":7`)},
		{
			name: "a transfer with no connected role", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + transfer + `","kind":"VIEW","role":"outsider","privilege":"SET"`),
		},
		{
			name: "a transfer with no privilege", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + transfer + `","kind":"VIEW","role":"outsider","connected":"ptah"`),
		},
		{
			name: "a privilege that is not a word of Ptah's", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + transfer + `","kind":"VIEW","role":"outsider","connected":"ptah","privilege":"SET; GRANT"`),
		},
		{
			name: "a privilege in lower case", code: dataplane.SchemaRefusalOwnerTransfer,
			listed: owner(`"statement":"` + transfer + `","kind":"VIEW","role":"outsider","connected":"ptah","privilege":"set"`),
		},
		{
			name: "a schema refusal with no schema", code: dataplane.SchemaRefusalOwnerSchemaPrivilege,
			listed: owner(`"statement":"` + schema + `","kind":"VIEW","role":"nocreate"`),
		},
		{
			name: "holds that is not a boolean", code: dataplane.SchemaRefusalMissingOwnerRole,
			listed: owner(`"statement":"` + missing + `","kind":"VIEW","role":"no_such_role","holds":"true"`),
		},
		{
			// An owner is read only under a code that names one.
			name: "an owner under another code", code: "view-definer",
			listed: owner(`"statement":"` + missing + `","kind":"VIEW","role":"no_such_role"`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for document, refusal := range decodeRefusalOfBoth(t, test.code, test.listed) {
				if refusal == nil || refusal.Code != test.code {
					t.Fatalf("%s: Refusal = %+v, want the code %s", document, refusal, test.code)
				}
				got, ok := refusal.NamedOwner()
				if ok != test.readable || got != test.want {
					t.Errorf("%s: NamedOwner() = %+v, %t, want %+v, %t", document, got, ok, test.want, test.readable)
				}
			}
		})
	}
}

// The runner harness's dry runs of a plan that gives a view to an owner the
// server refuses, at the pinned build: the document names the owner as the
// rows above shape it.
func TestTheCapturedOwnerRefusalsNameTheOwner(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		file, code string
		want       dataplane.SchemaRefusalOwner
	}{
		{
			file: "differing/apply-owner-transfer-refused.json", code: dataplane.SchemaRefusalOwnerTransfer,
			want: dataplane.SchemaRefusalOwner{
				Statement: `ALTER VIEW "public"."differs_notes_view" OWNER TO "harness_view_outsider"`, Kind: "VIEW",
				Role: "harness_view_outsider", Connected: "harness_view_transferer", Privilege: "SET",
			},
		},
		{
			file: "differing/apply-owner-missing-refused.json", code: dataplane.SchemaRefusalMissingOwnerRole,
			want: dataplane.SchemaRefusalOwner{
				Statement: `ALTER VIEW "public"."differs_notes_view" OWNER TO "harness_view_nobody"`, Kind: "VIEW",
				Role: "harness_view_nobody",
			},
		},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			report, err := dataplane.DecodeSchemaApply(loadCapture(t, test.file))
			if err != nil {
				t.Fatalf("DecodeSchemaApply() error = %v", err)
			}
			if report.Refusal == nil || report.Refusal.Code != test.code {
				t.Fatalf("Refusal = %+v, want the code %s", report.Refusal, test.code)
			}
			if got, ok := report.Refusal.NamedOwner(); !ok || got != test.want {
				t.Fatalf("NamedOwner() = %+v, %t, want %+v", got, ok, test.want)
			}
		})
	}
}
