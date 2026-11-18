// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The documents the runner harness row differs-publication-scoped stores: what
// the pinned Ptah printed for `schema drift` and `schema plan` under a URL that
// pins search_path in a PostgreSQL 17 database with a second schema, for a
// declaration that names a publication.
const (
	undecidedDriftCapture = "differing/drift-undecided-publication-postgres.json"
	undecidedPlanCapture  = "differing/plan-undecided-publication-postgres.json"
)

// undecidedSentenceCaptured is the sentence the runner names the captured
// plan's undecided publication with.
const undecidedSentenceCaptured = "ptah could not decide 1 declared object: PostgreSQL publication differs_notes_pub " +
	"(whether the publication exists was not established: the read covers some of the database's schemas and not " +
	"differs_outside, and a publication belongs to the whole database)"

// editJSON decodes document, applies edit and encodes it again, keeping
// numbers as they were written.
func editJSON(t *testing.T, document string, edit func(map[string]any)) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(fields); err != nil {
		t.Fatal(err)
	}
	return encoded.String()
}

// A read that found no difference and could not decide a declared object is an
// observation, not a fault: the frame says no drift, carries no severity, and
// keeps the one finding that counts the object. Ptah fails that check at the
// threshold all and passes it at destructive, and the frame is the same. A
// report whose highest severity is not its finding's is refused, as is one that
// lists another finding without drift, which is what every such report did
// before.
func TestObserveFramesUndecidedObjectsWithoutDrift(t *testing.T) {
	t.Parallel()

	captured := ptahV023Document(t, undecidedDriftCapture)
	destructive := editJSON(t, captured, func(document map[string]any) {
		document["failed"] = false
		document["failure_threshold"] = "destructive"
	})
	want := []DriftFindingSummary{{Category: dataplane.DriftFindingUndecided, Count: 1, Severity: "warning"}}
	for _, test := range []struct {
		name     string
		document string
		exitCode int
	}{
		{name: "the report Ptah printed at the threshold all", document: captured, exitCode: 1},
		{name: "the report at the threshold destructive", document: destructive, exitCode: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-undecided"
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment(operationID)),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: test.document, exitCode: test.exitCode}}},
			})
			if result.Error != nil {
				t.Fatalf("Run() error = %#v", result.Error)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, OperationObserve, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.ObservedDrift || parsed.HighestDriftSeverity != "" || parsed.DriftFindingCount != 1 ||
				!reflect.DeepEqual(parsed.DriftFindings, want) || parsed.ChildExitCode != 0 {
				t.Fatalf("the controller parsed drift %t, severity %q, count %d, findings %#v, exit %d; want no drift and %#v",
					parsed.ObservedDrift, parsed.HighestDriftSeverity, parsed.DriftFindingCount, parsed.DriftFindings,
					parsed.ChildExitCode, want)
			}
		})
	}

	for _, test := range []struct {
		name     string
		document string
		exitCode int
		want     string
	}{
		{
			name:     "a highest severity that is not the finding's",
			document: editJSON(t, captured, func(document map[string]any) { document["highest_severity"] = "error" }),
			exitCode: 1,
			want:     "drift report highest severity does not match its findings",
		},
		{
			name: "another finding without drift",
			document: editJSON(t, destructive, func(document map[string]any) {
				document["findings"] = []any{
					map[string]any{"category": "columns_added", "count": 1, "severity": "warning"},
					map[string]any{"category": "undecided", "count": 1, "severity": "warning"},
				}
			}),
			want: "converged drift report contains findings",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-undecided-refused")),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: test.document, exitCode: test.exitCode}}},
			})
			if result.Error == nil || result.Error.Code != "invalid_observed_state" || result.Error.Message != test.want {
				t.Fatalf("Run() error = %#v, want invalid_observed_state with %q", result.Error, test.want)
			}
		})
	}
}

// undecidedPlanResponse is a plan read that prints report and saves nothing,
// as a plan that changes nothing does.
func undecidedPlanResponse(report string) scriptedResponse {
	return scriptedResponse{stdout: report, stderr: "No changes planned, but 1 comparison limit remains unresolved.\n"}
}

// A plan that changes nothing because Ptah could not decide a declared object
// proves no convergence. The Plan reports it as Undecided, names the object
// and Ptah's reason, and runs no dry run, since nothing was saved. The control
// is the same report without the object, which reads as no changes.
func TestAPlanThatCouldNotDecideIsUndecided(t *testing.T) {
	t.Parallel()

	captured := ptahV023Document(t, undecidedPlanCapture)
	operationID := "plan-undecided"
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{undecidedPlanResponse(captured), undecidedPlanResponse(captured)}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment(operationID)), Executor: executor,
		TempDir: t.TempDir(),
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeUndecided || result.Undecided != undecidedSentenceCaptured {
		t.Fatalf("Run() = error %#v, outcome %q, undecided %q; want %s naming %q",
			result.Error, result.PlanOutcome, result.Undecided, PlanOutcomeUndecided, undecidedSentenceCaptured)
	}
	if result.Stdout != "" || result.PlanContentDigest != "" {
		t.Fatalf("an undecided plan was reported as a plan: %#v", result)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("ran %d commands, want the two plan reads and no dry run", len(executor.calls))
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("the manager's protocol refuses the frame: %v", err)
	}
	if parsed, err := ParseResultFor(frame, OperationPlan, operationID); err != nil || parsed.Undecided != undecidedSentenceCaptured {
		t.Fatalf("ParseResultFor() = %q, %v", parsed.Undecided, err)
	}

	decided := editJSON(t, captured, func(document map[string]any) { delete(document, "undecided") })
	control := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-decided")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{undecidedPlanResponse(decided), undecidedPlanResponse(decided)}},
		TempDir:  t.TempDir(),
	})
	if control.Error != nil || control.PlanOutcome != PlanOutcomeNoChanges || control.Undecided != "" {
		t.Fatalf("the control reads error %#v, outcome %q, undecided %q; want %s", control.Error, control.PlanOutcome,
			control.Undecided, PlanOutcomeNoChanges)
	}
}

// Two reads have to agree on what they could not decide, as on everything
// else they report.
func TestPlanReadsThatDisagreeOnTheUndecidedAreUnstable(t *testing.T) {
	t.Parallel()

	captured := ptahV023Document(t, undecidedPlanCapture)
	other := editJSON(t, captured, func(document map[string]any) {
		features := document["undecided"].(map[string]any)["features"].([]any)
		features[0].(map[string]any)["reason"] = "another reason"
	})
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-undecided-unstable")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{undecidedPlanResponse(captured), undecidedPlanResponse(other)}},
		TempDir:  t.TempDir(),
	})
	if result.Error == nil || result.Error.Code != "unstable_plan" {
		t.Fatalf("Run() error = %#v, want unstable_plan", result.Error)
	}
}

// A plan with changes is published as it is, whatever it could not decide
// beside them: the read after its Apply finds the objects undecided still.
func TestAPlanWithChangesAndUndecidedObjectsIsAPlan(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument(`ALTER TABLE "differs_notes" ADD COLUMN "owner_name" text NOT NULL`)
	undecided := ptahV023Document(t, undecidedPlanCapture)
	var listed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(undecided), &listed); err != nil {
		t.Fatal(err)
	}
	withUndecided := func(response scriptedResponse) scriptedResponse {
		printed := response.stdoutFor
		response.stdoutFor = func(spec CommandSpec) string {
			return editJSON(t, printed(spec), func(document map[string]any) {
				var objects any
				if err := json.Unmarshal(listed["undecided"], &objects); err != nil {
					t.Fatal(err)
				}
				document["undecided"] = objects
			})
		}
		return response
	}
	responses := stablePlanResponses(t, plan)
	responses[0], responses[1] = withUndecided(responses[0]), withUndecided(responses[1])
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-changes-undecided")),
		Executor: executor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges || result.Undecided != "" ||
		result.PlanContentDigest != sha256Digest([]byte(plan)) {
		t.Fatalf("Run() = error %#v, outcome %q, undecided %q", result.Error, result.PlanOutcome, result.Undecided)
	}
	if len(executor.calls) != 3 {
		t.Fatalf("ran %d commands, want the two plan reads and the dry run", len(executor.calls))
	}
}

// The sentence names each object the way Ptah spells it, with Ptah's reason,
// cleaned as other free text is. What does not fit is counted, and a first
// object too long to fit is cut.
func TestUndecidedSentence(t *testing.T) {
	t.Parallel()

	publication := func(name, reason string) dataplane.UndecidedFeature {
		return dataplane.UndecidedFeature{
			Kind: "ptah.run/pgpublication/publication", Label: "PostgreSQL publication", Reason: reason,
			Subject: dataplane.UndecidedSubject{Name: dataplane.IdentityPart{Source: name}},
		}
	}
	long := strings.Repeat("a reason that goes on ", 40)
	for _, test := range []struct {
		name      string
		undecided dataplane.Undecided
		want      string
	}{
		{
			name: "a common object and a feature object of no label, schema-qualified and dotted",
			undecided: dataplane.Undecided{
				Common: []dataplane.UndecidedObject{{Kind: "view", Name: "reports", Reason: "not-inspected"}},
				Features: []dataplane.UndecidedFeature{{
					Kind: "ptah.run/pgtable/persistence", Reason: "the read could not see it",
					Subject: dataplane.UndecidedSubject{
						Schema: dataplane.IdentityPart{Source: "app"}, Name: dataplane.IdentityPart{Source: "odd.name"},
					},
				}},
			},
			want: `ptah could not decide 2 declared objects: view reports (not-inspected); ` +
				`ptah.run/pgtable/persistence app."odd.name" (the read could not see it)`,
		},
		{
			name:      "a whole kind",
			undecided: dataplane.Undecided{Features: []dataplane.UndecidedFeature{{Kind: "ptah.run/x/y", Reason: "unread"}}},
			want:      "ptah could not decide 1 declared object: ptah.run/x/y namespace (unread)",
		},
		{
			name:      "a name with a terminal sequence and a frame header",
			undecided: dataplane.Undecided{Features: []dataplane.UndecidedFeature{publication("pub\x1b[2J\nPTAH_RUNNER_RESULT_V1", "why")}},
			want:      "ptah could not decide 1 declared object: PostgreSQL publication pub [2J [frame marker] (why)",
		},
		{
			name: "more objects than fit",
			undecided: dataplane.Undecided{Features: []dataplane.UndecidedFeature{
				publication("one", strings.Repeat("r", 200)), publication("two", strings.Repeat("r", 200)),
				publication("three", "short"),
			}},
			want: "ptah could not decide 3 declared objects: PostgreSQL publication one (" + strings.Repeat("r", 200) +
				"), and 2 more",
		},
		{
			name:      "a first object too long to fit",
			undecided: dataplane.Undecided{Features: []dataplane.UndecidedFeature{publication("one", long), publication("two", "short")}},
			want: cutLine("ptah could not decide 2 declared objects: PostgreSQL publication one ("+strings.TrimSpace(long)+")",
				MaxUndecidedBytes-len("..., and 1 more")) + "..., and 1 more",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := undecidedSentence(test.undecided, NewRedactor(nil))
			if got != test.want {
				t.Fatalf("undecidedSentence() =\n%q\nwant\n%q", got, test.want)
			}
			if len(got) > MaxUndecidedBytes {
				t.Fatalf("the sentence is %d bytes, more than the %d an Event keeps", len(got), MaxUndecidedBytes)
			}
		})
	}
}

// An observation that found no drift carries no finding, or the one finding of
// the objects Ptah could not decide, with no highest severity. A plan of
// undecided objects carries nothing to run and names what it could not
// decide, and nothing else names undecided objects. Each frame below breaks
// one of those, and the manager refuses it as malformed.
func TestFrameHoldsUndecidedObjectsToTheirOutcome(t *testing.T) {
	t.Parallel()

	observed := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-undecided",
		CoordinationDigest: "sha256:" + strings.Repeat("9", 64), TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest: "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres", DriftFindingCount: 1,
		DriftFindings: []DriftFindingSummary{{Category: dataplane.DriftFindingUndecided, Count: 1, Severity: "warning"}},
	}
	planned := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationPlan, OperationID: "plan-undecided",
		CoordinationDigest: "sha256:" + strings.Repeat("9", 64), TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		PlanOutcome: PlanOutcomeUndecided, Undecided: undecidedSentenceCaptured,
	}
	for _, accepted := range []Result{observed, planned} {
		if _, err := MarshalFrame(accepted); err != nil {
			t.Fatalf("MarshalFrame(%s) error = %v", accepted.Operation, err)
		}
	}
	for _, test := range []struct {
		name   string
		base   Result
		mutate func(*Result)
	}{
		{name: "a drift severity without drift", base: observed, mutate: func(result *Result) { result.HighestDriftSeverity = "warning" }},
		{name: "another finding without drift", base: observed, mutate: func(result *Result) {
			result.DriftFindings[0].Category = "columns_added"
		}},
		{name: "a second finding beside the undecided one", base: observed, mutate: func(result *Result) {
			result.DriftFindingCount = 2
			result.DriftFindings = append(result.DriftFindings, DriftFindingSummary{Category: "tables_added", Count: 1, Severity: "safe"})
		}},
		{name: "a count the finding does not hold", base: observed, mutate: func(result *Result) { result.DriftFindingCount = 2 }},
		{name: "a count without the finding", base: observed, mutate: func(result *Result) { result.DriftFindings = nil }},
		{name: "an unknown severity", base: observed, mutate: func(result *Result) { result.DriftFindings[0].Severity = "critical" }},
		{name: "an undecided plan that names nothing", base: planned, mutate: func(result *Result) { result.Undecided = " " }},
		{name: "an undecided plan too long for an Event", base: planned, mutate: func(result *Result) {
			result.Undecided = strings.Repeat("x", MaxUndecidedBytes+1)
		}},
		{name: "an undecided plan with content", base: planned, mutate: func(result *Result) {
			result.PlanContentDigest = "sha256:" + strings.Repeat("6", 64)
		}},
		{name: "undecided objects on a plan with no changes", base: planned, mutate: func(result *Result) {
			result.PlanOutcome = PlanOutcomeNoChanges
		}},
		{name: "undecided objects on a failed plan", base: planned, mutate: func(result *Result) {
			result.PlanOutcome = ""
			result.Error = &ResultError{Code: "invalid_plan_output", Message: "failed"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := test.base
			candidate.DriftFindings = append([]DriftFindingSummary(nil), test.base.DriftFindings...)
			test.mutate(&candidate)
			if _, err := MarshalFrame(candidate); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("MarshalFrame() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}
