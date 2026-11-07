// Package recording is what the runner harness keeps of a run inside the
// executor image: a runner.Executor that records every child it starts, and the
// transcript test/runnerharness/recorder prints for the harness to read.
package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// stopDelay bounds how long a canceled child may keep running. The harness
// stops nothing on purpose, so it only matters when a run is abandoned.
const stopDelay = 20 * time.Second

// Child is one process the runner started.
type Child struct {
	Args   []string `json:"args"`
	Stdout []byte   `json:"stdout"`
	Stderr []byte   `json:"stderr"`
	Exit   int      `json:"exit"`
	// PeakKiB is the child's maximum resident set, in KiB.
	PeakKiB int64 `json:"peakKiB"`
	// Error is what stopped the child from being run or finished, if anything
	// did. A child that exited nonzero has none.
	Error string `json:"error,omitempty"`
}

// Transcript is everything one container's run says, printed on its standard
// output as one JSON document.
type Transcript struct {
	Children []Child `json:"children"`
	// Frame is the result frame the runner would have written, for an
	// operation. A fetch or a guard writes none.
	Frame []byte `json:"frame,omitempty"`
	// SummaryError is why the frame has no termination summary.
	SummaryError string `json:"summaryError,omitempty"`
	// Refusal is the error a fetch or a guard returned.
	Refusal string `json:"refusal,omitempty"`
	// TerminationMessage is what a schema fetch would leave in its termination
	// message, the one thing it says that the manager reads.
	TerminationMessage string `json:"terminationMessage,omitempty"`
}

// Executor runs a child as runner.OSExecutor does and keeps its arguments,
// output, exit status and peak memory. AppendArgs goes on the end of every
// child's arguments; it is how a row asks for the plan validation's --dry-run
// where no operation reaches it.
type Executor struct {
	AppendArgs []string

	mu       sync.Mutex
	children []Child
}

// Children is what ran, in order.
func (e *Executor) Children() []Child {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.children)
}

// Execute implements runner.Executor.
func (e *Executor) Execute(ctx context.Context, spec runner.CommandSpec, stdout, stderr io.Writer) (int, error) {
	args := append(slices.Clone(spec.Args), e.AppendArgs...)
	command := exec.CommandContext(ctx, spec.Path, args...)
	command.Env = slices.Clone(spec.Env)
	var kept, keptErrors bytes.Buffer
	command.Stdout = io.MultiWriter(stdout, &kept)
	command.Stderr = io.MultiWriter(stderr, &keptErrors)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = stopDelay

	err := command.Run()
	child := Child{Args: args, Stdout: kept.Bytes(), Stderr: keptErrors.Bytes(), PeakKiB: peakKiB(command.ProcessState)}
	exit, returned := classify(ctx, err)
	child.Exit = exit
	if returned != nil {
		child.Error = returned.Error()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.children = append(e.children, child)
	return exit, returned
}

// classify answers as runner.OSExecutor does: a child that ran has an exit
// status, a canceled context is the context's error, and anything else is an
// error of its own.
func classify(ctx context.Context, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() >= 0 {
		return exitError.ExitCode(), nil
	}
	return -1, fmt.Errorf("execute ptah process: %w", err)
}

// peakKiB is a finished child's maximum resident set. Linux reports it in KiB;
// the harness runs nowhere else.
func peakKiB(state *os.ProcessState) int64 {
	if state == nil {
		return 0
	}
	if usage, ok := state.SysUsage().(*syscall.Rusage); ok {
		return usage.Maxrss
	}
	return 0
}
