package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The termination summary is a second, much smaller copy of what a result frame
// says, written where the kubelet keeps it after the container's log is gone.
//
// A frame exists only in the container log on the node. Container garbage
// collection, node loss, or a log that never received the frame's last bytes
// leave the controller with an Apply it cannot account for. The kubelet copies
// a container's termination message into Pod status when the container exits,
// which the controller already reads, and which needs no credential in the Pod.
//
// It carries only what a decision needs and nothing a frame is careful about:
// no plan, no native output, no error message, no SQL. It is bound to the frame
// it summarizes by that frame's SHA-256, so a log that holds a different frame
// is caught, and it says nothing a frame would not.
const (
	summaryMarker = "PTAH_RUNNER_SUMMARY_V1 "

	// TerminationMessagePath is where the runner writes its summary. It is the
	// path Kubernetes uses when a container names none; the workload builders
	// name it anyway, so the runner and the Pod spec cannot disagree about it.
	TerminationMessagePath = "/dev/termination-log"

	// MaxSummaryBytes bounds the summary line, marker included.
	//
	// The kubelet keeps at most 4 KiB of a termination message and 12 KiB for
	// the whole Pod, divided evenly among its containers. An operation Pod has
	// up to four -- the runner installer, the source guard, the fetch, and the
	// executor -- so the executor's share can be 3 KiB. The runtime may put its
	// own message in front, and a summary cut short is refused rather than
	// read, so the bound leaves a kilobyte of that share unused.
	MaxSummaryBytes = 2 << 10
)

// ErrSummaryNotFound reports a termination message that carries no runner
// summary at all: a container that did not write one, or a runtime message
// alone. It is absence, not a refusal.
var ErrSummaryNotFound = errors.New("the termination message carries no runner summary")

// Summary is the termination summary of one result frame.
type Summary struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Operation       Operation `json:"operation"`
	OperationID     string    `json:"operationId"`
	// FrameDigest is the SHA-256 of the frame payload, the digest the frame's
	// own header declares.
	FrameDigest     string `json:"frameDigest"`
	MutationStarted bool   `json:"mutationStarted,omitempty"`
	Uncertain       bool   `json:"uncertain,omitempty"`
	// ErrorCode is the frame's error code. The message is left out: it is
	// sanitized for the frame, and a summary has no need of it.
	ErrorCode            string            `json:"errorCode,omitempty"`
	CoordinationDigest   string            `json:"coordinationDigest,omitempty"`
	TargetIdentityDigest string            `json:"targetIdentityDigest,omitempty"`
	Migration            *MigrationSummary `json:"migration,omitempty"`
}

// MigrationSummary is what a migration Apply's report said, reduced to what
// fits: the outcome, and the applied versions as a count and the first and
// last of them in the order Ptah applied them.
type MigrationSummary struct {
	Outcome      string `json:"outcome"`
	AppliedCount int    `json:"appliedCount"`
	FirstApplied int64  `json:"firstApplied,omitempty"`
	LastApplied  int64  `json:"lastApplied,omitempty"`
}

// EncodedResult is a result frame and the termination summary bound to it.
type EncodedResult struct {
	Frame []byte
	// Summary is nil when the result cannot be summarized, and SummaryErr says
	// why. The frame never depends on it: the summary is a copy of part of the
	// frame, and losing the copy must never cost the original.
	Summary    []byte
	SummaryErr error
}

// EncodeResult encodes the result frame and the termination summary bound to
// it.
//
// Both come from the one result, and the summary names the digest of the frame
// returned beside it, so the two cannot describe different runs. A result that
// cannot be framed is an error, as it is for MarshalFrame. Every result the
// runner frames can be summarized: each rule a summary is held to restates one
// its frame already met, and every field it carries is bounded, which
// TestTheLargestSummaryFitsItsBound measures against MaxSummaryBytes.
func EncodeResult(result Result) (EncodedResult, error) {
	if result.ProtocolVersion == 0 {
		result.ProtocolVersion = ProtocolVersion
	}
	frame, digest, err := marshalFrame(result)
	if err != nil {
		return EncodedResult{}, err
	}
	encoded := EncodedResult{Frame: frame}
	encoded.Summary, encoded.SummaryErr = marshalSummary(summaryOf(result, digest))
	return encoded, nil
}

// WriteTerminationSummary writes a summary to the termination message file.
//
// The file is opened, never created. The kubelet mounts it into every
// container, so a path that does not exist is a process running outside a Pod,
// and writing a new file there would be writing somewhere nobody reads.
func WriteTerminationSummary(path string, summary []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(summary)
	closeErr := file.Close()
	switch {
	case writeErr != nil:
		return writeErr
	case written != len(summary):
		return io.ErrShortWrite
	default:
		return closeErr
	}
}

func summaryOf(result Result, frameDigest string) Summary {
	summary := Summary{
		ProtocolVersion:      result.ProtocolVersion,
		Operation:            result.Operation,
		OperationID:          result.OperationID,
		FrameDigest:          frameDigest,
		MutationStarted:      result.MutationStarted,
		Uncertain:            result.Uncertain,
		CoordinationDigest:   result.CoordinationDigest,
		TargetIdentityDigest: result.TargetIdentityDigest,
	}
	if result.Error != nil {
		summary.ErrorCode = result.Error.Code
	}
	if report := result.MigrationRun; report != nil {
		migration := &MigrationSummary{Outcome: report.Outcome, AppliedCount: len(report.Applied)}
		if len(report.Applied) > 0 {
			migration.FirstApplied = report.Applied[0]
			migration.LastApplied = report.Applied[len(report.Applied)-1]
		}
		summary.Migration = migration
	}
	return summary
}

func marshalSummary(summary Summary) ([]byte, error) {
	if err := summary.validate(ParseOptions{}); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	encoded.WriteString(summaryMarker)
	encoder := json.NewEncoder(&encoded)
	// The reader is a controller, never a browser, and escaping would spend
	// six bytes on each of three characters an operation ID may contain.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(summary); err != nil {
		return nil, fmt.Errorf("encode the summary: %w", err)
	}
	if encoded.Len() > MaxSummaryBytes {
		return nil, fmt.Errorf("the summary is %d bytes; the maximum is %d", encoded.Len(), MaxSummaryBytes)
	}
	return encoded.Bytes(), nil
}

// ParseSummaryFor reads the summary out of a container's termination message
// and binds it to the operation that claimed the Job.
//
// The message may carry the container runtime's own text in front, which the
// kubelet joins to the file's content with a colon, so the summary is found by
// its marker rather than taken from the start. It has to run to the end of the
// message, be the only summary there, and account for itself exactly as a
// frame must; anything else is refused, because a summary cut short or edited
// is one that could say what the run did not.
func ParseSummaryFor(message string, operation Operation, operationID string) (Summary, error) {
	start := strings.Index(message, summaryMarker)
	if start < 0 {
		return Summary{}, ErrSummaryNotFound
	}
	line := message[start:]
	if strings.Contains(line[len(summaryMarker):], summaryMarker) {
		return Summary{}, errors.New("the termination message carries more than one runner summary")
	}
	if len(line) > MaxSummaryBytes {
		return Summary{}, errors.New("the runner summary exceeds its bound")
	}
	document := strings.TrimSuffix(line[len(summaryMarker):], "\n")
	if strings.ContainsAny(document, "\r\n") {
		return Summary{}, errors.New("the runner summary is not one line")
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var summary Summary
	if err := decoder.Decode(&summary); err != nil {
		return Summary{}, errors.New("the runner summary is not a document this build can decode")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Summary{}, errors.New("the runner summary carries data after its document")
	}
	if err := summary.validate(ParseOptions{ExpectedOperation: operation, ExpectedOperationID: operationID}); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

// StandsInFor reports whether this summary may stand in for the frame the log
// was expected to hold, and why not when it may not.
//
// Only a frame that is missing may be replaced: a log with no frame in it, or
// one that ends inside the frame. A frame that is present and was refused --
// the wrong digest, the wrong operation, a payload this build does not accept
// -- is an answer, and a summary of it cannot be a better one. And a log whose
// frame headers declare a digest other than the one this summary is bound to
// holds a different result, so the two are not read together at all.
func (s Summary) StandsInFor(logs []byte, parseErr error) error {
	switch {
	case parseErr == nil:
		return errors.New("the log holds a readable frame, and the frame is the account")
	case !MayStillArrive(parseErr):
		return errors.New("the log holds a frame that was refused, and a summary cannot say more than it")
	}
	for _, digest := range frameHeaderDigests(logs) {
		if digest != s.FrameDigest {
			return errors.New("the log names a frame other than the one the summary is bound to")
		}
	}
	return nil
}

// frameHeaderDigests is every digest a complete frame header in the log
// declares, whether or not the payload it declares ever arrived.
func frameHeaderDigests(logs []byte) []string {
	marker := []byte(frameHeader)
	var digests []string
	for searchAt := 0; searchAt < len(logs); {
		relative := bytes.Index(logs[searchAt:], marker)
		if relative < 0 {
			break
		}
		headerStart := searchAt + relative + len(marker)
		searchAt = headerStart
		end := bytes.IndexByte(logs[headerStart:], '\n')
		if end < 0 || end > 96 {
			continue
		}
		fields := bytes.Fields(logs[headerStart : headerStart+end])
		if len(fields) != 2 {
			continue
		}
		digest, err := hex.DecodeString(string(fields[1]))
		if err != nil || len(digest) != sha256.Size {
			continue
		}
		digests = append(digests, "sha256:"+hex.EncodeToString(digest))
	}
	return digests
}

// validate holds a summary to the rules its frame had to satisfy, and to the
// ones that tie its fields to each other.
//
// Every rule here restates one validateResult or decodeMigrationReport already
// applies to the frame, so a summary the runner wrote passes, and a summary
// that passes says nothing a frame could not have said.
func (s Summary) validate(options ParseOptions) error {
	if s.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("the runner summary speaks protocol %d, not %d", s.ProtocolVersion, ProtocolVersion)
	}
	if !s.Operation.Valid() {
		return errors.New("the runner summary names an unsupported operation")
	}
	if s.OperationID == "" || len(s.OperationID) > 256 || hasControlCharacter(s.OperationID) {
		return errors.New("the runner summary carries an invalid operation ID")
	}
	if options.ExpectedOperation != "" && s.Operation != options.ExpectedOperation {
		return errors.New("the runner summary answers another operation")
	}
	if options.ExpectedOperationID != "" && s.OperationID != options.ExpectedOperationID {
		return errors.New("the runner summary answers another operation attempt")
	}
	if !validProtocolDigest(s.FrameDigest) {
		return errors.New("the runner summary is not bound to a frame digest")
	}
	if s.ErrorCode != "" && !runnerRequirementPattern.MatchString(s.ErrorCode) {
		return errors.New("the runner summary carries an invalid error code")
	}
	if s.Uncertain && !s.MutationStarted {
		return errors.New("the runner summary is uncertain about a mutation it does not claim")
	}
	if !s.Operation.Mutating() && (s.MutationStarted || s.Uncertain) {
		return errors.New("the runner summary claims a mutation for a read-only operation")
	}
	for _, digest := range []string{s.CoordinationDigest, s.TargetIdentityDigest} {
		if digest != "" && !validProtocolDigest(digest) {
			return errors.New("the runner summary carries an invalid digest")
		}
	}
	if s.ErrorCode == "" && operationNeedsDatabase(s.Operation) && s.CoordinationDigest == "" {
		return errors.New("the runner summary of a successful database operation names no realm")
	}
	if s.ErrorCode == "" && s.Operation == OperationApply && (!s.MutationStarted || s.Uncertain) {
		return errors.New("the runner summary of a successful apply lacks a completed mutation")
	}
	if s.Migration == nil {
		if s.ErrorCode == "" && s.Operation == OperationMigrationApply {
			return errors.New("the runner summary of a successful migration run lacks its outcome")
		}
		return nil
	}
	if s.Operation != OperationMigrationApply {
		return errors.New("the runner summary carries a migration outcome for another operation")
	}
	return s.Migration.validate(s.MutationStarted, s.Uncertain)
}

func (m MigrationSummary) validate(mutationStarted, uncertain bool) error {
	if !dataplane.IsKnownMigrationOutcome(m.Outcome) {
		return errors.New("the runner summary reports a migration outcome this build does not know")
	}
	switch {
	case m.AppliedCount < 0:
		return errors.New("the runner summary counts a negative number of applied migrations")
	case m.AppliedCount == 0 && (m.FirstApplied != 0 || m.LastApplied != 0):
		return errors.New("the runner summary names applied versions it does not count")
	case m.AppliedCount == 1 && m.FirstApplied != m.LastApplied:
		return errors.New("the runner summary names two versions for one applied migration")
	}
	// The same derivation decodeMigrationReport makes from the report, so
	// flags that disagree with the outcome did not come from one.
	if mutationStarted != (m.Outcome != dataplane.MigrationOutcomeUpToDate && m.Outcome != dataplane.MigrationOutcomeDryRun) ||
		uncertain != (m.Outcome == dataplane.MigrationOutcomePartial || m.Outcome == dataplane.MigrationOutcomeUnknown) {
		return errors.New("the runner summary's mutation flags disagree with its migration outcome")
	}
	return nil
}
