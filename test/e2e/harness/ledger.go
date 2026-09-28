package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Ledger appends stage rows to the timing ledger the driver names in
// E2E_TIMING_LEDGER. It writes the row hack/e2e-timing.sh writes, byte for
// byte, so hack/e2etiming reads a Go phase's scenarios exactly as it reads a
// shell phase's.
//
// Nothing here can decide a run. The outcome comes from the caller, a ledger
// that cannot be written is reported once and skipped, and a Ledger with no
// path does nothing, which is what a phase run by hand without a ledger gets.
type Ledger struct {
	path     string
	warnings io.Writer

	mu       sync.Mutex
	reported bool
}

// NewLedger returns a ledger appending to path, reporting a failed append to
// warnings once. An empty path records nothing.
func NewLedger(path string, warnings io.Writer) *Ledger {
	return &Ledger{path: path, warnings: warnings}
}

// ledgerRow is one stage, in the field order the shell stopwatch prints.
type ledgerRow struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Seconds int64  `json:"seconds"`
}

// Row appends one stage. Kind is bootstrap, phase or scenario; outcome is
// what the caller already decided.
func (l *Ledger) Row(kind, name, outcome string, start, end time.Time) {
	if l == nil || l.path == "" {
		return
	}
	encoded, err := json.Marshal(ledgerRow{
		Kind:    ledgerLabel(kind),
		Name:    ledgerLabel(name),
		Outcome: ledgerLabel(outcome),
		Start:   ledgerInstant(start),
		End:     ledgerInstant(end),
		Seconds: end.Unix() - start.Unix(),
	})
	if err != nil {
		l.report(err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// One write of one line, appended, as the shell's >> does: phases in other
	// processes append to the same file, and a row must never interleave.
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // The driver names the ledger.
	if err != nil {
		l.reportLocked(err)
		return
	}
	_, writeErr := file.Write(append(encoded, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		l.reportLocked(writeErr)
	} else if closeErr != nil {
		l.reportLocked(closeErr)
	}
}

func (l *Ledger) report(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reportLocked(err)
}

func (l *Ledger) reportLocked(err error) {
	if l.reported {
		return
	}
	l.reported = true
	if l.warnings != nil {
		_, _ = fmt.Fprintf(l.warnings, "e2e timing: %s cannot be appended to (%v); stage durations are lost, the run is not\n",
			l.path, err)
	}
}

// ledgerInstant is the shell's `date -u +%Y-%m-%dT%H:%M:%SZ`.
func ledgerInstant(instant time.Time) string {
	return instant.UTC().Format("2006-01-02T15:04:05Z")
}

// ledgerLabel is the shell's `tr -c 'A-Za-z0-9 ._:/=+-' '-'`, byte by byte:
// it replaces what a label may not carry rather than escaping it, so a row
// stays one short line either writer could have produced.
func ledgerLabel(label string) string {
	labelled := []byte(label)
	for index, character := range labelled {
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			strings.IndexByte(" ._:/=+-", character) >= 0:
		default:
			labelled[index] = '-'
		}
	}
	return string(labelled)
}
