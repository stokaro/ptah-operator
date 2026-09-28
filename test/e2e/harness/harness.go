// Package harness runs the acceptance phases test/e2e carries, inside the
// cluster hack/e2e-kind.sh stands up.
//
// The driver keeps the bootstrap: the kind cluster, the images, the registry,
// the databases and the chart install. It builds one test binary from
// test/e2e, and for a phase ported to Go it runs that binary with the phase's
// name. Main picks the test that is the phase out of package phases, bounds it
// by the phase's own timeout, and fails a run in which no test reached the end
// of the phase, so a binary asked for a phase it does not carry cannot pass by
// running nothing; only a phase that passed writes the completion record the
// driver passes it on. Begin loads the inputs the driver bound, and Run records
// the phase's scenarios in the same timing ledger the shell phases write.
package harness

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// timeoutGrace is how far the test binary's own timeout sits past the phase's
// context deadline. The deadline ends every wait with a message that names
// what it was waiting for; the timeout is the backstop that dumps every
// goroutine when something did not honor the deadline.
const timeoutGrace = 2 * time.Minute

var (
	// requested is the phase Main was asked for; Begin holds a test to it.
	requested phases.Phase
	// completed records that the requested phase's test reached its end with
	// every scenario run and nothing failed.
	completed atomic.Bool
)

// Main runs the phase named by -e2e.phase and returns the exit code. A
// package of acceptance phases calls it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
func Main(m *testing.M) int {
	phaseName := flag.String("e2e.phase", "",
		"the acceptance phase to run, as test/e2e/phases names it; the binary runs that phase's test and nothing else")
	record := flag.String("e2e.completed", "",
		"a file to write the phase's name to once the phase has passed, and only then")
	flag.Parse()
	phase, ok := phases.Lookup(*phaseName)
	if !ok {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: -e2e.phase=%q names no phase the Go harness carries; it carries %s\n",
			*phaseName, strings.Join(phaseNames(), ", "))
		return 2
	}
	requested = phase
	// The phase decides what runs and for how long. A -test.skip would pass a
	// scenario over while the phase reported the pass, so it is cleared too.
	for name, value := range map[string]string{
		"test.run":     "^" + regexp.QuoteMeta(phase.Test) + "$",
		"test.skip":    "",
		"test.timeout": (phase.Timeout + timeoutGrace).String(),
	} {
		if err := flag.Set(name, value); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e: select phase %s: set -%s: %v\n", phase.Name, name, err)
			return 2
		}
	}
	code, message := conclude(phase, m.Run(), completed.Load(), *record)
	if message != "" {
		_, _ = fmt.Fprintln(os.Stderr, message)
	}
	return code
}

// conclude turns the test run's exit code into the phase's, and writes the
// completion record when the phase passed. A run that passed without the
// phase's test completing ran nothing that counts: its test was renamed,
// skipped or never matched, and go test reports all three as a pass.
//
// The driver reads the record rather than the exit status alone, so a binary
// that exits 0 without running the phase -- or a program that is not the
// binary at all -- is a failed phase.
func conclude(phase phases.Phase, code int, completed bool, record string) (int, string) {
	if code != 0 {
		return code, ""
	}
	if !completed {
		return 1, fmt.Sprintf("e2e: phase %s: %s did not run to its end, so nothing passed; "+
			"a skipped, renamed or unmatched test is not a passing phase", phase.Name, phase.Test)
	}
	if record != "" {
		if err := os.WriteFile(record, []byte(phase.Name), 0o600); err != nil {
			return 1, fmt.Sprintf("e2e: phase %s passed and its completion could not be recorded: %v", phase.Name, err)
		}
	}
	return 0, ""
}

func phaseNames() []string {
	var names []string
	for _, phase := range phases.All() {
		names = append(names, phase.Name)
	}
	return names
}

// Run is one phase in progress: the scenarios it has run and where their
// durations go.
type Run struct {
	t      *testing.T
	phase  phases.Phase
	ledger *Ledger
	ctx    context.Context

	mu       sync.Mutex
	next     int
	prepared bool
}

// Begin starts phase p in test t. It holds t to the phase the binary was
// asked for, loads the inputs the driver bound, and returns them with the Run
// the phase records its scenarios through. The Run's context ends at the
// phase's timeout.
func Begin[T any](t *testing.T, p phases.Of[T]) (*Run, T) {
	t.Helper()
	switch requested.Name {
	case "":
		t.Fatalf("e2e: %s is phase %s and runs only through harness.Main, which names the phase", p.Test, p.Name)
	case p.Name:
	default:
		t.Fatalf("e2e: the binary was asked for phase %s, and %s is phase %s", requested.Name, p.Test, p.Name)
	}
	inputs, err := phases.Load(p, os.LookupEnv)
	if err != nil {
		t.Fatalf("e2e: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
	run := &Run{
		t:      t,
		phase:  p.Phase,
		ledger: NewLedger(os.Getenv("E2E_TIMING_LEDGER"), os.Stderr),
		ctx:    ctx,
	}
	t.Cleanup(func() {
		cancel()
		if run.finish() {
			completed.Store(true)
		}
	})
	return run, inputs
}

// Context ends when the phase's timeout passes.
func (r *Run) Context() context.Context {
	return r.ctx
}

// Scenario runs body as the phase's next scenario, as a subtest named for it,
// and records its duration and outcome in the timing ledger. It returns
// whether the scenario passed; a phase stops at the first one that did not,
// because each scenario starts from the state the one before it left.
//
// The name has to be the next one the phase declared. A skipped scenario
// fails the phase: it would otherwise read as a scenario that passed.
func (r *Run) Scenario(name string, body func(t *testing.T)) bool {
	r.t.Helper()
	if err := r.advance(name); err != nil {
		r.t.Fatalf("e2e: %v", err)
	}
	started := time.Now()
	ran, skipped := false, false
	passed := r.t.Run(name, func(t *testing.T) {
		ran = true
		defer func() {
			if recovered := recover(); recovered != nil {
				r.ledger.Row("scenario", name, "fail", started, time.Now())
				panic(recovered)
			}
			outcome := "fail"
			switch {
			case t.Skipped():
				skipped, outcome = true, "skipped"
			case !t.Failed():
				outcome = "pass"
			}
			r.ledger.Row("scenario", name, outcome, started, time.Now())
		}()
		body(t)
	})
	switch {
	case !ran:
		// t.Run reports a subtest a filter passed over as one that passed.
		r.t.Fatalf("e2e: phase %s did not run scenario %s; a filter passed it over", r.phase.Name, name)
	case skipped:
		r.t.Fatalf("e2e: phase %s skipped scenario %s, and a skipped scenario is not a passing one", r.phase.Name, name)
	}
	return passed
}

// advance takes the next declared scenario, refusing one out of order.
func (r *Run) advance(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prepared {
		return fmt.Errorf("phase %s ran scenario %s after it stopped at its preparation boundary", r.phase.Name, name)
	}
	if r.next >= len(r.phase.Scenarios) {
		return fmt.Errorf("phase %s ran scenario %s after the last one it declares", r.phase.Name, name)
	}
	if want := r.phase.Scenarios[r.next]; name != want {
		return fmt.Errorf("phase %s ran scenario %s where it declares %s", r.phase.Name, name, want)
	}
	r.next++
	return nil
}

// Prepared ends the phase at its preparation boundary: the scenarios another
// suite runs it for have passed, and the phase's own acceptance is not what
// this run is for. It has to be called exactly there. A phase with no
// preparation mode, or a run that has not reached the boundary or has gone
// past it, fails, so preparation cannot pass for a phase that stopped early.
func (r *Run) Prepared() {
	r.t.Helper()
	if err := r.prepare(); err != nil {
		r.t.Fatalf("e2e: %v", err)
	}
}

func (r *Run) prepare() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.phase.Preparation == 0:
		return fmt.Errorf("phase %s has no preparation mode", r.phase.Name)
	case r.next != r.phase.Preparation:
		return fmt.Errorf("phase %s stopped for preparation after %d scenarios; its preparation is the first %d",
			r.phase.Name, r.next, r.phase.Preparation)
	}
	r.prepared = true
	return nil
}

// finish reports whether the phase reached its end: nothing failed, nothing
// was skipped, and every declared scenario ran, or the run stopped where
// Prepared said. A phase that returned early with every scenario it ran
// passing has not proved the ones it left out.
func (r *Run) finish() bool {
	if r.t.Failed() || r.t.Skipped() {
		return false
	}
	if r.stoppedPrepared() {
		return true
	}
	if missing := r.unrun(); len(missing) > 0 {
		r.t.Errorf("e2e: phase %s ended without running %s", r.phase.Name, strings.Join(missing, ", "))
		return false
	}
	return true
}

// stoppedPrepared reports a run that Prepared ended and that ran nothing past
// the boundary afterwards.
func (r *Run) stoppedPrepared() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepared && r.next == r.phase.Preparation
}

// unrun is the declared scenarios the phase has not reached.
func (r *Run) unrun() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.phase.Scenarios[r.next:]...)
}

// Logf writes a progress line the way the shell phases printed theirs.
func (r *Run) Logf(format string, arguments ...any) {
	r.t.Helper()
	r.t.Logf(format, arguments...)
}

// Stderr is where diagnostics for a failure go, ahead of the failure itself.
func (r *Run) Stderr() io.Writer {
	return os.Stderr
}
