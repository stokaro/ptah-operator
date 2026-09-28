package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Check reads the cluster once. It reports whether the awaited state holds,
// and says what it saw in a form a failure message can carry. An error ends
// the wait at once: it is for a reading that no later poll can fix, and a
// transient failure a wait should ride out is an observation instead.
type Check func(ctx context.Context) (done bool, observed string, err error)

// ErrWaitTimeout is what Wait wraps when the awaited state did not arrive.
var ErrWaitTimeout = errors.New("did not arrive in time")

// Wait calls check until it reports done, returns an error, the timeout
// passes, or ctx ends. check runs at least once, and once more at the deadline
// if the interval would carry it past, so the last reading is taken no earlier
// than the deadline allows.
//
// A failure names what was awaited, how long it was given and the last thing
// check saw. A bare timeout costs whoever reads the log the whole
// investigation.
func Wait(ctx context.Context, what string, timeout, interval time.Duration, check Check) error {
	return wait(ctx, what, timeout, interval, check, time.Now, sleep)
}

func wait(
	ctx context.Context,
	what string,
	timeout, interval time.Duration,
	check Check,
	now func() time.Time,
	pause func(context.Context, time.Duration) error,
) error {
	deadline := now().Add(timeout)
	last := "no reading completed"
	for {
		done, observed, err := check(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if observed != "" {
			last = observed
		}
		if done {
			return nil
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return fmt.Errorf("%s: %w within %s; last observed: %s", what, ErrWaitTimeout, timeout, last)
		}
		if err := pause(ctx, min(interval, remaining)); err != nil {
			return fmt.Errorf("%s: %w; last observed: %s", what, err, last)
		}
	}
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
