package runner

import (
	"context"
	"strings"
	"testing"
)

// convergedDriftReport is a drift read that found nothing, which is all a row
// that only asks whether the child ran needs from it.
const convergedDriftReport = `{"contract_version":1,"command":"schema drift","drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[],"diff":{}}`

// boundReadEnvironment is a read of databaseURL, bound to expected when it is
// not empty.
func boundReadEnvironment(operationID, databaseURL, expected string) []string {
	environment := append(environmentWithout(databaseEnvironment(operationID), envDatabaseURL), envDatabaseURL+"="+databaseURL)
	if expected != "" {
		environment = append(environment, envExpectedTargetDigest+"="+expected)
	}
	return withRunnerProtocol(environment)
}

// childReads is what a read's child is scripted to answer, so a row in which
// the child runs can finish.
func childReads(t *testing.T, operation Operation) []scriptedResponse {
	t.Helper()
	if operation == OperationPlan {
		return stablePlanResponses(t, validPlanDocument("CREATE TABLE bound_read (id bigint);"))
	}
	return []scriptedResponse{{stdout: convergedDriftReport}}
}

// The reads that prove a schema Apply carry the digest of the Apply's plan. A
// URL that names another target is refused before the child starts, so nothing
// logs in there, and the frame still names the target the URL gave, which is
// what the controller stops on. The refusal is a framed result every manager
// parses, under the same code an Apply on a moved target gets.
func TestAReadBoundToATargetRefusesAnotherBeforeItsChild(t *testing.T) {
	t.Parallel()
	const moved = "postgres://app:secret@other.example/app"
	movedDigest, err := TargetIdentityDigest(moved)
	if err != nil {
		t.Fatal(err)
	}
	if movedDigest == databaseTargetDigest(t) {
		t.Fatal("the moved URL names the bound target, so the row refuses nothing")
	}
	for _, operation := range []Operation{OperationObserve, OperationPlan, OperationMigrationHistory} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			operationID := "bound-read-" + string(operation)
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{
				Operation:   operation,
				Environment: boundReadEnvironment(operationID, moved, databaseTargetDigest(t)),
				Executor:    executor,
			})
			if result.Error == nil || result.Error.Code != "target_binding_mismatch" {
				t.Fatalf("error = %#v, want target_binding_mismatch", result.Error)
			}
			if len(executor.calls) != 0 || result.ChildExitCode != -1 {
				t.Fatalf("the child ran: calls=%d exit=%d", len(executor.calls), result.ChildExitCode)
			}
			if result.TargetIdentityDigest != movedDigest || result.CoordinationDigest != testCoordinationDigest() {
				t.Fatalf("the refusal names target %q and realm %q, want %q and %q",
					result.TargetIdentityDigest, result.CoordinationDigest, movedDigest, testCoordinationDigest())
			}
			if result.MutationStarted || result.Uncertain || result.Stdout != "" {
				t.Fatalf("a refused read claims more than it did: %#v", result)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, operation, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.TargetIdentityDigest != movedDigest || parsed.Error == nil || parsed.Error.Code != "target_binding_mismatch" {
				t.Fatalf("the parsed refusal = %#v", parsed)
			}
		})
	}
}

// The binding refuses only another target. A read of the target it names runs,
// a new password on the same login is the same target, and a read that carries
// no binding reads wherever its URL points: that is how a new target is learned,
// and how a Job built before the binding existed still runs.
func TestAReadRunsItsChildWhenItsTargetIsTheOneBoundOrNoneIs(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		url      string
		expected string
	}{
		{name: "the bound target", url: "postgres://app:secret@db.example/app", expected: "bound"},
		{name: "a new password on the bound target", url: "postgres://app:rotated@db.example/app", expected: "bound"},
		{name: "no binding, another target", url: "postgres://app:secret@other.example/app"},
	} {
		for _, operation := range []Operation{OperationObserve, OperationPlan} {
			t.Run(row.name+"/"+string(operation), func(t *testing.T) {
				t.Parallel()
				expected := row.expected
				if expected == "bound" {
					expected = databaseTargetDigest(t)
				}
				want, err := TargetIdentityDigest(row.url)
				if err != nil {
					t.Fatal(err)
				}
				executor := &scriptedExecutor{t: t, responses: childReads(t, operation)}
				result := Run(context.Background(), Config{
					Operation:   operation,
					Environment: boundReadEnvironment("bound-read-runs", row.url, expected),
					Executor:    executor,
				})
				if result.Error != nil || len(executor.calls) == 0 {
					t.Fatalf("the read did not run: calls=%d error=%#v", len(executor.calls), result.Error)
				}
				if result.TargetIdentityDigest != want {
					t.Fatalf("target = %q, want %q", result.TargetIdentityDigest, want)
				}
				for _, call := range executor.calls {
					if _, leaked := environmentMap(call.Env)[envExpectedTargetDigest]; leaked {
						t.Fatalf("the child received %s", envExpectedTargetDigest)
					}
				}
			})
		}
	}
}

// A binding that is not a digest is a Job no manager built, and it is refused as
// an Apply's is, rather than read as no binding at all.
func TestAReadRefusesABindingThatIsNotADigest(t *testing.T) {
	t.Parallel()
	for _, operation := range []Operation{OperationObserve, OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{
				Operation: operation,
				Environment: boundReadEnvironment("bound-read-malformed", "postgres://app:secret@db.example/app",
					"sha256:"+strings.Repeat("A", 64)),
				Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "missing_target_binding" || len(executor.calls) != 0 {
				t.Fatalf("Run() error = %#v with %d calls, want missing_target_binding and no child", result.Error, len(executor.calls))
			}
		})
	}
}
