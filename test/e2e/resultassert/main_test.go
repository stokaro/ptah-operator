package main

import (
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestParseExactResult(t *testing.T) {
	t.Parallel()
	result := runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationObserve,
		OperationID:          "observe-operation",
		ChildExitCode:        0,
		CoordinationDigest:   testDigest('1'),
		TargetIdentityDigest: testDigest('2'),
		DriftReportDigest:    testDigest('3'),
		ObservedDialect:      "postgres",
		ObservedDrift:        true,
		HighestDriftSeverity: "warning",
		DriftFindingCount:    1,
		DriftFindings: []runner.DriftFindingSummary{{
			Category: "columns_added", Count: 1, Severity: "warning",
		}},
	}
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	logs := append([]byte("diagnostic before frame\n"), frame...)
	parsed, err := parseExactResult(logs, runner.OperationObserve, result.OperationID)
	if err != nil {
		t.Fatalf("parse exact result: %v", err)
	}
	if parsed.Stdout != "" || parsed.DriftReportDigest != result.DriftReportDigest ||
		parsed.ObservedDialect != result.ObservedDialect || !parsed.ObservedDrift ||
		parsed.HighestDriftSeverity != result.HighestDriftSeverity || parsed.DriftFindingCount != 1 ||
		!reflect.DeepEqual(parsed.DriftFindings, result.DriftFindings) {
		t.Fatalf("parsed credential-free observation = %#v", parsed)
	}
}

func TestParseExactResultRejectsMultipleFrames(t *testing.T) {
	t.Parallel()
	result := runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationObserve,
		OperationID:          "observe-operation",
		ChildExitCode:        0,
		CoordinationDigest:   testDigest('1'),
		TargetIdentityDigest: testDigest('2'),
		DriftReportDigest:    testDigest('3'),
		ObservedDialect:      "postgres",
		ObservedDrift:        true,
		HighestDriftSeverity: "warning",
		DriftFindingCount:    1,
		DriftFindings: []runner.DriftFindingSummary{{
			Category: "columns_added", Count: 1, Severity: "warning",
		}},
	}
	frame, err := runner.MarshalFrame(result)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	logs := append(append([]byte(nil), frame...), frame...)
	if _, err := parseExactResult(logs, runner.OperationObserve, result.OperationID); err == nil {
		t.Fatal("parseExactResult accepted multiple frames")
	}
}

func testDigest(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return "sha256:" + string(value)
}
