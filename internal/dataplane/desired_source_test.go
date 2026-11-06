package dataplane_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// desiredSourceFixture is one file of testdata/desired-source, as the image
// support/ptah.json pins printed it on standard output, and `make
// runner-harness-write` regenerates them. validate-* is `schema validate
// --dialect=<engine>` with PTAH_SCHEMA_FILE and no network. scratch-* is
// `migrations status --db-url=sqlite:///:memory: --json` with
// PTAH_MIGRATIONS_DIR and no network. drift-*, plan-* and status-* are the
// commands the operator runs, against PostgreSQL 17.
func desiredSourceFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "desired-source", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSchemaSourceProblemReadsALoadFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, fixture, engine, want string
	}{
		{
			name: "the SQL file a PARTITION OF statement stops", fixture: "validate-sql.txt", engine: "PostgreSQL",
			want: "error parsing schema file: read SQL schema file /work/schema.sql: expected '(' after table name: expected Operator, got Identifier at position 180",
		},
		{
			name: "the same file for MySQL", fixture: "validate-sql-mysql.txt", engine: "MySQL",
			want: "error parsing schema file: read SQL schema file /work/schema.sql: expected '(' after table name: expected Operator, got Identifier at position 180",
		},
		{
			name: "YAML that does not parse", fixture: "validate-yaml.txt", engine: "PostgreSQL",
			want: "error parsing schema file: parse YAML schema: yaml: line 1: did not find expected '-' indicator",
		},
		{
			name: "a YAML error that spans two lines", fixture: "validate-yaml-value.txt", engine: "PostgreSQL",
			want: "error parsing schema file: parse YAML schema: yaml: unmarshal errors: line 1: cannot unmarshal !!str `hunter2...` into map[string]yamlschema.tableSpec",
		},
		{
			name: "HCL that does not parse", fixture: "validate-hcl.txt", engine: "PostgreSQL",
			want: "error parsing schema file: parse HCL schema: /work/bad.hcl:3,31-32: Invalid single-argument block definition; A single-line block definition must end with a closing brace immediately after its single argument definition.",
		},
		{
			name: "DBML that does not parse", fixture: "validate-dbml.txt", engine: "PostgreSQL",
			want: `error parsing schema file: /work/bad.dbml:3:1: expected a setting, found "}"`,
		},
		{
			name: "a file that is not there", fixture: "validate-missing.txt", engine: "PostgreSQL",
			want: "error parsing schema file: schema file does not exist: /work/absent.sql",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, found := dataplane.SchemaSourceProblem(desiredSourceFixture(t, test.fixture), test.engine)
			if !found || got != test.want {
				t.Fatalf("SchemaSourceProblem() = %q, %v, want %q", got, found, test.want)
			}
		})
	}
}

// TestSchemaSourceProblemRefusesWhatDoesNotExplainAFailure holds the filter to
// what a validate run can print without having failed to load the file. The
// index fixture is the one drift accepts: read as a reason, it would blame the
// file for a failure with another cause.
func TestSchemaSourceProblemRefusesWhatDoesNotExplainAFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		stdout []byte
		engine string
	}{
		{"an index drift accepts", desiredSourceFixture(t, "validate-index.txt"), "PostgreSQL"},
		{"a whole-schema fault", desiredSourceFixture(t, "validate-foreign-key.txt"), "PostgreSQL"},
		{"a file with no problem", desiredSourceFixture(t, "validate-good.txt"), "PostgreSQL"},
		{"another engine's dialect", desiredSourceFixture(t, "validate-sql.txt"), "MySQL"},
		{"no engine", desiredSourceFixture(t, "validate-sql.txt"), ""},
		{"no reason", []byte("postgres: source: \n"), "PostgreSQL"},
		{"no dialect", []byte("source: error parsing schema file\n"), "PostgreSQL"},
		{"source in an object's sentence", []byte(`postgres: index "i": source: names nothing` + "\n"), "PostgreSQL"},
		{"a usage error", []byte("--dialect is required\n"), "PostgreSQL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, found := dataplane.SchemaSourceProblem(test.stdout, test.engine); found {
				t.Fatalf("SchemaSourceProblem() = %q, want no reason", got)
			}
		})
	}
}

func TestMigrationFilesProblemReadsALoadFailure(t *testing.T) {
	t.Parallel()

	for fixture, want := range map[string]string{
		"scratch-name.json":      `no migration files matched format "auto"; unrecognized SQL files: init.sql, not-a-migration.down.sql`,
		"scratch-directive.json": `failed to load up migration 0000000001_init.up.sql: invalid migration directives in 0000000001_init.up.sql: invalid +ptah no_transaction value "sometimes": expected true or false`,
		"scratch-down.json":      `incomplete migrations found (missing up or down files): [1]`,
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			got, found := dataplane.MigrationFilesProblem(desiredSourceFixture(t, fixture))
			if !found || got != want {
				t.Fatalf("MigrationFilesProblem() = %q, %v, want %q", got, found, want)
			}
		})
	}
}

func TestMigrationFilesProblemRefusesWhatIsNotAboutTheFiles(t *testing.T) {
	t.Parallel()

	unreachable, err := os.ReadFile(filepath.Join("testdata", "target-failures", "pg-refused", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, stdout := range map[string][]byte{
		"a status":             desiredSourceFixture(t, "scratch-good.json"),
		"a target failure":     unreachable,
		"another command":      desiredSourceFixture(t, "drift-sql.json"),
		"nothing":              nil,
		"a read of the status": []byte(`{"contract_version":2,"command":"migrations status","error":{"message":"error getting migration status: permission denied for table schema_migrations"}}`),
		"a kind on a load": []byte(`{"contract_version":2,"command":"migrations status","error":{"kind":"database_unreachable",` +
			`"message":"error registering migrations: no migration files matched"}}`),
		"a prefix with no reason": []byte(`{"contract_version":2,"command":"migrations status","error":{"message":"error registering migrations: "}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, found := dataplane.MigrationFilesProblem(stdout); found {
				t.Fatalf("MigrationFilesProblem() = %q, want no reason", got)
			}
		})
	}
}

// TestACheckSaysWhatTheFailedCommandSaid holds the premise of asking again: on
// a file that does not load, the run with no target says what the run with one
// said. When a repin moves either sentence, the captures disagree here.
func TestACheckSaysWhatTheFailedCommandSaid(t *testing.T) {
	t.Parallel()

	reason, found := dataplane.SchemaSourceProblem(desiredSourceFixture(t, "validate-sql.txt"), "PostgreSQL")
	if !found {
		t.Fatal("validate-sql.txt reports no load failure")
	}
	drift, err := dataplane.DecodeDriftFailure(desiredSourceFixture(t, "drift-sql.json"))
	if err != nil {
		t.Fatal(err)
	}
	if drift.Message != reason {
		t.Errorf("schema drift said %q, schema validate %q", drift.Message, reason)
	}
	plan, err := dataplane.DecodeSchemaPlan(desiredSourceFixture(t, "plan-sql.json"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != dataplane.SchemaPlanOutcomeFailed || plan.Error == nil || plan.Error.Message != reason {
		t.Errorf("schema plan said %+v, schema validate %q", plan.Error, reason)
	}

	status, err := dataplane.DecodeMigrationStatusFailure(desiredSourceFixture(t, "status-directive.json"))
	if err != nil {
		t.Fatal(err)
	}
	files, found := dataplane.MigrationFilesProblem(desiredSourceFixture(t, "scratch-directive.json"))
	if !found || !strings.HasSuffix(status.Message, ": "+files) {
		t.Errorf("migrations status said %q, the scratch run %q", status.Message, files)
	}

	// The fault a check does not report: drift refuses the foreign key, and it
	// is not a load failure, so it keeps the exit status it had.
	foreignKey, err := dataplane.DecodeDriftFailure(desiredSourceFixture(t, "drift-foreign-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(foreignKey.Message, "error comparing schemas: ") {
		t.Errorf("schema drift said %q of the foreign key", foreignKey.Message)
	}
}
