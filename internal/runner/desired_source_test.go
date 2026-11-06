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
	partitionReason   = "error parsing schema file: read SQL schema file /work/schema.sql: " +
		"expected '(' after table name: expected Operator, got Identifier at position 180"
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
			reason:       partitionReason,
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
			reason:       partitionReason,
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
	failed   scriptedResponse
	calls    int
	deadline time.Time
	bounded  bool
}

func (e *deadlineRecordingExecutor) Execute(ctx context.Context, _ CommandSpec, stdout, _ io.Writer) (int, error) {
	e.calls++
	if e.calls == 1 {
		_, _ = io.WriteString(stdout, e.failed.stdout)
		return e.failed.exitCode, nil
	}
	e.deadline, e.bounded = ctx.Deadline()
	return 0, nil
}

func TestACheckIsBoundedInTime(t *testing.T) {
	t.Parallel()

	run := desiredSourceRuns()["observe"]
	executor := &deadlineRecordingExecutor{failed: run.failed(t)}
	started := time.Now()
	result := Run(context.Background(), Config{
		Operation: run.operation, Environment: withRunnerProtocol(run.environment), Executor: executor, TempDir: t.TempDir(),
	})
	if result.Error == nil || result.Error.Code != "child_exit" || executor.calls != 2 {
		t.Fatalf("Run() error = %#v after %d children", result.Error, executor.calls)
	}
	if !executor.bounded || executor.deadline.After(started.Add(desiredSourceCheckTimeout+time.Minute)) {
		t.Fatalf("the check ran with deadline %v (set: %v), want one within %s", executor.deadline, executor.bounded, desiredSourceCheckTimeout)
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
	t.Run("a credential in the file", func(t *testing.T) {
		t.Parallel()
		result, _, diagnostics := runDesiredSource(t, run, run.failed(t), source("unsupported SQL statement: secret at position 0"))
		want := "unsupported SQL statement: " + RedactionMarker + " at position 0"
		if result.Error == nil || result.Error.Message != want {
			t.Fatalf("Run() error = %#v, want %q", result.Error, want)
		}
		if strings.Contains(diagnostics, "secret") {
			t.Fatalf("the Pod log carries the password:\n%s", diagnostics)
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
		result, _, _ := runDesiredSource(t, run, run.failed(t), source(strings.Repeat("x", maxDesiredSourceReasonBytes-1)+"é and more"))
		if result.Error == nil || len(result.Error.Message) > maxDesiredSourceReasonBytes ||
			!utf8.ValidString(result.Error.Message) || strings.Contains(result.Error.Message, "more") {
			t.Fatalf("Run() error = %#v, want at most %d bytes of valid text", result.Error, maxDesiredSourceReasonBytes)
		}
	})
}
