package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// These tests start this test binary twice over: once as the runner, which is
// sent a real SIGTERM the way the kubelet sends one, and once more, by that
// runner, as the Ptah child it supervises. Which one a process is follows from
// its arguments: the runner is started with flags, and it starts Ptah with a
// verb.
const (
	testRoleEnv           = "PTAH_RUNNER_TEST_PTAH"
	testReadyEnv          = "PTAH_RUNNER_TEST_READY"
	testTermEnv           = "PTAH_RUNNER_TEST_TERM"
	testTerminationLogEnv = "PTAH_RUNNER_TEST_TERMINATION_LOG"

	// ptahStopsBetweenFiles answers SIGTERM as the pinned Ptah does: it
	// finishes the migration it is in, writes its account, and exits 143.
	ptahStopsBetweenFiles = "stops-between-files"
	// ptahIgnoresTerm notes the signal and keeps running.
	ptahIgnoresTerm = "ignores-term"

	// stoppedBetweenFiles is the account a run stopped after the first of two
	// planned migrations committed leaves: failed, with that one applied.
	stoppedBetweenFiles = `{"contract_version":1,"direction":"up","outcome":"failed","planned":[3,4],"applied":[3],` +
		`"error":"context canceled","status":{"contract_version":1,"current_version":3,"total_migrations":4,` +
		`"has_pending_changes":true}}` + "\n"
)

func TestMain(m *testing.M) {
	mode := os.Getenv(testRoleEnv)
	switch {
	case mode == "":
		os.Exit(m.Run())
	case len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--"):
		os.Exit(runUntilSignaled(os.Args[1:], os.Stdout, os.Stderr, os.Environ(), os.Getenv(testTerminationLogEnv)))
	default:
		os.Exit(runTestPtah(mode))
	}
}

func runTestPtah(mode string) int {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv(testReadyEnv), []byte("ready"), 0o600); err != nil {
		return 3
	}
	<-terms
	switch mode {
	case ptahStopsBetweenFiles:
		fmt.Print(stoppedBetweenFiles)
		return 143
	case ptahIgnoresTerm:
		_ = os.WriteFile(os.Getenv(testTermEnv), []byte("term"), 0o600)
		time.Sleep(time.Minute)
		return 0
	default:
		return 3
	}
}

// signaledRun is what one runner process left behind after it was sent
// SIGTERM.
type signaledRun struct {
	exitCode       int
	stdout, stderr string
	summary        string
	afterSignal    time.Duration
	ptahSawTerm    bool
	operationID    string
}

// runAndSignal starts the runner for one migration Apply whose Pod has grace,
// waits until its Ptah child is running, and sends the runner SIGTERM.
func runAndSignal(t *testing.T, mode string, grace time.Duration) signaledRun {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	term := filepath.Join(directory, "term")
	terminationLog := filepath.Join(directory, "termination-log")
	// The kubelet mounts the file before the container starts; the runner
	// writes into it and never creates it.
	if err := os.WriteFile(terminationLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	operationID := "sha256:" + strings.Repeat("a", 64)
	command := exec.Command(self, "--ptah-binary", self, "--operation", string(runner.OperationMigrationApply))
	command.Env = append(migrationApplyEnvironment(t, operationID, grace),
		testRoleEnv+"="+mode,
		testReadyEnv+"="+ready,
		testTermEnv+"="+term,
		testTerminationLogEnv+"="+terminationLog,
		"TMPDIR="+directory,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Ptah child never started under the runner: stderr=%q", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	signaled := time.Now()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(30 * time.Second):
		t.Fatalf("the runner did not exit within 30s of SIGTERM: stderr=%q", stderr.String())
	}
	run := signaledRun{
		afterSignal: time.Since(signaled), stdout: stdout.String(), stderr: stderr.String(),
		operationID: operationID,
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatal(waitErr)
	}
	summary, err := os.ReadFile(terminationLog)
	if err != nil {
		t.Fatal(err)
	}
	run.summary = string(summary)
	_, err = os.Stat(term)
	run.ptahSawTerm = err == nil
	return run
}

// The runner passes SIGTERM on, reads the account Ptah wrote as it stopped,
// and writes the frame and the summary of it, inside the Pod's grace.
func TestSIGTERMStopsAMigrationBetweenFilesAndReportsWhatItApplied(t *testing.T) {
	t.Parallel()

	const grace = 6 * time.Second
	run := runAndSignal(t, ptahStopsBetweenFiles, grace)

	if run.exitCode != 0 {
		t.Fatalf("runner exit code = %d, want 0 for a written frame: stderr=%q", run.exitCode, run.stderr)
	}
	if run.afterSignal >= runner.ChildStopDelay(grace) {
		t.Fatalf("the runner took %s after SIGTERM for a child that stopped at once", run.afterSignal)
	}
	result, err := runner.ParseResultFor([]byte(run.stdout), runner.OperationMigrationApply, run.operationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v, stdout=%q stderr=%q", err, run.stdout, run.stderr)
	}
	if result.MigrationRun == nil || result.MigrationRun.Outcome != "failed" ||
		!slices.Equal(result.MigrationRun.Applied, []int64{3}) {
		t.Fatalf("frame migration run = %#v, want the failed run with version 3 applied", result.MigrationRun)
	}
	if result.Error == nil || !strings.Contains(result.Error.Message, "because the Pod is terminating") {
		t.Fatalf("frame error = %#v, want the stop named", result.Error)
	}
	summary, err := runner.ParseSummaryFor(run.summary, runner.OperationMigrationApply, run.operationID)
	if err != nil {
		t.Fatalf("ParseSummaryFor() error = %v, termination log = %q", err, run.summary)
	}
	want := runner.MigrationSummary{Outcome: "failed", AppliedCount: 1, FirstApplied: 3, LastApplied: 3}
	if summary.Migration == nil || *summary.Migration != want {
		t.Fatalf("summary migration = %#v, want %#v", summary.Migration, want)
	}
	if err := summary.StandsInFor([]byte(run.stdout), nil); err == nil {
		t.Fatal("the summary stood in for a frame the log holds")
	}
	if err := summary.StandsInFor(nil, runner.ErrFrameNotFound); err != nil {
		t.Fatalf("the summary could not stand in for a lost log: %v", err)
	}
	cut := []byte(run.stdout[:len(run.stdout)-len("\nPTAH_RUNNER_RESULT_END_V1\n")])
	if _, parseErr := runner.ParseResultFor(cut, runner.OperationMigrationApply, run.operationID); summary.StandsInFor(cut, parseErr) != nil {
		t.Fatalf("the summary disagrees with the frame header it was written beside: %v", summary.StandsInFor(cut, parseErr))
	}
}

// A child that ignores SIGTERM is killed once the stop delay has passed, and
// the runner still writes its frame before the grace runs out.
func TestSIGTERMKillsAChildThatIgnoresItAfterTheDelay(t *testing.T) {
	t.Parallel()

	const grace = 6 * time.Second
	run := runAndSignal(t, ptahIgnoresTerm, grace)

	if !run.ptahSawTerm {
		t.Fatal("the child was never sent SIGTERM, so it was killed without being asked to stop")
	}
	if delay := runner.ChildStopDelay(grace); run.afterSignal < delay {
		t.Fatalf("the child was killed %s after SIGTERM, before its %s stop delay", run.afterSignal, delay)
	}
	if run.afterSignal >= grace {
		t.Fatalf("the runner exited %s after SIGTERM, past the %s grace the kubelet would have allowed", run.afterSignal, grace)
	}
	if run.exitCode != 0 {
		t.Fatalf("runner exit code = %d, want 0 for a written frame: stderr=%q", run.exitCode, run.stderr)
	}
	result, err := runner.ParseResultFor([]byte(run.stdout), runner.OperationMigrationApply, run.operationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v, stdout=%q stderr=%q", err, run.stdout, run.stderr)
	}
	if result.MigrationRun != nil || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("frame = %#v, want a started, uncertain run with no account of the database", result)
	}
	summary, err := runner.ParseSummaryFor(run.summary, runner.OperationMigrationApply, run.operationID)
	if err != nil {
		t.Fatalf("ParseSummaryFor() error = %v, termination log = %q", err, run.summary)
	}
	if summary.Migration != nil || !summary.MutationStarted || !summary.Uncertain {
		t.Fatalf("summary = %#v, want the same started, uncertain run", summary)
	}
}

// Without a mounted termination log the frame is still written: the summary
// is a copy of part of it, and losing the copy costs nothing else.
func TestRunWritesTheFrameWhenNoTerminationLogIsMounted(t *testing.T) {
	t.Parallel()

	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Fatalf("echo is unavailable: %v", err)
	}
	directory := t.TempDir()
	mounted := filepath.Join(directory, "termination-log")
	if err := os.WriteFile(mounted, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name        string
		path        string
		wantSummary bool
	}{
		{name: "mounted", path: mounted, wantSummary: true},
		{name: "not mounted", path: filepath.Join(directory, "missing")},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(),
			[]string{"--ptah-binary", echo, "--operation", "resolve"},
			&stdout, &stderr,
			[]string{"PTAH_OPERATION_ID=resolve-summary", "PTAH_REQUESTED_REFERENCE=oci://registry.example/schema:main", runnerProtocol},
			row.path,
		)
		if code != 0 {
			t.Fatalf("%s: run() exit code = %d, stderr = %q", row.name, code, stderr.String())
		}
		if _, err := runner.ParseResultFor(stdout.Bytes(), runner.OperationResolve, "resolve-summary"); err != nil {
			t.Fatalf("%s: ParseResultFor() error = %v", row.name, err)
		}
		content, _ := os.ReadFile(row.path)
		summary, err := runner.ParseSummaryFor(string(content), runner.OperationResolve, "resolve-summary")
		if row.wantSummary != (err == nil) {
			t.Fatalf("%s: termination log = %q, %v, want a summary=%t", row.name, content, err, row.wantSummary)
		}
		if row.wantSummary && summary.ErrorCode == "" {
			t.Fatalf("%s: summary = %#v, want the frame's error code", row.name, summary)
		}
		if !row.wantSummary && !strings.Contains(stderr.String(), "could not write the termination summary") {
			t.Fatalf("%s: stderr = %q, want the missing summary said", row.name, stderr.String())
		}
	}
}

// migrationApplyEnvironment is what a migration Apply Pod carries, with the
// bindings the runner checks before it starts Ptah.
func migrationApplyEnvironment(t *testing.T, operationID string, grace time.Duration) []string {
	t.Helper()
	const databaseURL = "postgres://app:secret@db.example/app"
	target, err := runner.TargetIdentityDigest(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	entries := []fingerprint.SequenceEntry{
		{Version: 3, VersionKey: "3", Checksum: "sha256:" + strings.Repeat("e", 64)},
		{Version: 4, VersionKey: "4", Checksum: "sha256:" + strings.Repeat("f", 64), TransactionMode: "file"},
	}
	sequence, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	sequenceDigest, err := fingerprint.MigrationSequenceDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	coordination := "sha256:" + strings.Repeat("9", 64)
	return []string{
		runner.EnvOperationID + "=" + operationID,
		runner.EnvDatabaseURL + "=" + databaseURL,
		runner.EnvExpectedDatabaseEngine + "=PostgreSQL",
		runner.EnvCoordinationDigest + "=" + coordination,
		runner.EnvExpectedCoordinationDigest + "=" + coordination,
		runner.EnvExpectedTargetIdentityDigest + "=" + target,
		runner.EnvExpectedMigrationSequence + "=" + string(sequence),
		runner.EnvExpectedMigrationSequenceDigest + "=" + sequenceDigest,
		runner.EnvExpectedMigrationHistoryFingerprint + "=sha256:" + strings.Repeat("d", 64),
		runner.EnvDispatchNotAfter + "=2099-01-01T00:00:00Z",
		runner.EnvExecutionNotAfter + "=2099-01-01T00:00:00Z",
		runner.EnvTerminationGracePeriod + "=" + strconv.FormatInt(int64(grace/time.Second), 10),
		runner.EnvMigrationsDir + "=/source/migrations",
		runnerProtocol,
	}
}
