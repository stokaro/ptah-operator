package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run a real child: this test binary, started again with
// testChildEnv naming what it should do. A signal is a property of a process,
// and a fake executor can only say what a test decided a signal would do.
const (
	testChildEnv      = "PTAH_RUNNER_TEST_CHILD"
	testChildReadyEnv = "PTAH_RUNNER_TEST_CHILD_READY"
	testChildTermEnv  = "PTAH_RUNNER_TEST_CHILD_TERM"

	// childStopsOnTerm answers SIGTERM the way Ptah does: it stops its work,
	// writes its account of it, and exits with the signal status.
	childStopsOnTerm = "stops-on-term"
	// childIgnoresTerm notes the signal and keeps running.
	childIgnoresTerm = "ignores-term"
)

// stoppedBetweenFiles is what `ptah migrations up --json` writes when it is
// stopped after the first of two planned migrations committed and before the
// second began: a failed run that accounts for the one it applied.
const stoppedBetweenFiles = `{"contract_version":1,"direction":"up","outcome":"failed","planned":[3,4],"applied":[3],` +
	`"error":"context canceled","status":{"contract_version":1,"current_version":3,"total_migrations":4,` +
	`"has_pending_changes":true}}` + "\n"

func TestMain(m *testing.M) {
	if mode := os.Getenv(testChildEnv); mode != "" {
		os.Exit(runTestChild(mode))
	}
	os.Exit(m.Run())
}

func runTestChild(mode string) int {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv(testChildReadyEnv), []byte("ready"), 0o600); err != nil {
		return 3
	}
	switch mode {
	case childStopsOnTerm:
		<-terms
		fmt.Print(stoppedBetweenFiles)
		return 143
	case childIgnoresTerm:
		<-terms
		_ = os.WriteFile(os.Getenv(testChildTermEnv), []byte("term"), 0o600)
		time.Sleep(time.Minute)
		return 0
	default:
		return 3
	}
}

// testChild is one started child and the files it reports through.
type testChild struct {
	spec  CommandSpec
	ready string
	term  string
}

func newTestChild(t *testing.T, mode string) testChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	child := testChild{
		ready: filepath.Join(directory, "ready"),
		term:  filepath.Join(directory, "term"),
	}
	child.spec = CommandSpec{Path: self, Env: []string{
		testChildEnv + "=" + mode,
		testChildReadyEnv + "=" + child.ready,
		testChildTermEnv + "=" + child.term,
	}}
	return child
}

// waitReady blocks until the child has installed its handler, so a signal
// sent afterwards is one the child can see.
func (c testChild) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.ready); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the child never wrote %s, so it never reached its signal handler", c.ready)
}

func (c testChild) sawTerm() bool {
	_, err := os.Stat(c.term)
	return err == nil
}

type executed struct {
	exitCode int
	err      error
	stdout   *boundedBuffer
	elapsed  time.Duration
}

// executeAndCancel starts the child, cancels its context once it is ready,
// and reports what Execute returned and how long it took after the cancel.
func executeAndCancel(t *testing.T, executor OSExecutor, child testChild) executed {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := newBoundedBuffer(1 << 20)
	stderr := newBoundedBuffer(1 << 20)
	done := make(chan executed, 1)
	go func() {
		code, err := executor.Execute(ctx, child.spec, stdout, stderr)
		done <- executed{exitCode: code, err: err, stdout: stdout}
	}()
	child.waitReady(t)
	canceled := time.Now()
	cancel()
	select {
	case result := <-done:
		result.elapsed = time.Since(canceled)
		return result
	case <-time.After(30 * time.Second):
		t.Fatal("Execute did not return within 30s of its context being canceled")
		return executed{}
	}
}

// A child that answers SIGTERM stops before the delay runs out, and what it
// wrote on the way out is kept.
func TestOSExecutorStopsAChildWithSIGTERM(t *testing.T) {
	t.Parallel()

	child := newTestChild(t, childStopsOnTerm)
	const delay = 20 * time.Second
	result := executeAndCancel(t, OSExecutor{StopDelay: delay}, child)

	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want the context's cancellation", result.err)
	}
	if result.elapsed >= delay {
		t.Fatalf("a child that answered SIGTERM took %s, the whole stop delay", result.elapsed)
	}
	if got := string(result.stdout.bytes()); got != stoppedBetweenFiles {
		t.Fatalf("the stopped child's report = %q, want the document it wrote on SIGTERM", got)
	}
}

// A child that ignores SIGTERM is killed, and not before the delay.
func TestOSExecutorKillsAChildThatIgnoresSIGTERMAfterTheDelay(t *testing.T) {
	t.Parallel()

	child := newTestChild(t, childIgnoresTerm)
	const delay = 500 * time.Millisecond
	result := executeAndCancel(t, OSExecutor{StopDelay: delay}, child)

	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want the context's cancellation", result.err)
	}
	if !child.sawTerm() {
		t.Fatal("the child never received SIGTERM, so the kill was not preceded by a request to stop")
	}
	if result.elapsed < delay {
		t.Fatalf("the child was killed after %s, before its %s stop delay", result.elapsed, delay)
	}
	// The child sleeps for a minute after SIGTERM, so returning well inside
	// that means it was killed rather than waited for.
	if result.elapsed > 20*time.Second {
		t.Fatalf("the child outlived its stop delay by %s", result.elapsed-delay)
	}
}

// Without a delay the child is killed at once, as exec.CommandContext always
// did: a SIGTERM with nothing bounding what follows would let a child that
// ignores it outlive the runner's deadlines.
func TestOSExecutorWithoutADelayKillsAtOnce(t *testing.T) {
	t.Parallel()

	child := newTestChild(t, childIgnoresTerm)
	result := executeAndCancel(t, OSExecutor{}, child)

	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want the context's cancellation", result.err)
	}
	if child.sawTerm() {
		t.Fatal("a child with no stop delay was sent SIGTERM instead of being killed")
	}
	if result.elapsed > 20*time.Second {
		t.Fatalf("a child with no stop delay took %s to be killed", result.elapsed)
	}
}

// A migration child the runner stopped still reports what it applied: this is
// the whole run, through Run and the real executor, with Ptah's answer to
// SIGTERM played by a child that stops between two files.
func TestMigrationApplyStoppedBetweenFilesReportsWhatItApplied(t *testing.T) {
	t.Parallel()

	child := newTestChild(t, childStopsOnTerm)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		done <- Run(ctx, Config{
			Operation:   OperationMigrationApply,
			PtahBinary:  child.spec.Path,
			Environment: append(migrationApplyEnvironment(t, "migration-apply-stopped-cleanly"), child.spec.Env...),
			TempDir:     t.TempDir(),
		})
	}()
	child.waitReady(t)
	cancel()
	var result Result
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of its context being canceled")
	}

	if result.Error == nil || result.Error.Code != "execution_error" ||
		!strings.Contains(result.Error.Message, "because the Pod is terminating") {
		t.Fatalf("Run() error = %#v, want the stop named as the Pod terminating", result.Error)
	}
	if result.MigrationRun == nil || result.MigrationRun.Outcome != "failed" ||
		!slices.Equal(result.MigrationRun.Applied, []int64{3}) {
		t.Fatalf("Run() migration run = %#v, want the failed run with version 3 applied that the stopped child wrote",
			result.MigrationRun)
	}
	if !result.MutationStarted || result.Uncertain {
		t.Fatalf("a run that stopped cleanly reported MutationStarted=%t Uncertain=%t, want a started mutation the database accounted for",
			result.MutationStarted, result.Uncertain)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}
