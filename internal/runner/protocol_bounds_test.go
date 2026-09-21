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

// The other shape of the same attack, and the one the length filter alone does
// not stop: a header declaring a payload so short that every filler line is
// exactly that length. The hashing stays bounded -- 32768 hashes of one byte
// cost nothing -- but each of those candidates is a plausible payload, so each
// one used to ask whether a footer closed it, and each question walked the rest
// of the window line by line. That is quadratic, about half a billion
// iterations here, on the reconcile worker that is reading the log. Runner logs
// are unbounded and carry whatever the child wrote, so the shape is reachable.
func TestFrameFooterSearchIsLinearOverManySameLengthLines(t *testing.T) {
	t.Parallel()

	const declared = 1
	sum := sha256.Sum256([]byte("z")) // a digest none of the filler matches
	var log bytes.Buffer
	fmt.Fprintf(&log, "PTAH_RUNNER_RESULT_V1 %d %s\n", declared, hex.EncodeToString(sum[:]))
	// Every one of these is a line of exactly the declared length, and no
	// footer closes any of them.
	for i := 0; i < 32768; i++ {
		log.WriteString("x\n")
	}

	started := time.Now()
	_, err := ParseResultFor(log.Bytes(), OperationResolve, "op")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a log whose declared payload never matched was accepted")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the footer search took %s over same-length lines; it is not linear", elapsed)
	}
	t.Logf("linear footer search finished in %s", elapsed)
}

// The third shape, and the one no per-frame bound reaches: many headers rather
// than many lines under one.
//
// Each header starts its own search over its own window, and the windows of
// successive headers overlap almost completely, so a log full of syntactically
// valid headers pays a whole window for each. Runner logs carry whatever the
// child wrote, so a child can write them.
func TestFrameSearchIsBoundedAcrossManyHeaders(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256(bytes.Repeat([]byte("z"), 4096)) // a digest nothing here matches
	var log bytes.Buffer
	// Ten megabytes of headers, each syntactically valid and each declaring a
	// payload that is never there.
	for log.Len() < 10<<20 {
		fmt.Fprintf(&log, "PTAH_RUNNER_RESULT_V1 %d %s\n", 4096, hex.EncodeToString(sum[:]))
	}

	started := time.Now()
	_, err := ParseResultFor(log.Bytes(), OperationResolve, "op")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a log of headers that declare payloads nothing carries was accepted")
	}
	// The refusal has to be terminal. Reporting this as a frame still arriving
	// would send the controller back to re-read a log that cannot change, and
	// then mark an Apply uncertain when the window ran out.
	if MayStillArrive(err) {
		t.Fatalf("a log the parser refused to finish searching was reported as still arriving: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the search took %s over many headers; it is not bounded across them", elapsed)
	}
	t.Logf("bounded across headers in %s", elapsed)
}

// The shape a fixed per-header charge misses: few headers, each with an
// enormous reach.
//
// Sixty-four headers is nothing against a budget counted in windows, but each
// declares the largest payload the protocol allows and is followed by a suffix
// with no newline in it, so the footer search and the search for the end of a
// candidate line each read most of the log. Charging a window apiece counts a
// few megabytes while the parse walks gigabytes, on the reconcile worker that
// is reading the log.
func TestFrameSearchChargesTheSpanItActuallyScans(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("z")) // a digest nothing here matches
	var log bytes.Buffer
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&log, "PTAH_RUNNER_RESULT_V1 %d %s\n", DefaultMaxFrameBytes, hex.EncodeToString(sum[:]))
	}
	// Longer than the declared length, or every header is refused for not
	// fitting and nothing is searched at all. No newline anywhere in it, so
	// every search for the end of a line runs to the end of the log.
	log.Write(bytes.Repeat([]byte("x"), int(DefaultMaxFrameBytes)+(8<<20)))

	started := time.Now()
	_, err := ParseResultFor(log.Bytes(), OperationResolve, "op")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a log of headers declaring payloads nothing carries was accepted")
	}
	// The wall clock is the wrong assertion here: these scans are IndexByte
	// over memory, so charging a fixed window instead of the span costs
	// milliseconds at this size and seconds only at a log size a test cannot
	// allocate. What separates them exactly is which refusal comes out. One
	// header of this reach spends the whole budget, so the next is refused for
	// it; a fixed charge of one window apiece leaves room for all sixty-four,
	// and the refusal is then whatever the last header happened to be.
	if !strings.Contains(err.Error(), "more frame headers than one read will search") {
		t.Fatalf("refusal = %v, want the budget refusing to search further", err)
	}
	if MayStillArrive(err) {
		t.Fatalf("a log the parser refused to finish searching was reported as still arriving: %v", err)
	}
	t.Logf("span-charged search finished in %s", elapsed)
}
