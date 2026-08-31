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
	"strings"

	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const (
	// ProtocolVersion is part of the controller-to-Job approval binding. Any
	// incompatible result format must use a new version.
	ProtocolVersion = 3

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

// Result is the complete credential-free result emitted by ptah-runner.
type Result struct {
	ProtocolVersion          int                 `json:"protocolVersion"`
	Operation                Operation           `json:"operation"`
	OperationID              string              `json:"operationId"`
	ChildExitCode            int                 `json:"childExitCode"`
	Stdout                   string              `json:"stdout"`
	CoordinationDigest       string              `json:"coordinationDigest,omitempty"`
	TargetIdentityDigest     string              `json:"targetIdentityDigest,omitempty"`
	VerificationPolicyDigest string              `json:"verificationPolicyDigest,omitempty"`
	DriftReportDigest        string              `json:"driftReportDigest,omitempty"`
	ObservedDialect          string              `json:"observedDialect,omitempty"`
	ObservedDrift            bool                `json:"observedDrift,omitempty"`
	HighestDriftSeverity     string              `json:"highestDriftSeverity,omitempty"`
	DriftFindingCount        int32               `json:"driftFindingCount,omitempty"`
	ObservedArtifactType     string              `json:"observedArtifactType,omitempty"`
	ResolvedDigest           string              `json:"resolvedDigest,omitempty"`
	ResolvedReference        string              `json:"resolvedReference,omitempty"`
	ResolvedMediaType        string              `json:"resolvedMediaType,omitempty"`
	ResolvedSize             int64               `json:"resolvedSize,omitempty"`
	VerificationRequirements []string            `json:"verificationRequirements,omitempty"`
	PlanContentDigest        string              `json:"planContentDigest,omitempty"`
	PlanOutcome              PlanOutcome         `json:"planOutcome,omitempty"`
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
	if result.Operation != OperationApply && (result.MutationStarted || result.Uncertain) {
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
		} else if result.HighestDriftSeverity != "" || result.DriftFindingCount != 0 {
			return fmt.Errorf("%w: converged observation carries drift findings", ErrMalformedFrame)
		}
	} else if result.ObservedDialect != "" || result.ObservedDrift || result.HighestDriftSeverity != "" || result.DriftFindingCount != 0 {
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
