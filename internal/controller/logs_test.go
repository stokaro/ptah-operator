package controller

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// chunkedReader hands out a fixed number of bytes per Read, because a tail
// that is correct only when each chunk happens to align with the bound is not
// correct. A kubelet's stream aligns with nothing.
type chunkedReader struct {
	data []byte
	size int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(min(len(p), r.size), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// The tail is the end of the log, whatever the log's size and whatever the
// stream hands over at a time.
func TestTheResultLogTailIsTheEndOfTheLog(t *testing.T) {
	t.Parallel()

	const limit = 4096
	for _, size := range []int{0, 1, limit - 1, limit, limit + 1, 3*limit + 17, 64 << 10} {
		for _, chunk := range []int{1, 7, 512, limit - 1, limit, limit + 3, 1 << 20} {
			log := make([]byte, size)
			for i := range log {
				log[i] = byte(i % 251)
			}
			want := log
			if len(want) > limit {
				want = want[len(want)-limit:]
			}

			actual, err := readLogTail(&chunkedReader{data: append([]byte(nil), log...), size: chunk}, limit)
			if err != nil {
				t.Fatalf("size=%d chunk=%d: %v", size, chunk, err)
			}
			if !bytes.Equal(actual, want) {
				t.Fatalf("size=%d chunk=%d: the tail is not the end of the log (%d bytes, want %d)",
					size, chunk, len(actual), len(want))
			}
			if int64(cap(actual)) > limit {
				t.Fatalf("size=%d chunk=%d: the tail reserved %d bytes for a bound of %d",
					size, chunk, cap(actual), limit)
			}
		}
	}
}

// The reason the bound takes the end and not the beginning: the frame is the
// last thing the runner writes, so a log the executor filled ahead of it still
// yields its result.
func TestAResultFrameSurvivesALogFarLargerThanTheBound(t *testing.T) {
	t.Parallel()

	const limit = 128 << 10
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	noise := strings.Repeat("the executor said something about a migration\n", 40<<10)
	log := append([]byte(noise), frame...)
	if int64(len(log)) <= 8*limit {
		t.Fatalf("the log is %d bytes, which is not far larger than the %d-byte bound", len(log), limit)
	}

	tail, err := readLogTail(&chunkedReader{data: log, size: 8191}, limit)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(tail)) != limit {
		t.Fatalf("the tail is %d bytes, want exactly the bound %d", len(tail), limit)
	}
	result, err := runner.ParseResultFor(tail, runner.OperationResolve, testDigest)
	if err != nil {
		t.Fatalf("the frame at the end of an oversized log was lost: %v", err)
	}
	if result.ResolvedDigest != testDigest {
		t.Fatalf("the frame read back as %#v", result)
	}
}

// And the floor under that bound: a tail shorter than the frame cannot hold
// it, which is why runner.MaxResultLogBytes is derived from what the protocol
// admits rather than chosen for the size of the allocation.
func TestATailShorterThanTheFrameLosesIt(t *testing.T) {
	t.Parallel()

	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: testDigest, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := readLogTail(&chunkedReader{data: append([]byte(nil), frame...), size: 64}, int64(len(frame)/2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ParseResultFor(tail, runner.OperationResolve, testDigest); err == nil {
		t.Fatal("half a frame parsed as a result")
	}
}

// A transport that fails part way through is a failed read, not a short log
// that happens to have no frame in it.
func TestAFailedStreamIsNotAShortLog(t *testing.T) {
	t.Parallel()

	failure := errors.New("the stream was reset")
	_, err := readLogTail(io.MultiReader(strings.NewReader("some output\n"), failingStream{failure}), 4096)
	if !errors.Is(err, failure) {
		t.Fatalf("readLogTail() error = %v, want the transport's own failure", err)
	}
}

type failingStream struct{ err error }

func (r failingStream) Read([]byte) (int, error) { return 0, r.err }

// A bound of zero or less names no tail at all, and silently reading the whole
// log instead would be the defect this exists to remove.
func TestAResultLogTailRefusesANonPositiveBound(t *testing.T) {
	t.Parallel()

	for _, limit := range []int64{0, -1} {
		if _, err := readLogTail(strings.NewReader("output"), limit); err == nil {
			t.Fatalf("a bound of %d was accepted", limit)
		}
	}
}
