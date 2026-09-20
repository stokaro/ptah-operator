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
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const (
	// ProtocolVersion is part of the controller-to-Job approval binding. Any
	// incompatible result format must use a new version.
	ProtocolVersion = 5

	legacyProtocolVersion = 4

	// JSON escaping can expand a bounded plan payload. This shared cap includes
	// the worst-case expansion plus fixed result-envelope headroom.
	DefaultMaxFrameBytes int64 = plancontract.MaxResultPayloadBytes

	frameHeader = "PTAH_RUNNER_RESULT_V1 "
	frameFooter = "\nPTAH_RUNNER_RESULT_END_V1"
)

var (
	ErrFrameNotFound  = errors.New("ptah runner result frame not found")
	ErrMalformedFrame = errors.New("malformed ptah runner result frame")
	ErrFrameTooLarge  = errors.New("ptah runner result frame exceeds the configured limit")
	// ErrIncompleteFrame marks a frame the log ends inside. Every rejection
	// wrapping it is also ErrMalformedFrame, and its message is unchanged; what
	// it adds is that the bytes may simply not have arrived yet (#154).
	ErrIncompleteFrame = errors.New("ptah runner result frame has not finished arriving")
)

// incompleteFrameError is a malformed-frame rejection that describes a log
// ending inside a frame. It reads exactly as the malformed rejection always has.
type incompleteFrameError struct{ reason string }

func (e incompleteFrameError) Error() string { return ErrMalformedFrame.Error() + ": " + e.reason }

func (e incompleteFrameError) Is(target error) bool {
	return target == ErrMalformedFrame || target == ErrIncompleteFrame
}

// MayStillArrive reports whether a parse failure could be the log being read
// before the runner's output reached it, rather than output that is wrong.
//
// The runner writes its frame in one write as its last output, but the
// container runtime copies that output into the log asynchronously, and a
// terminated container does not promise the copy has finished. A read in that
// window ends inside the frame or before it. Every other rejection describes
// bytes that are present and wrong, and reading them again changes nothing.
func MayStillArrive(err error) bool {
	return errors.Is(err, ErrIncompleteFrame) || errors.Is(err, ErrFrameNotFound)
}

// Operation is one of the fixed operations understood by the runner.
type Operation string

const (
	OperationResolve Operation = "resolve"
	OperationVerify  Operation = "verify"
	OperationObserve Operation = "observe"
	OperationPlan    Operation = "plan"
	OperationApply   Operation = "apply"
	// OperationMigrationHistory reads the database's own migration history
	// against a migration artifact, and changes nothing.
	OperationMigrationHistory Operation = "migration-history"
	// OperationMigrationApply runs the pending migrations of a migration
	// artifact.
	OperationMigrationApply Operation = "migration-apply"
)

func (o Operation) Valid() bool {
	switch o {
	case OperationResolve, OperationVerify, OperationObserve, OperationPlan, OperationApply,
		OperationMigrationHistory, OperationMigrationApply:
		return true
	default:
		return false
	}
}

// Mutating reports whether an operation may change the database.
//
// A runner that predates an operation refuses it by name, which is what keeps
// an old executor from being asked to run migrations it cannot account for.
func (o Operation) Mutating() bool {
	switch o {
	case OperationApply, OperationMigrationApply:
		return true
	default:
		return false
	}
}

// PlanOutcome distinguishes an executable immutable plan from an exact
// read-only proof that the managed scope has no changes. It is explicit so an
// empty or truncated plan payload can never be mistaken for convergence.
type PlanOutcome string

const (
	PlanOutcomeChanges   PlanOutcome = "Changes"
	PlanOutcomeNoChanges PlanOutcome = "NoChanges"
)

func (o PlanOutcome) Valid() bool {
	return o == PlanOutcomeChanges || o == PlanOutcomeNoChanges
}

// ResultError describes an operation-level failure without relying on Job
// process exit semantics. Message is always sanitized before framing.
type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TruncationMetadata records output that was drained but intentionally not
// retained. The child is never back-pressured by these limits.
type TruncationMetadata struct {
	Stdout             bool  `json:"stdout,omitempty"`
	StdoutBytesDropped int64 `json:"stdoutBytesDropped,omitempty"`
	Stderr             bool  `json:"stderr,omitempty"`
	StderrBytesDropped int64 `json:"stderrBytesDropped,omitempty"`
}

// DriftFindingSummary is the only per-category drift detail permitted across
// the runner boundary. Category names are a fixed machine vocabulary; raw
// object names, SQL, schema literals, and diff payloads are never included.
type DriftFindingSummary struct {
	Category string `json:"category"`
	Count    int32  `json:"count"`
	Severity string `json:"severity"`
}

// Result is the complete credential-free result emitted by ptah-runner.
type Result struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Operation       Operation `json:"operation"`
	OperationID     string    `json:"operationId"`
	// ChildExitCode is -1 when the runner refused the operation before child dispatch.
	ChildExitCode            int                   `json:"childExitCode"`
	Stdout                   string                `json:"stdout"`
	CoordinationDigest       string                `json:"coordinationDigest,omitempty"`
	TargetIdentityDigest     string                `json:"targetIdentityDigest,omitempty"`
	VerificationPolicyDigest string                `json:"verificationPolicyDigest,omitempty"`
	DriftReportDigest        string                `json:"driftReportDigest,omitempty"`
	ObservedDialect          string                `json:"observedDialect,omitempty"`
	ObservedDrift            bool                  `json:"observedDrift,omitempty"`
	HighestDriftSeverity     string                `json:"highestDriftSeverity,omitempty"`
	DriftFindingCount        int32                 `json:"driftFindingCount,omitempty"`
	DriftFindings            []DriftFindingSummary `json:"driftFindings,omitempty"`
	DriftFindingsTruncated   bool                  `json:"driftFindingsTruncated,omitempty"`
	ObservedArtifactType     string                `json:"observedArtifactType,omitempty"`
	ResolvedDigest           string                `json:"resolvedDigest,omitempty"`
	ResolvedReference        string                `json:"resolvedReference,omitempty"`
	ResolvedMediaType        string                `json:"resolvedMediaType,omitempty"`
	ResolvedSize             int64                 `json:"resolvedSize,omitempty"`
	VerificationRequirements []string              `json:"verificationRequirements,omitempty"`
	PlanContentDigest        string                `json:"planContentDigest,omitempty"`
	PlanOutcome              PlanOutcome           `json:"planOutcome,omitempty"`
	MutationStarted          bool                  `json:"mutationStarted,omitempty"`
	Uncertain                bool                  `json:"uncertain,omitempty"`
	// MigrationHistory is the history a migration-history operation read, and
	// MigrationRun the evidence a migration-apply operation left. Both are the
	// documents Ptah produced, validated before they were carried here: the
	// controller decides from the database's own account rather than from this
	// process's exit status.
	MigrationHistory *dataplane.MigrationStatusReport `json:"migrationHistory,omitempty"`
	MigrationRun     *dataplane.MigrationRunReport    `json:"migrationRun,omitempty"`
	Error            *ResultError                     `json:"error,omitempty"`
	Truncation       *TruncationMetadata              `json:"truncation,omitempty"`
}

// ParseOptions optionally binds a parsed frame to the Job contract that
// created it. A zero ExpectedProtocolVersion selects ProtocolVersion; legacy
// protocol frames are accepted only when their version is explicitly set.
type ParseOptions struct {
	MaxFrameBytes           int64
	ExpectedProtocolVersion int
	ExpectedOperation       Operation
	ExpectedOperationID     string
}

// MarshalFrame encodes a length- and digest-bound frame. The byte length and
// SHA-256 cover exactly the JSON payload bytes between the header and footer.
func MarshalFrame(result Result) ([]byte, error) {
	if result.ProtocolVersion == 0 {
		result.ProtocolVersion = ProtocolVersion
	}
	if err := validateResult(result, ParseOptions{}); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal runner result: %w", err)
	}
	if int64(len(payload)) > DefaultMaxFrameBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes; maximum is %d", ErrFrameTooLarge, len(payload), DefaultMaxFrameBytes)
	}
	digest := sha256.Sum256(payload)
	header := fmt.Sprintf("%s%d %s\n", frameHeader, len(payload), hex.EncodeToString(digest[:]))

	frame := make([]byte, 0, len(header)+len(payload)+len(frameFooter)+1)
	frame = append(frame, header...)
	frame = append(frame, payload...)
	frame = append(frame, frameFooter...)
	frame = append(frame, '\n')
	return frame, nil
}

func WriteFrame(w io.Writer, result Result) error {
	frame, err := MarshalFrame(result)
	if err != nil {
		return err
	}
	written, err := w.Write(frame)
	if err != nil {
		return err
	}
	if written != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

func ParseResult(logs []byte) (Result, error) {
	return ParseResultWithOptions(logs, ParseOptions{})
}

func ParseResultWithLimit(logs []byte, maxFrameBytes int64) (Result, error) {
	return ParseResultWithOptions(logs, ParseOptions{MaxFrameBytes: maxFrameBytes})
}

// ParseResultFor extracts a frame and verifies its operation binding.
func ParseResultFor(logs []byte, expectedOperation Operation, expectedOperationID string) (Result, error) {
	return ParseResultWithOptions(logs, ParseOptions{
		ExpectedOperation:   expectedOperation,
		ExpectedOperationID: expectedOperationID,
	})
}

// ParseResultWithOptions scans mixed Pod logs for complete valid frames. It
// ignores marker-like diagnostic text and returns the last valid frame.
//
// When no frame survives, the error says why the last candidate was rejected.
// The rejections are different failures with different answers -- a log read
// before the frame finished arriving, a build that wrote a frame this one will
// not accept, a payload that does not match its own digest -- and reporting all
// of them as one sentence leaves the reader to guess which happened. The
// reasons name structure and never payload text, so a frame carrying a
// credential cannot disclose it here.
func ParseResultWithOptions(logs []byte, options ParseOptions) (Result, error) {
	limit := options.MaxFrameBytes
	if limit <= 0 {
		limit = DefaultMaxFrameBytes
	}

	marker := []byte(frameHeader)
	searchAt := 0
	sawMarker := false
	sawOversized := false
	var last *Result
	var rejection error
	reject := func(reason string) { rejection = fmt.Errorf("%w: %s", ErrMalformedFrame, reason) }
	rejectIncomplete := func(reason string) { rejection = incompleteFrameError{reason: reason} }
	// A missing footer has two causes with different answers, and the absence
	// alone does not separate them. A log that simply ends was read before the
	// frame finished arriving. A log that runs on past the bound pushed the
	// footer out of reach of the scan. How much log follows the payload says
	// which, and it is structure, so it says so without quoting a byte of the
	// log.
	rejectUnclosed := func(payloadEnd int) {
		if len(logs)-payloadEnd <= maxInterleavedFrameBytes {
			rejectIncomplete("the log ends after the payload without the footer that closes it, so the frame never finished arriving")
		} else {
			reject("no footer closes the payload within the bound on log lines interleaved after it")
		}
	}

	for searchAt < len(logs) {
		relative := bytes.Index(logs[searchAt:], marker)
		if relative < 0 {
			break
		}
		sawMarker = true
		start := searchAt + relative
		headerStart := start + len(marker)
		headerEndRelative := bytes.IndexByte(logs[headerStart:], '\n')
		if headerEndRelative < 0 && len(logs)-headerStart <= 96 {
			rejectIncomplete("the frame header has no end of line within its bounds")
			searchAt = start + len(marker)
			continue
		}
		if headerEndRelative < 0 || headerEndRelative > 96 {
			reject("the frame header has no end of line within its bounds")
			searchAt = start + len(marker)
			continue
		}
		headerEnd := headerStart + headerEndRelative
		fields := bytes.Fields(logs[headerStart:headerEnd])
		if len(fields) != 2 {
			reject("the frame header does not carry a length and a digest")
			searchAt = start + len(marker)
			continue
		}

		payloadLength, err := strconv.ParseInt(string(fields[0]), 10, 64)
		if err != nil || payloadLength < 0 {
			reject("the frame header does not declare a length")
			searchAt = start + len(marker)
			continue
		}
		if payloadLength > limit {
			sawOversized = true
			searchAt = start + len(marker)
			continue
		}
		claimedDigest, err := hex.DecodeString(string(fields[1]))
		if err != nil || len(claimedDigest) != sha256.Size {
			reject("the frame header does not declare a SHA-256 digest")
			searchAt = start + len(marker)
			continue
		}

		declaredStart := headerEnd + 1
		if payloadLength > int64(len(logs)-declaredStart) {
			rejectIncomplete("the log ends before the length the frame header declares, so the frame never finished arriving")
			searchAt = start + len(marker)
			continue
		}
		search := findFramePayload(logs, declaredStart, payloadLength, claimedDigest)
		if !search.found {
			// Nothing within reach hashes to what the header declares. A footer
			// closing any of it says the frame is all here and its payload is
			// wrong; no footer at all says the log was cut, or the footer sits
			// past the bound, which the payload the header points at reports.
			if search.closed {
				reject("the frame payload does not match the digest its header declares")
			} else {
				rejectUnclosed(declaredStart + int(payloadLength))
			}
			searchAt = start + len(marker)
			continue
		}
		payloadStart := search.start
		payloadEnd := payloadStart + int(payloadLength)
		if !search.closed {
			rejectUnclosed(payloadEnd)
			searchAt = start + len(marker)
			continue
		}
		payload := logs[payloadStart:payloadEnd]

		var result Result
		if err := json.Unmarshal(payload, &result); err != nil {
			reject("the frame payload is not a result document this build can decode")
			searchAt = start + len(marker)
			continue
		}
		if err := validateResult(result, options); err != nil {
			rejection = err
			searchAt = start + len(marker)
			continue
		}
		copyOfResult := result
		last = &copyOfResult
		searchAt = search.footerEnd
	}

	if last != nil {
		return *last, nil
	}
	if sawOversized {
		return Result{}, ErrFrameTooLarge
	}
	if sawMarker {
		if rejection != nil {
			return Result{}, rejection
		}
		return Result{}, ErrMalformedFrame
	}
	return Result{}, ErrFrameNotFound
}

// maxInterleavedFrameBytes bounds how far a payload may sit from the header
// line that declares it, and how far past that payload the footer may sit.
// Something has to bound it, or a frame whose footer never arrives would scan
// the whole log for every marker-like line in it.
const maxInterleavedFrameBytes = 64 << 10

// maxInterleavedFrameLines bounds how many line starts the payload search will
// try. The byte bound limits how far it looks; this limits how much it does,
// because every candidate hashes the whole declared payload.
const maxInterleavedFrameLines = 64

// framePayloadSearch is what a scan for a frame's payload settled on.
type framePayloadSearch struct {
	// start is where the payload begins, and footerEnd one past the footer
	// that closes it. Both are set only when found and closed are both true.
	start     int
	footerEnd int
	// found reports that a candidate's bytes hash to the digest the header
	// declares. Nothing else identifies the payload.
	found bool
	// closed reports that a footer closes the payload found -- or, when none
	// was found, that a footer closed some candidate, which says the frame
	// finished arriving even though its payload is not the one declared.
	closed bool
}

// findFramePayload locates the payload of a frame whose header declares
// payloadLength bytes hashing to claimedDigest.
//
// The payload is not required to sit against the header line, for the reason
// the footer is not required to sit against the payload: a container log is
// line-oriented, and the kubelet has merged standard error into standard output
// by the time a line is read, so a diagnostic the runner wrote microseconds
// earlier lands inside the frame it was explaining. After the payload that was
// already tolerated. Ahead of it, it stayed fatal -- the parser took the payload
// by position, hashed a diagnostic, found no footer where the declared length
// happened to land, and reported a frame still arriving for a log where nothing
// was still arriving (seen three times in acceptance, on the custom-CA
// rejection both fixes were found on).
//
// The digest the header declares is what identifies the payload, and it is the
// only thing that does. Each complete line that could begin the payload is
// tried and the one whose bytes hash to that digest is it, so a candidate that
// merely sits where a payload could is accepted only when it is the payload.
// Only complete lines may be skipped: a partial one means the log was cut,
// which is what the footer exists to catch. The bound on interleaving after the
// payload bounds this scan too.
func findFramePayload(logs []byte, declaredStart int, payloadLength int64, claimedDigest []byte) framePayloadSearch {
	var search framePayloadSearch
	limit := declaredStart + maxInterleavedFrameBytes
	if limit > len(logs) {
		limit = len(logs)
	}
	lines := 0
	for candidate := declaredStart; candidate <= limit; {
		// Each candidate costs a hash of the whole payload, so the byte bound
		// alone is not a bound on work: 64 KiB of two-byte lines is 32768
		// candidates, and a log declaring a multi-megabyte payload then takes
		// minutes on the reconcile worker that reads it. A real interleaved
		// diagnostic is a handful of lines, so the line count is bounded too.
		if lines > maxInterleavedFrameLines {
			break
		}
		lines++
		payloadEnd := candidate + int(payloadLength)
		if payloadEnd > len(logs) {
			break
		}
		actualDigest := sha256.Sum256(logs[candidate:payloadEnd])
		if bytes.Equal(claimedDigest, actualDigest[:]) {
			search.start = candidate
			search.found = true
			search.footerEnd, search.closed = frameFooterEnd(logs, payloadEnd)
			return search
		}
		if !search.closed {
			_, search.closed = frameFooterEnd(logs, payloadEnd)
		}
		lineEndRelative := bytes.IndexByte(logs[candidate:limit], '\n')
		if lineEndRelative < 0 {
			break
		}
		candidate += lineEndRelative + 1
	}
	return search
}

// frameFooterEnd finds the end of the footer that closes a payload, and reports
// whether the frame is closed at all.
//
// The footer is not required to sit against the payload. A container log is
// line-oriented and carries two streams: the runner writes the whole frame in
// one call to standard output, and the kubelet still stores it as one entry per
// line, merged with standard error by the time each line was read. A
// diagnostic the runner wrote microseconds earlier therefore lands between the
// payload and the footer often enough to matter -- measured twice on Kubernetes
// 1.37, and reproduced in a retained lab, where the refusal text of a custom-CA
// rejection sat inside the frame it was explaining.
//
// Skipping those lines costs no integrity. The payload is taken by the length
// the header declares and checked against the digest the header carries, so
// what the footer adds is proof that the writer finished rather than proof of
// what it wrote. Only complete lines may be skipped: a partial line means the
// log was cut, which is exactly what the footer exists to catch.
func frameFooterEnd(logs []byte, payloadEnd int) (int, bool) {
	footer := []byte(frameFooter)
	if bytes.HasPrefix(logs[payloadEnd:], footer) {
		return payloadEnd + len(footer), true
	}
	if payloadEnd >= len(logs) || logs[payloadEnd] != '\n' {
		return 0, false
	}
	limit := payloadEnd + maxInterleavedFrameBytes
	if limit > len(logs) {
		limit = len(logs)
	}
	// Every skipped line ends in a newline, and the footer is recognized at the
	// newline that begins it, so the scan walks line starts rather than bytes.
	for lineStart := payloadEnd + 1; lineStart < limit; {
		lineEndRelative := bytes.IndexByte(logs[lineStart:limit], '\n')
		if lineEndRelative < 0 {
			return 0, false
		}
		lineEnd := lineStart + lineEndRelative
		if bytes.HasPrefix(logs[lineEnd:], footer) {
			return lineEnd + len(footer), true
		}
		lineStart = lineEnd + 1
	}
	return 0, false
}

func validateResult(result Result, options ParseOptions) error {
	expectedProtocolVersion := options.ExpectedProtocolVersion
	if expectedProtocolVersion == 0 {
		expectedProtocolVersion = ProtocolVersion
	}
	if expectedProtocolVersion != legacyProtocolVersion && expectedProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: unsupported expected protocol version %d", ErrMalformedFrame, expectedProtocolVersion)
	}
	if result.ProtocolVersion != expectedProtocolVersion {
		return fmt.Errorf(
			"%w: protocol version binding mismatch: got %d, expected %d",
			ErrMalformedFrame,
			result.ProtocolVersion,
			expectedProtocolVersion,
		)
	}
	if !result.Operation.Valid() {
		return fmt.Errorf("%w: unsupported operation %q", ErrMalformedFrame, result.Operation)
	}
	if result.OperationID == "" {
		return fmt.Errorf("%w: missing operation ID", ErrMalformedFrame)
	}
	if len(result.OperationID) > 256 || hasControlCharacter(result.OperationID) {
		return fmt.Errorf("%w: invalid operation ID", ErrMalformedFrame)
	}
	if result.ChildExitCode < -1 {
		return fmt.Errorf("%w: invalid child exit code", ErrMalformedFrame)
	}
	if result.Error != nil && (result.Error.Code == "" || result.Error.Message == "") {
		return fmt.Errorf("%w: incomplete error metadata", ErrMalformedFrame)
	}
	if result.Truncation != nil {
		stdoutValid := result.Truncation.Stdout == (result.Truncation.StdoutBytesDropped > 0)
		stderrValid := result.Truncation.Stderr == (result.Truncation.StderrBytesDropped > 0)
		if !stdoutValid || !stderrValid || (!result.Truncation.Stdout && !result.Truncation.Stderr) {
			return fmt.Errorf("%w: invalid truncation metadata", ErrMalformedFrame)
		}
	}
	if result.Error != nil && result.Error.Code == "invalid_oci_access" {
		preChildOperation := result.Operation == OperationResolve || result.Operation == OperationVerify
		expected := Result{
			ProtocolVersion: result.ProtocolVersion,
			Operation:       result.Operation,
			OperationID:     result.OperationID,
			ChildExitCode:   -1,
			Error:           result.Error,
		}
		if !preChildOperation || !reflect.DeepEqual(result, expected) {
			return fmt.Errorf("%w: invalid OCI access result lacks an exact pre-child binding", ErrMalformedFrame)
		}
	}
	if result.Error == nil {
		if result.ChildExitCode != 0 {
			return fmt.Errorf("%w: successful result has a nonzero child exit code", ErrMalformedFrame)
		}
		if result.Truncation != nil {
			return fmt.Errorf("%w: successful result carries truncated output", ErrMalformedFrame)
		}
		if result.Operation == OperationApply && (!result.MutationStarted || result.Uncertain) {
			return fmt.Errorf("%w: successful apply lacks an exact completed mutation", ErrMalformedFrame)
		}
	}
	if result.Uncertain && !result.MutationStarted {
		return fmt.Errorf("%w: uncertain result without a mutation attempt", ErrMalformedFrame)
	}
	if !result.Operation.Mutating() && (result.MutationStarted || result.Uncertain) {
		return fmt.Errorf("%w: mutation metadata on a read-only operation", ErrMalformedFrame)
	}
	if result.Operation != OperationPlan && result.Stdout != "" {
		return fmt.Errorf("%w: non-plan result carries protected native output", ErrMalformedFrame)
	}
	if operationNeedsDatabase(result.Operation) && result.Error == nil && result.CoordinationDigest == "" {
		return fmt.Errorf("%w: successful database result lacks a coordination digest", ErrMalformedFrame)
	}
	for name, digest := range map[string]string{
		"coordination":        result.CoordinationDigest,
		"target identity":     result.TargetIdentityDigest,
		"verification policy": result.VerificationPolicyDigest,
		"drift report":        result.DriftReportDigest,
		"resolved source":     result.ResolvedDigest,
		"plan content":        result.PlanContentDigest,
	} {
		if digest != "" && !validProtocolDigest(digest) {
			return fmt.Errorf("%w: invalid %s digest", ErrMalformedFrame, name)
		}
	}
	if result.Operation == OperationResolve {
		if result.Error == nil {
			if result.ResolvedReference == "" || result.ResolvedMediaType == "" || result.ResolvedDigest == "" || result.ResolvedSize < 0 {
				return fmt.Errorf("%w: successful resolution lacks typed descriptor evidence", ErrMalformedFrame)
			}
			if err := ocireference.ValidatePinned(result.ResolvedReference, result.ResolvedDigest); err != nil {
				return fmt.Errorf("%w: invalid resolved reference", ErrMalformedFrame)
			}
		}
	} else if result.ResolvedReference != "" || result.ResolvedMediaType != "" || result.ResolvedSize != 0 {
		return fmt.Errorf("%w: resolved descriptor metadata on a non-resolve operation", ErrMalformedFrame)
	}
	if result.ResolvedDigest != "" && result.Operation != OperationResolve && result.Operation != OperationVerify {
		return fmt.Errorf("%w: resolved digest on an unrelated operation", ErrMalformedFrame)
	}
	if result.MigrationHistory != nil && result.Operation != OperationMigrationHistory {
		return fmt.Errorf("%w: migration history on an unrelated operation", ErrMalformedFrame)
	}
	if result.MigrationRun != nil && result.Operation != OperationMigrationApply {
		return fmt.Errorf("%w: migration run report on an unrelated operation", ErrMalformedFrame)
	}
	if result.Error == nil && result.Operation == OperationMigrationHistory && result.MigrationHistory == nil {
		return fmt.Errorf("%w: successful migration history lacks its report", ErrMalformedFrame)
	}
	if result.Error == nil && result.Operation == OperationMigrationApply && result.MigrationRun == nil {
		return fmt.Errorf("%w: successful migration run lacks its report", ErrMalformedFrame)
	}
	if result.ObservedArtifactType != "" && result.Operation != OperationVerify {
		return fmt.Errorf("%w: artifact type on a non-verify operation", ErrMalformedFrame)
	}
	if len(result.VerificationRequirements) > 0 {
		if result.Operation != OperationVerify || result.Error == nil || result.Error.Code != "verification_refused" ||
			len(result.VerificationRequirements) > 64 {
			return fmt.Errorf("%w: verification requirements lack a refusal binding", ErrMalformedFrame)
		}
		previous := ""
		for _, requirement := range result.VerificationRequirements {
			if !runnerRequirementPattern.MatchString(requirement) || previous != "" && strings.Compare(previous, requirement) >= 0 {
				return fmt.Errorf("%w: invalid verification requirement set", ErrMalformedFrame)
			}
			previous = requirement
		}
	}
	if result.Operation == OperationVerify && result.Error != nil && result.Error.Code == "verification_refused" &&
		len(result.VerificationRequirements) == 0 {
		return fmt.Errorf("%w: verification refusal lacks typed requirements", ErrMalformedFrame)
	}
	if result.Operation == OperationVerify && result.Error != nil && result.Error.Code == "verification_refused" {
		localDigestPinRefusal := result.ChildExitCode == 0 &&
			len(result.VerificationRequirements) == 1 && result.VerificationRequirements[0] == "require_digest_pin"
		if result.ChildExitCode != 2 && !localDigestPinRefusal {
			return fmt.Errorf("%w: verification refusal has an invalid child exit binding", ErrMalformedFrame)
		}
	}
	if result.Operation == OperationVerify && result.Error == nil &&
		(result.ResolvedDigest == "" || result.ObservedArtifactType == "") {
		return fmt.Errorf("%w: successful verification lacks typed artifact evidence", ErrMalformedFrame)
	}
	if result.Operation == OperationPlan && result.Error == nil {
		if !result.PlanOutcome.Valid() {
			return fmt.Errorf("%w: successful plan result lacks an explicit outcome", ErrMalformedFrame)
		}
		switch result.PlanOutcome {
		case PlanOutcomeChanges:
			if result.Stdout == "" || result.PlanContentDigest == "" {
				return fmt.Errorf("%w: changed plan outcome lacks immutable content", ErrMalformedFrame)
			}
		case PlanOutcomeNoChanges:
			if result.Stdout != "" || result.PlanContentDigest != "" {
				return fmt.Errorf("%w: no-change plan outcome carries executable content", ErrMalformedFrame)
			}
		}
	} else if result.PlanOutcome != "" {
		return fmt.Errorf("%w: plan outcome is set on a non-successful plan result", ErrMalformedFrame)
	}
	if result.Operation == OperationObserve && result.Error == nil {
		if result.Stdout != "" || result.DriftReportDigest == "" || result.TargetIdentityDigest == "" ||
			!validObservedDialect(result.ObservedDialect) || result.DriftFindingCount < 0 {
			return fmt.Errorf("%w: successful observation lacks a credential-free summary", ErrMalformedFrame)
		}
		if result.ObservedDrift {
			if !validDriftSeverity(result.HighestDriftSeverity) {
				return fmt.Errorf("%w: drift observation has invalid severity", ErrMalformedFrame)
			}
			if err := validateDriftFindingSummaries(result); err != nil {
				return err
			}
		} else if result.HighestDriftSeverity != "" || result.DriftFindingCount != 0 ||
			len(result.DriftFindings) != 0 || result.DriftFindingsTruncated {
			return fmt.Errorf("%w: converged observation carries drift findings", ErrMalformedFrame)
		}
	} else if result.ObservedDialect != "" || result.ObservedDrift || result.HighestDriftSeverity != "" ||
		result.DriftFindingCount != 0 || len(result.DriftFindings) != 0 || result.DriftFindingsTruncated {
		return fmt.Errorf("%w: observation summary is set on a non-successful observation", ErrMalformedFrame)
	}
	if options.ExpectedOperation != "" && result.Operation != options.ExpectedOperation {
		return fmt.Errorf("%w: operation binding mismatch", ErrMalformedFrame)
	}
	if options.ExpectedOperationID != "" && result.OperationID != options.ExpectedOperationID {
		return fmt.Errorf("%w: operation ID binding mismatch", ErrMalformedFrame)
	}
	return nil
}

func validateDriftFindingSummaries(result Result) error {
	const maxFindings = 64
	if result.ProtocolVersion == legacyProtocolVersion {
		if len(result.DriftFindings) != 0 || result.DriftFindingsTruncated {
			return fmt.Errorf("%w: legacy drift observation carries structured findings", ErrMalformedFrame)
		}
		return nil
	}
	if len(result.DriftFindings) == 0 {
		if result.DriftFindingsTruncated {
			return fmt.Errorf("%w: truncated drift observation has no finding summaries", ErrMalformedFrame)
		}
		return fmt.Errorf("%w: drift observation has no finding summaries", ErrMalformedFrame)
	}
	if len(result.DriftFindings) > maxFindings {
		return fmt.Errorf("%w: drift observation has an invalid finding summary count", ErrMalformedFrame)
	}
	var carried int64
	previousRank := int(^uint(0) >> 1)
	previousCategory := ""
	seen := make(map[string]struct{}, len(result.DriftFindings))
	for index, finding := range result.DriftFindings {
		if !dataplane.IsKnownDriftFindingCategory(finding.Category) || finding.Count <= 0 ||
			!validDriftSeverity(finding.Severity) {
			return fmt.Errorf("%w: drift observation has an invalid finding summary", ErrMalformedFrame)
		}
		if _, duplicate := seen[finding.Category]; duplicate {
			return fmt.Errorf("%w: drift observation has duplicate finding summaries", ErrMalformedFrame)
		}
		seen[finding.Category] = struct{}{}
		rank := driftSeverityRank(finding.Severity)
		if index > 0 && (rank > previousRank || rank == previousRank && strings.Compare(previousCategory, finding.Category) >= 0) {
			return fmt.Errorf("%w: drift finding summaries are not in canonical order", ErrMalformedFrame)
		}
		if index == 0 && finding.Severity != result.HighestDriftSeverity {
			return fmt.Errorf("%w: drift finding summaries do not match the highest severity", ErrMalformedFrame)
		}
		carried += int64(finding.Count)
		if carried > int64(^uint32(0)>>1) {
			return fmt.Errorf("%w: drift finding summary count exceeds the supported range", ErrMalformedFrame)
		}
		previousRank = rank
		previousCategory = finding.Category
	}
	if result.DriftFindingsTruncated {
		if len(result.DriftFindings) != maxFindings || carried >= int64(result.DriftFindingCount) {
			return fmt.Errorf("%w: truncated drift finding summaries lack omitted findings", ErrMalformedFrame)
		}
	} else if carried != int64(result.DriftFindingCount) {
		return fmt.Errorf("%w: drift finding summaries do not match the total count", ErrMalformedFrame)
	}
	return nil
}

func validObservedDialect(value string) bool {
	switch value {
	case "postgres", "postgresql", "mysql", "mariadb":
		return true
	default:
		return false
	}
}

func validDriftSeverity(value string) bool {
	switch value {
	case "safe", "info", "warning", "error", "destructive":
		return true
	default:
		return false
	}
}

func driftSeverityRank(value string) int {
	switch value {
	case "safe":
		return 1
	case "info":
		return 2
	case "warning":
		return 3
	case "error":
		return 4
	case "destructive":
		return 5
	default:
		return 0
	}
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func validProtocolDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
