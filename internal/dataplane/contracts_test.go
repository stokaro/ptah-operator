package dataplane_test

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDriftFindingVocabularyV1(t *testing.T) {
	t.Parallel()

	if dataplane.DriftFindingVocabularyVersion != 1 {
		t.Fatalf("DriftFindingVocabularyVersion = %d, want 1", dataplane.DriftFindingVocabularyVersion)
	}
	want := []string{
		"columns_added", "columns_modified", "columns_removed",
		"constraints_added", "constraints_removed",
		"enum_values_added", "enum_values_removed", "enums_added", "enums_removed",
		"extensions_added", "extensions_modified", "extensions_removed",
		"functions_added", "functions_modified", "functions_removed",
		"indexes_added", "indexes_removed",
		"rls_enabled_tables_added", "rls_enabled_tables_removed",
		"rls_policies_added", "rls_policies_modified", "rls_policies_removed",
		"roles_added", "roles_modified", "roles_removed",
		"table_constraints_added", "table_constraints_removed",
		"tables_added", "tables_removed", "unique_protections_removed",
		"vector_dimension_changed",
	}
	got := dataplane.DriftFindingCategories()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DriftFindingCategories() = %q, want %q", got, want)
	}
	for _, category := range got {
		if !dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("v1 vocabulary rejected %q", category)
		}
	}
	for _, category := range []string{"", "private_schema_name", "columns_changed", "tables-added"} {
		if dataplane.IsKnownDriftFindingCategory(category) {
			t.Errorf("v1 vocabulary accepted %q", category)
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
	failed := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}]}`
	if _, err := dataplane.DecodeDrift([]byte(failed), 1); err != nil {
		t.Fatalf("DecodeDrift() rejected expected drift: %v", err)
	}
	if _, err := dataplane.DecodeDrift([]byte(failed), 0); err == nil {
		t.Fatal("DecodeDrift() accepted an inconsistent exit code")
	}

	belowThreshold := `{"drift":true,"failed":false,"failure_threshold":"destructive","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}]}`
	if _, err := dataplane.DecodeDrift([]byte(belowThreshold), 0); err != nil {
		t.Fatalf("DecodeDrift() rejected non-failing drift: %v", err)
	}
	if _, err := dataplane.DecodeDrift([]byte(belowThreshold), 1); err == nil {
		t.Fatal("DecodeDrift() accepted exit 1 when failed=false")
	}

	inconsistent := `{"drift":false,"failed":true,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[]}`
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
			report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":` + test.findings + `}`
			if _, err := dataplane.DecodeDrift([]byte(report), 1); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeDrift() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDriftReportDigestIsCanonicalAndRequiresDiff(t *testing.T) {
	t.Parallel()

	left, err := dataplane.DecodeDrift([]byte(`{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}],"diff":{"tables_removed":[],"columns_added":["app.users.email"]}}`), 1)
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
