package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A migration child this process stopped is read like one that stopped on its
// own: what it wrote decides what the frame claims about the database, and
// only a document it did not finish leaves the claim at started and uncertain.
func TestMigrationApplyReadsTheReportOfAChildItStopped(t *testing.T) {
	t.Parallel()

	half := migrationRunDocument("failed")
	half = half[:len(half)/2]
	for _, row := range []struct {
		name            string
		stdout          string
		err             error
		wantReport      string
		wantStarted     bool
		wantUncertain   bool
		wantMessagePart string
	}{
		{
			name:   "stopped because the Pod is terminating, between two files",
			stdout: migrationRunDocument("failed"), err: context.Canceled,
			wantReport: "failed", wantStarted: true, wantUncertain: false,
			wantMessagePart: "because the Pod is terminating",
		},
		{
			name:   "stopped at the execution deadline, between two files",
			stdout: migrationRunDocument("failed"), err: context.DeadlineExceeded,
			wantReport: "failed", wantStarted: true, wantUncertain: false,
			wantMessagePart: "at its execution deadline",
		},
		{
			name:   "stopped inside a file it could not roll back",
			stdout: migrationRunDocument("partial"), err: context.Canceled,
			wantReport: "partial", wantStarted: true, wantUncertain: true,
		},
		{
			// Killed before it said anything: nothing narrows the claim.
			name: "killed before it wrote a report", err: context.Canceled,
			wantStarted: true, wantUncertain: true,
		},
		{
			// Killed while it wrote one: half a document is not a document.
			name: "killed part way through its report", stdout: half, err: context.Canceled,
			wantStarted: true, wantUncertain: true,
		},
		{
			// Any other failure to run the child is not a stop, and nothing it
			// may have written is read.
			name:   "the child could not be run",
			stdout: migrationRunDocument("failed"), err: errors.New("execute ptah process: exec format error"),
			wantStarted: true, wantUncertain: true,
			wantMessagePart: "could not be completed",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: row.stdout, exitCode: -1, err: row.err,
			}}}
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: migrationApplyEnvironment(t, "migration-apply-stopped"),
				Executor:    executor,
			})
			if len(executor.calls) != 1 {
				t.Fatalf("executor calls = %d, want the child to have run", len(executor.calls))
			}
			if result.Error == nil || result.Error.Code != "execution_error" {
				t.Fatalf("Run() error = %#v, want execution_error", result.Error)
			}
			if !strings.Contains(result.Error.Message, row.wantMessagePart) {
				t.Fatalf("Run() error message = %q, want it to say %q", result.Error.Message, row.wantMessagePart)
			}
			switch {
			case row.wantReport == "" && result.MigrationRun != nil:
				t.Fatalf("Run() read a report %#v from a child that left none", result.MigrationRun)
			case row.wantReport != "" && (result.MigrationRun == nil || result.MigrationRun.Outcome != row.wantReport):
				t.Fatalf("Run() migration run = %#v, want outcome %q", result.MigrationRun, row.wantReport)
			}
			if result.MutationStarted != row.wantStarted || result.Uncertain != row.wantUncertain {
				t.Fatalf("Run() MutationStarted=%t Uncertain=%t, want %t and %t",
					result.MutationStarted, result.Uncertain, row.wantStarted, row.wantUncertain)
			}
			if _, err := MarshalFrame(result); err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
		})
	}
}

// The stop delay is two thirds of the grace, so the rest of it is the
// runner's to decode, summarize and frame, for every grace the API allows.
func TestChildStopDelayLeavesTheRunnerAThirdOfTheGrace(t *testing.T) {
	t.Parallel()

	if got := ChildStopDelay(DefaultTerminationGracePeriod); got != 20*time.Second {
		t.Fatalf("ChildStopDelay(%s) = %s, want 20s", DefaultTerminationGracePeriod, got)
	}
	checked := 0
	for grace := time.Second; grace <= MaxTerminationGracePeriod; grace += time.Second {
		delay := ChildStopDelay(grace)
		if delay <= 0 || delay >= grace || grace-delay < grace/3 {
			t.Fatalf("ChildStopDelay(%s) = %s, want a positive delay leaving at least a third of the grace", grace, delay)
		}
		checked++
	}
	if checked != 300 {
		t.Fatalf("checked %d graces, want every whole second from 1s to 300s", checked)
	}
	if got := ChildStopDelay(0); got != 0 {
		t.Fatalf("ChildStopDelay(0) = %s, want no delay for no grace", got)
	}
}

func TestTerminationGracePeriodIsRequiredOfAMutatingPod(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		value     *string
		operation Operation
		want      time.Duration
		wantErr   bool
	}{
		{name: "a mutating Pod names its grace", value: ptrTo("45"), operation: OperationMigrationApply, want: 45 * time.Second},
		{name: "a schema Apply names its grace", value: ptrTo("30"), operation: OperationApply, want: 30 * time.Second},
		{name: "a mutating Pod that names none", operation: OperationApply, wantErr: true},
		{name: "a mutating Pod that names an empty one", value: ptrTo(""), operation: OperationMigrationApply, wantErr: true},
		{name: "a read-only Pod takes the default", operation: OperationPlan, want: DefaultTerminationGracePeriod},
		{name: "a read-only Pod may name one", value: ptrTo("1"), operation: OperationResolve, want: time.Second},
		{name: "zero", value: ptrTo("0"), operation: OperationApply, wantErr: true},
		{name: "past the API's bound", value: ptrTo("301"), operation: OperationApply, wantErr: true},
		{name: "negative", value: ptrTo("-30"), operation: OperationApply, wantErr: true},
		{name: "a duration rather than seconds", value: ptrTo("30s"), operation: OperationApply, wantErr: true},
		{name: "a fraction", value: ptrTo("1.5"), operation: OperationResolve, wantErr: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			values := map[string]string{}
			if row.value != nil {
				values[envTerminationGracePeriod] = *row.value
			}
			got, err := terminationGracePeriod(values, row.operation)
			if row.wantErr {
				if err == nil {
					t.Fatalf("terminationGracePeriod() = %s, want a refusal", got)
				}
				return
			}
			if err != nil || got != row.want {
				t.Fatalf("terminationGracePeriod() = %s, %v, want %s", got, err, row.want)
			}
		})
	}
}

// A mutating Pod that does not say how long it has to stop is refused before
// the child starts, as one that does not name its deadlines is.
func TestMutatingOperationsRefuseAPodWithoutItsGraceBeforeDispatch(t *testing.T) {
	t.Parallel()

	for _, operation := range []Operation{OperationApply, OperationMigrationApply} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			environment := environmentWithout(migrationApplyEnvironment(t, "missing-grace"), envTerminationGracePeriod)
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{Operation: operation, Environment: environment, Executor: executor})
			if result.Error == nil || result.Error.Code != "missing_termination_grace" {
				t.Fatalf("Run() error = %#v, want missing_termination_grace", result.Error)
			}
			if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
				t.Fatalf("a Pod without its grace dispatched a mutation: calls=%d result=%#v", len(executor.calls), result)
			}
		})
	}
}

// The grace is the runner's to read and nobody else's: Ptah is not told it.
func TestTheChildIsNotToldTheGrace(t *testing.T) {
	t.Parallel()

	var childEnvironment []string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout:  migrationRunDocument("applied"),
		inspect: func(spec CommandSpec) { childEnvironment = spec.Env },
	}}}
	Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: migrationApplyEnvironment(t, "grace-not-inherited"),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	if len(executor.calls) != 1 {
		t.Fatalf("executor calls = %d, want the child to have run", len(executor.calls))
	}
	for _, entry := range childEnvironment {
		if strings.HasPrefix(entry, envTerminationGracePeriod+"=") {
			t.Fatalf("the child inherited %s", entry)
		}
	}
	if !strings.Contains(fmt.Sprint(migrationApplyEnvironment(t, "control")), envTerminationGracePeriod+"=") {
		t.Fatal("the fixture never set the grace, so its absence from the child proves nothing")
	}
}

func ptrTo[T any](value T) *T { return &value }
