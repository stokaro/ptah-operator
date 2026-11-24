package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Since Ptah v0.35.1 a MySQL plan refuses a declared REVOKE on one table that
// a grant on the whole database keeps (stokaro/ptah#6492 and #6512), under the
// code declaration-refused. v0.35.0 planned nothing for it and read the schema
// as in sync while the role kept reading the table. The runner asks the
// credential-free check about the refusal, the check finds nothing wrong with
// the file, and the plan stops as plan_refused with Ptah's sentence in the Pod
// log alone. The documents are what both images printed against MySQL 8.4;
// testdata/wide-grant-revoke says where they are from.
func TestARevokeAWideGrantKeepsIsAPlanRefusal(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", "wide-grant-revoke", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	run := desiredSourceRuns()["plan"]
	run.environment = environmentFor(run.environment, "mysql://app:secret@tcp(db.example:3306)/app", "MySQL")
	checkArgs := []string{"schema", "validate", "--dialect=MySQL"}
	const sentence = `compare database schema: role "wide_reader" holds SELECT ON wide.*, which covers table t3, ` +
		`so a revoke on the table cannot take it: revoke SELECT ON wide.* from "wide_reader" instead, ` +
		`or grant the privilege on each table that needs it`

	t.Run("v0.35.1 refuses the revoke", func(t *testing.T) {
		t.Parallel()
		// Validate printed nothing and exited 0 for desired.sql: the file
		// alone has no fault, so the refusal keeps its code.
		result, executor, diagnostics := runDesiredSource(t, run,
			scriptedResponse{stdout: read("schema-plan-mysql-8.4.json"), stderr: read("schema-plan-mysql-8.4.stderr"), exitCode: 2},
			scriptedResponse{exitCode: 0})
		const want = "ptah refused to plan the change (declaration-refused)"
		if result.Error == nil || result.Error.Code != CodePlanRefused || result.Error.Message != want {
			t.Fatalf("Run() error = %#v, want %s with %q", result.Error, CodePlanRefused, want)
		}
		if len(executor.calls) != 2 || !slices.Equal(executor.calls[1].Args, checkArgs) {
			t.Fatalf("ran %d children, the second as %q; want the plan and then %q", len(executor.calls), argsOf(executor, 1), checkArgs)
		}
		if result.PlanOutcome != "" || result.PlanContentDigest != "" || result.Uncertain || result.MutationStarted {
			t.Fatalf("a refused plan was reported as a plan: %#v", result)
		}
		if strings.Count(diagnostics, "ptah-runner: ptah reported: ") != 1 ||
			!strings.Contains(diagnostics, "ptah-runner: ptah reported: "+sentence+"\n") {
			t.Fatalf("the Pod log does not carry Ptah's sentence whole on one line:\n%s", diagnostics)
		}
		encoded, err := EncodeResult(result)
		if err != nil {
			t.Fatalf("EncodeResult() error = %v", err)
		}
		for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary} {
			if bytes.Contains(crossing, []byte("wide_reader")) || bytes.Contains(crossing, []byte("wide.*")) {
				t.Errorf("%s carries Ptah's sentence: %s", name, crossing)
			}
		}
	})

	t.Run("v0.35.0 planned nothing for it", func(t *testing.T) {
		t.Parallel()
		document := scriptedResponse{stdout: read("schema-plan-mysql-8.4-v0.35.0.json"), stderr: read("schema-plan-mysql-8.4-v0.35.0.stderr")}
		result, executor, diagnostics := runDesiredSource(t, run, document, document)
		if result.Error != nil || result.PlanOutcome != PlanOutcomeNoChanges || len(executor.calls) != 2 {
			t.Fatalf("Run() = %#v after %d children, want %s from the two plan reads", result, len(executor.calls), PlanOutcomeNoChanges)
		}
		if strings.Contains(diagnostics, "wide_reader") {
			t.Fatalf("a plan with no changes logged a refusal:\n%s", diagnostics)
		}
	})
}

// argsOf is the argument list of the executor's call at index, or nil when it
// made fewer calls.
func argsOf(executor *scriptedExecutor, index int) []string {
	if index >= len(executor.calls) {
		return nil
	}
	return executor.calls[index].Args
}
