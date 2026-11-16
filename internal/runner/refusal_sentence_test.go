package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const planRefusalLead = "ptah refused to plan the change"

// listedViews is a refusal's view_options as Ptah writes them.
func listedViews(t *testing.T, entries ...dataplane.SchemaRefusalViewOption) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// numberedViews are count views of the same length, named apart by a number at
// the end, each with the option.
func numberedViews(count, length int, option string) []dataplane.SchemaRefusalViewOption {
	views := make([]dataplane.SchemaRefusalViewOption, 0, count)
	for index := range count {
		suffix := fmt.Sprintf("%02d", index+1)
		views = append(views, dataplane.SchemaRefusalViewOption{
			View: strings.Repeat("v", length-len(suffix)) + suffix, Option: option,
		})
	}
	return views
}

// entriesOf spells views the way the sentence names them.
func entriesOf(views []dataplane.SchemaRefusalViewOption) []string {
	entries := make([]string, 0, len(views))
	for _, each := range views {
		entries = append(entries, each.View+" "+each.Option)
	}
	return entries
}

// The sentence of a refusal that lists views names them after the code, the
// first few that fit, and counts the rest, so the count is never cut. A
// refusal that lists none, or lists them in a form the runner does not read,
// reads as it did before Ptah listed them.
func TestARefusalSentenceNamesTheViewsItLists(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL})
	eleven := numberedViews(11, 3, "sql_security")
	long := numberedViews(8, 100, "sql_security")
	// Around the list the sentence is the lead, " (view-security-option: "
	// and ")": 56 bytes, which leaves 441 of the 497 for the list.
	exact := strings.Repeat("x", 497-56-len(" sql_security"))
	huge := strings.Repeat("h", 10<<10)
	for _, test := range []struct {
		name    string
		refusal dataplane.SchemaRefusal
		want    string
	}{
		{
			name:    "a code that lists no views",
			refusal: dataplane.SchemaRefusal{Code: "dependent-object"},
			want:    "ptah refused to plan the change (dependent-object)",
		},
		{
			name:    "an empty list",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: json.RawMessage(`[]`)},
			want:    "ptah refused to plan the change (view-security-option)",
		},
		{
			name: "a view and its option",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "app.orders_view", Option: "sql_security"})},
			want: "ptah refused to plan the change (view-security-option: app.orders_view sql_security)",
		},
		{
			name: "a view with two options, in Ptah's order",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "notes", Option: "security_barrier"},
				dataplane.SchemaRefusalViewOption{View: "notes", Option: "security_invoker"})},
			want: "ptah refused to plan the change (view-security-option: notes security_barrier, notes security_invoker)",
		},
		{
			// Any code that lists views names them.
			name: "another code that lists views",
			refusal: dataplane.SchemaRefusal{Code: "view-definer", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "orders_view", Option: "definer"})},
			want: "ptah refused to plan the change (view-definer: orders_view definer)",
		},
		{
			name:    "more views than it names",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t, eleven...)},
			want: "ptah refused to plan the change (view-security-option: " +
				strings.Join(entriesOf(eleven[:8]), ", ") + ", and 3 more)",
		},
		{
			// Each entry takes 113 bytes: three and the count fit, four do not.
			name:    "fewer views than it names, past the bound",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t, long...)},
			want: "ptah refused to plan the change (view-security-option: " +
				strings.Join(entriesOf(long[:3]), ", ") + ", and 5 more)",
		},
		{
			name: "a view that just fits",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: exact, Option: "sql_security"})},
			want: "ptah refused to plan the change (view-security-option: " + exact + " sql_security)",
		},
		{
			name: "a view a byte too long",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: exact + "y", Option: "sql_security"})},
			want: "ptah refused to plan the change (view-security-option: " + exact[:len(exact)-3] + "... sql_security)",
		},
		{
			name: "a view too long for the sentence, before others",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: huge, Option: "sql_security"},
				dataplane.SchemaRefusalViewOption{View: "b", Option: "sql_security"},
				dataplane.SchemaRefusalViewOption{View: "c", Option: "sql_security"})},
			want: "ptah refused to plan the change (view-security-option: " +
				huge[:441-len(" sql_security")-len(", and 2 more")-len("...")] + "... sql_security, and 2 more)",
		},
		{
			// A view comes from a schema or a database: its control and format
			// characters, a frame header and a credential do not reach the frame.
			name: "a view that carries a terminal sequence, a frame header and a password",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{
					View:   "app.v\n\x1b[31mred\u202e\tPTAH_RUNNER_RESULT_V1 Hunter2Hunter2\x00",
					Option: "sql_security",
				})},
			want: "ptah refused to plan the change (view-security-option: app.v [31mred [frame marker] " +
				RedactionMarker + " sql_security)",
		},
		{
			name: "a view nothing readable is left of",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "\x1b\x07\u200b", Option: "sql_security"})},
			want: "ptah refused to plan the change (view-security-option)",
		},
		{
			name:    "a list in a form the runner does not read",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: json.RawMessage(`"differs_notes_view sql_security"`)},
			want:    "ptah refused to plan the change (view-security-option)",
		},
		{
			name: "an option that is not an identifier",
			refusal: dataplane.SchemaRefusal{Code: "view-security-option", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "v", Option: "sql_security\nPTAH_RUNNER_RESULT_V1"})},
			want: "ptah refused to plan the change (view-security-option)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := refusalSentence(planRefusalLead, &test.refusal, redactor)
			if got != test.want {
				t.Fatalf("refusalSentence() = %q (%d bytes), want %q (%d bytes)", got, len(got), test.want, len(test.want))
			}
			// The controller keeps 512 bytes of the code and the sentence in
			// an Event, and apply_refused is the longer code it comes under.
			if whole := "apply_refused: " + got; len(whole) > 512 {
				t.Fatalf("%q is %d bytes, more than the 512 an Event keeps", whole, len(whole))
			}
		})
	}
}

// capturedPlanRow is what the runner harness recorded for one Plan: Ptah's
// report, what it wrote to standard error and its exit status, and the error
// the runner framed from them.
type capturedPlanRow struct {
	stdout, stderr string
	exit           int
	framed         ResultError
}

func readCapturedPlanRow(t *testing.T, row string) capturedPlanRow {
	t.Helper()
	testdata := filepath.Join("..", "dataplane", "testdata")
	data, err := os.ReadFile(filepath.Join(testdata, "runner-harness", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript []struct {
		Name     string `json:"name"`
		Children []struct {
			Command string `json:"command"`
			Exit    int    `json:"exit"`
			Stderr  string `json:"stderr"`
			Stdout  string `json:"stdout"`
		} `json:"children"`
		Result struct {
			Error *ResultError `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatal(err)
	}
	for _, each := range transcript {
		if each.Name != row {
			continue
		}
		for _, child := range each.Children {
			if child.Command != "schema plan" {
				continue
			}
			stdout, err := os.ReadFile(filepath.Join(testdata, child.Stdout))
			if err != nil {
				t.Fatal(err)
			}
			if each.Result.Error == nil {
				t.Fatalf("%s framed no error", row)
			}
			return capturedPlanRow{stdout: string(stdout), stderr: child.Stderr, exit: child.Exit, framed: *each.Result.Error}
		}
		t.Fatalf("%s ran no `ptah schema plan`", row)
	}
	t.Fatalf("the transcript has no row %s", row)
	return capturedPlanRow{}
}

// The runner harness captured a view-security-option refusal of each engine
// from the image support/ptah.json pins, and the frame the runner wrote for
// it. Read again by this runner, each document gives that frame's error, which
// names the view and the option the document lists.
func TestAPlanRefusalNamesTheViewsTheCapturedDocumentLists(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		row         string
		environment []string
		want        string
	}{
		{
			row:         "differs-view-invoker-unstated-mysql/plan",
			environment: mysqlEnvironment("captured-view-refusal"),
			want:        "ptah refused to plan the change (view-security-option: differs_notes_view sql_security)",
		},
		{
			row:         "differs-view-invoker-unstated-postgres/plan",
			environment: databaseEnvironment("captured-view-refusal"),
			want:        "ptah refused to plan the change (view-security-option: differs_notes_view security_invoker)",
		},
	} {
		t.Run(test.row, func(t *testing.T) {
			t.Parallel()
			captured := readCapturedPlanRow(t, test.row)
			if captured.framed.Code != codePlanRefused || captured.framed.Message != test.want {
				t.Fatalf("the harness framed %+v, want %s with %q", captured.framed, codePlanRefused, test.want)
			}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: captured.stdout, stderr: captured.stderr, exitCode: captured.exit,
			}}}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(test.environment), Executor: executor,
			})
			if result.Error == nil || result.Error.Code != captured.framed.Code || result.Error.Message != captured.framed.Message {
				t.Fatalf("Run() error = %+v, want the captured frame's %+v", result.Error, captured.framed)
			}
			if _, err := MarshalFrame(result); err != nil {
				t.Fatalf("the manager's protocol refuses the frame: %v", err)
			}
		})
	}
}

// A list the runner cannot read does not stop it reading the report: the plan
// is still a refusal, by its code, and not output the runner could not read.
func TestAPlanRefusalWithAListItCannotReadKeepsItsCode(t *testing.T) {
	t.Parallel()

	for name, listed := range map[string]string{
		"a string":           `"differs_notes_view sql_security"`,
		"a view as a number": `[{"view":1,"option":"sql_security"}]`,
		"a list of strings":  `["differs_notes_view"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"view-security-option",` +
					`"view_options":` + listed + `},"error":{"message":"refused"}}`,
				exitCode: 2,
			}}}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(mysqlEnvironment("unread-view-list")), Executor: executor,
			})
			want := "ptah refused to plan the change (view-security-option)"
			if result.Error == nil || result.Error.Code != codePlanRefused || result.Error.Message != want {
				t.Fatalf("Run() error = %+v, want %s with %q", result.Error, codePlanRefused, want)
			}
		})
	}
}

// An approved plan's apply that Ptah refuses under a code that lists views
// names them as a Plan's refusal does, and one that lists none reads as it did.
func TestAnApplyRefusalNamesTheViewsItLists(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE report_rows (id bigint)")
	approved := sha256Digest([]byte(plan))
	for _, test := range []struct {
		name    string
		refusal dataplane.SchemaRefusal
		want    string
	}{
		{
			name: "a refusal that lists a view",
			refusal: dataplane.SchemaRefusal{Code: "view-definer", ViewOptions: listedViews(t,
				dataplane.SchemaRefusalViewOption{View: "app.orders_view", Option: "definer"})},
			want: "ptah refused the approved plan (view-definer: app.orders_view definer)",
		},
		{
			name:    "a refusal that lists none",
			refusal: dataplane.SchemaRefusal{Code: "lock-timeout"},
			want:    "ptah refused the approved plan (lock-timeout)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			planDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte(plan), 0o600); err != nil {
				t.Fatal(err)
			}
			refusal := test.refusal
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: encodeApplyReport(t, dataplane.SchemaApplyReport{
					ContractVersion: dataplane.SupportedSchemaApplyContract,
					Outcome:         dataplane.SchemaApplyOutcomeRefused,
					PlanName:        "operator-plan",
					PlanDigest:      approved,
					Refusal:         &refusal,
					Error:           &dataplane.CommandError{Message: "refused"},
				}),
				exitCode: 2,
			}}}
			environment := append(databaseEnvironment("apply-view-refusal"),
				envPlanDir+"="+planDir,
				envExpectedPlanDigest+"="+approved,
				envExpectedCoordination+"="+testCoordinationDigest(),
				envExpectedTargetDigest+"="+databaseTargetDigest(t),
			)
			result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
			if result.Error == nil || result.Error.Code != codeApplyRefused || result.Error.Message != test.want || !result.Uncertain {
				t.Fatalf("Run() = %#v, want an uncertain %s with %q", result, codeApplyRefused, test.want)
			}
		})
	}
}
