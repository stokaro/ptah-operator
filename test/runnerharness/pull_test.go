package runnerharness_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// pulls is a pull that fails the first failing calls and then succeeds, and
// records each call and each wait.
type pulls struct {
	failing int
	calls   int
	waits   []int
}

func (p *pulls) pull() error {
	p.calls++
	if p.calls <= p.failing {
		return fmt.Errorf("refused %d", p.calls)
	}
	return nil
}

func (p *pulls) wait(attempt int) { p.waits = append(p.waits, attempt) }

func TestRetryPullStopsAtTheFirstSuccess(t *testing.T) {
	for _, failing := range []int{0, 1, pullAttempts - 1} {
		t.Run(fmt.Sprintf("after %d refusals", failing), func(t *testing.T) {
			p := &pulls{failing: failing}
			if err := retryPull(pullAttempts, p.pull, p.wait); err != nil {
				t.Fatalf("retryPull = %v, want nil", err)
			}
			if p.calls != failing+1 {
				t.Errorf("pulled %d times, want %d", p.calls, failing+1)
			}
			if want := waitsBefore(failing); !slices.Equal(p.waits, want) {
				t.Errorf("waited %v, want %v", p.waits, want)
			}
		})
	}
}

// A pull that never succeeds is tried pullAttempts times, with no wait after
// the last, and the error names every refusal.
func TestRetryPullReportsEveryFailure(t *testing.T) {
	p := &pulls{failing: pullAttempts + 1}

	err := retryPull(pullAttempts, p.pull, p.wait)

	if err == nil {
		t.Fatal("retryPull = nil after every pull failed")
	}
	if p.calls != pullAttempts {
		t.Errorf("pulled %d times, want %d", p.calls, pullAttempts)
	}
	if want := waitsBefore(pullAttempts - 1); !slices.Equal(p.waits, want) {
		t.Errorf("waited %v, want %v", p.waits, want)
	}
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		if line := fmt.Sprintf("attempt %d: refused %d", attempt, attempt); !strings.Contains(err.Error(), line) {
			t.Errorf("error %q does not say %q", err, line)
		}
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != pullAttempts {
		t.Errorf("error %q does not hold %d failures", err, pullAttempts)
	}
}

func waitsBefore(attempts int) []int {
	var waits []int
	for attempt := range attempts {
		waits = append(waits, attempt)
	}
	return waits
}
