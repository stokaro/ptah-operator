package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// A log shaped like an attack: a header declaring a multi-megabyte payload,
// then newline-dense filler. Every candidate start costs a hash of the whole
// declared length, so without a bound on candidates this is minutes of work on
// the reconcile worker that reads it.
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
