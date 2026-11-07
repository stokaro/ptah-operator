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
// commands the operator runs, against PostgreSQL 17, or MySQL 8.4 for key-mysql.
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
			// Joining the lines is the runner's, after it has redacted them.
			name: "a YAML error that spans two lines", fixture: "validate-yaml-value.txt", engine: "PostgreSQL",
			want: "error parsing schema file: parse YAML schema: yaml: unmarshal errors:\n  line 1: cannot unmarshal !!str `hunter2...` into map[string]yamlschema.tableSpec",
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

// failedSentence is what a captured failed document says of itself: the
// sentence a whole-schema fault is compared with.
func failedSentence(t *testing.T, fixture string) string {
	t.Helper()
	data := desiredSourceFixture(t, fixture)
	if strings.HasPrefix(fixture, "plan-") {
		report, err := dataplane.DecodeSchemaPlan(data)
		if err != nil || report.Outcome != dataplane.SchemaPlanOutcomeFailed || report.Error == nil {
			t.Fatalf("%s is not a failed plan: %+v, %v", fixture, report, err)
		}
		return report.Error.Message
	}
	failure, err := dataplane.DecodeDriftFailure(data)
	if err != nil {
		t.Fatalf("%s: %v", fixture, err)
	}
	return failure.Message
}

func TestSchemaStructuralProblemReadsAFaultTheCommandStoppedOn(t *testing.T) {
	t.Parallel()

	const constraintKey = `invalid foreign key: constraint "t_nope" references unknown table "nope"`
	for _, test := range []struct {
		name, fixture, engine, want string
	}{
		{"a key into a table nothing declares", "foreign-key", "PostgreSQL", `invalid foreign key: field "id" references unknown table "nope"`},
		{"the key after a line for an index", "key-and-index", "PostgreSQL", constraintKey},
		{"the key on MySQL", "key-mysql", "MySQL", constraintKey},
		{"a fault that does not open with the key", "not-unique", "PostgreSQL", `postgres requires referenced columns code on table "parent" to be declared unique`},
	} {
		for _, command := range []string{"drift", "plan"} {
			t.Run(test.name+"/"+command, func(t *testing.T) {
				t.Parallel()
				got, found := dataplane.SchemaStructuralProblem(desiredSourceFixture(t, "validate-"+test.fixture+".txt"), test.engine,
					failedSentence(t, command+"-"+test.fixture+".json"))
				if !found || got != test.want {
					t.Fatalf("SchemaStructuralProblem() = %q, %v, want %q", got, found, test.want)
				}
			})
		}
	}
}

// TestSchemaStructuralProblemRefusesWhatTheCommandDidNotStopOn holds the filter
// to the measured near misses first. On a database that holds a schema the file
// declares nothing of, drift and plan stop on a key into that schema, which
// validate cannot know, so validate's own fault does not explain the failure and
// the failed command's sentence, which names what the database holds, must not
// come back.
func TestSchemaStructuralProblemRefusesWhatTheCommandDidNotStopOn(t *testing.T) {
	t.Parallel()

	for name, fixture := range map[string]string{
		"an index on a table nothing declares":          "index-table",
		"another key than the one the database refuses": "other-and-key",
		"an index, and a key the database refuses":      "other-and-index",
	} {
		for _, command := range []string{"drift", "plan"} {
			t.Run(name+"/"+command, func(t *testing.T) {
				t.Parallel()
				if got, found := dataplane.SchemaStructuralProblem(desiredSourceFixture(t, "validate-"+fixture+".txt"), "PostgreSQL",
					failedSentence(t, command+"-"+fixture+".json")); found {
					t.Fatalf("SchemaStructuralProblem() = %q, want no reason", got)
				}
			})
		}
	}

	const (
		fault   = `invalid foreign key: field "id" references unknown table "nope"`
		line    = "postgres: schema: " + fault + "\n"
		wrapped = "error comparing schemas: " + fault
	)
	for _, test := range []struct {
		name, stdout, engine, failed string
	}{
		{"a failure with another cause", line, "PostgreSQL", "error reading database schema: permission denied for schema public"},
		{"no sentence from the failed command", line, "PostgreSQL", ""},
		{"more words after the fault", line, "PostgreSQL", wrapped + `: the database compared holds schema "other"`},
		{"the words with no boundary", line, "PostgreSQL", "error comparing schemas:" + fault},
		{"the words inside a longer word", line, "PostgreSQL", "error comparing schemas: x" + fault},
		{"the words alone", line, "PostgreSQL", fault},
		{"the words in another case", line, "PostgreSQL", strings.ToUpper(wrapped)},
		{"another engine's line", line, "MySQL", wrapped},
		{"no engine", line, "", wrapped},
		{"no dialect", "schema: " + fault + "\n", "PostgreSQL", wrapped},
		{"no fault", "postgres: schema: \n", "PostgreSQL", "error comparing schemas: "},
		{"an index line with the same words", `postgres: index "i": ` + fault + "\n", "PostgreSQL", wrapped},
		{"the kind in an index's name", `postgres: index "schema: x": ` + fault + "\n", "PostgreSQL", wrapped},
		{"a load failure", string(desiredSourceFixture(t, "validate-sql.txt")), "PostgreSQL", failedSentence(t, "drift-sql.json")},
		{"a file with no problem", string(desiredSourceFixture(t, "validate-good.txt")), "PostgreSQL", wrapped},
		{"a line after the fault that the command did not say", line + `postgres: index "i": names nothing`, "PostgreSQL", wrapped},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, found := dataplane.SchemaStructuralProblem([]byte(test.stdout), test.engine, test.failed); found {
				t.Fatalf("SchemaStructuralProblem() = %q, want no reason", got)
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
		// What a file name can carry through a registry, as the runner receives it.
		"scratch-control-bytes.json": "failed to load up migration 0000000001_a\x1b]0;pwn\x07.up.sql: invalid migration directives in " +
			"0000000001_a\x1b]0;pwn\x07.up.sql: invalid +ptah no_transaction value \"sometimes\": expected true or false",
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

	// A whole-schema fault is a different sentence in each command's wrapper,
	// and the words validate prints after its kind are the words inside it.
	const foreignKey = `invalid foreign key: field "id" references unknown table "nope"`
	if validated := strings.TrimSpace(string(desiredSourceFixture(t, "validate-foreign-key.txt"))); validated != "postgres: schema: "+foreignKey {
		t.Errorf("schema validate said %q of the foreign key", validated)
	}
	if said := failedSentence(t, "drift-foreign-key.json"); said != "error comparing schemas: "+foreignKey {
		t.Errorf("schema drift said %q of the foreign key", said)
	}
	if said := failedSentence(t, "plan-foreign-key.json"); said != "compare database schema: "+foreignKey {
		t.Errorf("schema plan said %q of the foreign key", said)
	}
}
