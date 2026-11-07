package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// desiredSourceOutput is what the pinned Ptah image printed, as
// internal/dataplane/testdata/desired-source holds it.
func desiredSourceOutput(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "desired-source", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const (
	checkedSchemaFile = "/source/schema/schema.sql"
	inheritsReason    = "error parsing schema file: read SQL schema file /work/schema.sql:2:29: " +
		"unsupported table option: INHERITS"
)

// desiredSourceRun is one operation that reads the desired files, the document
// it prints when they do not load, and the code a failure with no reason gets.
type desiredSourceRun struct {
	operation    Operation
	environment  []string
	failed       func(t *testing.T) scriptedResponse
	checkArgs    []string
	checkEnv     []string
	unreadable   func(t *testing.T) scriptedResponse
	reason       string
	unclassified string
}

func desiredSourceRuns() map[string]desiredSourceRun {
	schemaEnvironment := func(id string) []string {
		return append(databaseEnvironment(id), envSchemaFile+"="+checkedSchemaFile, "HOME=/work", "TMPDIR=/work",
			EnvDevelopmentDatabaseURL+"=postgres://dev:devsecret@dev.example/dev", "PTAH_CONNECT_TIMEOUT=5s")
	}
	validate := func(t *testing.T) scriptedResponse {
		return scriptedResponse{stdout: desiredSourceOutput(t, "validate-sql.txt"), stderr: "1 structural problem\n", exitCode: 1}
	}
	schemaCheckEnv := []string{envSchemaFile + "=" + checkedSchemaFile, "HOME=/work", "TMPDIR=/work"}
	return map[string]desiredSourceRun{
		"observe": {
			operation:   OperationObserve,
			environment: schemaEnvironment("desired-source-observe"),
			failed: func(t *testing.T) scriptedResponse {
				return scriptedResponse{stdout: desiredSourceOutput(t, "drift-sql.json"), exitCode: 2}
			},
			checkArgs:    []string{"schema", "validate", "--dialect=PostgreSQL"},
			checkEnv:     schemaCheckEnv,
			unreadable:   validate,
			reason:       inheritsReason,
			unclassified: "child_exit",
		},
		"plan": {
			operation:   OperationPlan,
			environment: schemaEnvironment("desired-source-plan"),
			failed: func(t *testing.T) scriptedResponse {
				return scriptedResponse{stdout: desiredSourceOutput(t, "plan-sql.json"), exitCode: 2}
			},
			checkArgs:    []string{"schema", "validate", "--dialect=PostgreSQL"},
			checkEnv:     schemaCheckEnv,
			unreadable:   validate,
			reason:       inheritsReason,
			unclassified: "invalid_plan_output",
		},
		"migration history": {
			operation:   OperationMigrationHistory,
			environment: append(migrationEnvironment("desired-source-history"), "HOME=/work", "TMPDIR=/work"),
			failed: func(t *testing.T) scriptedResponse {
				return scriptedResponse{stdout: desiredSourceOutput(t, "status-directive.json"), exitCode: 2}
			},
			checkArgs: []string{"migrations", "status", "--db-url=sqlite:///:memory:", "--json"},
			checkEnv:  []string{envMigrationsDir + "=/source/migrations", "HOME=/work", "TMPDIR=/work"},
			unreadable: func(t *testing.T) scriptedResponse {
				return scriptedResponse{stdout: desiredSourceOutput(t, "scratch-directive.json"), exitCode: 2}
			},
			reason: `failed to load up migration 0000000001_init.up.sql: invalid migration directives in ` +
				`0000000001_init.up.sql: invalid +ptah no_transaction value "sometimes": expected true or false`,
			unclassified: "child_exit",
		},
	}
}

func runDesiredSource(t *testing.T, run desiredSourceRun, responses ...scriptedResponse) (Result, *scriptedExecutor, string) {
	t.Helper()
	executor := &scriptedExecutor{t: t, responses: responses}
	diagnostics := &bytes.Buffer{}
	result := Run(context.Background(), Config{
		Operation:   run.operation,
		Environment: withRunnerProtocol(run.environment),
		Executor:    executor,
		Diagnostics: diagnostics,
		TempDir:     t.TempDir(),
	})
	encoded, err := EncodeResult(result)
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if encoded.SummaryErr != nil {
		t.Fatalf("the result has no termination summary: %v", encoded.SummaryErr)
	}
	if _, err := ParseResultFor(encoded.Frame, run.operation, result.OperationID); err != nil {
		t.Fatalf("the frame does not parse: %v", err)
	}
	return result, executor, diagnostics.String()
}

// TestADesiredFileThatDoesNotLoadIsReportedInPtahsWords runs each operation on
// the output the pinned image printed for a file it cannot read, and holds the
// frame to the reason and the second child to what it may know.
func TestADesiredFileThatDoesNotLoadIsReportedInPtahsWords(t *testing.T) {
	t.Parallel()

	for name, run := range desiredSourceRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, executor, diagnostics := runDesiredSource(t, run, run.failed(t), run.unreadable(t))
			if result.Error == nil || result.Error.Code != CodeDesiredSourceUnreadable || result.Error.Message != run.reason {
				t.Fatalf("Run() error = %#v, want code %s and %q", result.Error, CodeDesiredSourceUnreadable, run.reason)
			}
			// The exit status of the command that failed, and no claim about
			// the database: nothing here started a mutation.
			if result.ChildExitCode != 2 || result.Uncertain || result.MutationStarted {
				t.Fatalf("Run() = exit %d, uncertain %v, mutation %v", result.ChildExitCode, result.Uncertain, result.MutationStarted)
			}
			if !strings.Contains(diagnostics, run.reason) {
				t.Fatalf("the Pod log does not say why:\n%s", diagnostics)
			}
			if len(executor.calls) != 2 {
				t.Fatalf("ran %d children, want the failed command and its check", len(executor.calls))
			}
			check := executor.calls[1]
			if !slices.Equal(check.Args, run.checkArgs) {
				t.Fatalf("check arguments = %q, want %q", check.Args, run.checkArgs)
			}
			if !slices.Equal(check.Env, run.checkEnv) {
				t.Fatalf("check environment = %q, want %q and nothing else", check.Env, run.checkEnv)
			}
			for _, key := range protectedEnvironmentKeys {
				for _, entry := range check.Env {
					if strings.HasPrefix(entry, key+"=") {
						t.Fatalf("the check was handed %s", key)
					}
				}
			}
		})
	}
}

// TestAFailureACheckCannotExplainKeepsItsCode gives each operation a check
// that answers anything other than a file that does not load. The failure
// stays as it was, and nothing the check printed reaches the frame.
func TestAFailureACheckCannotExplainKeepsItsCode(t *testing.T) {
	t.Parallel()

	for name, run := range desiredSourceRuns() {
		schemaCheck := run.operation != OperationMigrationHistory
		answers := map[string]scriptedResponse{
			"the check could not be run": {err: errors.New("CHECK-TEXT")},
		}
		if schemaCheck {
			answers["the file loads"] = scriptedResponse{stdout: desiredSourceOutput(t, "validate-good.txt"), exitCode: 0}
			answers["an index that drift accepts"] = scriptedResponse{stdout: desiredSourceOutput(t, "validate-index.txt"), exitCode: 1}
			answers["a whole-schema fault"] = scriptedResponse{stdout: desiredSourceOutput(t, "validate-foreign-key.txt"), exitCode: 1}
			answers["a load failure with another exit status"] = scriptedResponse{stdout: desiredSourceOutput(t, "validate-sql.txt"), exitCode: 2}
			answers["another dialect"] = scriptedResponse{stdout: desiredSourceOutput(t, "validate-sql-mysql.txt"), exitCode: 1}
			// A reason in the right shape, cut off by the bound: what is left is
			// not known to be the reason.
			answers["a load failure past the bound"] = scriptedResponse{
				stdout: "postgres: source: " + strings.Repeat("CHECK-TEXT ", maxDesiredSourceCheckBytes), exitCode: 1,
			}
		} else {
			answers["the files load"] = scriptedResponse{stdout: desiredSourceOutput(t, "scratch-good.json"), exitCode: 0}
			answers["a load failure with exit 0"] = scriptedResponse{stdout: desiredSourceOutput(t, "scratch-directive.json"), exitCode: 0}
			answers["a document past the bound"] = scriptedResponse{stdout: strings.Repeat("CHECK-TEXT ", maxDesiredSourceCheckBytes), exitCode: 2}
			answers["a failure that is not a load"] = scriptedResponse{
				stdout:   `{"contract_version":2,"command":"migrations status","error":{"message":"CHECK-TEXT error getting migration status"}}`,
				exitCode: 2,
			}
		}
		for label, answer := range answers {
			t.Run(name+"/"+label, func(t *testing.T) {
				t.Parallel()
				result, executor, diagnostics := runDesiredSource(t, run, run.failed(t), answer)
				if result.Error == nil || result.Error.Code != run.unclassified {
					t.Fatalf("Run() error = %#v, want code %s", result.Error, run.unclassified)
				}
				if len(executor.calls) != 2 {
					t.Fatalf("ran %d children, want the failed command and its check", len(executor.calls))
				}
				if strings.Contains(result.Error.Message, "CHECK-TEXT") || strings.Contains(diagnostics, "CHECK-TEXT") {
					t.Fatalf("the runner repeated what the check printed: %q\n%s", result.Error.Message, diagnostics)
				}
			})
		}
	}
}

// TestASchemaFaultTheCommandStoppedOnIsNamed runs Observe and Plan on the output
// the pinned image printed for a schema that loads and that drift and plan
// refuse. The child that said it is the same check, with the same environment.
func TestASchemaFaultTheCommandStoppedOnIsNamed(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		operation, failed, validated, want string
	}{
		{"observe", "drift-foreign-key.json", "validate-foreign-key.txt", `schema.sql: invalid foreign key: field "id" references unknown table "nope"`},
		{"plan", "plan-foreign-key.json", "validate-foreign-key.txt", `schema.sql: invalid foreign key: field "id" references unknown table "nope"`},
		{"observe", "drift-key-and-index.json", "validate-key-and-index.txt", `schema.sql: invalid foreign key: constraint "t_nope" references unknown table "nope"`},
		{"plan", "plan-key-and-index.json", "validate-key-and-index.txt", `schema.sql: invalid foreign key: constraint "t_nope" references unknown table "nope"`},
	} {
		t.Run(test.operation+"/"+test.failed, func(t *testing.T) {
			t.Parallel()
			run := desiredSourceRuns()[test.operation]
			result, executor, diagnostics := runDesiredSource(t, run,
				scriptedResponse{stdout: desiredSourceOutput(t, test.failed), exitCode: 2},
				scriptedResponse{stdout: desiredSourceOutput(t, test.validated), stderr: "1 structural problem\n", exitCode: 1})
			if result.Error == nil || result.Error.Code != CodeDesiredSchemaInvalid || result.Error.Message != test.want {
				t.Fatalf("Run() error = %#v, want code %s and %q", result.Error, CodeDesiredSchemaInvalid, test.want)
			}
			if result.ChildExitCode != 2 || result.Uncertain || result.MutationStarted {
				t.Fatalf("Run() = exit %d, uncertain %v, mutation %v", result.ChildExitCode, result.Uncertain, result.MutationStarted)
			}
			if !strings.Contains(diagnostics, test.want) {
				t.Fatalf("the Pod log does not say why:\n%s", diagnostics)
			}
			if len(executor.calls) != 2 || !slices.Equal(executor.calls[1].Env, run.checkEnv) {
				t.Fatalf("ran %+v, want the failed command and a check told only %q", executor.calls, run.checkEnv)
			}
		})
	}
}

// TestAFaultTheCommandDidNotStopOnKeepsItsCode gives each operation the captures
// of a file validate faults while drift and plan stop on something else, and
// documents that hold no sentence to compare. Nothing either child printed
// reaches the frame or the Pod log.
func TestAFaultTheCommandDidNotStopOnKeepsItsCode(t *testing.T) {
	t.Parallel()

	foreignKey := func(t *testing.T) scriptedResponse {
		return scriptedResponse{stdout: desiredSourceOutput(t, "validate-foreign-key.txt"), exitCode: 1}
	}
	captured := func(failed, validated string) func(*testing.T) (scriptedResponse, scriptedResponse) {
		return func(t *testing.T) (scriptedResponse, scriptedResponse) {
			return scriptedResponse{stdout: desiredSourceOutput(t, failed), exitCode: 2},
				scriptedResponse{stdout: desiredSourceOutput(t, validated), exitCode: 1}
		}
	}
	for _, test := range []struct {
		name, operation, code string
		answers               func(*testing.T) (scriptedResponse, scriptedResponse)
	}{
		{"an index on a table nothing declares", "observe", "child_exit", captured("drift-index-table.json", "validate-index-table.txt")},
		{"an index on a table nothing declares", "plan", "invalid_plan_output", captured("plan-index-table.json", "validate-index-table.txt")},
		{"a key the database refuses, beside another", "observe", "child_exit", captured("drift-other-and-key.json", "validate-other-and-key.txt")},
		// Ptah v0.23.0 refuses these plans as declaration-refused rather than
		// failing them, and a refusal the file does not explain keeps its code.
		{"a key the database refuses, beside another", "plan", "plan_refused", captured("plan-other-and-key.json", "validate-other-and-key.txt")},
		{"a key the database refuses, beside an index", "observe", "child_exit", captured("drift-other-and-index.json", "validate-other-and-index.txt")},
		{"a key the database refuses, beside an index", "plan", "plan_refused", captured("plan-other-and-index.json", "validate-other-and-index.txt")},
		{"a document that does not decode", "observe", "child_exit", func(t *testing.T) (scriptedResponse, scriptedResponse) {
			return scriptedResponse{stdout: "error comparing schemas: invalid foreign key", exitCode: 2}, foreignKey(t)
		}},
		{"a failed plan with no error", "plan", "invalid_plan_output", func(t *testing.T) (scriptedResponse, scriptedResponse) {
			return scriptedResponse{stdout: `{"contract_version":3,"command":"schema plan","outcome":"failed"}`, exitCode: 2}, foreignKey(t)
		}},
	} {
		t.Run(test.name+"/"+test.operation, func(t *testing.T) {
			t.Parallel()
			failed, validated := test.answers(t)
			result, executor, diagnostics := runDesiredSource(t, desiredSourceRuns()[test.operation], failed, validated)
			if result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("Run() error = %#v, want code %s", result.Error, test.code)
			}
			if len(executor.calls) != 2 {
				t.Fatalf("ran %d children, want the failed command and its check", len(executor.calls))
			}
			for _, word := range []string{"invalid foreign key", "names table", "names column", "holds schema"} {
				if strings.Contains(result.Error.Message, word) || strings.Contains(diagnostics, word) {
					t.Fatalf("the runner repeated %q: %q\n%s", word, result.Error.Message, diagnostics)
				}
			}
		})
	}
}

// TestATargetFailureAsksNoQuestion holds the order: a kind says why, so nothing
// else runs. The scripted executor fails on a second child.
func TestATargetFailureAsksNoQuestion(t *testing.T) {
	t.Parallel()

	failure := `{"kind":"database_unreachable","message":"` + childText + `"}`
	for name, stdout := range map[string]string{
		"observe":           `{"contract_version":1,"command":"schema drift","error":` + failure + `}`,
		"plan":              `{"contract_version":3,"command":"schema plan","outcome":"failed","error":` + failure + `}`,
		"migration history": `{"contract_version":2,"command":"migrations status","error":` + failure + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, executor, _ := runDesiredSource(t, desiredSourceRuns()[name], scriptedResponse{stdout: stdout, exitCode: 2})
			if result.Error == nil || result.Error.Code != "database_unreachable" || len(executor.calls) != 1 {
				t.Fatalf("Run() error = %#v after %d children, want the kind after one", result.Error, len(executor.calls))
			}
		})
	}
}

// TestNoCheckRunsWithoutItsInputs holds the two values a schema check cannot do
// without. The executor fails on a second child, so an empty environment, which
// exec would read as the runner's own, is never reached.
func TestNoCheckRunsWithoutItsInputs(t *testing.T) {
	t.Parallel()

	without := func(environment []string, key string) []string {
		return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, key+"=") })
	}
	// A migration has no input the check could lack: a run with no directory is
	// refused before the command it would explain, so no child runs at all.
	t.Run("no migration directory", func(t *testing.T) {
		t.Parallel()
		stripped := desiredSourceRuns()["migration history"]
		stripped.environment = without(stripped.environment, envMigrationsDir)
		result, executor, _ := runDesiredSource(t, stripped)
		if result.Error == nil || result.Error.Code != "invalid_input" || len(executor.calls) != 0 {
			t.Fatalf("Run() error = %#v after %d children, want invalid_input after none", result.Error, len(executor.calls))
		}
	})
	run := desiredSourceRuns()["observe"]
	for name, key := range map[string]string{"no schema file": envSchemaFile, "no engine": envExpectedDatabaseEngine} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stripped := run
			stripped.environment = without(run.environment, key)
			result, executor, _ := runDesiredSource(t, stripped, run.failed(t))
			if result.Error == nil || result.Error.Code != "child_exit" || len(executor.calls) != 1 {
				t.Fatalf("Run() error = %#v after %d children, want child_exit after one", result.Error, len(executor.calls))
			}
		})
	}
}

// deadlineRecordingExecutor answers the first child with failed and records the
// deadline the second one was given.
type deadlineRecordingExecutor struct {
	failed scriptedResponse
	calls  int
	// startedAt is when the check started, which is what its deadline is
	// measured from.
	startedAt time.Time
	deadline  time.Time
	bounded   bool
}

func (e *deadlineRecordingExecutor) Execute(ctx context.Context, _ CommandSpec, stdout, _ io.Writer) (int, error) {
	e.calls++
	if e.calls == 1 {
		_, _ = io.WriteString(stdout, e.failed.stdout)
		return e.failed.exitCode, nil
	}
	e.startedAt = time.Now()
	e.deadline, e.bounded = ctx.Deadline()
	return 0, nil
}

func TestACheckIsBoundedInTime(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["observe"]
	executor := &deadlineRecordingExecutor{failed: run.failed(t)}
	result := Run(context.Background(), Config{
		Operation: run.operation, Environment: withRunnerProtocol(run.environment), Executor: executor, TempDir: t.TempDir(),
	})
	if result.Error == nil || result.Error.Code != "child_exit" || executor.calls != 2 {
		t.Fatalf("Run() error = %#v after %d children", result.Error, executor.calls)
	}
	// The bound is written out, so a change to the constant has to change this
	// too. The deadline was computed just before the check started, so what
	// remains is at most the bound and only slightly under it.
	const bound, slack = 30 * time.Second, 5 * time.Second
	remaining := executor.deadline.Sub(executor.startedAt)
	if !executor.bounded || remaining > bound || remaining < bound-slack {
		t.Fatalf("the check had %s left when it started (deadline set: %v), want about %s", remaining, executor.bounded, bound)
	}
}

// cancelingExecutor answers the first child with failed, ends the run's context
// in the second, and records what the check's own context says then.
type cancelingExecutor struct {
	failed scriptedResponse
	cancel context.CancelFunc
	calls  int
	err    error
}

func (e *cancelingExecutor) Execute(ctx context.Context, _ CommandSpec, stdout, _ io.Writer) (int, error) {
	e.calls++
	if e.calls == 1 {
		_, _ = io.WriteString(stdout, e.failed.stdout)
		return e.failed.exitCode, nil
	}
	e.cancel()
	e.err = ctx.Err()
	return 0, e.err
}

// TestACheckEndsWithThePod ends the run's context while the check runs. A check
// on a context of its own would keep a Pod that is being terminated.
func TestACheckEndsWithThePod(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := desiredSourceRuns()["observe"]
	executor := &cancelingExecutor{failed: run.failed(t), cancel: cancel}
	result := Run(ctx, Config{
		Operation: run.operation, Environment: withRunnerProtocol(run.environment), Executor: executor, TempDir: t.TempDir(),
	})
	if executor.calls != 2 || !errors.Is(executor.err, context.Canceled) {
		t.Fatalf("after %d children the check's context says %v, want it canceled with the run's", executor.calls, executor.err)
	}
	if result.Error == nil || result.Error.Code != "child_exit" {
		t.Fatalf("Run() error = %#v, want the failure it had", result.Error)
	}
}

// TestAReasonIsRedactedAndBounded quotes the database password from the Pod's
// environment, which the child never held, and a reason longer than the frame
// keeps, cut in the middle of a character.
func TestAReasonIsRedactedAndBounded(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["observe"]
	source := func(reason string) scriptedResponse {
		return scriptedResponse{stdout: "postgres: source: " + reason + "\n", exitCode: 1}
	}
	// Ptah prints the token it stopped at in capitals, so that is how a file
	// quotes the password: validate-sql-folded.txt is the capture.
	t.Run("a credential in the file", func(t *testing.T) {
		t.Parallel()
		result, _, diagnostics := runDesiredSource(t, run, run.failed(t), source("read SQL schema file "+checkedSchemaFile+":1:1: unsupported SQL statement: SECRET"))
		want := "read SQL schema file " + checkedSchemaFile + ":1:1: unsupported SQL statement: " + RedactionMarker
		if result.Error == nil || result.Error.Message != want {
			t.Fatalf("Run() error = %#v, want %q", result.Error, want)
		}
		if strings.Contains(strings.ToLower(diagnostics), "secret") {
			t.Fatalf("the Pod log carries the password:\n%s", diagnostics)
		}
	})
	// A fault's words come from the same child, whatever the failed command said
	// beside them, and go through the same function.
	t.Run("a credential in a fault", func(t *testing.T) {
		t.Parallel()
		const fault = `invalid foreign key: field "id" references unknown table "secret"`
		failed := scriptedResponse{stdout: `{"contract_version":1,"command":"schema drift","error":{"message":"error comparing schemas: invalid foreign key: ` +
			`field \"id\" references unknown table \"secret\""}}`, exitCode: 2}
		result, _, diagnostics := runDesiredSource(t, run, failed, scriptedResponse{stdout: "postgres: schema: " + fault + "\n", exitCode: 1})
		want := `schema.sql: invalid foreign key: field "id" references unknown table "` + RedactionMarker + `"`
		if result.Error == nil || result.Error.Code != CodeDesiredSchemaInvalid || result.Error.Message != want {
			t.Fatalf("Run() error = %#v, want %q", result.Error, want)
		}
		if strings.Contains(diagnostics, "secret") {
			t.Fatalf("the Pod log carries the password:\n%s", diagnostics)
		}
	})
	t.Run("the capture of a credential in capitals", func(t *testing.T) {
		t.Parallel()
		folded := run
		folded.environment = append(slices.Clone(run.environment), EnvDatabaseURL+"=postgres://app:Hunter2Hunter2@db.example/orders")
		result, _, _ := runDesiredSource(t, folded, run.failed(t), scriptedResponse{stdout: desiredSourceOutput(t, "validate-sql-folded.txt"), exitCode: 1})
		if result.Error == nil || result.Error.Code != CodeDesiredSourceUnreadable || strings.Contains(result.Error.Message, "HUNTER2") {
			t.Fatalf("Run() error = %#v, want the password redacted", result.Error)
		}
	})
	// The bound falls inside the password. Redacting only after the cut would
	// leave its first bytes, which no longer match it.
	t.Run("a credential at the bound", func(t *testing.T) {
		t.Parallel()
		reason := strings.Repeat("x", maxDesiredSourceReasonBytes-2) + "secret"
		result, _, _ := runDesiredSource(t, run, run.failed(t), source(reason))
		if result.Error == nil || strings.HasSuffix(result.Error.Message, "se") {
			t.Fatalf("Run() error = %#v, which keeps the start of the password", result.Error)
		}
	})
	t.Run("a long reason", func(t *testing.T) {
		t.Parallel()
		// The whole sentence is what the Event keeps 512 bytes of, so a reason
		// at the bound reads the same in the condition and the Event.
		if whole := len(CodeDesiredSourceUnreadable+": ") + maxDesiredSourceReasonBytes; whole != 512 {
			t.Fatalf("the code and the longest reason take %d bytes, want the 512 an Event keeps", whole)
		}
		for _, code := range []string{CodeDesiredSourceUnreadable, CodeDesiredSchemaInvalid} {
			if whole := len(code+": ") + maxDesiredSourceReasonBytes; whole > 512 {
				t.Fatalf("%s and the longest reason take %d bytes, past the 512 an Event keeps", code, whole)
			}
		}
		result, _, _ := runDesiredSource(t, run, run.failed(t), source(strings.Repeat("x", maxDesiredSourceReasonBytes-1)+"é and more"))
		if result.Error == nil || len(result.Error.Message) > maxDesiredSourceReasonBytes ||
			!utf8.ValidString(result.Error.Message) || strings.Contains(result.Error.Message, "more") {
			t.Fatalf("Run() error = %#v, want at most %d bytes of valid text", result.Error, maxDesiredSourceReasonBytes)
		}
	})
}

// TestAReasonNamesTheSchemaFile prints what a YAML parser prints, which names no
// file, and what a SQL parser prints, which does. The name is added once, and only
// where Ptah's words leave it out.
func TestAReasonNamesTheSchemaFile(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["observe"]
	for fixture, want := range map[string]string{
		"validate-yaml.txt": "schema.sql: error parsing schema file: parse YAML schema: yaml: line 1: did not find expected '-' indicator",
		"validate-yaml-value.txt": "schema.sql: error parsing schema file: parse YAML schema: yaml: unmarshal errors: " +
			"line 1: cannot unmarshal !!str `hunter2...` into map[string]yamlschema.tableSpec",
		"validate-sql.txt": inheritsReason,
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			result, _, _ := runDesiredSource(t, run, run.failed(t), scriptedResponse{stdout: desiredSourceOutput(t, fixture), exitCode: 1})
			if result.Error == nil || result.Error.Code != CodeDesiredSourceUnreadable || result.Error.Message != want {
				t.Fatalf("Run() error = %#v, want %q", result.Error, want)
			}
		})
	}
}

// TestAFileNameCannotReachAPersonsTerminal delivers the document a migration
// directory printed through a registry, with a file name that sets a terminal's
// title. None of it may reach the frame or the Pod log.
func TestAFileNameCannotReachAPersonsTerminal(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["migration history"]
	result, _, diagnostics := runDesiredSource(t, run, run.failed(t), scriptedResponse{stdout: desiredSourceOutput(t, "scratch-control-bytes.json"), exitCode: 2})
	const want = "failed to load up migration 0000000001_a ]0;pwn .up.sql: invalid migration directives in " +
		`0000000001_a ]0;pwn .up.sql: invalid +ptah no_transaction value "sometimes": expected true or false`
	if result.Error == nil || result.Error.Code != CodeDesiredSourceUnreadable || result.Error.Message != want {
		t.Fatalf("Run() error = %#v, want %q", result.Error, want)
	}
	if strings.ContainsAny(diagnostics, "\x1b\x07") {
		t.Fatalf("the Pod log carries control bytes: %q", diagnostics)
	}
}

// TestAReasonWithNothingReadableKeepsTheCode leaves the failure as it was, since
// a frame's error needs a message and this reason has none to give.
func TestAReasonWithNothingReadableKeepsTheCode(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["migration history"]
	document := `{"contract_version":2,"command":"migrations status","error":{"message":"error registering migrations: \u001b\u0007"}}`
	result, _, _ := runDesiredSource(t, run, run.failed(t), scriptedResponse{stdout: document, exitCode: 2})
	if result.Error == nil || result.Error.Code != run.unclassified {
		t.Fatalf("Run() error = %#v, want code %s", result.Error, run.unclassified)
	}
}
