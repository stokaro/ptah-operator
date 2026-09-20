package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A log shaped like an attack: a header declaring a multi-megabyte payload,
// then newline-dense filler. A search that hashed the declared length at every
// line start would spend minutes of it on the reconcile worker that reads the
// log. None of this filler is a line of the declared length, so none of it is
// hashed at all.
func TestFramePayloadSearchIsBoundedAgainstNewlineDenseLogs(t *testing.T) {
	// The declared length has to fit in what follows, or the search stops at
	// the first candidate and measures nothing. Everything after the filler is
	// long enough for every candidate to hash a full declared payload.
	const declared = 8 << 20
	sum := sha256.Sum256(bytes.Repeat([]byte("z"), declared)) // a digest nothing here matches
	var log bytes.Buffer
	fmt.Fprintf(&log, "PTAH_RUNNER_RESULT_V1 %d %s\n", declared, hex.EncodeToString(sum[:]))
	for i := 0; i < 32768; i++ {
		log.WriteString("a\n")
	}
	log.Write(bytes.Repeat([]byte("x"), declared))

	started := time.Now()
	_, err := ParseResultFor(log.Bytes(), OperationResolve, "op")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a log whose declared payload never arrived was accepted")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the payload search took %s on a newline-dense log; it is not bounded", elapsed)
	}
	t.Logf("bounded search finished in %s", elapsed)
}

// A log whose diagnostics run to hundreds of lines before the payload. The
// bound on interleaving is a bound on bytes, and these lines stay well inside
// it, so every one of them is a candidate the search has to reach. A search
// that gave up after a fixed number of candidates would stop short of a payload
// that is present and digest-valid, and the caller would report a complete
// frame as one still arriving -- sending the controller back to re-read a log
// that cannot change, and marking an Apply uncertain once the window ran out.
func TestFramePayloadFoundBehindHundredsOfInterleavedLines(t *testing.T) {
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
	const lines = 512
	diagnostics := bytes.Repeat([]byte("ptah-runner: retrying the registry handshake\n"), lines)
	if len(diagnostics) >= maxInterleavedFrameBytes {
		t.Fatalf("the diagnostics are %d bytes, past the %d-byte bound this test is not about",
			len(diagnostics), maxInterleavedFrameBytes)
	}

	got, err := ParseResultFor(interleavedAfterTheHeaderLine(t, frame, diagnostics), OperationResolve, wanted.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("ParseResultFor() = %#v, want %#v", got, wanted)
	}
}

// The payload search skips a candidate line whose length is not the one the
// header declares, and that is the only reason it can afford to look at every
// line. It is sound because a payload is one whole line: json.Marshal escapes
// every control character, so no newline of the payload's own can divide it.
// A writer that started indenting its JSON would break the search quietly, in
// the direction of reporting complete frames as unfinished, so the property is
// pinned here rather than left to the reader of MarshalFrame.
func TestMarshalledPayloadIsOneWholeLine(t *testing.T) {
	t.Parallel()

	frame, err := MarshalFrame(Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       OperationResolve,
		OperationID:     "sha256:" + strings.Repeat("c", 64),
		ChildExitCode:   -1,
		Error: &ResultError{
			Code: "invalid_oci_access",
			// Newlines a result legitimately carries. They travel escaped,
			// which is the property this pins.
			Message: "registry rejected the handshake:\n  x509: certificate signed by unknown authority\n",
		},
	})
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	headerEnd := bytes.IndexByte(frame, '\n') + 1
	fields := bytes.Fields(frame[len(frameHeader):headerEnd])
	declared, err := strconv.Atoi(string(fields[0]))
	if err != nil {
		t.Fatalf("the frame header does not declare a length: %v", err)
	}
	payload := frame[headerEnd : headerEnd+declared]
	if i := bytes.IndexByte(payload, '\n'); i >= 0 {
		t.Fatalf("the payload carries a newline at byte %d, so it is not one line and the search cannot skip by length", i)
	}
	if frame[headerEnd+declared] != '\n' {
		t.Fatal("the payload is not followed by the newline the footer begins with")
	}
}
