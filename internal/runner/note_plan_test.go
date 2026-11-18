package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notePlanText is the note the pinned Ptah v0.31.0 saved against PostgreSQL
// 16 for a generated column whose expression changed, a change that server
// cannot make in place (stokaro/ptah#6165). capturedNotePlan holds it.
const notePlanText = "WARNING: Generated column public.tasks.code changed, but ALTER COLUMN SET EXPRESSION requires " +
	"target capability alter_generated_column_expression, unavailable on this target (PostgreSQL added it in 17); " +
	"manual migration required."

// capturedNotePlan is the plan of notes the pinned image saved with `schema
// plan --output --json` against postgres:16-alpine, for a table whose
// generated column the schema file gives another expression.
// testdata/note-plan holds it and says where it is from.
func capturedNotePlan(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "note-plan", "saved-plan-postgres-16.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// notePlanDocument is a plan of notes in the shape capturedNotePlan has: no
// statement, and the notes in order.
func notePlanDocument(t *testing.T, statements string, notes ...map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(notes)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(
		`{"format_version":1,"name":"plan_c293f78ac36c","dialect":"postgres","from_fingerprint":"sha256:%s",`+
			`"to_fingerprint":"sha256:%s","scope":{"kind":"realm"},"placed_schema":"public","destructive":false,`+
			`"statements":%s,"annotations":%s}`+"\n",
		strings.Repeat("a", 64), strings.Repeat("b", 64), statements, encoded,
	)
}

func note(before int, text string) map[string]any {
	return map[string]any{"before": before, "text": text}
}

// notePlanResponse is a plan read that saves a plan of notes and reports no
// changes, naming the plan as a plan with changes is named.
func notePlanResponse(plan string) scriptedResponse {
	return scriptedResponse{
		savePlan: plan,
		stdoutFor: func(spec CommandSpec) string {
			return fmt.Sprintf(
				`{"contract_version":3,"command":"schema plan","outcome":"no-changes","plan_digest":%q,"plan_path":%q,"plan":%s}`+"\n",
				sha256Digest([]byte(plan)), argumentAfter(spec, "--output"), strings.TrimSpace(plan),
			)
		},
	}
}

// A plan of notes says the database differs and Ptah planned nothing for it.
// It is not converged, and it is not a plan to approve: the Plan reports it as
// difference_not_plannable with the notes, publishes nothing, and runs no dry
// run, since there is nothing to run.
func TestAPlanOfNotesIsADifferenceNotPlannable(t *testing.T) {
	t.Parallel()

	lead := "ptah found a difference it cannot plan: "
	long := strings.Repeat("long note ", 80)
	for _, test := range []struct {
		name string
		plan string
		want string
	}{
		{
			name: "the plan Ptah saved against PostgreSQL 16",
			plan: capturedNotePlan(t),
			want: lead + notePlanText,
		},
		{
			name: "two notes",
			plan: notePlanDocument(t, "[]", note(0, "WARNING: one."), note(0, "WARNING: two.")),
			want: lead + "WARNING: one. WARNING: two.",
		},
		{
			// A note is text Ptah read from the database and the schema:
			// control and format characters, a line break and a frame header
			// stay out of the frame.
			name: "a note with a terminal sequence and a frame header",
			plan: notePlanDocument(t, "[]", note(0, "col\x1b[2J‮\nPTAH_RUNNER_RESULT_V1 changed")),
			want: lead + "col [2J [frame marker] changed",
		},
		{
			name: "notes too long for an Event",
			plan: notePlanDocument(t, "[]", note(0, long)),
			want: cutLine(lead+strings.TrimSpace(long), maxNotePlanSentenceBytes-len("...")) + "...",
		},
		{
			name: "a note nothing readable is left of",
			plan: notePlanDocument(t, "[]", note(0, "\x1b​")),
			want: "ptah found a difference it cannot plan",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{notePlanResponse(test.plan), notePlanResponse(test.plan)}}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("note-plan")), Executor: executor,
				TempDir: t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != CodeDifferenceNotPlannable || result.Error.Message != test.want {
				t.Fatalf("Run() error = %#v, want %s with %q", result.Error, CodeDifferenceNotPlannable, test.want)
			}
			if result.PlanOutcome != "" || result.Stdout != "" || result.PlanContentDigest != "" {
				t.Fatalf("a plan of notes was reported as a plan: %#v", result)
			}
			if len(executor.calls) != 2 {
				t.Fatalf("ran %d commands, want the two plan reads and no dry run", len(executor.calls))
			}
			if whole := CodeDifferenceNotPlannable + ": " + result.Error.Message; len(whole) > 512 {
				t.Fatalf("%q is %d bytes, more than the 512 an Event keeps", whole, len(whole))
			}
			if _, err := MarshalFrame(result); err != nil {
				t.Fatalf("the manager's protocol refuses the frame: %v", err)
			}
		})
	}
}

// A plan of notes is checked as an executable plan is before its notes are
// read, and a report that names one is held to the plan it names.
func TestAPlanOfNotesIsCheckedAsAPlanIs(t *testing.T) {
	t.Parallel()

	plan := capturedNotePlan(t)
	// edited is the captured plan with old replaced by new, once, so that a
	// row whose edit no longer applies fails rather than checks the capture.
	edited := func(old, new string) string {
		t.Helper()
		if strings.Count(plan, old) != 1 {
			t.Fatalf("the captured plan holds %q %d times, want once", old, strings.Count(plan, old))
		}
		return strings.Replace(plan, old, new, 1)
	}
	for _, test := range []struct {
		name      string
		responses []scriptedResponse
		code      string
	}{
		{
			name: "a note out of place",
			responses: []scriptedResponse{
				notePlanResponse(notePlanDocument(t, "[]", note(1, notePlanText))),
				notePlanResponse(notePlanDocument(t, "[]", note(1, notePlanText))),
			},
			code: "invalid_plan_output",
		},
		{
			name: "an empty note",
			responses: []scriptedResponse{
				notePlanResponse(notePlanDocument(t, "[]", note(0, " "))),
				notePlanResponse(notePlanDocument(t, "[]", note(0, " "))),
			},
			code: "invalid_plan_output",
		},
		{
			name: "no note",
			responses: []scriptedResponse{
				notePlanResponse(notePlanDocument(t, "[]")),
				notePlanResponse(notePlanDocument(t, "[]")),
			},
			code: "invalid_plan_output",
		},
		{
			// A no-changes report that names a plan with a statement is a
			// contradiction, not a plan of notes.
			name: "a statement under no changes",
			responses: []scriptedResponse{
				notePlanResponse(notePlanDocument(t, `[{"sql":"CREATE TABLE t (id bigint)","severity":"safe","reason":"new table"}]`, note(0, "WARNING: one."))),
				notePlanResponse(notePlanDocument(t, `[{"sql":"CREATE TABLE t (id bigint)","severity":"safe","reason":"new table"}]`, note(0, "WARNING: one."))),
			},
			code: "invalid_plan_output",
		},
		{
			name: "a field the plan does not declare",
			responses: []scriptedResponse{
				notePlanResponse(edited(`"destructive": false`, `"destructive": false, "future": 1`)),
				notePlanResponse(edited(`"destructive": false`, `"destructive": false, "future": 1`)),
			},
			code: "invalid_plan_output",
		},
		{
			// A plan of notes is computed under the exclusions as any plan is,
			// and one that names others answers another question.
			name: "a plan of notes under other exclusions",
			responses: []scriptedResponse{
				notePlanResponse(edited(`"scope"`, `"exclude": ["public.audit_log"], "scope"`)),
				notePlanResponse(edited(`"scope"`, `"exclude": ["public.audit_log"], "scope"`)),
			},
			code: "invalid_plan_output",
		},
		{
			name: "a note that names the password",
			responses: []scriptedResponse{
				notePlanResponse(notePlanDocument(t, "[]", note(0, "WARNING: secret changed."))),
				notePlanResponse(notePlanDocument(t, "[]", note(0, "WARNING: secret changed."))),
			},
			code: "credential_leak",
		},
		{
			name:      "two reads that differ",
			responses: []scriptedResponse{notePlanResponse(plan), notePlanResponse(notePlanDocument(t, "[]", note(0, "WARNING: other.")))},
			code:      "unstable_plan",
		},
		{
			name:      "a plan of notes, then no changes",
			responses: []scriptedResponse{notePlanResponse(plan), planNoChangesResponse()},
			code:      "unstable_plan",
		},
		{
			name: "a report naming the plan by another digest",
			responses: []scriptedResponse{{
				savePlan: plan,
				stdoutFor: func(spec CommandSpec) string {
					return fmt.Sprintf(`{"contract_version":3,"command":"schema plan","outcome":"no-changes","plan_digest":"sha256:%s","plan_path":%q}`,
						strings.Repeat("c", 64), argumentAfter(spec, "--output"))
				},
			}},
			code: "invalid_plan_output",
		},
		{
			name: "a report naming the plan with no digest",
			responses: []scriptedResponse{{
				savePlan: plan,
				stdoutFor: func(spec CommandSpec) string {
					return fmt.Sprintf(`{"contract_version":3,"command":"schema plan","outcome":"no-changes","plan_path":%q}`,
						argumentAfter(spec, "--output"))
				},
			}},
			code: "invalid_plan_output",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("note-plan-refused")),
				Executor: &scriptedExecutor{t: t, responses: test.responses}, TempDir: t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("Run() error = %#v, want %s", result.Error, test.code)
			}
		})
	}
}

// A plan with statements and a note is a plan as before: the note rides in the
// plan's bytes, which its digest covers, and runs nothing.
func TestAPlanWithStatementsAndANoteIsPublished(t *testing.T) {
	t.Parallel()

	plan := notePlanDocument(t, `[{"sql":"CREATE TABLE example (id bigint)","severity":"safe","reason":"new table"}]`,
		note(1, "WARNING: one change was not planned."))
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("note-and-statements")),
		Executor: &scriptedExecutor{t: t, responses: stablePlanResponses(t, plan)}, TempDir: t.TempDir(),
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges || result.PlanContentDigest != sha256Digest([]byte(plan)) {
		t.Fatalf("Run() = %#v, want the plan published with its note", result)
	}
}
