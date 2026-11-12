package dataplane_test

import (
	"os"
	"path/filepath"
	"regexp"
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
			name: "the SQL file an INHERITS clause stops", fixture: "validate-sql.txt", engine: "PostgreSQL",
			want: "error parsing schema file: read SQL schema file /work/schema.sql:2:29: unsupported table option: INHERITS",
		},
		{
			name: "the same file for MySQL", fixture: "validate-sql-mysql.txt", engine: "MySQL",
			want: "error parsing schema file: read SQL schema file /work/schema.sql:2:29: unsupported table option: INHERITS",
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
// sentence a whole-schema fault is compared with. A plan Ptah v0.23.0 refuses
// as declaration-refused says it in the same words a failed plan did, and the
// runner asks the same question of both.
func failedSentence(t *testing.T, fixture string) string {
	t.Helper()
	data := desiredSourceFixture(t, fixture)
	if strings.HasPrefix(fixture, "plan-") {
		report, err := dataplane.DecodeSchemaPlan(data)
		refused := report.Outcome == dataplane.SchemaPlanOutcomeRefused && report.Refusal != nil &&
			report.Refusal.Code == dataplane.SchemaRefusalDeclarationRefused
		if err != nil || (report.Outcome != dataplane.SchemaPlanOutcomeFailed && !refused) || report.Error == nil {
			t.Fatalf("%s is neither a failed plan nor a declaration refusal: %+v, %v", fixture, report, err)
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

// boundaryMessage is the sentence a failed command in testdata/boundaries
// holds, which `make runner-harness-write` regenerates from the pinned image.
func boundaryMessage(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "boundaries", name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(name, "drift-") {
		failure, err := dataplane.DecodeDriftFailure(data)
		if err != nil {
			t.Fatal(err)
		}
		return failure.Message
	}
	report, err := dataplane.DecodeSchemaPlan(data)
	if err != nil || report.Error == nil {
		t.Fatalf("DecodeSchemaPlan() = %#v, %v, want a failed plan", report, err)
	}
	return report.Error.Message
}

func TestRowFileOutsideRootReadsTheGuardsSentence(t *testing.T) {
	t.Parallel()

	climbs := `managed data file "../../proc/self/environ" for table public.regions: "/proc/<pid>/environ" is outside allowed root "/work"`
	rooted := `managed data file "/proc/self/environ" for table public.regions: a path starting at a root is outside allowed root "/work"`
	beside := `managed data file "regions.yaml" for table public.regions: "/source/schema/regions.yaml" is outside allowed root "/work"`
	processID := regexp.MustCompile(`/proc/\d+/`)
	for fixture, want := range map[string]string{
		"drift-row-file.json":        climbs,
		"plan-row-file.json":         climbs,
		"drift-row-file-rooted.json": rooted,
		"plan-row-file-rooted.json":  rooted,
		"drift-row-file-beside.json": beside,
		"plan-row-file-beside.json":  beside,
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			message := boundaryMessage(t, fixture)
			got, found := dataplane.RowFileOutsideRoot(message)
			if !found || processID.ReplaceAllString(got, "/proc/<pid>/") != want {
				t.Fatalf("RowFileOutsideRoot(%q) = %q, %v, want %q", message, got, found, want)
			}
		})
	}
	// What a declaration can put in the parts Ptah quotes or leaves bare.
	for name, sentence := range map[string]string{
		"a quote in the declared path":   `managed data file "a\": \"/b\" is outside allowed root \"/c" for table t: "/d" is outside allowed root "/work"`,
		"a colon and quote in the table": `managed data file "x" for table s.t: "y: "/d" is outside allowed root "/work"`,
		"a table named like the words":   `managed data file "/x" for table a path starting at a root: a path starting at a root is outside allowed root "/work"`,
		"escapes in the resolved path":   `managed data file "x" for table t: "/d\té" is outside allowed root "/w\"x"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, found := dataplane.RowFileOutsideRoot("error comparing declared rows: " + sentence); !found || got != sentence {
				t.Fatalf("RowFileOutsideRoot() = %q, %v, want %q", got, found, sentence)
			}
		})
	}
}

// TestRowFileOutsideRootRefusesAnythingElse holds the reader to the guard's own
// form. Each refusal is a sentence that differs from it where a database's answer
// or another failure would: another wrapper, words or a line after the root, a
// part that is not one quoted string, or another refusal of a row file.
func TestRowFileOutsideRootRefusesAnythingElse(t *testing.T) {
	t.Parallel()

	const tail = ` for table public.regions: "/x" is outside allowed root "/work"`
	for name, failed := range map[string]string{
		"nothing":                          "",
		"another wrapper":                  `error reading database schema: managed data file "../x"` + tail,
		"a doubled wrapper":                `error comparing declared rows: error comparing declared rows: managed data file "../x"` + tail,
		"a statement after the root":       `managed data file "../x"` + tail + "\nSQL: SELECT 1",
		"a carriage return":                `managed data file "../x"` + tail + "\r",
		"a newline in the table":           "managed data file \"../x\" for table pub\nlic: \"/x\" is outside allowed root \"/work\"",
		"words after the root":             `managed data file "../x"` + tail + `: permission denied`,
		"a second quoted string":           `managed data file "../x"` + tail + ` "/y"`,
		"a root that is not quoted":        `managed data file "../x" for table t: "/x" is outside allowed root /work`,
		"a root that is relative":          `managed data file "../x" for table t: "/x" is outside allowed root "work"`,
		"a root in backquotes":             "managed data file \"../x\" for table t: \"/x\" is outside allowed root `/work`",
		"a root that does not unquote":     `managed data file "../x" for table t: "/x" is outside allowed root "/wo\qrk"`,
		"a resolved path not quoted":       `managed data file "../x" for table t: /x is outside allowed root "/work"`,
		"a resolved path that is relative": `managed data file "../x" for table t: "x" is outside allowed root "/work"`,
		"no table":                         `managed data file "../x" for table : "/x" is outside allowed root "/work"`,
		"no table before a rooted path":    `managed data file "/x" for table : a path starting at a root is outside allowed root "/work"`,
		"a declared path not quoted":       `managed data file ../x` + tail,
		"a declared path cut short":        `managed data file "../x` + tail,
		"no root was given":                `managed data file "../x" for table t: outside allowed root: no root was given`,
		"a file that is not there":         `read managed data file "/work/x" for table "regions": open /work/x: no such file or directory`,
		"a file that does not parse":       `parse managed data file "/work/x" for table "regions": yaml: line 1: did not find expected key`,
		"the guard's words alone":          `"/x" is outside allowed root "/work"`,
		"a lookup on the way":              `managed data file "../x" for table t: resolve path relative to allowed root: lstat /x: permission denied`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, found := dataplane.RowFileOutsideRoot(failed); found {
				t.Fatalf("RowFileOutsideRoot(%q) = %q, want no reason", failed, got)
			}
		})
	}
}
