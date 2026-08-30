package runner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestFrameRoundTripFromMixedLogs(t *testing.T) {
	t.Parallel()

	wanted := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationObserve,
		OperationID:     "observe-42",
		ChildExitCode:   1,
		Stdout:          `{"drift":true}`,
	}
	frame, err := MarshalFrame(wanted)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	logs := append([]byte("unrelated diagnostics\nPTAH_RUNNER_RESULT_V1 not-a-frame\n"), frame...)
	logs = append(logs, []byte("trailing log line\n")...)

	got, err := ParseResultFor(logs, OperationObserve, "observe-42")
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if got != wanted {
		t.Fatalf("ParseResultFor() = %#v, want %#v", got, wanted)
	}
}

func TestFrameRejectsTruncationAndDigestMismatch(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationPlan,
		OperationID:     "plan-7",
		ChildExitCode:   0,
		Stdout:          "payload-original",
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}

	truncated := frame[:len(frame)-8]
	if _, err := ParseResult(truncated); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResult(truncated) error = %v, want ErrMalformedFrame", err)
	}

	tampered := bytes.Replace(frame, []byte("payload-original"), []byte("payload-tampered"), 1)
	if len(tampered) != len(frame) {
		t.Fatal("test mutation unexpectedly changed frame length")
	}
	if _, err := ParseResult(tampered); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResult(tampered) error = %v, want ErrMalformedFrame", err)
	}
}

func TestFrameRejectsMissingMalformedOversizedAndMismatchedBindings(t *testing.T) {
	t.Parallel()

	if _, err := ParseResult([]byte("ordinary logs")); !errors.Is(err, ErrFrameNotFound) {
		t.Fatalf("ParseResult(missing) error = %v, want ErrFrameNotFound", err)
	}
	if _, err := ParseResult([]byte(frameHeader + "bogus\n{}" + frameFooter)); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResult(malformed) error = %v, want ErrMalformedFrame", err)
	}

	result := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "resolve-1",
		ChildExitCode:   0,
		Stdout:          strings.Repeat("x", 128),
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if _, err := ParseResultWithLimit(frame, 16); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ParseResultWithLimit() error = %v, want ErrFrameTooLarge", err)
	}
	if _, err := ParseResultFor(frame, OperationVerify, result.OperationID); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultFor(operation mismatch) error = %v, want ErrMalformedFrame", err)
	}
	if _, err := ParseResultFor(frame, result.Operation, "different-id"); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultFor(ID mismatch) error = %v, want ErrMalformedFrame", err)
	}
}
