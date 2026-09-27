package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// EnvRunnerProtocolVersion is the runner protocol the manager that built the
// Job speaks. The builder writes runner.ProtocolVersion into every runner
// container, the one that installs nothing: the guard that authorizes OCI
// access and the one that starts the executor.
//
// The runner is installed from execution.runnerImage, which the chart takes
// from whoever installs, and nothing else ties that image to the manager. A
// runner that speaks another protocol enforces another contract, and a plan
// and its approval bind the protocol, not the image. So the runner compares
// the two before it does anything else, and refuses a Job that names another
// protocol, or none, before the executor starts.
const EnvRunnerProtocolVersion = "PTAH_RUNNER_PROTOCOL_VERSION"

// CodeRunnerProtocolMismatch is the error code of that refusal. It is the one
// code a manager reads from a runner of any protocol version: see
// ProtocolMismatchError.
const CodeRunnerProtocolMismatch = "runner_protocol_mismatch"

// ErrRunnerProtocolMismatch reports a runner that refused a Job because the
// Job expected another runner protocol.
var ErrRunnerProtocolMismatch = errors.New("the runner speaks another protocol than the Job expects")

// CheckProtocolBinding refuses an environment that does not name this
// runner's protocol version.
func CheckProtocolBinding(environment []string) error {
	return checkProtocolBinding(environmentMap(environment))
}

func checkProtocolBinding(values map[string]string) error {
	value, present := values[EnvRunnerProtocolVersion]
	if !present || value == "" {
		return fmt.Errorf("the Job names no runner protocol version; this runner speaks protocol %d", ProtocolVersion)
	}
	expected, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(expected) != value || expected < 1 {
		return fmt.Errorf("the Job names runner protocol %q, which is not a protocol version; this runner speaks protocol %d",
			value, ProtocolVersion)
	}
	if expected != ProtocolVersion {
		return fmt.Errorf("the Job expects runner protocol %d; this runner speaks protocol %d", expected, ProtocolVersion)
	}
	return nil
}

// ProtocolMismatchError is the refusal a runner of another protocol version
// framed. A frame is held to the reader's own ProtocolVersion, so this is the
// one document read across versions, and it is read only in one exact shape:
//
//	{"protocolVersion":N,"operation":...,"operationId":...,"childExitCode":-1,
//	 "stdout":"","error":{"code":"runner_protocol_mismatch","message":...}}
//
// with no other key. That shape says the runner stopped before it started
// anything; a frame from another version that says more is not read at all,
// because what it says is written in a contract this reader does not hold.
// Every protocol version has to keep writing the refusal in exactly this
// shape, which TestProtocolRefusalDocumentIsPinned holds.
type ProtocolMismatchError struct {
	// RunnerVersion is the protocol the refusing runner speaks.
	RunnerVersion int
	// Message is the runner's own account of the refusal.
	Message string
}

func (e *ProtocolMismatchError) Error() string {
	return fmt.Sprintf("the runner speaks protocol %d and refused a Job built for protocol %d before it started the executor",
		e.RunnerVersion, ProtocolVersion)
}

func (e *ProtocolMismatchError) Unwrap() error { return ErrRunnerProtocolMismatch }

// protocolRefusalKeys are the keys of the refusal document, and all of them.
var protocolRefusalKeys = []string{"childExitCode", "error", "operation", "operationId", "protocolVersion", "stdout"}

// foreignProtocolRefusal reads a payload whose protocol version is not this
// build's as a protocol refusal, and reports false for anything else.
func foreignProtocolRefusal(payload []byte, result Result, options ParseOptions) (*ProtocolMismatchError, bool) {
	if result.ProtocolVersion < 1 || result.ProtocolVersion == ProtocolVersion {
		return nil, false
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, protocolRefusalKeys) {
		return nil, false
	}
	var refusal map[string]json.RawMessage
	if err := json.Unmarshal(document["error"], &refusal); err != nil || len(refusal) != 2 {
		return nil, false
	}
	if _, ok := refusal["code"]; !ok {
		return nil, false
	}
	if _, ok := refusal["message"]; !ok {
		return nil, false
	}
	if !result.Operation.Valid() || result.OperationID == "" || len(result.OperationID) > 256 ||
		hasControlCharacter(result.OperationID) || result.ChildExitCode != -1 || result.Stdout != "" ||
		result.Error == nil || result.Error.Code != CodeRunnerProtocolMismatch || result.Error.Message == "" ||
		int64(len(result.Error.Message)) > maxErrorMessageBytes {
		return nil, false
	}
	if options.ExpectedOperation != "" && result.Operation != options.ExpectedOperation {
		return nil, false
	}
	if options.ExpectedOperationID != "" && result.OperationID != options.ExpectedOperationID {
		return nil, false
	}
	return &ProtocolMismatchError{RunnerVersion: result.ProtocolVersion, Message: result.Error.Message}, true
}
