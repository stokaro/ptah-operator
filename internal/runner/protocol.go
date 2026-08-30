package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	// ProtocolVersion is part of the controller-to-Job approval binding. Any
	// incompatible result format must use a new version.
	ProtocolVersion = 1

	// JSON escaping can expand a bounded 8 MiB stdout payload. Keep the parser
	// cap above that worst case while still rejecting unbounded log claims.
	DefaultMaxFrameBytes int64 = 64 << 20

	frameHeader = "PTAH_RUNNER_RESULT_V1 "
	frameFooter = "\nPTAH_RUNNER_RESULT_END_V1"
)

var (
	ErrFrameNotFound  = errors.New("ptah runner result frame not found")
	ErrMalformedFrame = errors.New("malformed ptah runner result frame")
	ErrFrameTooLarge  = errors.New("ptah runner result frame exceeds the configured limit")
)

// Operation is one of the fixed operations understood by the runner.
type Operation string

const (
	OperationResolve Operation = "resolve"
	OperationVerify  Operation = "verify"
	OperationObserve Operation = "observe"
	OperationPlan    Operation = "plan"
	OperationApply   Operation = "apply"
)

func (o Operation) Valid() bool {
	switch o {
	case OperationResolve, OperationVerify, OperationObserve, OperationPlan, OperationApply:
		return true
	default:
		return false
	}
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

// Result is the complete credential-free result emitted by ptah-runner.
type Result struct {
	ProtocolVersion          int                 `json:"protocolVersion"`
	Operation                Operation           `json:"operation"`
	OperationID              string              `json:"operationId"`
	ChildExitCode            int                 `json:"childExitCode"`
	Stdout                   string              `json:"stdout"`
	TargetIdentityDigest     string              `json:"targetIdentityDigest,omitempty"`
	VerificationPolicyDigest string              `json:"verificationPolicyDigest,omitempty"`
	ObservedStateFingerprint string              `json:"observedStateFingerprint,omitempty"`
	ObservedArtifactType     string              `json:"observedArtifactType,omitempty"`
	ResolvedDigest           string              `json:"resolvedDigest,omitempty"`
	PlanContentDigest        string              `json:"planContentDigest,omitempty"`
	MutationStarted          bool                `json:"mutationStarted,omitempty"`
	Uncertain                bool                `json:"uncertain,omitempty"`
	Error                    *ResultError        `json:"error,omitempty"`
	Truncation               *TruncationMetadata `json:"truncation,omitempty"`
}

// ParseOptions optionally binds a parsed frame to the operation that created
// the Job. Empty expected values disable the corresponding check.
type ParseOptions struct {
	MaxFrameBytes       int64
	ExpectedOperation   Operation
	ExpectedOperationID string
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
func ParseResultWithOptions(logs []byte, options ParseOptions) (Result, error) {
	limit := options.MaxFrameBytes
	if limit <= 0 {
		limit = DefaultMaxFrameBytes
	}

	marker := []byte(frameHeader)
	footer := []byte(frameFooter)
	searchAt := 0
	sawMarker := false
	sawOversized := false
	var last *Result

	for searchAt < len(logs) {
		relative := bytes.Index(logs[searchAt:], marker)
		if relative < 0 {
			break
		}
		sawMarker = true
		start := searchAt + relative
		headerStart := start + len(marker)
		headerEndRelative := bytes.IndexByte(logs[headerStart:], '\n')
		if headerEndRelative < 0 || headerEndRelative > 96 {
			searchAt = start + len(marker)
			continue
		}
		headerEnd := headerStart + headerEndRelative
		fields := bytes.Fields(logs[headerStart:headerEnd])
		if len(fields) != 2 {
			searchAt = start + len(marker)
			continue
		}

		payloadLength, err := strconv.ParseInt(string(fields[0]), 10, 64)
		if err != nil || payloadLength < 0 {
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
			searchAt = start + len(marker)
			continue
		}

		payloadStart := headerEnd + 1
		if payloadLength > int64(len(logs)-payloadStart) {
			searchAt = start + len(marker)
			continue
		}
		payloadEnd := payloadStart + int(payloadLength)
		if !bytes.HasPrefix(logs[payloadEnd:], footer) {
			searchAt = start + len(marker)
			continue
		}
		payload := logs[payloadStart:payloadEnd]
		actualDigest := sha256.Sum256(payload)
		if !bytes.Equal(claimedDigest, actualDigest[:]) {
			searchAt = start + len(marker)
			continue
		}

		var result Result
		if err := json.Unmarshal(payload, &result); err != nil {
			searchAt = start + len(marker)
			continue
		}
		if err := validateResult(result, options); err != nil {
			searchAt = start + len(marker)
			continue
		}
		copyOfResult := result
		last = &copyOfResult
		searchAt = payloadEnd + len(footer)
	}

	if last != nil {
		return *last, nil
	}
	if sawOversized {
		return Result{}, ErrFrameTooLarge
	}
	if sawMarker {
		return Result{}, ErrMalformedFrame
	}
	return Result{}, ErrFrameNotFound
}

func validateResult(result Result, options ParseOptions) error {
	if result.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: unsupported protocol version %d", ErrMalformedFrame, result.ProtocolVersion)
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
	if result.Uncertain && !result.MutationStarted {
		return fmt.Errorf("%w: uncertain result without a mutation attempt", ErrMalformedFrame)
	}
	if result.Operation != OperationApply && (result.MutationStarted || result.Uncertain) {
		return fmt.Errorf("%w: mutation metadata on a read-only operation", ErrMalformedFrame)
	}
	for name, digest := range map[string]string{
		"target identity":     result.TargetIdentityDigest,
		"verification policy": result.VerificationPolicyDigest,
		"observed state":      result.ObservedStateFingerprint,
		"resolved source":     result.ResolvedDigest,
		"plan content":        result.PlanContentDigest,
	} {
		if digest != "" && !validProtocolDigest(digest) {
			return fmt.Errorf("%w: invalid %s digest", ErrMalformedFrame, name)
		}
	}
	if options.ExpectedOperation != "" && result.Operation != options.ExpectedOperation {
		return fmt.Errorf("%w: operation binding mismatch", ErrMalformedFrame)
	}
	if options.ExpectedOperationID != "" && result.OperationID != options.ExpectedOperationID {
		return fmt.Errorf("%w: operation ID binding mismatch", ErrMalformedFrame)
	}
	return nil
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
