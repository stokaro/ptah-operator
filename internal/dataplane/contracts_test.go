package dataplane_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDriftFindingVocabulary(t *testing.T) {
	t.Parallel()

	if dataplane.DriftFindingVocabularyVersion != 4 {
		t.Fatalf("DriftFindingVocabularyVersion = %d, want 4", dataplane.DriftFindingVocabularyVersion)
	}
	want := []string{
		"column_identities_removed", "column_types_narrowed",
		"columns_added", "columns_modified", "columns_removed", "comments_changed", "constraints_added",
		"constraints_removed", "constraints_validated", "data_rows_deleted", "data_rows_inserted",
		"data_rows_updated", "enum_values_added", "enum_values_removed", "enums_added", "enums_removed",
		"functions_added", "functions_modified", "functions_removed", "grant_options_added",
		"grant_options_removed", "grants_added", "grants_removed", "index_visibility_changed",
		"indexes_added", "indexes_removed", "indexes_renamed", "materialized_views_added",
		"materialized_views_modified", "materialized_views_removed", "rls_enabled_tables_added",
		"rls_enabled_tables_removed", "rls_force_added", "rls_force_removed", "rls_policies_added",
		"rls_policies_modified", "rls_policies_removed", "role_memberships_added",
		"role_memberships_removed", "roles_added", "roles_modified", "roles_removed", "schemas_added",
		"schemas_modified", "schemas_removed", "sequences_added", "sequences_modified",
		"sequences_removed", "table_constraints_added", "table_constraints_removed", "tables_added",
		"tables_removed", "triggers_added", "triggers_modified", "triggers_removed", "undecided",
		"unique_protections_removed", "vector_dimension_changed", "views_added", "views_modified",
		"views_removed",
	}
	got := dataplane.DriftFindingCategories()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DriftFindingCategories() = %q, want %q", got, want)
	}
	for _, category := range got {
		if !dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("the vocabulary rejected %q", category)
		}
	}
	for _, category := range []string{"", "private_schema_name", "columns_changed", "tables-added", "data_rows", "rows_updated"} {
		if dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("the vocabulary accepted %q", category)
		}
	}

	got[0] = "private_schema_name"
	got = append(got, "another_private_category")
	if fresh := dataplane.DriftFindingCategories(); !reflect.DeepEqual(fresh, want) {
		t.Fatalf("mutating DriftFindingCategories() result changed canonical vocabulary: got %q, want %q", fresh, want)
	}
	if dataplane.IsKnownDriftFindingCategory("private_schema_name") ||
		dataplane.IsKnownDriftFindingCategory("another_private_category") {
		t.Fatal("mutating DriftFindingCategories() result changed category membership")
	}
}

// A qualified category is admitted bare or with a kind of the expected form
// and length, and refused with anything else after the colon. Each refusal is
// a near miss of an admitted value.
func TestDriftFindingQualifiedCategories(t *testing.T) {
	t.Parallel()

	want := []string{"feature_access_narrowed", "feature_access_unchanged", "feature_access_unknown",
		"feature_access_widened", "feature_changes"}
	if got := dataplane.DriftFindingQualifiedCategories(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DriftFindingQualifiedCategories() = %q, want %q", got, want)
	}
	longest := strings.Repeat("a", 60) + ".io/" + strings.Repeat("b", dataplane.MaxDriftFindingKindBytes-64)
	for _, category := range []string{
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
		"feature_changes:ptah.run/clickhouse/view-storage/v1",
		"feature_changes:example.org/fixture/retention_Object.v2",
		"feature_changes:" + longest,
	} {
		if !dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("the vocabulary refused %q", category)
		}
	}
	for _, category := range []string{
		"feature_changes:",
		"feature_changes:" + longest + "b",
		"feature_changes:ptah.run",
		"feature_changes:ptahrun/extension",
		"feature_changes:Ptah.run/extension",
		"feature_changes:-ptah.run/extension",
		"feature_changes:ptah.run/.hidden",
		"feature_changes:ptah.run/../extension",
		"feature_changes:ptah.run//extension",
		"feature_changes:ptah.run/extension/",
		"feature_changes:ptah.run/ext ension",
		"feature_changes:ptah.run/ext\\ension",
		"feature_changes:ptah.run/extension:again",
		"feature_change:ptah.run/extension",
		"tables_added:ptah.run/extension",
		"Feature_changes",
	} {
		if dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("the vocabulary accepted %q", category)
		}
	}
}

func TestDecodeResolve(t *testing.T) {
	t.Parallel()
	data := `{"reference":"oci://registry.example/app:v1","pinned_reference":"oci://registry.example/app@` + digest + `","digest":"` + digest + `","media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`
	report, err := dataplane.DecodeResolve([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if report.Digest != digest {
		t.Fatalf("Digest = %q, want %q", report.Digest, digest)
	}
}

func TestDecodeResolveBindsPinnedRepository(t *testing.T) {
	t.Parallel()

	valid := `{"reference":"oci://registry.example/app:latest","pinned_reference":"oci://registry.example/app@` + digest + `","digest":"` + digest + `","media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`
	if _, err := dataplane.DecodeResolve([]byte(valid)); err != nil {
		t.Fatalf("DecodeResolve() rejected a canonical default tag: %v", err)
	}

	for _, pinned := range []string{
		"oci://other.example/app@" + digest,
		"oci://registry.example/other@" + digest,
		"oci://registry.example/app@sha256:" + strings.Repeat("f", 64),
	} {
		document := strings.Replace(valid, "oci://registry.example/app@"+digest, pinned, 1)
		if _, err := dataplane.DecodeResolve([]byte(document)); err == nil {
			t.Fatalf("DecodeResolve() accepted pinned reference %q", pinned)
		}
	}
}

func TestDecodeResolveCannotRedirectDigestSelectedReference(t *testing.T) {
	t.Parallel()

	otherDigest := "sha256:" + strings.Repeat("f", 64)
	document := `{"reference":"oci://registry.example/app@` + digest +
		`","pinned_reference":"oci://registry.example/app@` + otherDigest +
		`","digest":"` + otherDigest +
		`","media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`
	if _, err := dataplane.DecodeResolve([]byte(document)); err == nil {
		t.Fatal("DecodeResolve() redirected a digest-selected reference to different content")
	}
}

func TestDecodeVerifyRequiresArrayShape(t *testing.T) {
	t.Parallel()
	data := `{"reference":"oci://registry.example/app@` + digest + `","digest":"` + digest + `","satisfied":null,"findings":[]}`
	if _, err := dataplane.DecodeVerify([]byte(data)); err == nil {
		t.Fatal("DecodeVerify() accepted a null satisfied list")
	}
}

func TestDecodeInspectAcceptsAndDiscardsDocumentedNativeDetail(t *testing.T) {
	t.Parallel()

	data := `{"reference":"oci://registry.example/app@` + digest +
		`","pinned_reference":"oci://registry.example/app@` + digest +
		`","digest":"` + digest +
		`","media_type":"application/vnd.oci.image.manifest.v1+json","size":42,` +
		`"artifact_type":"application/vnd.stokaro.ptah.schema.v1",` +
		`"annotations":{"private":"must-not-cross-boundary"},` +
		`"layers":[{"name":"schema.sql","media_type":"application/sql","size":7,"digest":"` + digest + `"}]}`
	report, err := dataplane.DecodeInspect([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if report.Digest != digest || report.ArtifactType != dataplane.SchemaArtifactType {
		t.Fatalf("DecodeInspect() = %#v", report)
	}
}

func TestDecodeDriftThresholdExitContract(t *testing.T) {
	t.Parallel()
	failed := `{"contract_version":1,"command":"schema drift","drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}]}`
	if _, err := dataplane.DecodeDrift([]byte(failed), 1); err != nil {
		t.Fatalf("DecodeDrift() rejected expected drift: %v", err)
	}
	if _, err := dataplane.DecodeDrift([]byte(failed), 0); err == nil {
		t.Fatal("DecodeDrift() accepted an inconsistent exit code")
	}

	belowThreshold := `{"contract_version":1,"command":"schema drift","drift":true,"failed":false,"failure_threshold":"destructive","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}]}`
	if _, err := dataplane.DecodeDrift([]byte(belowThreshold), 0); err != nil {
		t.Fatalf("DecodeDrift() rejected non-failing drift: %v", err)
	}
	if _, err := dataplane.DecodeDrift([]byte(belowThreshold), 1); err == nil {
		t.Fatal("DecodeDrift() accepted exit 1 when failed=false")
	}

	inconsistent := `{"contract_version":1,"command":"schema drift","drift":false,"failed":true,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[]}`
	if _, err := dataplane.DecodeDrift([]byte(inconsistent), 1); err == nil {
		t.Fatal("DecodeDrift() accepted failed=true without drift")
	}
}

func TestDecodeDriftRejectsUnsafeFindingSummaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		findings string
		want     string
	}{
		{name: "zero count", findings: `[{"category":"columns_added","count":0,"severity":"warning"}]`, want: "invalid finding"},
		{name: "negative count", findings: `[{"category":"columns_added","count":-1,"severity":"warning"}]`, want: "invalid finding"},
		{name: "invalid category", findings: `[{"category":"app.users.email","count":1,"severity":"warning"}]`, want: "invalid finding"},
		{name: "unknown identifier category", findings: `[{"category":"private_schema_name","count":1,"severity":"warning"}]`, want: "invalid finding"},
		{name: "oversized category", findings: `[{"category":"` + strings.Repeat("a", 65) + `","count":1,"severity":"warning"}]`, want: "invalid finding"},
		{name: "invalid severity", findings: `[{"category":"columns_added","count":1,"severity":"critical"}]`, want: "invalid finding"},
		{name: "duplicate category", findings: `[{"category":"columns_added","count":1,"severity":"warning"},{"category":"columns_added","count":2,"severity":"warning"}]`, want: "duplicate finding category"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report := `{"contract_version":1,"command":"schema drift","drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":` + test.findings + `}`
			if _, err := dataplane.DecodeDrift([]byte(report), 1); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeDrift() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDriftReportDigestIsCanonicalAndRequiresDiff(t *testing.T) {
	t.Parallel()

	left, err := dataplane.DecodeDrift([]byte(`{"contract_version":1,"command":"schema drift","drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}],"diff":{"tables_removed":[],"columns_added":["app.users.email"]}}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	right := left
	right.Diff = []byte(`{"columns_added":["app.users.email"],"tables_removed":[]}`)
	leftDigest, err := dataplane.DriftReportDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := dataplane.DriftReportDigest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("semantically equal drift diffs produced %q and %q", leftDigest, rightDigest)
	}

	left.Diff = nil
	if _, err := dataplane.DriftReportDigest(left); err == nil {
		t.Fatal("DriftReportDigest() accepted a report without a comparison diff")
	}
}

func TestDecodePlanFailsClosed(t *testing.T) {
	t.Parallel()
	base := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":"DROP TABLE users","severity":"destructive","reason":"drops data"}]}`
	decoded, err := dataplane.DecodePlan([]byte(base), "PostgreSQL")
	if err != nil || !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
		t.Fatalf("DecodePlan() = %#v, %v; want conservative destructive elevation", decoded, err)
	}

	unknown := strings.Replace(base, `"destructive":false`, `"destructive":true,"future":1`, 1)
	if _, err := dataplane.DecodePlan([]byte(unknown), "PostgreSQL"); err == nil {
		t.Fatal("DecodePlan() accepted an unknown plan field")
	}
	unknownInStatement := strings.Replace(base, `"reason":"drops data"`, `"reason":"drops data","future":1`, 1)
	if _, err := dataplane.DecodePlan([]byte(unknownInStatement), "PostgreSQL"); err == nil {
		t.Fatal("DecodePlan() accepted an unknown statement field")
	}
}

// Ptah v0.17.0 writes inputs on a statement for an engine an executable
// provider serves. The operator plans no such engine, so the field stays
// undeclared and refuses the plan: declaring it is a decision about what an
// Apply Pod can run, not a field to add so that a decode passes.
//
// Annotations are notes, and since Ptah v0.30.0 a built-in engine's plan
// carries them (stokaro/ptah#6165). A note runs nothing and the plan's digest
// covers it, so a plan with one decodes and carries it, in order, and a note
// out of place or with nothing to say refuses the plan.
func TestDecodePlanRefusesProviderExecutionFields(t *testing.T) {
	t.Parallel()
	base := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to",` +
		`"destructive":false,"statements":[{"sql":"CREATE TABLE t (id bigint)","severity":"safe","reason":"new table"}]}`
	if _, err := dataplane.DecodePlan([]byte(base), "PostgreSQL"); err != nil {
		t.Fatalf("DecodePlan() refused the plan without provider fields: %v", err)
	}
	inputs := strings.Replace(base, `"reason":"new table"`,
		`"reason":"new table","inputs":[{"source":"environment","name":"TOKEN"}]`, 1)
	if _, err := dataplane.DecodePlan([]byte(inputs), "PostgreSQL"); err == nil || !strings.Contains(err.Error(), "inputs") {
		t.Fatalf("DecodePlan() error = %v, want the inputs field refused", err)
	}

	annotated := func(notes string) string {
		return strings.Replace(base, `"destructive":false`, `"destructive":false,"annotations":`+notes, 1)
	}
	decoded, err := dataplane.DecodePlan([]byte(annotated(`[{"before":0,"text":"first"},{"before":1,"text":"last"}]`)), "PostgreSQL")
	if err != nil {
		t.Fatalf("DecodePlan() refused a plan with notes: %v", err)
	}
	if want := []dataplane.PlanAnnotation{{Before: 0, Text: "first"}, {Before: 1, Text: "last"}}; !reflect.DeepEqual(decoded.Annotations, want) {
		t.Fatalf("Annotations = %#v, want %#v", decoded.Annotations, want)
	}
	for name, notes := range map[string]string{
		"a note past the last statement": `[{"before":2,"text":"note"}]`,
		"a note before the first":        `[{"before":-1,"text":"note"}]`,
		"notes out of order":             `[{"before":1,"text":"last"},{"before":0,"text":"first"}]`,
		"an empty note":                  `[{"before":0,"text":" "}]`,
		"a note that is not text":        `[{"before":0,"text":7}]`,
		"a note field it does not know":  `[{"before":0,"text":"note","run":"DROP TABLE t"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodePlan([]byte(annotated(notes)), "PostgreSQL"); err == nil {
				t.Fatal("DecodePlan() accepted the notes")
			}
		})
	}
}

func noteCapture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "runner", "testdata", "note-plan", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A plan of notes is what Ptah saves since v0.30.0 when every difference it
// found is one it cannot plan, such as a generated column's new expression on
// PostgreSQL 16 (stokaro/ptah#6165). It reads only as notes: a statement, a
// declared row, a destructive mark or no note at all makes it something else.
func TestDecodeNotePlanReadsOnlyNotes(t *testing.T) {
	t.Parallel()

	// The fixtures are what the image support/ptah.json pins printed and
	// saved with `schema plan --output --json` against postgres:16-alpine, for
	// a table whose generated column the schema file gives another
	// expression. The report says no changes and names the plan it saved.
	// internal/runner/testdata/note-plan holds them, outside the runner
	// harness's captures, since the harness runs PostgreSQL 17 only.
	saved := noteCapture(t, "saved-plan-postgres-16.json")
	captured, err := dataplane.DecodeNotePlan(saved, "PostgreSQL")
	if err != nil {
		t.Fatalf("DecodeNotePlan() refused the captured plan: %v", err)
	}
	if len(captured.Statements) != 0 || len(captured.Annotations) != 1 || captured.Annotations[0].Before != 0 ||
		!strings.HasPrefix(captured.Annotations[0].Text, "WARNING: Generated column public.tasks.code changed") ||
		captured.PlacedSchema != "public" {
		t.Fatalf("DecodeNotePlan() = %#v, want one note before no statement", captured)
	}
	if _, err := dataplane.DecodePlan(saved, "PostgreSQL"); err == nil {
		t.Fatal("DecodePlan() accepted the captured plan of notes as one to apply")
	}
	report, err := dataplane.DecodeSchemaPlan(noteCapture(t, "schema-plan-postgres-16.json"))
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() error = %v", err)
	}
	sum := sha256.Sum256(saved)
	if report.Outcome != dataplane.SchemaPlanOutcomeNoChanges || report.PlanPath == "" ||
		report.PlanDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("report = %#v, want no changes naming the saved plan", report)
	}

	base := `{"format_version":1,"name":"plan_c293f78ac36c","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to",` +
		`"scope":{"kind":"realm"},"placed_schema":"public","destructive":false,"statements":[],` +
		`"annotations":[{"before":0,"text":"WARNING: Generated column public.tasks.code changed"}]}`
	plan, err := dataplane.DecodeNotePlan([]byte(base), "PostgreSQL")
	if err != nil || len(plan.Annotations) != 1 || plan.Annotations[0].Text != "WARNING: Generated column public.tasks.code changed" {
		t.Fatalf("DecodeNotePlan() = %#v, %v", plan, err)
	}
	if _, err := dataplane.DecodePlan([]byte(base), "PostgreSQL"); err == nil {
		t.Fatal("DecodePlan() accepted a plan of notes as one to apply")
	}
	for name, document := range map[string]string{
		"a statement": strings.Replace(base, `"statements":[]`,
			`"statements":[{"sql":"CREATE TABLE t (id bigint)","severity":"safe","reason":"new table"}]`, 1),
		"no note":          strings.Replace(base, `[{"before":0,"text":"WARNING: Generated column public.tasks.code changed"}]`, `[]`, 1),
		"destructive":      strings.Replace(base, `"destructive":false`, `"destructive":true`, 1),
		"a declared row":   strings.Replace(base, `"destructive":false`, `"rows_fingerprint":"sha256:x","destructive":false`, 1),
		"another engine":   strings.Replace(base, `"dialect":"postgres"`, `"dialect":"mysql"`, 1),
		"an unknown field": strings.Replace(base, `"destructive":false`, `"destructive":false,"future":1`, 1),
		"no fingerprint":   strings.Replace(base, `"from_fingerprint":"from"`, `"from_fingerprint":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodeNotePlan([]byte(document), "PostgreSQL"); err == nil {
				t.Fatal("DecodeNotePlan() accepted the plan")
			}
		})
	}
}

// The fixtures are plan files the image support/ptah.json pins saved with
// `schema plan --output` against PostgreSQL 17, stored as written by `make
// runner-harness-write`. They are pg_trgm and a table on an empty database; a
// table with a row-security policy on an empty database; the policy alone on a
// database that already held the table with row security on; and the drop of
// a policy the database holds and the schema file does not declare, under an
// exclude entry that names it by its schema and its own name, which names
// nothing since stokaro/ptah#6070. The v0.14.0 release image saved the first
// three in the same shapes. Ptah marks each statement its extension or policy
// owner rendered, and the operator refused those plans until the fields were
// declared. Ptah's verdict reaches the plan through the severity alone: the
// operator still reads the policy from the SQL when Ptah calls it safe, and
// reads the drop as a privilege change as well as destructive, so no apply
// policy runs it without an approval.
func TestDecodePlanCarriesTheStatementsAFeatureOwnerRendered(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		file        string
		owned       int
		severity    string
		access      string
		destructive bool
		privileges  []string
	}{
		{"plan-owned-extension.json", 0, "safe", "", false, nil},
		{"plan-owned-policy-new-table.json", 2, "safe", "unchanged", false, []string{dataplane.PrivilegeRowSecurityPolicy}},
		{"plan-owned-policy-widens.json", 0, "destructive", "widens", true, []string{dataplane.PrivilegeRowSecurityPolicy}},
		{"plan-policy-selector-by-schema.json", 0, "destructive", "narrows", true, []string{dataplane.PrivilegeRowSecurityPolicy}},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := dataplane.DecodePlan(data, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() refused the plan Ptah saved: %v", err)
			}
			for index, statement := range decoded.Statements {
				if statement.Owned != (index == test.owned) {
					t.Errorf("statement %d owned = %t, want only statement %d owned", index, statement.Owned, test.owned)
				}
			}
			owned := decoded.Statements[test.owned]
			if !slices.Equal(owned.Transactions, []string{"allowed"}) || owned.Severity != test.severity ||
				owned.Access != test.access || (owned.AccessReason == "") != (test.access == "") {
				t.Errorf("owned statement = %+v, want transactions [allowed], severity %s and access %q with its reason",
					owned, test.severity, test.access)
			}
			if decoded.Destructive != test.destructive || !slices.Equal(decoded.PrivilegeChanges, test.privileges) {
				t.Errorf("plan destructive = %t and privilege changes %q, want %t and %q",
					decoded.Destructive, decoded.PrivilegeChanges, test.destructive, test.privileges)
			}
		})
	}
}

// A plan that changes declared rows carries what it read them as. The operator
// reads plans strictly, so the fields Ptah adds for declared rows have to be
// part of the contract rather than an unknown field that refuses the plan --
// which is how the reference-data lifecycle failed against Ptah v0.7.0.
func TestDecodePlanCarriesTheDeclaredRowSetsItRead(t *testing.T) {
	t.Parallel()
	document := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from",` +
		`"to_fingerprint":"to","managed_rows":[{"table":"regions","keys":["code"],"columns":["code","name"]},` +
		`{"schema":"ref","table":"countries","keys":["code"],"columns":["code","name","region_code"]}],` +
		`"rows_fingerprint":"sha256:` + strings.Repeat("a", 64) + `","destructive":false,` +
		`"statements":[{"sql":"INSERT INTO regions (code, name) VALUES ('emea', 'Europe')","severity":"safe","reason":"declared row"}]}`
	decoded, err := dataplane.DecodePlan([]byte(document), "PostgreSQL")
	if err != nil {
		t.Fatalf("DecodePlan() error = %v", err)
	}
	if len(decoded.ManagedRows) != 2 || decoded.ManagedRows[0].Table != "regions" ||
		decoded.ManagedRows[1].Schema != "ref" || decoded.ManagedRows[1].Table != "countries" {
		t.Fatalf("declared row sets were not carried: %#v", decoded.ManagedRows)
	}
	if decoded.RowsFingerprint != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("rows fingerprint = %q", decoded.RowsFingerprint)
	}
	// Names and a digest, and nothing else: a row set describes the columns a
	// declaration owns, and no declared value appears in it.
	for _, rowSet := range decoded.ManagedRows {
		for _, name := range append(append([]string(nil), rowSet.Keys...), rowSet.Columns...) {
			if strings.ContainsAny(name, " '\"") {
				t.Fatalf("a declared row set carries something that is not an identifier: %q", name)
			}
		}
	}
}

// Ptah records the schemas a plan's fingerprint covers beyond the URL's own
// scope when the desired state names one. The strict decoder refused such a
// plan as carrying an unknown field until the field was declared, so a
// schema-scoped URL and a desired state naming a second schema could not plan.
func TestDecodePlanAcceptsSchemasBeyondTheURLScope(t *testing.T) {
	t.Parallel()
	document := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from",` +
		`"to_fingerprint":"to","schemas_beyond_url":["audit","ref"],"destructive":false,` +
		`"statements":[{"sql":"CREATE TABLE audit.events (id bigint)","severity":"safe","reason":"new table"}]}`
	decoded, err := dataplane.DecodePlan([]byte(document), "PostgreSQL")
	if err != nil {
		t.Fatalf("DecodePlan() error = %v", err)
	}
	if !slices.Equal(decoded.SchemasBeyondURL, []string{"audit", "ref"}) {
		t.Fatalf("schemas beyond the URL = %v, want [audit ref]", decoded.SchemasBeyondURL)
	}
}

// Ptah v0.19.0 records what the URL covered in every plan it saves, so a decoder
// that does not declare it refuses every plan. A field inside it is still
// refused, as anywhere else in the plan.
func TestDecodePlanReadsTheScopeTheURLCovered(t *testing.T) {
	t.Parallel()
	plan := func(scope string) []byte {
		return []byte(`{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from",` +
			`"to_fingerprint":"to","scope":` + scope + `,"destructive":false,` +
			`"statements":[{"sql":"CREATE TABLE events (id bigint)","severity":"safe","reason":"new table"}]}`)
	}
	for scope, want := range map[string]dataplane.PlanScope{
		`{"kind":"realm"}`:                {Kind: "realm"},
		`{"kind":"schema","name":"shop"}`: {Kind: "schema", Name: "shop"},
	} {
		decoded, err := dataplane.DecodePlan(plan(scope), "PostgreSQL")
		if err != nil {
			t.Fatalf("DecodePlan(%s) error = %v", scope, err)
		}
		if decoded.Scope == nil || *decoded.Scope != want {
			t.Fatalf("scope of %s = %+v, want %+v", scope, decoded.Scope, want)
		}
	}
	if _, err := dataplane.DecodePlan(plan(`{"kind":"realm","schemas":["shop"]}`), "PostgreSQL"); err == nil {
		t.Fatal("a scope carrying a field the operator does not declare was accepted")
	}
}

// Ptah v0.30.0 records the schema it placed unqualified names in on every
// plan of a PostgreSQL URL that pins no search_path, so a decoder that does
// not declare it refuses every such plan. The statement and the fields around
// it are what the pinned v0.31.0 saved against PostgreSQL 17 for a generated
// column whose expression changes, as v0.30.0 did; the fingerprints are
// shortened.
func TestDecodePlanAcceptsThePlacedSchema(t *testing.T) {
	t.Parallel()
	document := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from",` +
		`"to_fingerprint":"to","scope":{"kind":"realm"},"placed_schema":"public","destructive":false,` +
		`"statements":[{"sql":"ALTER TABLE \"public\".\"tasks\" ALTER COLUMN \"code\" SET EXPRESSION AS (lower(title))",` +
		`"severity":"safe","reason":"does not remove data or tighten constraints"}]}`
	decoded, err := dataplane.DecodePlan([]byte(document), "PostgreSQL")
	if err != nil {
		t.Fatalf("DecodePlan() error = %v", err)
	}
	if decoded.PlacedSchema != "public" {
		t.Fatalf("placed schema = %q, want public", decoded.PlacedSchema)
	}
	if _, err := dataplane.DecodePlan([]byte(strings.Replace(document, `"placed_schema":"public"`, `"placed_schema":["public"]`, 1)), "PostgreSQL"); err == nil {
		t.Fatal("a placed schema that is not a string was accepted")
	}
}

// And a description the operator cannot carry is still refused, rather than
// accepted because the fields are now known.
func TestDecodePlanRefusesAnUnusableDeclaredRowDescription(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	rows := `[{"table":"regions","keys":["code"],"columns":["code","name"]}]`
	for name, document := range map[string]string{
		"a fingerprint that is not one": `"managed_rows":` + rows + `,"rows_fingerprint":"not-a-digest",`,
		"row sets with no fingerprint":  `"managed_rows":` + rows + `,`,
		"a fingerprint with no row set": `"rows_fingerprint":"` + digest + `",`,
		"a row set naming no table":     `"managed_rows":[{"keys":["code"],"columns":["code"]}],"rows_fingerprint":"` + digest + `",`,
		"a row set with no key":         `"managed_rows":[{"table":"regions","keys":[],"columns":["code"]}],"rows_fingerprint":"` + digest + `",`,
		"a row set with no column":      `"managed_rows":[{"table":"regions","keys":["code"],"columns":[]}],"rows_fingerprint":"` + digest + `",`,
		"a table that is not an identifier": `"managed_rows":[{"table":"regions; DROP TABLE users","keys":["code"],"columns":["code"]}],` +
			`"rows_fingerprint":"` + digest + `",`,
		"one table described twice": `"managed_rows":[{"table":"regions","keys":["code"],"columns":["code"]},` +
			`{"table":"regions","keys":["code"],"columns":["name"]}],"rows_fingerprint":"` + digest + `",`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			full := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from",` +
				`"to_fingerprint":"to",` + document + `"destructive":false,` +
				`"statements":[{"sql":"SELECT 1","severity":"safe","reason":"declared row"}]}`
			if _, err := dataplane.DecodePlan([]byte(full), "PostgreSQL"); err == nil {
				t.Fatalf("DecodePlan() accepted %s", name)
			}
		})
	}
}

func TestDecodePlanElevatesUnderclassifiedDropIndex(t *testing.T) {
	t.Parallel()

	document := `{"format_version":1,"name":"p","dialect":"mysql","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":"ALTER TABLE users DROP INDEX users_email_key","severity":"safe","reason":"index change"}]}`
	decoded, err := dataplane.DecodePlan([]byte(document), "MySQL")
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
		t.Fatalf("DecodePlan() = %#v, want destructive DROP INDEX", decoded)
	}
}

func TestDecodePlanElevatesEveryUnderclassifiedDestructiveSQLFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
	}{
		{name: "truncate", dialect: "postgres", statement: `TRUNCATE TABLE audit_log`},
		{name: "disable row level security", dialect: "postgres", statement: `ALTER TABLE tenant DISABLE ROW LEVEL SECURITY`},
		{name: "drop enum value", dialect: "postgres", statement: `ALTER TYPE account_state DROP VALUE 'closed'`},
		{name: "delete enum catalog row", dialect: "postgres", statement: `DELETE FROM pg_enum WHERE enumlabel = 'closed'`},
		{name: "PostgreSQL narrowing cast", dialect: "postgres", statement: `ALTER TABLE users ALTER COLUMN display_name TYPE varchar(10)`},
		{name: "PostgreSQL vector dimension cast", dialect: "postgres", statement: `ALTER TABLE embeddings ALTER COLUMN value SET DATA TYPE vector(3)`},
		{name: "MySQL modify column", dialect: "mysql", statement: `ALTER TABLE users MODIFY COLUMN display_name varchar(10)`},
		{name: "MySQL change column", dialect: "mysql", statement: `ALTER TABLE users CHANGE COLUMN display_name display_name varchar(10)`},
		{name: "MySQL executable truncate", dialect: "mysql", statement: `/*!50000 TRUNCATE TABLE audit_log */`},
		{name: "MariaDB executable drop", dialect: "mysql", statement: `ALTER TABLE users /*M!100100 DROP COLUMN private_note */`},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			document := `{"format_version":1,"name":"p","dialect":` + strconv.Quote(test.dialect) +
				`,"from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":` +
				strconv.Quote(test.statement) + `,"severity":"safe","reason":"underclassified"}]}`
			decoded, err := dataplane.DecodePlan([]byte(document), test.dialect)
			if err != nil {
				t.Fatal(err)
			}
			if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
				t.Fatalf("DecodePlan() = %#v, want destructive elevation", decoded)
			}
		})
	}
}

// Ptah v0.22.0 rates these below destructive: what it plans for a database that
// differs from its declaration, and a view rebuilt for a changed column list. They
// change or lose data, so the operator's own reading is what keeps each of them
// behind allowDestructive and an approval, under apply: Always too. A statement
// that takes nothing away stays as Ptah rated it, and Always runs it.
func TestDecodePlanRatesWhatPtahPlansForADatabaseThatDiffers(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, dialect, severity, statement string
		destructive                        bool
	}{
		{"a narrowed precision", "postgres", "safe", `ALTER TABLE "t" ALTER COLUMN "at" TYPE timestamp(0)`, true},
		{"a narrowed precision on MySQL", "mysql", "safe", "ALTER TABLE `t` MODIFY COLUMN `at` timestamp", true},
		{"an identity the declaration leaves out", "postgres", "safe", `ALTER TABLE "t" ALTER COLUMN "id" DROP IDENTITY`, true},
		{"a serial default the declaration leaves out", "postgres", "safe", `ALTER TABLE "t" ALTER COLUMN "id" DROP DEFAULT`, true},
		{"a view rebuilt for its column list", "postgres", "warning", `DROP VIEW "v"`, true},
		{"a schema the declaration adds", "postgres", "safe", `CREATE SCHEMA IF NOT EXISTS "app"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := `{"format_version":1,"name":"p","dialect":` + strconv.Quote(test.dialect) +
				`,"from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":` +
				strconv.Quote(test.statement) + `,"severity":` + strconv.Quote(test.severity) + `,"reason":"rated by Ptah"}]}`
			decoded, err := dataplane.DecodePlan([]byte(document), test.dialect)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Destructive != test.destructive {
				t.Fatalf("destructive = %v, want %v", decoded.Destructive, test.destructive)
			}
		})
	}
}

func TestDecodePlanRejectsCredentialBearingPrincipalDDL(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		`CREATE ROLE "application" WITH LOGIN PASSWORD 'artifact-secret'`,
		`ALTER USER app IDENTIFIED BY 'artifact-secret'`,
		`SET PASSWORD = 'artifact-secret'`,
		`SELECT 1; SET PASSWORD = 'artifact-secret'`,
		`GRANT ALL ON *.* TO app IDENTIFIED BY 'artifact-secret'`,
		`GRANT ALL ON *.* TO app IDENTIFIED WITH mysql_native_password BY 'artifact-secret'`,
	} {
		document := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":` +
			strconv.Quote(statement) + `,"severity":"safe","reason":"principal change"}]}`
		if _, err := dataplane.DecodePlan([]byte(document), "PostgreSQL"); err == nil || strings.Contains(err.Error(), "artifact-secret") {
			t.Fatalf("DecodePlan(%q) error = %v, want credential-safe refusal", statement, err)
		}
	}

	column := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":"CREATE TABLE users (password text)","severity":"safe","reason":"table change"}]}`
	if _, err := dataplane.DecodePlan([]byte(column), "PostgreSQL"); err != nil {
		t.Fatalf("DecodePlan() rejected an ordinary password-named column: %v", err)
	}
}

func TestDecodePlanRejectsCredentialDDLThroughMySQLLexicalEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
	}{
		{name: "single quoted escape", dialect: "mysql", statement: `CREATE USER 'bob\'ops'@'%' IDENTIFIED BY 'private-secret'`},
		{name: "escaped backslashes then quote", dialect: "mysql", statement: `CREATE USER 'bob\\\'ops'@'%' IDENTIFIED BY 'private-secret'`},
		{name: "double quoted escape", dialect: "mysql", statement: `CREATE USER "bob\"ops"@"%" IDENTIFIED BY "private-secret"`},
		{name: "backtick quoted escape", dialect: "mysql", statement: "CREATE USER `bob\\`ops`@`%` IDENTIFIED BY 'private-secret'"},
		{name: "MariaDB quoted escape", dialect: "mariadb", statement: `CREATE USER 'bob\'ops'@'%' IDENTIFIED BY 'private-secret'`},
		{name: "NO_BACKSLASH_ESCAPES", dialect: "mysql", statement: `CREATE USER 'bob\'@'%' IDENTIFIED BY 'private-secret'`},
		{name: "double dash followed by text", dialect: "mysql", statement: `CREATE USER app --x IDENTIFIED BY 'private-secret'`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			document := planDocument(test.dialect, test.statement)
			if _, err := dataplane.DecodePlan(document, "MySQL"); err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("DecodePlan(%q) error = %v, want credential-safe refusal", test.statement, err)
			}
		})
	}
}

func TestDecodePlanDoesNotTreatMySQLDashDashTextAsAComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
	}{
		{name: "identifier follower", dialect: "mysql", statement: `ALTER TABLE users --x DROP COLUMN private_note`},
		{name: "numeric follower", dialect: "mysql", statement: `SELECT 5--2; TRUNCATE TABLE audit_log`},
		{name: "punctuation follower", dialect: "mysql", statement: `SELECT 1---2; DROP TABLE audit_log`},
		{name: "MariaDB identifier follower", dialect: "mariadb", statement: `SELECT value--offset; DROP TABLE audit_log`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decoded, err := dataplane.DecodePlan(planDocument(test.dialect, test.statement), "MySQL")
			if err != nil {
				t.Fatal(err)
			}
			if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
				t.Fatalf("DecodePlan() = %#v, want destructive elevation", decoded)
			}
		})
	}
}

func TestDecodePlanHonorsMySQLDashDashCommentFollowers(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		"SELECT 1 -- DROP TABLE audit_log",
		"SELECT 1 --\tTRUNCATE TABLE audit_log",
		"SELECT 1 --\x01DROP TABLE audit_log",
		"SELECT 1 --\x7fDROP TABLE audit_log",
	} {
		decoded, err := dataplane.DecodePlan(planDocument("mysql", statement), "MySQL")
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Destructive || decoded.Statements[0].Severity != "safe" {
			t.Fatalf("DecodePlan(%q) = %#v, want the comment body ignored", statement, decoded)
		}
	}

	for _, statement := range []string{
		"SELECT 1 -- ignored DROP TABLE audit_log\nTRUNCATE TABLE audit_log",
		"SELECT 1 -- ignored DROP TABLE audit_log\rTRUNCATE TABLE audit_log",
		"SELECT 1 -- ignored DROP TABLE audit_log\r\nTRUNCATE TABLE audit_log",
	} {
		decoded, err := dataplane.DecodePlan(planDocument("mysql", statement), "MySQL")
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
			t.Fatalf("DecodePlan(%q) = %#v, want scanning to resume after the line ending", statement, decoded)
		}
	}
}

func TestDecodePlanKeepsUnambiguousMySQLQuotedKeywordsOpaque(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		`SELECT 'prefix'' DROP TABLE audit_log'`,
		`SELECT "prefix"" TRUNCATE TABLE audit_log"`,
		"SELECT `prefix`` DROP TABLE audit_log`",
	} {
		decoded, err := dataplane.DecodePlan(planDocument("mysql", statement), "MySQL")
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Destructive || decoded.Statements[0].Severity != "safe" {
			t.Fatalf("DecodePlan(%q) = %#v, want quoted keywords ignored", statement, decoded)
		}
	}
}

func TestDecodePlanElevatesMySQLNOBackslashEscapesDestructiveSQL(t *testing.T) {
	t.Parallel()

	for _, dialect := range []string{"mysql", "mariadb"} {
		statement := `SELECT 'prefix\'; DROP TABLE audit_log`
		decoded, err := dataplane.DecodePlan(planDocument(dialect, statement), "MySQL")
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
			t.Fatalf("DecodePlan(%q, %q) = %#v, want destructive elevation", dialect, statement, decoded)
		}
	}
}

func TestDecodePlanMySQLEscapedBackslashDoesNotEscapeTheClosingQuote(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		`SELECT 'prefix\\'; DROP TABLE audit_log`,
		`SELECT "prefix\\"; TRUNCATE TABLE audit_log`,
		"SELECT `prefix\\\\`; DROP TABLE audit_log",
	} {
		decoded, err := dataplane.DecodePlan(planDocument("mysql", statement), "MySQL")
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
			t.Fatalf("DecodePlan(%q) = %#v, want scanning after the closing quote", statement, decoded)
		}
	}
}

func TestDecodePlanKeepsPostgreSQLLexingConservative(t *testing.T) {
	t.Parallel()

	comment, err := dataplane.DecodePlan(planDocument("postgres", `SELECT 1 --x DROP TABLE audit_log`), "PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	if comment.Destructive || comment.Statements[0].Severity != "safe" {
		t.Fatalf("DecodePlan() = %#v, want PostgreSQL --x to remain a comment", comment)
	}

	for _, statement := range []string{
		`SELECT 'literal\' DROP TABLE audit_log`,
		"SELECT 1 -- ignored DROP TABLE audit_log\rTRUNCATE TABLE audit_log",
	} {
		decoded, err := dataplane.DecodePlan(planDocument("postgres", statement), "PostgreSQL")
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
			t.Fatalf("DecodePlan(%q) = %#v, want conservative destructive elevation", statement, decoded)
		}
	}
}

func TestDecodePlanRejectsCredentialBearingDDLInMySQLExecutableComments(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		`CREATE /*!50003 USER*/ app IDENTIFIED BY 'artifact-secret'`,
		`CREATE /*M!100100 USER*/ app IDENTIFIED BY 'artifact-secret'`,
		`/*!50003 CREATE USER 'bob\'ops'@'%' IDENTIFIED BY 'artifact-secret' */`,
	} {
		document := `{"format_version":1,"name":"p","dialect":"mysql","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":` +
			strconv.Quote(statement) + `,"severity":"safe","reason":"principal change"}]}`
		if _, err := dataplane.DecodePlan([]byte(document), "MySQL"); err == nil || strings.Contains(err.Error(), "artifact-secret") {
			t.Fatalf("DecodePlan(%q) error = %v, want credential-safe refusal", statement, err)
		}
	}
}

func planDocument(dialect, statement string) []byte {
	quotedDialect, _ := json.Marshal(dialect)
	quotedStatement, _ := json.Marshal(statement)
	return []byte(`{"format_version":1,"name":"p","dialect":` + string(quotedDialect) +
		`,"from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":` +
		string(quotedStatement) + `,"severity":"safe","reason":"underclassified"}]}`)
}
