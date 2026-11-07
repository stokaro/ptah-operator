package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The rows in this file run documents Ptah v0.23.0 printed through the runner,
// as the runner harness captured them from the pinned release image.

// ptahV023Document reads one of those documents.
func ptahV023Document(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// environmentFor replaces the database URL and the expected engine of a test
// environment.
func environmentFor(environment []string, databaseURL, engine string) []string {
	replaced := make([]string, 0, len(environment))
	for _, entry := range environment {
		switch {
		case strings.HasPrefix(entry, envDatabaseURL+"="):
			replaced = append(replaced, envDatabaseURL+"="+databaseURL)
		case strings.HasPrefix(entry, envExpectedDatabaseEngine+"="):
			replaced = append(replaced, envExpectedDatabaseEngine+"="+engine)
		default:
			replaced = append(replaced, entry)
		}
	}
	return replaced
}

// mysqlEnvironment is databaseEnvironment for a MySQL target.
func mysqlEnvironment(operationID string) []string {
	return environmentFor(databaseEnvironment(operationID), "mysql://app:secret@tcp(db.example:3306)/app", "MySQL")
}

// Ptah v0.23.0 reports a narrowed column type and a removed identity under
// categories of their own. The runner frames both, and the controller parses
// the frame with the category, count and severity Ptah wrote.
func TestObserveFramesPtahV023ColumnCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		document    string
		environment func(string) []string
		category    string
	}{
		{document: "differing/drift-column-types-narrowed-postgres.json", environment: databaseEnvironment, category: "column_types_narrowed"},
		{document: "differing/drift-column-types-narrowed-mysql.json", environment: mysqlEnvironment, category: "column_types_narrowed"},
		{document: "differing/drift-column-identities-removed-postgres.json", environment: databaseEnvironment, category: "column_identities_removed"},
	}
	for _, test := range tests {
		t.Run(test.document, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-v023-" + test.category
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(test.environment(operationID)),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: ptahV023Document(t, test.document), exitCode: 1}}},
			})
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v; result error %#v", err, result.Error)
			}
			parsed, err := ParseResultFor(frame, OperationObserve, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			want := []DriftFindingSummary{
				{Category: test.category, Count: 1, Severity: "destructive"},
				{Category: "columns_modified", Count: 1, Severity: "warning"},
			}
			if !parsed.ObservedDrift || parsed.DriftFindingCount != 2 || !reflect.DeepEqual(parsed.DriftFindings, want) {
				t.Fatalf("the controller parsed drift %t, count %d, findings %#v; want %#v",
					parsed.ObservedDrift, parsed.DriftFindingCount, parsed.DriftFindings, want)
			}
		})
	}
}

// A deferred foreign key that fails at commit: Ptah v0.23.0 writes statement 0
// beside commit true. The runner carries that failure to the controller and
// leaves out the database's sentence, which quotes the constraint. Nothing
// committed, so the run is not uncertain.
func TestMigrationRunCarriesPtahV023CommitFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		document string
		want     dataplane.MigrationRunFailure
	}{
		{document: "migrations-up-commit-failed.json",
			want: dataplane.MigrationRunFailure{Version: 1, Statement: 0, Statements: 3, Commit: true, SQLState: "23503"}},
		{document: "migrations-up-commit-failed-second.json",
			want: dataplane.MigrationRunFailure{Version: 2, Statement: 0, Statements: 1, Commit: true, SQLState: "23503"}},
	} {
		t.Run(test.document, func(t *testing.T) {
			t.Parallel()
			document := ptahV023Document(t, test.document)
			const quoted = "child_parent_id_fkey"
			if !strings.Contains(document, quoted) {
				t.Fatal("the captured document no longer quotes the constraint, so this row proves nothing about redaction")
			}
			operationID := "migration-v023-commit"
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: withRunnerProtocol(migrationApplyEnvironment(t, operationID)),
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: 2}}},
			})
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if strings.Contains(string(frame), quoted) {
				t.Fatalf("the framed result carries the database's sentence: %s", frame)
			}
			parsed, err := ParseResultFor(frame, OperationMigrationApply, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.MigrationRun == nil || parsed.MigrationRun.Outcome != dataplane.MigrationOutcomeFailed ||
				parsed.MigrationRun.Failure == nil || *parsed.MigrationRun.Failure != test.want {
				t.Fatalf("the controller parsed migration run %#v, want outcome failed and failure %+v", parsed.MigrationRun, test.want)
			}
			if parsed.Uncertain || !parsed.MutationStarted {
				t.Fatalf("parsed Uncertain %t MutationStarted %t, want a failed run that is not uncertain", parsed.Uncertain, parsed.MutationStarted)
			}
		})
	}
}

// A MySQL CREATE UNIQUE INDEX stopped by a duplicate key: v0.23.0 reports the
// run unknown. The runner carries the failure and the dirty row's counts,
// clears the row's free text, which here is Ptah's in-flight marker, and marks
// the run uncertain.
func TestMigrationRunCarriesPtahV023UnknownMySQLRun(t *testing.T) {
	t.Parallel()

	document := ptahV023Document(t, "statement-errors/my-unique-up.json")
	const marker = "statement execution outcome is unknown"
	if !strings.Contains(document, marker) {
		t.Fatal("the captured document no longer carries the in-flight marker, so this row proves nothing about it")
	}
	operationID := "migration-v023-unknown"
	result := Run(context.Background(), Config{
		Operation: OperationMigrationApply,
		// The apply binds the target digest to the URL, so this row keeps the
		// test environment's; the runner reads the document the same way for
		// either engine, as TestMigrationRunCarriesTheFailedStatementThroughTheFrame
		// does for a MySQL run.
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, operationID)),
		Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: 2}}},
	})
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if strings.Contains(string(frame), marker) || strings.Contains(string(frame), "Duplicate entry") {
		t.Fatalf("the framed result carries Ptah's free text: %s", frame)
	}
	parsed, err := ParseResultFor(frame, OperationMigrationApply, operationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	run := parsed.MigrationRun
	want := dataplane.MigrationRunFailure{Version: 1, Statement: 1, Statements: 1, SQLState: "23000", ErrorNumber: 1062}
	if run == nil || run.Outcome != dataplane.MigrationOutcomeUnknown || run.Failure == nil || *run.Failure != want {
		t.Fatalf("the controller parsed migration run %#v, want outcome unknown and failure %+v", run, want)
	}
	if dirty := run.Status.DirtyRevision; dirty == nil || dirty.Version != 1 || dirty.Applied != 0 || dirty.Total != 1 || dirty.Error != "" {
		t.Fatalf("the controller parsed dirty revision %+v, want version 1, 0 of 1, no text", dirty)
	}
	if !parsed.Uncertain {
		t.Fatal("parsed Uncertain = false, want an unknown run marked uncertain")
	}
}

// Ptah v0.23.0 refuses a desired schema whose declarations contradict each
// other with declaration-refused, where v0.22.0 failed the plan with the same
// message. The runner asks the credential-free check the same question it asks
// for a failure, so the fault the plan stopped on is named in the file's own
// words, and a fault it did not stop on keeps the refusal's code.
func TestADeclarationRefusalIsNamedLikeAFailure(t *testing.T) {
	t.Parallel()

	mysql := func(run desiredSourceRun) desiredSourceRun {
		run.environment = environmentFor(run.environment, "mysql://app:secret@tcp(db.example:3306)/app", "MySQL")
		run.checkArgs = []string{"schema", "validate", "--dialect=MySQL"}
		return run
	}
	for _, test := range []struct {
		name      string
		run       desiredSourceRun
		refused   string
		validated string
		code      string
		message   string
		children  int
	}{
		{
			name: "a foreign key to a table nothing declares", run: desiredSourceRuns()["plan"],
			refused: ptahV023Document(t, "desired-source/plan-foreign-key.json"), validated: ptahV023Document(t, "desired-source/validate-foreign-key.txt"),
			code: CodeDesiredSchemaInvalid, message: `schema.sql: invalid foreign key: field "id" references unknown table "nope"`, children: 2,
		},
		{
			name: "a MySQL key to a table nothing declares", run: mysql(desiredSourceRuns()["plan"]),
			refused: ptahV023Document(t, "desired-source/plan-key-mysql.json"), validated: ptahV023Document(t, "desired-source/validate-key-mysql.txt"),
			code: CodeDesiredSchemaInvalid, message: `schema.sql: invalid foreign key: constraint "t_nope" references unknown table "nope"`, children: 2,
		},
		{
			// The plan stopped on a key into a schema the database holds and
			// the file does not declare; validate faults another key. The two
			// sentences differ, so the refusal keeps its code.
			name: "a fault the plan did not stop on", run: desiredSourceRuns()["plan"],
			refused: ptahV023Document(t, "desired-source/plan-other-and-key.json"), validated: ptahV023Document(t, "desired-source/validate-other-and-key.txt"),
			code: "plan_refused", message: "ptah refused to plan the change (declaration-refused)", children: 2,
		},
		{
			name: "a declaration refusal with no message", run: desiredSourceRuns()["plan"],
			refused: `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"declaration-refused"}}`,
			code:    "plan_refused", message: "ptah refused to plan the change (declaration-refused)", children: 1,
		},
		{
			// Another refusal carries the same message: only the declaration
			// refusal is the fault the check can explain.
			name: "another refusal with the message of a declaration fault", run: desiredSourceRuns()["plan"],
			refused: `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"dependent-object"},` +
				`"error":{"message":"compare database schema: invalid foreign key: field \"id\" references unknown table \"nope\""}}`,
			code: "plan_refused", message: "ptah refused to plan the change (dependent-object)", children: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			responses := []scriptedResponse{{stdout: test.refused, exitCode: 2}}
			if test.validated != "" {
				responses = append(responses, scriptedResponse{stdout: test.validated, stderr: "1 structural problem\n", exitCode: 1})
			}
			result, executor, diagnostics := runDesiredSource(t, test.run, responses...)
			if result.Error == nil || result.Error.Code != test.code || result.Error.Message != test.message {
				t.Fatalf("Run() error = %#v, want code %s and %q", result.Error, test.code, test.message)
			}
			if len(executor.calls) != test.children {
				t.Fatalf("ran %d children, want %d", len(executor.calls), test.children)
			}
			if test.children == 2 && (!slices.Equal(executor.calls[1].Args, test.run.checkArgs) || !slices.Equal(executor.calls[1].Env, test.run.checkEnv)) {
				t.Fatalf("the check ran as %q with %q, want %q with %q",
					executor.calls[1].Args, executor.calls[1].Env, test.run.checkArgs, test.run.checkEnv)
			}
			if result.Uncertain || result.MutationStarted {
				t.Fatalf("Run() = uncertain %v, mutation %v", result.Uncertain, result.MutationStarted)
			}
			if test.code == "plan_refused" && (strings.Contains(result.Error.Message, "invalid foreign key") || strings.Contains(diagnostics, "invalid foreign key")) {
				t.Fatalf("the runner repeated Ptah's sentence: %q\n%s", result.Error.Message, diagnostics)
			}
		})
	}
}

// A MySQL account that cannot see every object: Ptah v0.23.0 reports
// permission_denied with missing_privileges, from drift and from plan. The
// frame carries the kinds of object and the runner's sentence about them, and
// nothing else Ptah wrote: no database, account, object or privilege.
func TestUnseenObjectsReachTheFrame(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		document  string
		operation Operation
		want      []string
		sentence  string
	}{
		{document: "differing/drift-permission-denied-triggers-routines.json", operation: OperationObserve,
			want:     []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines},
			sentence: "the target database server accepted the credentials, and the user cannot see every trigger and routine in the database"},
		{document: "differing/plan-permission-denied-triggers-routines.json", operation: OperationPlan,
			want:     []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines},
			sentence: "the target database server accepted the credentials, and the user cannot see every trigger and routine in the database"},
		{document: "differing/drift-permission-denied-named-routines.json", operation: OperationObserve,
			want:     []string{dataplane.UnseenRoutines},
			sentence: "the target database server accepted the credentials, and the user cannot see every routine in the database"},
		{document: "differing/drift-permission-denied-named-views.json", operation: OperationObserve,
			want:     []string{dataplane.UnseenViews},
			sentence: "the target database server accepted the credentials, and the user cannot see every view definition in the database"},
	} {
		t.Run(test.document, func(t *testing.T) {
			t.Parallel()
			operationID := "unseen-objects-" + string(test.operation)
			diagnostics := &strings.Builder{}
			result := Run(context.Background(), Config{
				Operation: test.operation, Environment: withRunnerProtocol(mysqlEnvironment(operationID)),
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: ptahV023Document(t, test.document), exitCode: 2}}},
				Diagnostics: diagnostics,
				TempDir:     t.TempDir(),
			})
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			for _, written := range []string{"harness_differs", "harness_login", "differs_hand_fn", "differs_notes_view", "SHOW_ROUTINE", "TRIGGER on"} {
				if strings.Contains(string(frame), written) || strings.Contains(diagnostics.String(), written) {
					t.Fatalf("the frame or the Pod log repeats %q from Ptah's document:\n%s\n%s", written, frame, diagnostics)
				}
			}
			parsed, err := ParseResultFor(frame, test.operation, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.Error == nil || parsed.Error.Code != string(dataplane.TargetPermissionDenied) ||
				parsed.Error.Message != test.sentence || !slices.Equal(parsed.Error.UnseenObjects, test.want) {
				t.Fatalf("the controller parsed error %#v, want permission_denied, %q and %q", parsed.Error, test.sentence, test.want)
			}
		})
	}
}

// A missing_privileges entry outside the vocabulary leaves the list out
// altogether, so the failure reads as the plain permission_denied rather than
// as a list shorter than what the account lacks.
func TestUnseenObjectsOutsideTheVocabularyStayOut(t *testing.T) {
	t.Parallel()

	document := `{"contract_version":1,"command":"schema drift","error":{"kind":"permission_denied","message":"cannot see",` +
		`"missing_privileges":[{"database":"app","objects":"triggers","privilege":"TRIGGER on app.*"},` +
		`{"database":"app","objects":"events","privilege":"EVENT on app.*"}]}}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(mysqlEnvironment("unseen-objects-unknown")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: 2}}},
	})
	if result.Error == nil || result.Error.Code != string(dataplane.TargetPermissionDenied) ||
		result.Error.Message != dataplane.TargetPermissionDenied.Description() || result.Error.UnseenObjects != nil {
		t.Fatalf("Run() error = %#v, want the plain permission_denied", result.Error)
	}
}

// The frame refuses an unseen objects list the runner could not have written:
// on another code, empty, with a kind outside the vocabulary, repeated, or out
// of order. The same frame with the runner's own list parses.
func TestTheFrameRefusesAnUnseenObjectsListTheRunnerCannotWrite(t *testing.T) {
	t.Parallel()

	operationID := "unseen-objects-frame"
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(mysqlEnvironment(operationID)),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{
			stdout: ptahV023Document(t, "differing/drift-permission-denied-triggers-routines.json"), exitCode: 2}}},
	})
	if result.Error == nil || len(result.Error.UnseenObjects) == 0 {
		t.Fatalf("Run() error = %#v, want unseen objects to forge from", result.Error)
	}
	framed := func(t *testing.T, mutate func(*Result)) []byte {
		t.Helper()
		forged := result
		failure := *result.Error
		forged.Error = &failure
		mutate(&forged)
		payload, err := json.Marshal(forged)
		if err != nil {
			t.Fatal(err)
		}
		// json.Marshal leaves an empty list out, so the empty list is
		// written into the payload the way another encoder could.
		if forged.Error.UnseenObjects != nil && len(forged.Error.UnseenObjects) == 0 {
			payload = []byte(strings.Replace(string(payload), `"message":`, `"unseenObjects":[],"message":`, 1))
			if !strings.Contains(string(payload), `"unseenObjects":[]`) {
				t.Fatal("the forged payload carries no empty list")
			}
		}
		return foreignFrame(payload)
	}
	if _, err := ParseResultFor(framed(t, func(*Result) {}), OperationObserve, operationID); err != nil {
		t.Fatalf("ParseResultFor(the runner's own list) error = %v", err)
	}
	for _, row := range []struct {
		name   string
		mutate func(*Result)
	}{
		{name: "another code", mutate: func(r *Result) { r.Error.Code = string(dataplane.TargetAuthenticationFailed) }},
		{name: "an empty list", mutate: func(r *Result) { r.Error.UnseenObjects = []string{} }},
		{name: "a kind outside the vocabulary", mutate: func(r *Result) { r.Error.UnseenObjects = []string{"triggers", "events"} }},
		{name: "a repeated kind", mutate: func(r *Result) { r.Error.UnseenObjects = []string{"triggers", "triggers"} }},
		{name: "kinds out of order", mutate: func(r *Result) { r.Error.UnseenObjects = []string{"routines", "triggers"} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseResultFor(framed(t, row.mutate), OperationObserve, operationID); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want a malformed frame", err)
			}
		})
	}
}
