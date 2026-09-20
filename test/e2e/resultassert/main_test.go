package main

import (
	"bytes"
	"os"
	"reflect"
	"regexp"
	"strings"
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

const (
	shapeOperation   = runner.OperationObserve
	shapeOperationID = "observe-operation"
)

// shapeResult is the converged observation the shapes below carry. Its
// coordination digest is the only field that varies, and every digest is the
// same length, so two results differ in content and never in the byte count the
// frame header declares.
func shapeResult(coordinationDigest string) runner.Result {
	return runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            shapeOperation,
		OperationID:          shapeOperationID,
		CoordinationDigest:   coordinationDigest,
		TargetIdentityDigest: testDigest('2'),
		DriftReportDigest:    testDigest('3'),
		ObservedDialect:      "postgres",
	}
}

type logShape struct {
	name string
	logs []byte
	// arriving is true when the log is a prefix of one that has not finished
	// being copied into the container log, so reading it again can still
	// produce the frame.
	arriving bool
	// reason is a substring the refusal must carry. It is empty for the one
	// shape that is accepted.
	reason string
}

func logShapes(t *testing.T) []logShape {
	t.Helper()
	frame, err := runner.MarshalFrame(shapeResult(testDigest('1')))
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	divergent, err := runner.MarshalFrame(shapeResult(testDigest('4')))
	if err != nil {
		t.Fatalf("marshal divergent frame: %v", err)
	}
	if len(divergent) != len(frame) {
		t.Fatalf("the divergent frame is %d bytes and the declared one is %d; splicing it would measure a short frame instead",
			len(divergent), len(frame))
	}
	headerEnd := bytes.IndexByte(frame, '\n') + 1
	if headerEnd <= 0 {
		t.Fatal("marshalled a frame whose header has no end of line")
	}
	payloadEnd := len(frame) - len("\nPTAH_RUNNER_RESULT_END_V1\n")

	return []logShape{
		{
			name:     "a log that has not started arriving",
			logs:     nil,
			arriving: true,
			reason:   "frame not found",
		},
		{
			name:     "a log carrying no frame at all",
			logs:     []byte("ptah: connecting to the target database\n"),
			arriving: true,
			reason:   "frame not found",
		},
		{
			name:     "a header cut inside its own line",
			logs:     frame[:headerEnd-10],
			arriving: true,
			reason:   "no end of line within its bounds",
		},
		{
			name:     "a header with none of its payload",
			logs:     frame[:headerEnd],
			arriving: true,
			reason:   "the log ends before the length the frame header declares, so the frame never finished arriving",
		},
		{
			name:     "a payload short of the length its header declares",
			logs:     frame[:headerEnd+(payloadEnd-headerEnd)/2],
			arriving: true,
			reason:   "the log ends before the length the frame header declares, so the frame never finished arriving",
		},
		{
			name:     "a payload with no footer behind it",
			logs:     frame[:payloadEnd],
			arriving: true,
			reason:   "the log ends after the payload without the footer that closes it, so the frame never finished arriving",
		},
		{
			name:     "a footer half arrived",
			logs:     frame[:payloadEnd+10],
			arriving: true,
			reason:   "the log ends after the payload without the footer that closes it, so the frame never finished arriving",
		},
		{
			// A diagnostic ahead of the payload adds no header and no footer,
			// so the counts this command pre-checks are unmoved and the parser
			// reads the frame it can verify. Refusing it sent the transport
			// wait around its whole window re-reading a log that was complete.
			name: "a diagnostic line ahead of the payload",
			logs: append(append(append([]byte{}, frame[:headerEnd]...),
				"ptah: registry certificate authority bytes do not match the grant\n"...), frame[headerEnd:]...),
		},
		{
			name: "a payload with no footer behind it, and a diagnostic line ahead of it",
			logs: append(append(append([]byte{}, frame[:headerEnd]...),
				"ptah: registry certificate authority bytes do not match the grant\n"...), frame[headerEnd:payloadEnd]...),
			arriving: true,
			reason:   "the log ends after the payload without the footer that closes it, so the frame never finished arriving",
		},
		{
			name:     "a payload that does not match the digest its header declares",
			logs:     append(append([]byte{}, frame[:headerEnd]...), divergent[headerEnd:]...),
			arriving: false,
			reason:   "the frame payload does not match the digest its header declares",
		},
		{
			name:     "a log carrying two complete frames",
			logs:     append(append([]byte{}, frame...), frame...),
			arriving: false,
			reason:   "carries 2 result frame headers rather than one",
		},
		{
			name:     "one frame closed by two footers",
			logs:     append(append([]byte{}, frame...), "PTAH_RUNNER_RESULT_END_V1\n"...),
			arriving: false,
			reason:   "carries 2 result frame footers rather than one",
		},
		{
			name: "the frame the runner finished writing",
			logs: frame,
		},
	}
}

// TestParseExactResultSeparatesArrivalFromRefusal measures the split this
// command exists to make. A log that may still be arriving and a log that is
// present and wrong are different failures with different answers, and the
// callers that re-read a transport choose between them by the reason printed.
func TestParseExactResultSeparatesArrivalFromRefusal(t *testing.T) {
	t.Parallel()
	for _, shape := range logShapes(t) {
		t.Run(shape.name, func(t *testing.T) {
			result, err := parseExactResult(shape.logs, shapeOperation, shapeOperationID)
			if shape.reason == "" {
				if err != nil {
					t.Fatalf("refused a complete frame: %v", err)
				}
				if result.OperationID != shapeOperationID {
					t.Fatalf("accepted frame carries operation ID %q, want %q", result.OperationID, shapeOperationID)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a log that is not one complete frame")
			}
			if !strings.Contains(err.Error(), shape.reason) {
				t.Fatalf("refusal is %q, want it to carry %q", err, shape.reason)
			}
		})
	}
}

// transportRetryPattern reads the alternation read_result_transport greps its
// stderr for. A refusal that does not match it is final.
var transportRetryPattern = regexp.MustCompile(`(?m)^[ \t]*if ! grep -Eq '([^']*)'`)

func transportRetrySet(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	matches := transportRetryPattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) != 1 {
		t.Fatalf("%s carries %d result-transport retry filters, want exactly one", path, len(matches))
	}
	return matches[0][1]
}

// TestRefusalsAgreeWithTheTransportRetrySet binds this command's words to the
// filter that reads them. read_result_transport waits only for a refusal
// matching its alternation, so wording that drifts out of that set turns the
// bounded wait off for the shape it was written for -- silently, and with every
// other gate still green. That is what stood between #155 and this test: the
// filter passed its author's intent and measured something else, and only
// reading it against the refusals the binary actually emits catches it.
func TestRefusalsAgreeWithTheTransportRetrySet(t *testing.T) {
	t.Parallel()
	dataPlaneSet := transportRetrySet(t, "../../../hack/e2e-dataplane.sh")
	faultsSet := transportRetrySet(t, "../../../hack/e2e-faults.sh")
	if dataPlaneSet != faultsSet {
		t.Fatalf("the two result-transport retry filters differ:\n  e2e-dataplane.sh: %s\n  e2e-faults.sh:    %s",
			dataPlaneSet, faultsSet)
	}
	retrySet, err := regexp.Compile(dataPlaneSet)
	if err != nil {
		t.Fatalf("compile the result-transport retry filter %q: %v", dataPlaneSet, err)
	}

	// Every alternative has to be reached by a shape that is genuinely still
	// arriving. One nothing reaches is a filter widened to swallow a refusal
	// that should have been fixed at the source instead.
	alternatives := strings.Split(dataPlaneSet, "|")
	reached := make(map[string]bool, len(alternatives))
	for _, alternative := range alternatives {
		reached[alternative] = false
	}

	for _, shape := range logShapes(t) {
		_, err := parseExactResult(shape.logs, shapeOperation, shapeOperationID)
		if err == nil {
			continue
		}
		matched := retrySet.MatchString(err.Error())
		if matched && !shape.arriving {
			t.Errorf("%s: the retry filter waits on its refusal %q, and reading the log again cannot change it",
				shape.name, err)
			continue
		}
		if !matched && shape.arriving {
			t.Errorf("%s: the retry filter refuses its refusal %q at once, and the frame may still be arriving",
				shape.name, err)
			continue
		}
		if !shape.arriving {
			continue
		}
		for alternative := range reached {
			if strings.Contains(err.Error(), alternative) {
				reached[alternative] = true
			}
		}
	}

	for alternative, seen := range reached {
		if !seen {
			t.Errorf("the retry filter waits for %q and no log that may still be arriving says it", alternative)
		}
	}
}
