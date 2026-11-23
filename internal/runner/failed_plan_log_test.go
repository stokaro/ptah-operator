package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plan Ptah reports failed with no refusal code, no target failure and no
// fault of the file gets the generic invalid_plan_output, and Ptah's sentence,
// which names the cause, goes to the Pod log on one line and nowhere else. The
// input is what Ptah v0.34.0 printed for a column PostgreSQL 18 holds as
// VIRTUAL and the schema declares with no kind; testdata/generated-kind says
// where it is from.
func TestAFailedPlanLogsPtahsSentenceAndFramesTheGenericMessage(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", "generated-kind", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	const sentence = "generate schema apply operations: unsupported feature: generated column public.items.total is VIRTUAL " +
		"and the schema declares it STORED; the postgres planner cannot change a generated column's kind in place, " +
		"so drop the column and add it back, or declare it VIRTUAL"
	for _, test := range []struct {
		name   string
		stdout string
		stderr string
		logged string
	}{
		{
			name:   "the captured failure",
			stdout: read("schema-plan-postgres-18-v0.34.0.json"),
			stderr: read("schema-plan-postgres-18-v0.34.0.stderr"),
			logged: sentence,
		},
		{
			name:   "a failure that says nothing",
			stdout: `{"contract_version":3,"command":"schema plan","outcome":"failed"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			diagnostics := &bytes.Buffer{}
			result := Run(context.Background(), Config{
				Operation:   OperationPlan,
				Environment: withRunnerProtocol(databaseEnvironment("failed-plan")),
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: test.stdout, stderr: test.stderr, exitCode: 2}}},
				Diagnostics: diagnostics,
				TempDir:     t.TempDir(),
			})
			const want = "plan command did not complete successfully"
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Error.Message != want {
				t.Fatalf("Run() error = %#v, want invalid_plan_output with %q", result.Error, want)
			}
			if result.PlanOutcome != "" || result.PlanContentDigest != "" {
				t.Fatalf("a failed plan was reported as a plan: %#v", result)
			}
			log := diagnostics.String()
			reported := strings.Count(log, "ptah-runner: ptah reported: ")
			if test.logged == "" {
				if reported != 0 {
					t.Fatalf("a failure with no sentence logged one:\n%s", log)
				}
			} else if reported != 1 || !strings.Contains(log, "ptah-runner: ptah reported: "+test.logged+"\n") {
				t.Fatalf("the log does not carry Ptah's sentence whole on one line:\n%s", log)
			}
			encoded, err := EncodeResult(result)
			if err != nil {
				t.Fatalf("EncodeResult() error = %v", err)
			}
			for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary} {
				if bytes.Contains(crossing, []byte("VIRTUAL")) || bytes.Contains(crossing, []byte("public.items")) {
					t.Errorf("%s carries Ptah's sentence: %s", name, crossing)
				}
			}
		})
	}
}
