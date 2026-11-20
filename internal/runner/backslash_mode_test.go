package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// backslashModeSentence is what Ptah v0.34.0 wrote for the refusal, measured
// against MySQL 8.4 whose global sql_mode names NO_BACKSLASH_ESCAPES, on a plan
// that creates a column with DEFAULT 'C:\\temp' (stokaro/ptah#6400).
const backslashModeSentence = "unsupported feature: the plan is written for a session whose sql_mode does not name " +
	"NO_BACKSLASH_ESCAPES, the session's sql_mode names NO_BACKSLASH_ESCAPES, and a statement holds a backslash, " +
	"which the two read differently; apply the desired schema without --plan, or connect with a sql_mode without " +
	"NO_BACKSLASH_ESCAPES"

// A plan Ptah computes on a session in NO_BACKSLASH_ESCAPES mode is written
// for the default mode, so the dry run the Plan runs on it refuses it on the
// same server, and on every retry. Since Ptah v0.34.0 the refusal has a code,
// and the Plan is refused the way an owner refusal is: the frame names the
// code, and Ptah's sentence, which names both modes and what to change, goes to
// the Pod log and nowhere else. Ptah v0.33.1 wrote the same sentence as a
// failed apply with no code, which still reads as a failed validation, and so
// does every other refusal the dry run may write.
func TestAPlanWhoseDryRunRefusesTheBackslashModeIsRefused(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE `paths` (\n  `id` int PRIMARY KEY,\n  `p` varchar(50) DEFAULT 'C:\\\\temp'\n)")
	dryRun := func(outcome, refusal string) []scriptedResponse {
		document := `{"contract_version":4,"command":"schema apply","outcome":"` + outcome + `","plan_name":"operator-plan",` +
			`"plan_digest":"` + sha256Digest([]byte(plan)) + `",`
		if refusal != "" {
			document += `"refusal":` + refusal + `,`
		}
		document += `"error":{"message":` + string(mustMarshal(t, backslashModeSentence)) + `}}`
		return []scriptedResponse{planResponse(plan), planResponse(plan), {stdout: document, exitCode: 2}}
	}
	run := func(t *testing.T, responses []scriptedResponse) (Result, string, EncodedResult) {
		t.Helper()
		diagnostics := &bytes.Buffer{}
		result := Run(context.Background(), Config{
			Operation:   OperationPlan,
			Environment: withRunnerProtocol(databaseEnvironment("plan-backslash-mode")),
			Executor:    &scriptedExecutor{t: t, responses: responses},
			Diagnostics: diagnostics,
			TempDir:     t.TempDir(),
		})
		encoded, err := EncodeResult(result)
		if err != nil {
			t.Fatalf("EncodeResult() error = %v", err)
		}
		return result, diagnostics.String(), encoded
	}

	result, log, encoded := run(t, dryRun("refused", `{"code":"`+dataplane.SchemaRefusalBackslashMode+`"}`))
	const want = "ptah refused to plan the change (backslash-mode)"
	if result.Error == nil || result.Error.Code != codePlanRefused || result.Error.Message != want {
		t.Fatalf("Run() error = %#v, want %s with %q", result.Error, codePlanRefused, want)
	}
	if result.MutationStarted || result.Uncertain || result.PlanContentDigest != "" {
		t.Fatalf("a refused Plan reported work: %#v", result)
	}
	if !strings.Contains(log, "ptah-runner: ptah reported: "+backslashModeSentence+"\n") {
		t.Fatalf("the log does not carry Ptah's sentence whole on one line:\n%s", log)
	}
	for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary} {
		if bytes.Contains(crossing, []byte("NO_BACKSLASH_ESCAPES")) {
			t.Errorf("%s carries Ptah's sentence: %s", name, crossing)
		}
	}

	// The controls: v0.33.1's failed apply, which carries the same sentence and
	// no code, and the refusals that do not say the plan is wrong, read as a
	// failed validation as before.
	for name, test := range map[string]struct{ outcome, refusal, reason string }{
		"Ptah v0.33.1's failed apply": {"failed", "", "failed"},
		"a stale plan":                {"refused", `{"code":"stale-plan","changed":"schema"}`, "refused (stale-plan)"},
		"a lock timeout":              {"refused", `{"code":"lock-timeout"}`, "refused (lock-timeout)"},
		"a code it does not know":     {"refused", `{"code":"backslash-modes"}`, "refused (backslash-modes)"},
	} {
		result, _, _ = run(t, dryRun(test.outcome, test.refusal))
		want := "plan validation reported " + test.reason + " rather than a dry run"
		if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Error.Message != want {
			t.Errorf("%s: Run() error = %#v, want invalid_plan_output with %q", name, result.Error, want)
		}
	}
}

// An approved plan's Apply meets the refusal where the server's sql_mode came
// to name NO_BACKSLASH_ESCAPES after the Plan's dry run passed. The runner
// needed no change for it: a refused Apply is apply_refused under the code, and
// Ptah's sentence goes to the Pod log. Under Ptah v0.33.1 the same Apply read
// as an apply that failed before it sent any statement.
func TestAnApplyRefusedForTheBackslashModeNamesTheCode(t *testing.T) {
	t.Parallel()

	document := `{"contract_version":4,"command":"schema apply","outcome":"refused","plan_name":"operator-plan",` +
		`"plan_digest":"` + sha256Digest([]byte(targetFailurePlan)) + `","refusal":{"code":"backslash-mode"},` +
		`"error":{"message":` + string(mustMarshal(t, backslashModeSentence)) + `}}`
	result, log, encoded := runRefusedStatement(t, targetFailureRuns()["apply"], document)
	const want = "ptah refused the approved plan (backslash-mode)"
	if result.Error == nil || result.Error.Code != codeApplyRefused || result.Error.Message != want {
		t.Fatalf("Run() error = %#v, want %s with %q", result.Error, codeApplyRefused, want)
	}
	if !strings.Contains(log, "ptah-runner: ptah reported: "+backslashModeSentence+"\n") {
		t.Fatalf("the log does not carry Ptah's sentence whole on one line:\n%s", log)
	}
	for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary} {
		if bytes.Contains(crossing, []byte("NO_BACKSLASH_ESCAPES")) {
			t.Errorf("%s carries Ptah's sentence: %s", name, crossing)
		}
	}
}
