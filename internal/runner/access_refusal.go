package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// The codes of a source whose access the runner refused before Ptah ran: the
// credential owner's grants, the trust the source selects, or the coordinates
// a credential would be sent to. A Resolve or a Verify reports them in its
// frame. The init container that checks the access before a fetch reports the
// same code and the same words in an access refusal.
const (
	CodeInvalidOCIAccess   = "invalid_oci_access"
	CodeInvalidGitAccess   = "invalid_git_access"
	CodeInvalidHTTPSAccess = "invalid_https_access"
)

// An access refusal is the reason a source-access check refused a fetch,
// written into the termination message of the init container that made it.
//
// Before an Observe, a Plan, or a migration's History or Apply fetches a
// source, an init container checks its access: the OCI guard, or the Git or
// HTTPS fetch itself. Such a container ends the Pod before the runner frames
// anything, so without this record the controller can say only which step
// exited. The same check at a Resolve puts its sentence in the frame; this
// carries that sentence, made the same way, to the same status.
//
// It carries the code and the message and nothing else. The message is the
// check's error, redacted with the container's own credentials as a frame's
// error is (setResultError). The checks name grants, keys, fields and hosts,
// never a credential's value, and the redaction is there for the day one
// would.
//
// A Git or an HTTPS fetch falls back to its log for a termination message, so
// the record is held to a shape no log tail can take. It is the whole message,
// one line that starts with the marker. Every line the runner writes there
// starts with "ptah-runner: ", or is free text, which takes the marker out
// (freeText). And it is shorter than the kubelet's log tail, so a tail cut in
// the middle of a line is never one record.
const (
	accessRefusalMarker = "PTAH_RUNNER_ACCESS_REFUSAL_V1 "

	// MaxAccessRefusalBytes bounds the record, marker included. The kubelet
	// keeps at most 2 KiB of a log tail, so a tail it cut short is longer
	// than any record. An OCI fetch Pod has four containers, each with a
	// 3 KiB share of the Pod's 12 KiB, and the runtime may put its own text
	// in front.
	MaxAccessRefusalBytes = 1536
	// maxAccessRefusalMessageBytes bounds the message the record carries.
	// The controller shows 512 bytes of it, as it does a frame's.
	maxAccessRefusalMessageBytes = 1 << 10
)

// AccessRefusal is the record of a refused source access.
type AccessRefusal struct {
	// Code is CodeInvalidOCIAccess, CodeInvalidGitAccess or
	// CodeInvalidHTTPSAccess.
	Code string `json:"code"`
	// Message is the check's sentence, redacted and bounded.
	Message string `json:"message"`
}

// String is the refusal as a frame's error reads: the code, then the
// sentence.
func (r AccessRefusal) String() string {
	return r.Code + ": " + r.Message
}

// AccessRefusalError is a source-access check's refusal, with the record it
// leaves. Its text is the record's message, so a Pod log line made from it
// says what the termination message does, redacted the same way.
type AccessRefusalError struct {
	Refusal AccessRefusal
	err     error
}

func (e *AccessRefusalError) Error() string { return e.Refusal.Message }
func (e *AccessRefusalError) Unwrap() error { return e.err }

// accessRefused records a refusal of the access check under code, with the
// message a frame's error would carry: redacted with the container's
// credentials, as valid UTF-8, bounded. A message JSON has to escape can
// encode to more than the record holds, so it is cut, at a character, until it
// fits: a record that cannot be written costs the status its sentence.
func accessRefused(code string, err error, redactor Redactor) *AccessRefusalError {
	message, _ := sanitizedCapturedText([]byte(err.Error()), redactor, false, maxAccessRefusalMessageBytes)
	refusal := AccessRefusal{Code: code, Message: message}
	for len(refusal.Message) > 0 && len(encodeAccessRefusal(refusal)) > MaxAccessRefusalBytes {
		_, size := utf8.DecodeLastRuneInString(refusal.Message)
		refusal.Message = refusal.Message[:len(refusal.Message)-size]
	}
	return &AccessRefusalError{Refusal: refusal, err: err}
}

// GuardOCISourceAccess is the OCI guard's check: every grant the registry
// credential's Secret makes, and, with snapshotTo, the CA the source selects,
// copied to snapshotTo once it is the one the Secret pins. It is the check a
// Resolve makes (PrepareOCISourceAccess). A refusal reads as the Resolve's
// does, under CodeInvalidOCIAccess, and wraps the AccessRefusalError that
// carries its record.
func GuardOCISourceAccess(reference string, environment []string, snapshotTo string) error {
	var err error
	if snapshotTo == "" {
		err = ValidateOCISourceAccess(reference, environment)
	} else {
		err = SnapshotOCISourceAccess(reference, environment, snapshotTo)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", CodeInvalidOCIAccess, accessRefused(CodeInvalidOCIAccess, err, NewRedactor(environment)))
	}
	return nil
}

// EncodeAccessRefusal returns the termination record of refusal.
func EncodeAccessRefusal(refusal AccessRefusal) ([]byte, error) {
	if err := refusal.validate(); err != nil {
		return nil, err
	}
	encoded := encodeAccessRefusal(refusal)
	if len(encoded) > MaxAccessRefusalBytes {
		return nil, fmt.Errorf("the access refusal is %d bytes; the maximum is %d", len(encoded), MaxAccessRefusalBytes)
	}
	return encoded, nil
}

// encodeAccessRefusal is the record of refusal, unchecked: the marker, the
// document and a line break.
func encodeAccessRefusal(refusal AccessRefusal) []byte {
	var encoded bytes.Buffer
	encoded.WriteString(accessRefusalMarker)
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	// Two strings encode whatever they hold: invalid UTF-8 becomes U+FFFD.
	_ = encoder.Encode(refusal)
	return encoded.Bytes()
}

// ParseAccessRefusal reads the access refusal an init container's termination
// message holds. The message has to be the record and nothing else: one line,
// starting with the marker, within its bound, holding one document this build
// decodes with no field it does not know. Anything else is refused, the
// runtime's own text in front included, so a log tail that quotes a record is
// never read as one.
func ParseAccessRefusal(message string) (AccessRefusal, error) {
	if !strings.HasPrefix(message, accessRefusalMarker) {
		return AccessRefusal{}, errors.New("the termination message is not an access refusal")
	}
	if len(message) > MaxAccessRefusalBytes {
		return AccessRefusal{}, errors.New("the access refusal exceeds its bound")
	}
	document := strings.TrimSuffix(message[len(accessRefusalMarker):], "\n")
	if strings.ContainsAny(document, "\r\n") {
		return AccessRefusal{}, errors.New("the access refusal is not one line")
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var refusal AccessRefusal
	if err := decoder.Decode(&refusal); err != nil {
		return AccessRefusal{}, errors.New("the access refusal is not a document this build can decode")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return AccessRefusal{}, errors.New("the access refusal carries data after its document")
	}
	if err := refusal.validate(); err != nil {
		return AccessRefusal{}, err
	}
	return refusal, nil
}

func (r AccessRefusal) validate() error {
	switch r.Code {
	case CodeInvalidOCIAccess, CodeInvalidGitAccess, CodeInvalidHTTPSAccess:
	default:
		return errors.New("the access refusal carries a code that is not a source access refusal")
	}
	switch {
	case r.Message == "":
		return errors.New("the access refusal carries no message")
	case len(r.Message) > maxAccessRefusalMessageBytes:
		return errors.New("the access refusal's message exceeds its bound")
	case !utf8.ValidString(r.Message):
		return errors.New("the access refusal's message is not valid UTF-8")
	}
	return nil
}
