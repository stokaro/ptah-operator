package resultretention_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultretention"
)

func TestWindowEndNamesAnInstantOnlyForAWindowThatHasOne(t *testing.T) {
	ends := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	window := &resultretention.WindowError{Ends: ends}
	for _, row := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "a window", err: window, want: true},
		{name: "a wrapped window", err: fmt.Errorf("authorize: %w", window), want: true},
		{name: "no refusal", err: nil},
		{name: "a window with no persisted start", err: resultretention.ErrWindow},
		{name: "a pin", err: resultretention.ErrPinned},
		{name: "a record that cannot be bound", err: resultretention.ErrRecord},
		{name: "a failed read", err: errors.New("the API server did not answer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, ok := resultretention.WindowEnd(row.err)
			if ok != row.want {
				t.Fatalf("WindowEnd(%v) = %v, want %v", row.err, ok, row.want)
			}
			if ok && !got.Equal(ends) {
				t.Fatalf("WindowEnd(%v) named %s, want %s", row.err, got, ends)
			}
			if !ok && !got.IsZero() {
				t.Fatalf("WindowEnd(%v) refused and still named %s", row.err, got)
			}
		})
	}
}

// Every caller that retains evidence on a window asks errors.Is, so a typed
// refusal has to go on answering it.
func TestWindowErrorIsTheWindowRefusal(t *testing.T) {
	err := error(&resultretention.WindowError{Ends: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)})
	if !errors.Is(err, resultretention.ErrWindow) {
		t.Fatalf("%v does not match ErrWindow", err)
	}
	if errors.Is(err, resultretention.ErrPinned) || errors.Is(err, resultretention.ErrRecord) {
		t.Fatalf("%v matches a refusal it is not", err)
	}
}
