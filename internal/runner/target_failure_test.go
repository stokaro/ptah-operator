package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// childText stands for what Ptah and the database write about a failure: the
// host, the role and the database a connection named. A result frame, its
// termination summary and the runner's own log carry none of it.
const childText = "CHILD-TEXT failed to connect to `user=app_owner database=orders`: db.internal.example:5432"

// targetFailureError is an error object as Ptah writes one, with the kind
// given as raw JSON (or nothing) and the child's own sentence beside it.
func targetFailureError(kind string) string {
	if kind == "" {
		return `{"message":"` + childText + `"}`
	}
	return `{"kind":` + kind + `,"message":"` + childText + `"}`
}

// targetFailureRun is one operation that opens the target, with the documents
// its child prints when the target cannot be opened and the error the runner
// reports when that document names no kind it knows.
type targetFailureRun struct {
	operation Operation
	// responses are what the executor answers, given the error object the
	// failing child's document carries and the status it exits with.
	responses func(t *testing.T, failure string, exit int) []scriptedResponse
	// environment is the Job environment the operation runs under.
	environment func(t *testing.T) []string
	// unclassified is the code a failure with no kind this runner knows gets.
	unclassified string
}

func targetFailureRuns() map[string]targetFailureRun {
	plan := validPlanDocument("CREATE TABLE target_failure (id bigint)")
	return map[string]targetFailureRun{
		"observe": {
			operation: OperationObserve,
			responses: func(_ *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{{
					stdout:   `{"contract_version":1,"command":"schema drift","error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment:  func(*testing.T) []string { return databaseEnvironment("target-failure-observe") },
			unclassified: "child_exit",
		},
		"plan": {
			operation: OperationPlan,
			responses: func(_ *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{{
					stdout:   `{"contract_version":3,"command":"schema plan","outcome":"failed","error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment:  func(*testing.T) []string { return databaseEnvironment("target-failure-plan") },
			unclassified: "invalid_plan_output",
		},
		"plan validation": {
			operation: OperationPlan,
			responses: func(t *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{planResponse(plan), planResponse(plan), {
					stdout: `{"contract_version":4,"command":"schema apply","outcome":"failed","plan_name":"operator-plan",` +
						`"plan_digest":"` + sha256Digest([]byte(plan)) + `","error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment:  func(*testing.T) []string { return databaseEnvironment("target-failure-plan-validation") },
			unclassified: "invalid_plan_output",
		},
		"apply": {
			operation: OperationApply,
			responses: func(_ *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{{
					stdout: `{"contract_version":4,"command":"schema apply","outcome":"failed","plan_name":"operator-plan",` +
						`"plan_digest":"` + sha256Digest([]byte(plan)) + `","error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment: func(t *testing.T) []string {
				planDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte(plan), 0o600); err != nil {
					t.Fatal(err)
				}
				return append(databaseEnvironment("target-failure-apply"),
					envPlanDir+"="+planDir,
					envExpectedPlanDigest+"="+sha256Digest([]byte(plan)),
					envExpectedCoordination+"="+testCoordinationDigest(),
					envExpectedTargetDigest+"="+databaseTargetDigest(t),
				)
			},
			unclassified: "apply_failed",
		},
		"migration history": {
			operation: OperationMigrationHistory,
			responses: func(_ *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{{
					stdout:   `{"contract_version":2,"command":"migrations status","error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment:  func(*testing.T) []string { return migrationEnvironment("target-failure-history") },
			unclassified: "child_exit",
		},
		"migration apply": {
			operation: OperationMigrationApply,
			responses: func(_ *testing.T, failure string, exit int) []scriptedResponse {
				return []scriptedResponse{{
					stdout: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed",` +
						`"error":` + failure + `}`,
					stderr:   "error: " + childText + "\n",
					exitCode: exit,
				}}
			},
			environment:  func(t *testing.T) []string { return migrationApplyEnvironment(t, "target-failure-migration-apply") },
			unclassified: "child_exit",
		},
	}
}

// runTargetFailure runs one operation against the scripted child, and returns
// the result with everything the runner wrote about it: the frame, the
// termination summary and its own log.
func runTargetFailure(t *testing.T, run targetFailureRun, failure string, exit int) (Result, string) {
	t.Helper()
	diagnostics := &bytes.Buffer{}
	result := Run(context.Background(), Config{
		Operation:   run.operation,
		Environment: withRunnerProtocol(run.environment(t)),
		Executor:    &scriptedExecutor{t: t, responses: run.responses(t, failure, exit)},
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
	return result, string(encoded.Frame) + "\n" + string(encoded.Summary) + "\n" + diagnostics.String()
}

// TestATargetThatCannotBeOpenedIsReportedByItsKind holds each kind, on every
// operation that opens the target, to the code and the sentence it produces,
// and holds what the runner wrote to none of what the child did.
func TestATargetThatCannotBeOpenedIsReportedByItsKind(t *testing.T) {
	t.Parallel()

	for name, run := range targetFailureRuns() {
		for _, kind := range dataplane.TargetFailureKinds() {
			t.Run(name+"/"+string(kind), func(t *testing.T) {
				t.Parallel()
				result, written := runTargetFailure(t, run, targetFailureError(`"`+string(kind)+`"`), 2)
				if result.Error == nil || result.Error.Code != string(kind) || result.Error.Message != kind.Description() {
					t.Fatalf("Run() error = %#v, want code %s and its sentence", result.Error, kind)
				}
				if result.ChildExitCode != 2 {
					t.Fatalf("ChildExitCode = %d, want the 2 the child exited with", result.ChildExitCode)
				}
				if strings.Contains(written, "CHILD-TEXT") || strings.Contains(written, "db.internal.example") {
					t.Fatalf("the runner wrote what the child did:\n%s", written)
				}
			})
		}
	}
}

// TestAFailureWithNoKnownKindKeepsItsCode holds a document that names no kind,
// or one this runner has no sentence for, to the code the operation reported
// before kinds existed. A kind outside the vocabulary is child text like the
// message beside it, so it is not repeated either.
func TestAFailureWithNoKnownKindKeepsItsCode(t *testing.T) {
	t.Parallel()

	for name, run := range targetFailureRuns() {
		for label, kind := range map[string]string{
			"no kind":                  "",
			"a kind added later":       `"lock_timeout"`,
			"a kind in capitals":       `"DATABASE_UNREACHABLE"`,
			"a kind with a space":      `"database_unreachable "`,
			"a kind carrying text":     `"database_unreachable: CHILD-TEXT db.internal.example"`,
			"a kind that is a number":  `7`,
			"a kind that is an object": `{"name":"CHILD-TEXT"}`,
		} {
			t.Run(name+"/"+label, func(t *testing.T) {
				t.Parallel()
				result, written := runTargetFailure(t, run, targetFailureError(kind), 2)
				if result.Error == nil {
					t.Fatalf("Run() = %#v, want a failure", result)
				}
				if _, known := dataplane.ParseTargetFailureKind(result.Error.Code); known {
					t.Fatalf("Run() error = %#v, want no kind", result.Error)
				}
				// A kind of the wrong JSON type fails the document as a
				// whole, which is a refusal of its own rather than the
				// unclassified failure.
				if kind != "7" && kind != `{"name":"CHILD-TEXT"}` && result.Error.Code != run.unclassified {
					t.Fatalf("Run() error code = %q, want %q", result.Error.Code, run.unclassified)
				}
				if strings.Contains(written, "CHILD-TEXT") || strings.Contains(written, "db.internal.example") {
					t.Fatalf("the runner wrote what the child did:\n%s", written)
				}
			})
		}
	}
}

// TestAKindIsReadOnlyFromAFailureItDescribes holds the classifier to the
// documents it reads a kind from. Each row carries a known kind where the
// runner must not read it: beside a child that exited zero, on a run that
// selected migrations, on a document of another command or contract, and from
// a child this process stopped.
func TestAKindIsReadOnlyFromAFailureItDescribes(t *testing.T) {
	t.Parallel()

	runs := targetFailureRuns()
	unreachable := targetFailureError(`"database_unreachable"`)
	for _, test := range []struct {
		name      string
		run       targetFailureRun
		responses []scriptedResponse
		want      string
	}{
		{
			name: "a failed plan that exited zero",
			run:  runs["plan"],
			responses: []scriptedResponse{{
				stdout: `{"contract_version":3,"command":"schema plan","outcome":"failed","error":` + unreachable + `}`,
			}},
			want: "invalid_plan_output",
		},
		{
			name: "a refused plan",
			run:  runs["plan"],
			responses: []scriptedResponse{{
				stdout: `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"quota-exceeded"},` +
					`"error":` + unreachable + `}`,
				exitCode: 2,
			}},
			want: "plan_refused",
		},
		{
			name: "a failed apply that exited zero",
			run:  runs["apply"],
			responses: []scriptedResponse{{
				stdout: `{"contract_version":4,"command":"schema apply","outcome":"failed","error":` + unreachable + `}`,
			}},
			want: "apply_failed",
		},
		{
			name: "an apply whose outcome nobody knows",
			run:  runs["apply"],
			responses: []scriptedResponse{{
				stdout:   `{"contract_version":4,"command":"schema apply","outcome":"unknown","error":` + unreachable + `}`,
				exitCode: 2,
			}},
			want: "apply_outcome_unknown",
		},
		{
			name: "a drift failure document beside the drift exit status",
			run:  runs["observe"],
			responses: []scriptedResponse{{
				stdout:   `{"contract_version":1,"command":"schema drift","error":` + unreachable + `}`,
				exitCode: 1,
			}},
			want: "invalid_observed_state",
		},
		{
			name: "a drift failure document of another contract version",
			run:  runs["observe"],
			responses: []scriptedResponse{{
				stdout:   `{"contract_version":2,"command":"schema drift","error":` + unreachable + `}`,
				exitCode: 2,
			}},
			want: "child_exit",
		},
		{
			name: "another command's failure document at an Observe",
			run:  runs["observe"],
			responses: []scriptedResponse{{
				stdout:   `{"contract_version":1,"command":"migrations status","error":` + unreachable + `}`,
				exitCode: 2,
			}},
			want: "child_exit",
		},
		{
			name: "a status failure document from a child that exited zero",
			run:  runs["migration history"],
			responses: []scriptedResponse{{
				stdout: `{"contract_version":2,"command":"migrations status","error":` + unreachable + `}`,
			}},
			want: "invalid_migration_history_output",
		},
		{
			name: "a status failure document from a child stopped at its deadline",
			run:  runs["migration history"],
			responses: []scriptedResponse{{
				stdout:   `{"contract_version":2,"command":"migrations status","error":` + unreachable + `}`,
				exitCode: -1, err: context.DeadlineExceeded,
			}},
			want: "execution_error",
		},
		{
			name: "a migration run that selected and read the history",
			run:  runs["migration apply"],
			responses: []scriptedResponse{{
				stdout: `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","error":` + unreachable +
					`,"status":{"contract_version":2,"current_version":2,"total_migrations":4,"has_pending_changes":true}}`,
				exitCode: 2,
			}},
			want: "child_exit",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			run := test.run
			run.responses = func(*testing.T, string, int) []scriptedResponse { return test.responses }
			result, written := runTargetFailure(t, run, "", 0)
			if result.Error == nil || result.Error.Code != test.want {
				t.Fatalf("Run() error = %#v, want %s", result.Error, test.want)
			}
			if strings.Contains(written, "CHILD-TEXT") {
				t.Fatalf("the runner wrote what the child did:\n%s", written)
			}
		})
	}
}

// TestAMigrationRunThatNeverSelectedKeepsItsReport carries the run document of
// a target that could not be opened to the controller, with its kind in the
// result's code and its error object dropped: the report is what says nothing
// was selected and nothing applied.
func TestAMigrationRunThatNeverSelectedKeepsItsReport(t *testing.T) {
	t.Parallel()

	result, written := runTargetFailure(t, targetFailureRuns()["migration apply"],
		targetFailureError(`"authentication_failed"`), 2)
	if result.Error == nil || result.Error.Code != string(dataplane.TargetAuthenticationFailed) {
		t.Fatalf("Run() error = %#v", result.Error)
	}
	run := result.MigrationRun
	if run == nil || !run.StoppedBeforeSelecting() || run.Error != nil || len(run.Planned) != 0 || len(run.Applied) != 0 {
		t.Fatalf("MigrationRun = %+v, want the report of a run that selected nothing, with no error object", run)
	}
	if strings.Contains(written, "CHILD-TEXT") {
		t.Fatalf("the runner wrote what the child did:\n%s", written)
	}
}
