package runner

import (
	"bytes"
	"sync"
)

type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int64
	total int64
}

func newBoundedBuffer(limit int64) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

// Write always reports the complete input as consumed. Bytes beyond the
// retention limit are counted and discarded so a noisy child cannot block.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.total += int64(len(p))
	remaining := b.limit - int64(b.buf.Len())
	if remaining > 0 {
		keep := int64(len(p))
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.buf.Write(p[:int(keep)])
	}
	return len(p), nil
}

func (b *boundedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *boundedBuffer) dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	dropped := b.total - int64(b.buf.Len())
	if dropped < 0 {
		return 0
	}
	return dropped
}
