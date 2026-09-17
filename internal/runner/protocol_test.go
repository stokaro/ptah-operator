package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

type shortWriter struct{}

func (shortWriter) Write(content []byte) (int, error) {
	return len(content) - 1, nil
}

func TestFrameRoundTripFromMixedLogs(t *testing.T) {
	t.Parallel()

	wanted := Result{
		ProtocolVersion:      ProtocolVersion,
		Operation:            OperationObserve,
		OperationID:          "observe-42",
		ChildExitCode:        0,
		CoordinationDigest:   "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64),
		ObservedDialect:      "postgres",
		ObservedDrift:        true,
		HighestDriftSeverity: "warning",
		DriftFindingCount:    1,
		DriftFindings: []DriftFindingSummary{{
			Category: "columns_added", Count: 1, Severity: "warning",
		}},
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
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("ParseResultFor() = %#v, want %#v", got, wanted)
	}
}

// TestFrameToleratesStderrInterleavedBeforeItsFooter is the shape a container
// log actually produced, twice on Kubernetes 1.37 and again in a retained lab:
// the runner writes the whole frame in one call to standard output, the kubelet
// stores it one line per entry merged with standard error by read time, and the
// refusal text the runner had just written landed between the payload and the
// footer. The payload is bounded by the length the header declares and checked
// against its digest, so the line costs no integrity -- but the parser used to
// require the footer against the payload and refused the frame it could verify.
func TestFrameToleratesStderrInterleavedBeforeItsFooter(t *testing.T) {
	t.Parallel()

	wanted := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "sha256:" + strings.Repeat("b", 64),
		ChildExitCode:   -1,
		Error: &ResultError{
			Code:    "invalid_oci_access",
			Message: "registry certificate authority bytes do not match the credential-owner grant",
		},
	}
	frame, err := MarshalFrame(wanted)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	footer := []byte(frameFooter)
	split := bytes.Index(frame, footer)
	if split < 0 {
		t.Fatal("marshalled frame carries no footer")
	}
	diagnostic := []byte("\nptah-runner: registry certificate authority bytes do not match the credential-owner grant")
	interleaved := append(append(append([]byte{}, frame[:split]...), diagnostic...), frame[split:]...)

	got, err := ParseResultFor(interleaved, OperationResolve, wanted.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("ParseResultFor() = %#v, want %#v", got, wanted)
	}
}

// TestFrameRejectsAnUnclosedPayloadAndAPartialInterleavedLine keeps what the
// footer is for. A payload the writer never closed, and one whose trailing text
// stops mid-line, are both logs that were cut, which is the case the footer
// exists to catch.
func TestFrameRejectsAnUnclosedPayloadAndAPartialInterleavedLine(t *testing.T) {
	t.Parallel()

	wanted := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "sha256:" + strings.Repeat("c", 64),
		ChildExitCode:   -1,
		Error: &ResultError{
			Code:    "invalid_oci_access",
			Message: "registry certificate authority bytes do not match the credential-owner grant",
		},
	}
	frame, err := MarshalFrame(wanted)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	footer := []byte(frameFooter)
	split := bytes.Index(frame, footer)
	if split < 0 {
		t.Fatal("marshalled frame carries no footer")
	}
	for _, test := range []struct {
		name string
		logs []byte
	}{
		{
			name: "the writer never closed the frame",
			logs: append([]byte{}, frame[:split]...),
		},
		{
			name: "the log stops inside an interleaved line",
			logs: append(append([]byte{}, frame[:split]...), []byte("\nptah-runner: cut here")...),
		},
		{
			// The line that follows the payload is complete and is not the
			// footer. Recognizing the footer by position rather than by content
			// would accept this, which is a frame nobody closed.
			name: "a complete line follows the payload and no footer does",
			logs: append(append([]byte{}, frame[:split]...), []byte("\nptah-runner: cut here\n")...),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseResultFor(test.logs, OperationResolve, wanted.OperationID)
			if !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

func TestLegacyProtocolFourRequiresExplicitVersionBinding(t *testing.T) {
	t.Parallel()

	legacy := Result{
		ProtocolVersion: legacyProtocolVersion, Operation: OperationObserve, OperationID: "legacy-observe",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		ObservedDrift: true, HighestDriftSeverity: "warning", DriftFindingCount: 1,
	}
	frame := handcraftedIntegrityValidFrame(t, legacy)
	if _, err := MarshalFrame(legacy); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("MarshalFrame(legacy v4) error = %v, want ErrMalformedFrame", err)
	}
	if _, err := ParseResultFor(frame, OperationObserve, legacy.OperationID); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultFor(legacy v4) error = %v, want ErrMalformedFrame", err)
	}

	got, err := ParseResultWithOptions(frame, ParseOptions{
		ExpectedProtocolVersion: legacyProtocolVersion,
		ExpectedOperation:       legacy.Operation,
		ExpectedOperationID:     legacy.OperationID,
	})
	if err != nil {
		t.Fatalf("ParseResultWithOptions(explicit legacy v4) error = %v", err)
	}
	if !reflect.DeepEqual(got, legacy) {
		t.Fatalf("ParseResultWithOptions(explicit legacy v4) = %#v, want %#v", got, legacy)
	}
}

func TestProtocolFiveDriftRequiresStructuredFindings(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-missing-findings",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		ObservedDrift: true, HighestDriftSeverity: "warning", DriftFindingCount: 1,
	}
	if _, err := MarshalFrame(result); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("MarshalFrame(v5 drift without findings) error = %v, want ErrMalformedFrame", err)
	}
	frame := handcraftedIntegrityValidFrame(t, result)
	if _, err := ParseResultFor(frame, result.Operation, result.OperationID); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultFor(v5 drift without findings) error = %v, want ErrMalformedFrame", err)
	}
}

func TestLegacyProtocolFourRejectsStructuredFindings(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion: legacyProtocolVersion, Operation: OperationObserve, OperationID: "legacy-structured-findings",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		ObservedDrift: true, HighestDriftSeverity: "warning", DriftFindingCount: 1,
		DriftFindings: []DriftFindingSummary{{Category: "columns_added", Count: 1, Severity: "warning"}},
	}
	if _, err := ParseResultWithOptions(handcraftedIntegrityValidFrame(t, result), ParseOptions{
		ExpectedProtocolVersion: legacyProtocolVersion,
		ExpectedOperation:       result.Operation,
		ExpectedOperationID:     result.OperationID,
	}); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultWithOptions(legacy v4 with structured findings) error = %v, want ErrMalformedFrame", err)
	}
}

func TestFrameRejectsInconsistentStructuredDriftFindings(t *testing.T) {
	t.Parallel()

	base := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-findings",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		ObservedDrift: true, HighestDriftSeverity: "warning", DriftFindingCount: 2,
		DriftFindings: []DriftFindingSummary{{Category: "columns_added", Count: 2, Severity: "warning"}},
	}
	tests := []struct {
		name   string
		mutate func(*Result)
	}{
		{name: "invalid category", mutate: func(result *Result) { result.DriftFindings[0].Category = "app.users" }},
		{name: "unknown identifier category", mutate: func(result *Result) { result.DriftFindings[0].Category = "private_schema_name" }},
		{name: "zero count", mutate: func(result *Result) { result.DriftFindings[0].Count = 0 }},
		{name: "count mismatch", mutate: func(result *Result) { result.DriftFindingCount = 3 }},
		{name: "highest mismatch", mutate: func(result *Result) { result.HighestDriftSeverity = "error" }},
		{name: "duplicate", mutate: func(result *Result) {
			result.DriftFindingCount = 4
			result.DriftFindings = append(result.DriftFindings, result.DriftFindings[0])
		}},
		{name: "noncanonical order", mutate: func(result *Result) {
			result.DriftFindingCount = 3
			result.DriftFindings = []DriftFindingSummary{
				{Category: "tables_added", Count: 1, Severity: "safe"},
				{Category: "columns_added", Count: 2, Severity: "warning"},
			}
		}},
		{name: "invalid truncation", mutate: func(result *Result) { result.DriftFindingsTruncated = true }},
		{name: "non-observe", mutate: func(result *Result) {
			result.Operation = OperationResolve
			result.ResolvedDigest = "sha256:" + strings.Repeat("6", 64)
			result.ResolvedReference = "oci://registry.example/schema@" + result.ResolvedDigest
			result.ResolvedMediaType = "application/vnd.example.schema"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := base
			candidate.DriftFindings = append([]DriftFindingSummary(nil), base.DriftFindings...)
			test.mutate(&candidate)
			if _, err := MarshalFrame(candidate); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("MarshalFrame() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

func TestFrameRejectsTruncationAndDigestMismatch(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion:    ProtocolVersion,
		Operation:          OperationPlan,
		OperationID:        "plan-7",
		ChildExitCode:      0,
		Stdout:             "payload-original",
		CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		PlanContentDigest:  "sha256:" + strings.Repeat("8", 64),
		PlanOutcome:        PlanOutcomeChanges,
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
		ProtocolVersion:    ProtocolVersion,
		Operation:          OperationPlan,
		OperationID:        "plan-1",
		ChildExitCode:      0,
		Stdout:             strings.Repeat("x", 128),
		CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		PlanContentDigest:  "sha256:" + strings.Repeat("8", 64),
		PlanOutcome:        PlanOutcomeChanges,
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

func TestParserRejectsSuccessfulVerifyFrameFromPreviousProtocol(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("9", 64)
	current := Result{
		ProtocolVersion:          ProtocolVersion,
		Operation:                OperationVerify,
		OperationID:              "verify-previous-protocol",
		ChildExitCode:            0,
		ResolvedDigest:           digest,
		VerificationPolicyDigest: digest,
		ObservedArtifactType:     "application/vnd.ptah.schema.layer.v1+tar",
	}
	if _, err := ParseResultFor(
		handcraftedIntegrityValidFrame(t, current),
		current.Operation,
		current.OperationID,
	); err != nil {
		t.Fatalf("ParseResultFor(current protocol) error = %v", err)
	}

	previous := current
	previous.ProtocolVersion = legacyProtocolVersion
	frame := handcraftedIntegrityValidFrame(t, previous)
	if _, err := ParseResultFor(frame, previous.Operation, previous.OperationID); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("ParseResultFor(previous protocol) error = %v, want ErrMalformedFrame", err)
	}
	if _, err := ParseResultWithOptions(frame, ParseOptions{
		ExpectedProtocolVersion: legacyProtocolVersion,
		ExpectedOperation:       previous.Operation,
		ExpectedOperationID:     previous.OperationID,
	}); err != nil {
		t.Fatalf("ParseResultWithOptions(explicit previous protocol) error = %v", err)
	}
}

func TestWriteFrameRejectsShortWrite(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "resolve-short-write",
		ChildExitCode:   -1,
		Error:           &ResultError{Code: "test_error", Message: "test error"},
	}
	if err := WriteFrame(shortWriter{}, result); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteFrame() error = %v, want io.ErrShortWrite", err)
	}
}

func TestMarshalFrameRejectsPayloadAboveParserCap(t *testing.T) {
	// Keep this serial because it intentionally constructs a multi-megabyte
	// worst-case escaped payload.
	result := Result{
		ProtocolVersion:    ProtocolVersion,
		Operation:          OperationPlan,
		OperationID:        "plan-oversized-frame",
		ChildExitCode:      0,
		Stdout:             strings.Repeat("<", int(DefaultMaxPlanBytes)+(1<<20)),
		CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		PlanContentDigest:  "sha256:" + strings.Repeat("8", 64),
		PlanOutcome:        PlanOutcomeChanges,
	}
	if _, err := MarshalFrame(result); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("MarshalFrame() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestParserRejectsImpossibleSuccessfulResultShapes(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("9", 64)
	tests := []struct {
		name   string
		result Result
	}{
		{
			name: "apply without mutation boundary",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-no-mutation",
				ChildExitCode: 0, CoordinationDigest: digest},
		},
		{
			name: "apply with nonzero exit",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-nonzero",
				ChildExitCode: 2, CoordinationDigest: digest, MutationStarted: true},
		},
		{
			name: "apply success marked uncertain",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-uncertain",
				ChildExitCode: 0, CoordinationDigest: digest, MutationStarted: true, Uncertain: true},
		},
		{
			name: "apply success with truncation",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-truncated",
				ChildExitCode: 0, CoordinationDigest: digest, MutationStarted: true,
				Truncation: &TruncationMetadata{Stderr: true, StderrBytesDropped: 1}},
		},
		{
			name: "apply success with native output",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-output",
				ChildExitCode: 0, CoordinationDigest: digest, MutationStarted: true, Stdout: "protected SQL"},
		},
		{
			name: "read-only success with nonzero exit",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationResolve, OperationID: "resolve-nonzero",
				ChildExitCode: 1},
		},
		{
			name: "migration history success without its report",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationMigrationHistory,
				OperationID: "history-no-report", ChildExitCode: 0, CoordinationDigest: digest},
		},
		{
			name: "migration run success without its report",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationMigrationApply,
				OperationID: "run-no-report", ChildExitCode: 0, CoordinationDigest: digest, MutationStarted: true},
		},
		{
			name: "migration history report on an unrelated operation",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-history",
				ChildExitCode: 0, CoordinationDigest: digest, MigrationHistory: &dataplane.MigrationStatusReport{ContractVersion: 1}},
		},
		{
			name: "migration run report on an unrelated operation",
			result: Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-run-report",
				ChildExitCode: 0, CoordinationDigest: digest, MutationStarted: true,
				MigrationRun: &dataplane.MigrationRunReport{ContractVersion: 1, Direction: "up", Outcome: dataplane.MigrationOutcomeApplied}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			frame := handcraftedIntegrityValidFrame(t, test.result)
			if _, err := ParseResultFor(frame, test.result.Operation, test.result.OperationID); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

func TestInvalidOCIAccessRequiresExactPreChildBinding(t *testing.T) {
	t.Parallel()

	base := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "resolve-invalid-oci-access",
		ChildExitCode:   -1,
		Error: &ResultError{
			Code:    "invalid_oci_access",
			Message: "OCI access was refused before child dispatch",
		},
	}
	for _, operation := range []Operation{OperationResolve, OperationVerify} {
		operation := operation
		t.Run("valid "+string(operation), func(t *testing.T) {
			t.Parallel()
			result := base
			result.Operation = operation
			result.OperationID = string(operation) + "-invalid-oci-access"
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if _, err := ParseResultFor(frame, result.Operation, result.OperationID); err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
		})
	}

	digest := "sha256:" + strings.Repeat("9", 64)
	invalid := map[string]Result{
		"child was started": func() Result {
			result := base
			result.ChildExitCode = 0
			return result
		}(),
		"unrelated operation": func() Result {
			result := base
			result.Operation = OperationObserve
			return result
		}(),
		"resolved descriptor evidence": func() Result {
			result := base
			result.ResolvedReference = "oci://registry.example/team/schema@" + digest
			result.ResolvedMediaType = "application/vnd.oci.image.manifest.v1+json"
			result.ResolvedDigest = digest
			result.ResolvedSize = 1
			return result
		}(),
		"verification evidence": func() Result {
			result := base
			result.Operation = OperationVerify
			result.VerificationPolicyDigest = digest
			return result
		}(),
		"database evidence": func() Result {
			result := base
			result.CoordinationDigest = digest
			return result
		}(),
		"target identity evidence": func() Result {
			result := base
			result.TargetIdentityDigest = digest
			return result
		}(),
		"artifact evidence": func() Result {
			result := base
			result.Operation = OperationVerify
			result.ObservedArtifactType = "migration-directory"
			return result
		}(),
		"plan evidence": func() Result {
			result := base
			result.PlanContentDigest = digest
			return result
		}(),
		"truncation evidence": func() Result {
			result := base
			result.Truncation = &TruncationMetadata{Stderr: true, StderrBytesDropped: 1}
			return result
		}(),
	}
	for name, result := range invalid {
		name, result := name, result
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frame := handcraftedIntegrityValidFrame(t, result)
			if _, err := ParseResultFor(frame, result.Operation, result.OperationID); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

func TestVerificationRefusalChildExitBinding(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("9", 64)
	base := Result{
		ProtocolVersion:          ProtocolVersion,
		Operation:                OperationVerify,
		OperationID:              "verify-refusal-binding",
		ResolvedDigest:           digest,
		VerificationPolicyDigest: digest,
		Error:                    &ResultError{Code: "verification_refused", Message: "artifact does not satisfy the verification policy"},
	}
	for name, result := range map[string]Result{
		"native refusal": func() Result {
			result := base
			result.ChildExitCode = 2
			result.VerificationRequirements = []string{"require_signature"}
			return result
		}(),
		"runner digest pin refusal": func() Result {
			result := base
			result.ChildExitCode = 0
			result.VerificationRequirements = []string{"require_digest_pin"}
			return result
		}(),
	} {
		name, result := name, result
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			got, err := ParseResultFor(frame, result.Operation, result.OperationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if !reflect.DeepEqual(got, result) {
				t.Fatalf("ParseResultFor() = %#v, want %#v", got, result)
			}
		})
	}

	for name, result := range map[string]Result{
		"exit zero for native requirement": func() Result {
			result := base
			result.ChildExitCode = 0
			result.VerificationRequirements = []string{"require_signature"}
			return result
		}(),
		"exit zero for mixed requirements": func() Result {
			result := base
			result.ChildExitCode = 0
			result.VerificationRequirements = []string{"require_digest_pin", "require_signature"}
			return result
		}(),
		"non-native refusal exit": func() Result {
			result := base
			result.ChildExitCode = 1
			result.VerificationRequirements = []string{"require_digest_pin"}
			return result
		}(),
	} {
		name, result := name, result
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frame := handcraftedIntegrityValidFrame(t, result)
			if _, err := ParseResultFor(frame, result.Operation, result.OperationID); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

func handcraftedIntegrityValidFrame(t *testing.T, result Result) []byte {
	t.Helper()
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal(result) error = %v", err)
	}
	digest := sha256.Sum256(payload)
	return []byte(fmt.Sprintf("%s%d %s\n%s%s\n", frameHeader, len(payload), hex.EncodeToString(digest[:]), payload, frameFooter))
}

// A migration apply writes to the database, so its frame carries the same
// mutation metadata an apply does -- and a stopped run carries it with the
// report that says what the database holds.
func TestFrameCarriesMigrationMutationMetadata(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("9", 64)
	report := &dataplane.MigrationRunReport{
		ContractVersion: 1,
		Direction:       "up",
		Outcome:         dataplane.MigrationOutcomePartial,
	}
	result := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationMigrationApply, OperationID: "run-partial",
		ChildExitCode: 1, CoordinationDigest: digest, MutationStarted: true, Uncertain: true,
		MigrationRun: report,
		Error:        &ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
	}

	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, result.Operation, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if !parsed.MutationStarted || !parsed.Uncertain || parsed.MigrationRun == nil ||
		parsed.MigrationRun.Outcome != dataplane.MigrationOutcomePartial {
		t.Fatalf("ParseResultFor() = %#v", parsed)
	}
}

// A frame carrying row drift is the other gate the vocabulary governs. The
// controller reads a drift report and the frame validator reads the summary
// built from it, and both ask the same vocabulary, so a category one accepts
// and the other refuses would leave the observation unreadable after it had
// already been decoded.
//
// The order is the canonical one: severity first, then the category name, which
// puts the two destructive row categories in front of the safe one.
func TestFrameCarriesManagedRowDriftFindings(t *testing.T) {
	t.Parallel()

	result := Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-rows",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		ObservedDrift: true, HighestDriftSeverity: "destructive", DriftFindingCount: 3,
		DriftFindings: []DriftFindingSummary{
			{Category: "data_rows_deleted", Count: 1, Severity: "destructive"},
			{Category: "data_rows_updated", Count: 1, Severity: "destructive"},
			{Category: "data_rows_inserted", Count: 1, Severity: "safe"},
		},
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseResultFor(frame, OperationObserve, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() rejected a frame carrying row drift: %v", err)
	}
	if len(parsed.DriftFindings) != len(result.DriftFindings) {
		t.Fatalf("DriftFindings = %d, want %d", len(parsed.DriftFindings), len(result.DriftFindings))
	}
	for index, finding := range parsed.DriftFindings {
		if finding != result.DriftFindings[index] {
			t.Fatalf("DriftFindings[%d] = %+v, want %+v", index, finding, result.DriftFindings[index])
		}
	}
}

// A frame that does not survive the scan is several different failures, and the
// one sentence they all used to produce sent a reader looking in the wrong
// place. A log read before the frame finished arriving and a build that wrote a
// frame this one refuses are not the same problem and do not have the same
// answer.
//
// The reasons are asserted as text because text is what reaches a person: the
// end-to-end phases print this error and nothing else about the frame.
func TestParseSaysWhyItRejectedTheLastFrame(t *testing.T) {
	t.Parallel()

	complete := func(t *testing.T) []byte {
		t.Helper()
		frame, err := MarshalFrame(Result{
			ProtocolVersion: ProtocolVersion, Operation: OperationResolve, OperationID: "why",
			ChildExitCode: -1,
			Error:         &ResultError{Code: "invalid_oci_access", Message: "refused before the child"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}

	tests := []struct {
		name string
		// mutate returns the log bytes a reader would hand the parser.
		mutate func(t *testing.T, frame []byte) []byte
		want   string
		// stillArriving is whether reading the log again could change the
		// answer: the log ends inside the frame, rather than holding bytes that
		// are wrong (#154).
		stillArriving bool
	}{
		{
			name: "a log that stops where the payload ends",
			mutate: func(t *testing.T, frame []byte) []byte {
				t.Helper()
				return frame[:len(frame)-len(frameFooter)]
			},
			want:          "never finished arriving",
			stillArriving: true,
		},
		{
			// The other reason a footer can be missing, and the one a longer
			// wait would never fix.
			name: "a footer pushed past the bound on interleaved lines",
			mutate: func(t *testing.T, frame []byte) []byte {
				t.Helper()
				cut := len(frame) - len(frameFooter)
				noise := bytes.Repeat([]byte("diagnostic line\n"), (maxInterleavedFrameBytes/16)+16)
				return append(append(append([]byte(nil), frame[:cut]...), noise...), frame[cut:]...)
			},
			want: "bound on log lines interleaved after it",
		},
		{
			name: "a log that stops inside the payload",
			mutate: func(t *testing.T, frame []byte) []byte {
				t.Helper()
				return frame[:len(frame)-len(frameFooter)-4]
			},
			want:          "never finished arriving",
			stillArriving: true,
		},
		{
			name: "a log that stops inside the header",
			mutate: func(t *testing.T, frame []byte) []byte {
				t.Helper()
				return frame[:len(frameHeader)+5]
			},
			want:          "no end of line within its bounds",
			stillArriving: true,
		},
		{
			name: "a payload edited after its digest was written",
			mutate: func(t *testing.T, frame []byte) []byte {
				t.Helper()
				return bytes.Replace(frame, []byte(`"operationId":"why"`), []byte(`"operationId":"whz"`), 1)
			},
			want: "does not match the digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseResultWithOptions(test.mutate(t, complete(t)), ParseOptions{})
			if !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("err = %v, want a malformed-frame error", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %q, want it to say %q", err.Error(), test.want)
			}
			if got := MayStillArrive(err); got != test.stillArriving {
				t.Fatalf("MayStillArrive(%q) = %v, want %v", err.Error(), got, test.stillArriving)
			}
			if got := errors.Is(err, ErrIncompleteFrame); got != test.stillArriving {
				t.Fatalf("errors.Is(%q, ErrIncompleteFrame) = %v, want %v", err.Error(), got, test.stillArriving)
			}
		})
	}

	// A log with no frame in it yet is the other read a later read can answer:
	// the frame is the runner's last output, so it is the last to arrive.
	t.Run("a log with no frame in it yet", func(t *testing.T) {
		t.Parallel()
		_, err := ParseResultWithOptions([]byte("diagnostic line\n"), ParseOptions{})
		if !errors.Is(err, ErrFrameNotFound) || !MayStillArrive(err) {
			t.Fatalf("err = %v, want a frame-not-found error that may still arrive", err)
		}
	})

	// A reader speaking another protocol version is the rejection that is not a
	// damaged log, and the one most likely to be read as one.
	t.Run("a frame this reader does not speak", func(t *testing.T) {
		t.Parallel()
		_, err := ParseResultWithOptions(complete(t), ParseOptions{ExpectedProtocolVersion: legacyProtocolVersion})
		if !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("err = %v, want a malformed-frame error", err)
		}
		if !strings.Contains(err.Error(), "protocol version binding mismatch") {
			t.Fatalf("err = %q, want it to name the version mismatch", err.Error())
		}
	})
}
