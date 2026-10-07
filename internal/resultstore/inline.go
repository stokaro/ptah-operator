package resultstore

import (
	"context"
	"encoding/json"
	"errors"
)

// The same reserved intent name arbitrates all writers, including a receiver
// from an older build. A small, partially published chunked result must finish
// in its original layout; it cannot be replaced by an inline record.
func (s Store) publishInline(ctx context.Context, b Binding, payload []byte, expectedDigest string, check func(context.Context) error) (Receipt, error) {
	name, _ := Name(b)
	m := manifest{Version: 1, Binding: b, Digest: expectedDigest, Size: int64(len(payload)), Inline: payload}
	encoded, _ := json.Marshal(m)
	intent := record(b.Namespace, name, "intent", owner(apiVersion, b.Kind, b.Name, b.UID), encoded)
	if check != nil {
		if err := check(ctx); err != nil {
			return Receipt{}, err
		}
	}
	if err := s.ensure(ctx, intent); err != nil {
		if errors.Is(err, ErrConflict) {
			previous, readErr := s.read(ctx, intent)
			if readErr != nil {
				return Receipt{}, readErr
			}
			stored, _, decodeErr := admissionManifest(previous)
			if decodeErr == nil && stored.Inline == nil && stored.Binding == b && stored.Digest == expectedDigest && stored.Size == int64(len(payload)) {
				// publishChunks compares the entire immutable manifest, including
				// every chunk digest, before resuming any interrupted writes.
				return s.publishChunks(ctx, b, name, payload, expectedDigest, check)
			}
		}
		return Receipt{}, err
	}
	// A successful CREATE or an identical retry still owes a direct readback.
	_, receipt, err := s.Load(ctx, b)
	return receipt, err
}
