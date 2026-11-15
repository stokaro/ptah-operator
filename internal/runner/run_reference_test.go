package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// reportedRunReference is the run reference a document reports for the log
// entries its run wrote, or "" when it reports none. A capture is the account
// of a run the runner harness gave its row's operation ID, so a replay runs
// under that ID: the runner refuses an account of entries written under any
// other.
func reportedRunReference(document string) string {
	var reported struct {
		ApplyLog     *dataplane.RunLogReference `json:"apply_log"`
		MigrationLog *dataplane.RunLogReference `json:"migration_log"`
	}
	if json.Unmarshal([]byte(document), &reported) != nil {
		return ""
	}
	for _, log := range []*dataplane.RunLogReference{reported.ApplyLog, reported.MigrationLog} {
		if log != nil && log.RunReference != "" {
			return log.RunReference
		}
	}
	return ""
}

// capturedOperationID is the operation ID a captured document was written
// under. A capture that reports none was not taken from a run given one.
func capturedOperationID(t *testing.T, document string) string {
	t.Helper()
	reference := reportedRunReference(document)
	if reference == "" {
		t.Fatal("the captured document reports no run reference, so it was not captured from an Apply given one")
	}
	return reference
}

// withOperationID replaces the operation ID of a test environment.
func withOperationID(t *testing.T, environment []string, operationID string) []string {
	t.Helper()
	replaced := slices.Clone(environment)
	index := slices.IndexFunc(replaced, func(entry string) bool { return strings.HasPrefix(entry, envOperationID+"=") })
	if index < 0 {
		t.Fatal("the environment carries no operation ID to replace")
	}
	replaced[index] = envOperationID + "=" + operationID
	return replaced
}

// runReferenceArguments lists the arguments of a call that pass Ptah a run
// reference, in whichever form Ptah reads.
func runReferenceArguments(spec CommandSpec) []string {
	var found []string
	for _, argument := range spec.Args {
		if argument == "--run-reference" || strings.HasPrefix(argument, "--run-reference=") {
			found = append(found, argument)
		}
	}
	return found
}

// testOperationID is shaped as the manager's are: a SHA-256 digest.
var testOperationID = "sha256:" + strings.Repeat("0a", 32)

func TestWithRunReferenceHoldsTheOperationIDToWhatPtahStores(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name, operationID string
		refused           bool
	}{
		{name: "the manager's digest", operationID: testOperationID},
		{name: "the lowest visible character", operationID: "!"},
		{name: "the widest Ptah stores", operationID: strings.Repeat("~", maxRunReferenceBytes)},
		// One argument carries the flag and the value, so a leading dash is a
		// value and not a flag.
		{name: "a value that starts with a dash", operationID: "--actor=forged"},
		{name: "none", operationID: "", refused: true},
		{name: "one byte wider than Ptah stores", operationID: strings.Repeat("a", maxRunReferenceBytes+1), refused: true},
		{name: "a space", operationID: "a b", refused: true},
		{name: "a tab", operationID: "a\tb", refused: true},
		{name: "a line break", operationID: "a\nb", refused: true},
		{name: "DEL", operationID: "a\x7fb", refused: true},
		{name: "a character outside ASCII", operationID: "José", refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			// Spare capacity, so an append that reused it would write into the
			// caller's array.
			args := make([]string, 2, 8)
			copy(args, []string{"migrations", "up"})
			spec, err := withRunReference(CommandSpec{Args: args}, row.operationID)
			if row.refused {
				if err == nil {
					t.Fatalf("withRunReference(%q) = %v, want a refusal", row.operationID, spec.Args)
				}
				if strings.Contains(err.Error(), row.operationID) && row.operationID != "" {
					t.Fatalf("the refusal quotes the operation ID: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("withRunReference(%q) error = %v", row.operationID, err)
			}
			if want := []string{"migrations", "up", "--run-reference=" + row.operationID}; !slices.Equal(spec.Args, want) {
				t.Fatalf("args = %q, want %q", spec.Args, want)
			}
			if spare := args[:3][2]; spare != "" {
				t.Fatalf("withRunReference wrote %q into the caller's arguments", spare)
			}
		})
	}
}

// An Apply's child, and only an Apply's, is told the claim's operation ID as
// its run reference, once. The other operations write no log entry, and every
// call they make runs without one. A PTAH_RUN_REFERENCE the Pod carries
// reaches no child, so the argument is the only value Ptah can read.
func TestOnlyAnApplyIsGivenItsOperationIDAsTheRunReference(t *testing.T) {
	t.Parallel()

	const forged = "forged-reference"
	for name, run := range targetFailureRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			environment := append(withOperationID(t, run.environment(t), testOperationID), ptahRunReferenceEnvironment+"="+forged)
			executor := &scriptedExecutor{t: t, responses: run.responses(t, targetFailureError(""), 2)}
			Run(context.Background(), Config{
				Operation:   run.operation,
				Environment: withRunnerProtocol(environment),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if len(executor.calls) == 0 {
				t.Fatal("the operation started no child, so this row proves nothing about what a child is given")
			}
			for index, call := range executor.calls {
				got := runReferenceArguments(call)
				want := []string(nil)
				if run.operation.Mutating() {
					want = []string{"--run-reference=" + testOperationID}
				}
				if !slices.Equal(got, want) {
					t.Errorf("call %d (%s) is given run reference arguments %q, want %q", index, strings.Join(call.Args[:2], " "), got, want)
				}
				for _, variable := range call.Env {
					if strings.HasPrefix(variable, ptahRunReferenceEnvironment+"=") || strings.Contains(variable, forged) {
						t.Errorf("call %d inherits %s", index, variable)
					}
				}
			}
		})
	}
}

// An operation ID Ptah would not store as given stops an Apply before its
// child runs, so no statement is sent under a reference the log would not
// carry.
func TestAnOperationIDThatCannotBeARunReferenceStopsTheApplyBeforeItRuns(t *testing.T) {
	t.Parallel()

	for _, family := range []string{"apply", "migration apply"} {
		t.Run(family, func(t *testing.T) {
			t.Parallel()
			run := targetFailureRuns()[family]
			executor := &scriptedExecutor{t: t, responses: run.responses(t, targetFailureError(""), 2)}
			result := Run(context.Background(), Config{
				Operation:   run.operation,
				Environment: withRunnerProtocol(withOperationID(t, run.environment(t), "operation id")),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != "invalid_input" {
				t.Fatalf("Run() error = %#v, want invalid_input", result.Error)
			}
			if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
				t.Fatalf("calls %d, MutationStarted %t, Uncertain %t: want the Apply refused before it started",
					len(executor.calls), result.MutationStarted, result.Uncertain)
			}
		})
	}
}

// loggedApplyReport is an applied schema apply document whose log report is
// the one given.
func loggedApplyReport(t *testing.T, plan string, log *dataplane.RunLogReference) string {
	t.Helper()
	decoded := mustDecodePlan(t, plan)
	return encodeApplyReport(t, dataplane.SchemaApplyReport{
		ContractVersion: dataplane.SupportedSchemaApplyContract,
		Outcome:         dataplane.SchemaApplyOutcomeApplied,
		PlanName:        decoded.Name,
		PlanDigest:      sha256Digest([]byte(plan)),
		Units:           sqlUnits(planStatements(decoded)...),
		ApplyLog:        log,
	})
}

// runReferenceRows are the log reports a run can come back with, judged
// against the run's own operation ID.
var runReferenceRows = []struct {
	name    string
	log     string
	refused bool
}{
	{name: "entries under the operation's reference", log: `{"written":2,"run_reference":"` + testOperationID + `"}`},
	{name: "no entry written and none named", log: `{"written":0}`},
	{name: "no log report", log: ""},
	{name: "entries under another reference", log: `{"written":2,"run_reference":"sha256:` + strings.Repeat("0b", 32) + `"}`, refused: true},
	{name: "entries under a reference that differs in case", log: `{"written":2,"run_reference":"` + strings.ToUpper(testOperationID) + `"}`, refused: true},
	{name: "entries under no reference", log: `{"written":2}`, refused: true},
	{name: "no entry and another reference", log: `{"written":0,"run_reference":"another-run"}`, refused: true},
}

// A schema apply document that accounts for log entries under another
// reference, or under none, is not this run's account, and settles nothing:
// the Apply stays a started, uncertain mutation, whatever outcome the document
// claims.
func TestASchemaApplyIsHeldToTheRunReferenceItWasGiven(t *testing.T) {
	t.Parallel()

	for _, row := range runReferenceRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var log *dataplane.RunLogReference
			if row.log != "" {
				log = &dataplane.RunLogReference{}
				if err := json.Unmarshal([]byte(row.log), log); err != nil {
					t.Fatal(err)
				}
			}
			run := targetFailureRuns()["apply"]
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: loggedApplyReport(t, targetFailurePlan, log)}}}
			result := Run(context.Background(), Config{
				Operation:   OperationApply,
				Environment: withRunnerProtocol(withOperationID(t, run.environment(t), testOperationID)),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if !row.refused {
				if result.Error != nil || !result.MutationStarted || result.Uncertain {
					t.Fatalf("Run() = %#v, want the applied run the child reported", result)
				}
				return
			}
			if result.Error == nil || result.Error.Code != "invalid_apply_output" || !result.MutationStarted || !result.Uncertain {
				t.Fatalf("Run() error %#v, MutationStarted %t, Uncertain %t: want invalid_apply_output on an uncertain mutation",
					result.Error, result.MutationStarted, result.Uncertain)
			}
			if strings.Contains(result.Error.Message, "another-run") || strings.Contains(result.Error.Message, "0b0b") {
				t.Fatalf("the refusal quotes the reported reference: %s", result.Error.Message)
			}
			requireFrameParses(t, result)
		})
	}
}

// The same holds for a migration run, whose report the frame then leaves out:
// the manager reads a started, uncertain mutation, as it does for a child
// whose output it could not read, and resolves the run from the revision
// table. A refusal never replaces the code a run that exited nonzero already
// carries.
func TestAMigrationRunIsHeldToTheRunReferenceItWasGiven(t *testing.T) {
	t.Parallel()

	for _, row := range runReferenceRows {
		for _, exit := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit %d", row.name, exit), func(t *testing.T) {
				t.Parallel()
				outcome, rest := "applied", `"applied":[3,4]`
				status := `{"contract_version":2,"current_version":4,"total_migrations":4,"has_pending_changes":false}`
				if exit != 0 {
					outcome, rest = "partial", `"applied":[3],"error":{"message":"failed to apply migration 4"}`
					status = `{"contract_version":2,"current_version":3,"total_migrations":4,"has_pending_changes":true}`
				}
				document := `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"` + outcome +
					`","planned":[3,4],` + rest
				if row.log != "" {
					document += `,"migration_log":` + row.log
				}
				document += `,"status":` + status + `}`
				executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: exit}}}
				result := Run(context.Background(), Config{
					Operation:   OperationMigrationApply,
					Environment: withRunnerProtocol(migrationApplyEnvironment(t, testOperationID)),
					Executor:    executor,
					TempDir:     t.TempDir(),
				})
				parsed := requireFrameParses(t, result)
				if !row.refused {
					if parsed.MigrationRun == nil || parsed.MigrationRun.Outcome != outcome {
						t.Fatalf("the manager parsed migration run %#v, want the %s run the child reported (error %#v)",
							parsed.MigrationRun, outcome, result.Error)
					}
					return
				}
				if parsed.MigrationRun != nil || !parsed.MutationStarted || !parsed.Uncertain {
					t.Fatalf("the manager parsed migration run %#v, MutationStarted %t, Uncertain %t: want no report on an uncertain mutation",
						parsed.MigrationRun, parsed.MutationStarted, parsed.Uncertain)
				}
				want := "invalid_migration_run_output"
				if exit != 0 {
					want = "child_exit"
				}
				if result.Error == nil || result.Error.Code != want {
					t.Fatalf("Run() error = %#v, want %s", result.Error, want)
				}
			})
		}
	}
}

// requireFrameParses is the result as the manager reads it from the frame.
func requireFrameParses(t *testing.T, result Result) Result {
	t.Helper()
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, result.Operation, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	return parsed
}
