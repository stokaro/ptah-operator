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
