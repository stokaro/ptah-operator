package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// summarizedRun is a migration Apply that stopped between two files, as the
// runner frames it.
func summarizedRun() Result {
	return Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationMigrationApply,
		OperationID: "sha256:" + strings.Repeat("a", 64), ChildExitCode: -1,
		CoordinationDigest:   "sha256:" + strings.Repeat("b", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("c", 64),
		MutationStarted:      true,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
			Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{3, 4, 5, 6}, Applied: []int64{3, 4, 5},
		},
		Error: &ResultError{Code: "execution_error", Message: "the ptah migration-apply process was stopped because the Pod is terminating"},
	}
}

// framePayloadDigest is the digest a frame's header declares, read off the
// frame the way a person holding the log would.
func framePayloadDigest(t *testing.T, frame []byte) string {
	t.Helper()
	header, rest, found := bytes.Cut(frame, []byte("\n"))
	if !found || !bytes.HasPrefix(header, []byte(frameHeader)) {
		t.Fatalf("frame %q has no header line", frame)
	}
	payload, _, found := bytes.Cut(rest, []byte("\n"))
	if !found {
		t.Fatalf("frame %q has no payload line", frame)
	}
	fields := bytes.Fields(header[len(frameHeader):])
	sum := sha256.Sum256(payload)
	if len(fields) != 2 || string(fields[1]) != hex.EncodeToString(sum[:]) {
		t.Fatalf("frame header %q does not declare the payload's digest", header)
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestTheSummaryIsBoundToTheFrameItSummarizes(t *testing.T) {
	t.Parallel()

	result := summarizedRun()
	encoded, err := EncodeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if encoded.SummaryErr != nil {
		t.Fatalf("EncodeResult() summary error = %v", encoded.SummaryErr)
	}
	summary, err := ParseSummaryFor(string(encoded.Summary), OperationMigrationApply, result.OperationID)
	if err != nil {
		t.Fatalf("ParseSummaryFor() error = %v", err)
	}
	if want := framePayloadDigest(t, encoded.Frame); summary.FrameDigest != want {
		t.Fatalf("summary frame digest = %q, want the frame's %q", summary.FrameDigest, want)
	}
	want := Summary{
		ProtocolVersion: ProtocolVersion, Operation: OperationMigrationApply, OperationID: result.OperationID,
		FrameDigest: summary.FrameDigest, MutationStarted: true, ErrorCode: "execution_error",
		CoordinationDigest: result.CoordinationDigest, TargetIdentityDigest: result.TargetIdentityDigest,
		Migration: &MigrationSummary{Outcome: "failed", AppliedCount: 3, FirstApplied: 3, LastApplied: 5},
	}
	if summary.Migration == nil || *summary.Migration != *want.Migration {
		t.Fatalf("summary migration = %#v, want %#v", summary.Migration, want.Migration)
	}
	summary.Migration, want.Migration = nil, nil
	if summary != want {
		t.Fatalf("summary = %#v, want %#v", summary, want)
	}
	// The frame is exactly what MarshalFrame writes, so a summary changes
	// nothing a reader of the log sees.
	frame, err := MarshalFrame(result)
	if err != nil || !bytes.Equal(frame, encoded.Frame) {
		t.Fatalf("EncodeResult() frame differs from MarshalFrame(): %v", err)
	}
}

// The largest result the runner can frame still has a summary within its
// bound, which is what lets the kubelet keep it whole.
func TestTheLargestSummaryFitsItsBound(t *testing.T) {
	t.Parallel()

	result := summarizedRun()
	// Every byte of the longest operation ID the frame admits doubles when
	// JSON escapes it.
	result.OperationID = strings.Repeat(`"`, 256)
	result.Error.Code = "a" + strings.Repeat("b", 63)
	result.MigrationRun.Planned = []int64{math.MaxInt64 - 1, math.MaxInt64}
	result.MigrationRun.Applied = []int64{math.MaxInt64 - 1, math.MaxInt64}
	encoded, err := EncodeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if encoded.SummaryErr != nil {
		t.Fatalf("the largest summary was refused: %v", encoded.SummaryErr)
	}
	if len(encoded.Summary) > MaxSummaryBytes {
		t.Fatalf("the largest summary is %d bytes, over the %d bound", len(encoded.Summary), MaxSummaryBytes)
	}
	if _, err := ParseSummaryFor(string(encoded.Summary), OperationMigrationApply, result.OperationID); err != nil {
		t.Fatalf("ParseSummaryFor() refused the largest summary: %v", err)
	}
	// A line past the bound is refused, even one that is otherwise valid.
	padded := strings.Replace(string(encoded.Summary), `{"protocolVersion"`,
		"{"+strings.Repeat(" ", MaxSummaryBytes)+`"protocolVersion"`, 1)
	if _, err := ParseSummaryFor(padded, OperationMigrationApply, result.OperationID); err == nil {
		t.Fatal("ParseSummaryFor() accepted a summary longer than its bound")
	}
}

// The frame keeps the child's error message out of the log except in
// sanitized form; the summary keeps it out altogether, along with the
// database URL and anything the child printed.
func TestTheSummaryCarriesNoCredentialAndNoChildText(t *testing.T) {
	t.Parallel()

	const password = "summary-password-7f3a"
	environment := migrationApplyEnvironment(t, "summary-credential-free")
	for index, entry := range environment {
		if strings.HasPrefix(entry, envDatabaseURL+"=") {
			environment[index] = envDatabaseURL + "=postgres://app:" + password + "@db.example/app"
		}
	}
	for index, entry := range environment {
		if strings.HasPrefix(entry, envExpectedTargetDigest+"=") {
			digest, err := TargetIdentityDigest("postgres://app:" + password + "@db.example/app")
			if err != nil {
				t.Fatal(err)
			}
			environment[index] = envExpectedTargetDigest + "=" + digest
		}
	}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout:   migrationRunDocument("partial"),
		stderr:   "connection to postgres://app:" + password + "@db.example/app lost",
		exitCode: 1,
	}}}
	result := Run(context.Background(), Config{
		Operation: OperationMigrationApply, Environment: withRunnerProtocol(environment), Executor: executor, TempDir: t.TempDir(),
	})
	if len(executor.calls) != 1 || result.MigrationRun == nil {
		t.Fatalf("Run() = %#v after %d calls, want a run that reached the child and kept its report",
			result, len(executor.calls))
	}
	encoded, err := EncodeResult(result)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
	}
	for _, forbidden := range []string{
		password, "postgres://", "db.example", "connection", "failed to apply migration",
		result.Error.Message,
	} {
		if strings.Contains(string(encoded.Summary), forbidden) {
			t.Fatalf("the summary carries %q: %s", forbidden, encoded.Summary)
		}
	}
}

func TestParseSummaryRefusesWhatItCannotAccountFor(t *testing.T) {
	t.Parallel()

	result := summarizedRun()
	encoded, err := EncodeResult(result)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
	}
	valid := string(encoded.Summary)
	edit := func(old, replacement string) string {
		t.Helper()
		if strings.Count(valid, old) != 1 {
			t.Fatalf("the summary %q does not hold %q exactly once", valid, old)
		}
		return strings.Replace(valid, old, replacement, 1)
	}
	for _, row := range []struct {
		name      string
		message   string
		operation Operation
		id        string
		wantErr   bool
	}{
		// The control: without it every refusal below passes against a
		// parser that refuses everything.
		{name: "the summary the runner wrote", message: valid},
		{name: "behind the runtime's own message", message: "Error: " + valid},
		{name: "without its trailing newline", message: strings.TrimSuffix(valid, "\n")},
		{name: "another operation", message: valid, operation: OperationMigrationHistory, wantErr: true},
		{name: "another attempt", message: valid, id: "sha256:" + strings.Repeat("f", 64), wantErr: true},
		{name: "another protocol", message: edit(
			`"protocolVersion":`+strconv.Itoa(ProtocolVersion), `"protocolVersion":`+strconv.Itoa(ProtocolVersion-1),
		), wantErr: true},
		{name: "a field this build does not know", message: edit(`{"protocolVersion"`, `{"extra":1,"protocolVersion"`), wantErr: true},
		{name: "data after the document", message: strings.TrimSuffix(valid, "\n") + " {}", wantErr: true},
		{name: "two summaries", message: valid + valid, wantErr: true},
		{name: "cut short", message: valid[:len(valid)/2], wantErr: true},
		{name: "not one line", message: edit(`,"operation"`, ",\n\"operation\""), wantErr: true},
		{name: "no frame digest", message: edit(`"frameDigest":"sha256:`, `"frameDigest":"md5:`), wantErr: true},
		{name: "uncertain without a mutation", message: edit(`"mutationStarted":true`, `"uncertain":true`), wantErr: true},
		{name: "flags that disagree with the outcome", message: edit(`"outcome":"failed"`, `"outcome":"partial"`), wantErr: true},
		{name: "an outcome this build does not know", message: edit(`"outcome":"failed"`, `"outcome":"rolled-back"`), wantErr: true},
		{name: "applied versions it does not count", message: edit(`"appliedCount":3`, `"appliedCount":0`), wantErr: true},
		{name: "one applied migration named twice", message: edit(`"appliedCount":3`, `"appliedCount":1`), wantErr: true},
		{name: "an invalid error code", message: edit(`"errorCode":"execution_error"`, `"errorCode":"Execution Error"`), wantErr: true},
		{name: "an invalid realm digest", message: edit(`"coordinationDigest":"sha256:`, `"coordinationDigest":"sha1:`), wantErr: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			operation, id := row.operation, row.id
			if operation == "" {
				operation = OperationMigrationApply
			}
			if id == "" {
				id = result.OperationID
			}
			_, err := ParseSummaryFor(row.message, operation, id)
			if row.wantErr != (err != nil) {
				t.Fatalf("ParseSummaryFor() error = %v, want refused=%t", err, row.wantErr)
			}
			if errors.Is(err, ErrSummaryNotFound) {
				t.Fatalf("ParseSummaryFor() reported a present summary as absent: %v", err)
			}
		})
	}
	for _, message := range []string{"", "OOMKilled", "Error on reading termination log /dev/termination-log: permission denied"} {
		if _, err := ParseSummaryFor(message, OperationMigrationApply, result.OperationID); !errors.Is(err, ErrSummaryNotFound) {
			t.Fatalf("ParseSummaryFor(%q) error = %v, want ErrSummaryNotFound", message, err)
		}
	}
}

// A summary replaces a frame that is missing and nothing else. Every row but
// the first two is a log that says something, and the summary may not speak
// over it.
func TestASummaryStandsInOnlyForAMissingFrame(t *testing.T) {
	t.Parallel()

	result := summarizedRun()
	encoded, err := EncodeResult(result)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
	}
	summary, err := ParseSummaryFor(string(encoded.Summary), OperationMigrationApply, result.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	other := summarizedRun()
	other.MigrationRun.Outcome = dataplane.MigrationOutcomePartial
	other.Uncertain = true
	otherFrame, err := MarshalFrame(other)
	if err != nil {
		t.Fatal(err)
	}
	cut := func(frame []byte) []byte { return frame[:len(frame)-len(frameFooter)-8] }
	wrongID := summarizedRun()
	wrongID.OperationID = "sha256:" + strings.Repeat("e", 64)
	wrongIDFrame, err := MarshalFrame(wrongID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name  string
		logs  []byte
		stand bool
	}{
		{name: "the log holds nothing", logs: nil, stand: true},
		{name: "the log ends inside this frame", logs: append([]byte("diagnostic\n"), cut(encoded.Frame)...), stand: true},
		{name: "the log holds this frame whole", logs: encoded.Frame},
		{name: "the log ends inside another frame", logs: cut(otherFrame)},
		{name: "the log holds a frame for another attempt", logs: wrongIDFrame},
		// Same length, so the footer still closes the payload and the parser
		// reads it as present and wrong rather than as still arriving.
		{name: "the log holds a frame that does not match its digest",
			logs: bytes.Replace(encoded.Frame, []byte(`"failed"`), []byte(`"falied"`), 1)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, parseErr := ParseResultFor(row.logs, OperationMigrationApply, result.OperationID)
			err := summary.StandsInFor(row.logs, parseErr)
			if row.stand != (err == nil) {
				t.Fatalf("StandsInFor() = %v with parse error %v, want standing in=%t", err, parseErr, row.stand)
			}
		})
	}
}

func TestWriteTerminationSummaryReplacesTheMountedFileAndCreatesNone(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	mounted := filepath.Join(directory, "termination-log")
	if err := os.WriteFile(mounted, []byte(strings.Repeat("stale ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteTerminationSummary(mounted, []byte("summary\n")); err != nil {
		t.Fatalf("WriteTerminationSummary() error = %v", err)
	}
	if content, err := os.ReadFile(mounted); err != nil || string(content) != "summary\n" {
		t.Fatalf("termination log = %q, %v, want the summary alone", content, err)
	}
	missing := filepath.Join(directory, "not-mounted")
	if err := WriteTerminationSummary(missing, []byte("summary\n")); err == nil {
		t.Fatal("WriteTerminationSummary() wrote to a path nothing mounted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("WriteTerminationSummary() created %s: %v", missing, err)
	}
}
