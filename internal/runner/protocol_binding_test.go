package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// withRunnerProtocol names this runner's protocol in an environment, as the
// builder does for every Job it writes.
func withRunnerProtocol(environment []string) []string {
	return append(append([]string(nil), environment...), EnvRunnerProtocolVersion+"="+strconv.Itoa(ProtocolVersion))
}

// foreignFrame frames a payload the way a runner of any protocol does. The
// frame envelope is shared across protocols; the payload is not, which is why
// this does not go through MarshalFrame.
func foreignFrame(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return []byte(fmt.Sprintf("%s%d %s\n%s%s\n", frameHeader, len(payload), hex.EncodeToString(digest[:]), payload, frameFooter))
}

// TestTheRunnerRefusesAJobBuiltForAnotherProtocol runs every operation under
// a Job that names another protocol, a malformed one, or none. Each is refused
// before anything else is read, with the refusal document and nothing more,
// and the executor never starts. The control row names this runner's own
// protocol and reaches the executor.
func TestTheRunnerRefusesAJobBuiltForAnotherProtocol(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		value   *string
		refused bool
	}{
		{name: "the Job names this runner's protocol", value: ptrTo(strconv.Itoa(ProtocolVersion))},
		{name: "the Job names an earlier protocol", value: ptrTo(strconv.Itoa(ProtocolVersion - 1)), refused: true},
		{name: "the Job names a later protocol", value: ptrTo(strconv.Itoa(ProtocolVersion + 1)), refused: true},
		{name: "the Job names no protocol", refused: true},
		{name: "the Job names an empty protocol", value: ptrTo(""), refused: true},
		{name: "the Job names a padded protocol", value: ptrTo("0" + strconv.Itoa(ProtocolVersion)), refused: true},
		{name: "the Job names something else", value: ptrTo("five"), refused: true},
	} {
		for _, operation := range []Operation{OperationResolve, OperationObserve, OperationPlan, OperationApply, OperationMigrationApply} {
			t.Run(row.name+"/"+string(operation), func(t *testing.T) {
				t.Parallel()

				environment := []string{EnvOperationID + "=protocol-" + string(operation)}
				if row.value != nil {
					environment = append(environment, EnvRunnerProtocolVersion+"="+*row.value)
				}
				executor := &countingExecutor{}
				result := Run(context.Background(), Config{
					Operation:   operation,
					Environment: environment,
					Executor:    executor,
				})
				calls := executor.calls
				refused := result.Error != nil && result.Error.Code == CodeRunnerProtocolMismatch
				if refused != row.refused {
					t.Fatalf("refused = %t (%#v), want %t", refused, result.Error, row.refused)
				}
				if !row.refused {
					return
				}
				if calls != 0 {
					t.Fatalf("the executor ran %d times under a Job built for another protocol", calls)
				}
				want := Result{
					ProtocolVersion: ProtocolVersion, Operation: operation, OperationID: "protocol-" + string(operation),
					ChildExitCode: -1, Error: result.Error,
				}
				if fmt.Sprintf("%#v", result) != fmt.Sprintf("%#v", want) {
					t.Fatalf("refusal = %#v, want the refusal document and nothing else", result)
				}
			})
		}
	}
}

// TestProtocolRefusalDocumentIsPinned holds the refusal to the exact bytes a
// manager of another protocol reads. A field added to Result without
// omitempty would change it, and the manager would stop reading refusals from
// runners of this version.
func TestProtocolRefusalDocumentIsPinned(t *testing.T) {
	t.Parallel()

	result := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: []string{EnvOperationID + "=pinned", EnvRunnerProtocolVersion + "=1"},
	})
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocolVersion":` + strconv.Itoa(ProtocolVersion) + `,"operation":"apply","operationId":"pinned",` +
		`"childExitCode":-1,"stdout":"","error":{"code":"runner_protocol_mismatch",` +
		`"message":"the Job expects runner protocol 1; this runner speaks protocol ` + strconv.Itoa(ProtocolVersion) + `"}}`
	if string(payload) != want {
		t.Fatalf("refusal document =\n%s\nwant\n%s", payload, want)
	}
}

// TestAManagerReadsARefusalFromARunnerOfAnotherProtocol parses frames a runner
// of another protocol wrote. The refusal document is read, bound to the
// operation the manager expects; every other shape from that runner is not,
// and neither is the refusal of another operation.
func TestAManagerReadsARefusalFromARunnerOfAnotherProtocol(t *testing.T) {
	t.Parallel()

	foreign := ProtocolVersion + 1
	refusal := func(mutate func(map[string]any)) []byte {
		document := map[string]any{
			"protocolVersion": foreign, "operation": "apply", "operationId": "apply-1",
			"childExitCode": -1, "stdout": "",
			"error": map[string]any{"code": CodeRunnerProtocolMismatch, "message": "the Job expects runner protocol 5"},
		}
		if mutate != nil {
			mutate(document)
		}
		payload, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return foreignFrame(payload)
	}
	for _, row := range []struct {
		name     string
		frame    []byte
		mismatch bool
	}{
		{name: "the refusal document", frame: refusal(nil), mismatch: true},
		{name: "a refusal for another operation", frame: refusal(func(d map[string]any) { d["operationId"] = "apply-2" })},
		{name: "a refusal of another operation type", frame: refusal(func(d map[string]any) { d["operation"] = "plan" })},
		{name: "a refusal that started the child", frame: refusal(func(d map[string]any) { d["childExitCode"] = 0 })},
		{name: "a refusal that says more", frame: refusal(func(d map[string]any) { d["mutationStarted"] = true })},
		{name: "a refusal with output", frame: refusal(func(d map[string]any) { d["stdout"] = "applied" })},
		{name: "another error from that runner", frame: refusal(func(d map[string]any) {
			d["error"] = map[string]any{"code": "apply_refused", "message": "no"}
		})},
		{name: "a refusal whose error says more", frame: refusal(func(d map[string]any) {
			d["error"] = map[string]any{"code": CodeRunnerProtocolMismatch, "message": "m", "detail": "x"}
		})},
		{name: "a refusal claiming protocol zero", frame: refusal(func(d map[string]any) { d["protocolVersion"] = 0 })},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseResultFor(row.frame, OperationApply, "apply-1")
			var mismatch *ProtocolMismatchError
			if got := errors.As(err, &mismatch); got != row.mismatch {
				t.Fatalf("ParseResultFor() error = %v, want a protocol mismatch = %t", err, row.mismatch)
			}
			if row.mismatch && (mismatch.RunnerVersion != foreign || !errors.Is(err, ErrRunnerProtocolMismatch)) {
				t.Fatalf("mismatch = %#v", mismatch)
			}
			if !row.mismatch && err == nil {
				t.Fatal("a foreign frame was read as this protocol's result")
			}
		})
	}

	// A frame of this protocol in the same log wins: it is this protocol's
	// account, and the foreign refusal is only what is left when there is none.
	own, err := MarshalFrame(Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-1", ChildExitCode: -1,
		Error: &ResultError{Code: "dispatch_deadline_expired", Message: "the dispatch deadline expired"},
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := append(append([]byte(nil), refusal(nil)...), own...)
	if result, err := ParseResultFor(logs, OperationApply, "apply-1"); err != nil ||
		result.Error == nil || result.Error.Code != "dispatch_deadline_expired" {
		t.Fatalf("ParseResultFor(refusal, own) = %#v, %v; want this protocol's frame", result, err)
	}
}

// TestTheGuardRefusesAnotherProtocol holds the OCI guard to the same binding.
func TestTheGuardRefusesAnotherProtocol(t *testing.T) {
	t.Parallel()

	for value, refused := range map[string]bool{
		strconv.Itoa(ProtocolVersion):     false,
		strconv.Itoa(ProtocolVersion + 1): true,
		"":                                true,
	} {
		err := CheckProtocolBinding([]string{EnvRunnerProtocolVersion + "=" + value})
		if (err != nil) != refused {
			t.Fatalf("CheckProtocolBinding(%q) = %v, want refused = %t", value, err, refused)
		}
		if refused && !strings.Contains(err.Error(), "this runner speaks protocol") {
			t.Fatalf("refusal %q does not name this runner's protocol", err)
		}
	}
}

// countingExecutor counts the children a run started, and starts none.
type countingExecutor struct{ calls int }

func (e *countingExecutor) Execute(context.Context, CommandSpec, io.Writer, io.Writer) (int, error) {
	e.calls++
	return 1, errors.New("the fixture carries no other input")
}
