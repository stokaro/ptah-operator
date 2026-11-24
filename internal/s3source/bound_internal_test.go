package s3source

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
)

// volume is a file system that holds capacity bytes and then answers as a
// full tmpfs does.
type volume struct {
	capacity int64
	held     int64
}

func (v *volume) Write(p []byte) (int, error) {
	room := v.capacity - v.held
	if int64(len(p)) > room {
		v.held += room
		return int(room), syscall.ENOSPC
	}
	v.held += int64(len(p))
	return len(p), nil
}

// TestABoundedCopyNeverWritesPastTheBound holds the copy to what a fetch
// step's volume can take: an object of exactly the bound fits a volume of
// exactly the bound, and a larger one is reported as larger, not as the
// volume running out of space.
func TestABoundedCopyNeverWritesPastTheBound(t *testing.T) {
	t.Parallel()
	const limit = 4096
	for _, test := range []struct {
		name       string
		size       int64
		larger     bool
		wantErr    error
		wantCopied int64
	}{
		{name: "an empty object", size: 0},
		{name: "an object under the bound", size: limit - 1, wantCopied: limit - 1},
		{name: "an object of exactly the bound", size: limit, wantCopied: limit},
		{name: "an object one byte over the bound", size: limit + 1, larger: true, wantCopied: limit},
		{name: "an object far over the bound", size: 4 * limit, larger: true, wantCopied: limit},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			disk := &volume{capacity: limit}
			copied, larger, err := copyBounded(disk, bytes.NewReader(make([]byte, test.size)), limit)
			if err != nil {
				t.Fatalf("copyBounded() error = %v, want none: a volume of the bound ran out of space", err)
			}
			if larger != test.larger || copied != test.wantCopied {
				t.Fatalf("copyBounded() = %d bytes, larger %t; want %d bytes, larger %t", copied, larger, test.wantCopied, test.larger)
			}
			if disk.held != test.wantCopied {
				t.Fatalf("the volume holds %d bytes, want %d", disk.held, test.wantCopied)
			}
		})
	}
}

// TestABoundedCopyPassesOnWhatTheBodyAndTheVolumeSay keeps the reasons a
// copy fails apart from a body that is merely too large: a body that ends
// early and a volume that is full are errors, not a size verdict.
func TestABoundedCopyPassesOnWhatTheBodyAndTheVolumeSay(t *testing.T) {
	t.Parallel()
	const limit = 4096
	t.Run("a body that ends early at the bound", func(t *testing.T) {
		t.Parallel()
		body := io.MultiReader(strings.NewReader(strings.Repeat("a", limit)), errorReader{io.ErrUnexpectedEOF})
		_, larger, err := copyBounded(io.Discard, body, limit)
		if larger || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("copyBounded() larger %t, error %v; want io.ErrUnexpectedEOF", larger, err)
		}
	})
	t.Run("a volume smaller than the bound", func(t *testing.T) {
		t.Parallel()
		_, larger, err := copyBounded(&volume{capacity: limit - 1}, bytes.NewReader(make([]byte, limit)), limit)
		if larger || !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("copyBounded() larger %t, error %v; want ENOSPC from a volume one byte short", larger, err)
		}
	})
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
