package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteWritesStartupFailureOnlyToPrivateFiles(t *testing.T) {
	requirePrivateModeSemantics(t)

	directory := t.TempDir()
	logPath := filepath.Join(directory, "capture.log")
	statusPath := filepath.Join(directory, "capture.status")
	readyPath := filepath.Join(directory, "capture.ready")
	errorPath := filepath.Join(directory, "capture.error")
	renderPath := filepath.Join(directory, "render.json")
	render, err := json.Marshal(validRenderedJob())
	if err != nil {
		t.Fatalf("encode render: %v", err)
	}
	if err := os.WriteFile(renderPath, render, 0o600); err != nil {
		t.Fatalf("write render: %v", err)
	}
	stdoutPath := filepath.Join(directory, "stdout")
	stderrPath := filepath.Join(directory, "stderr")
	stdout, err := os.OpenFile(stdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create stdout capture: %v", err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = stdout.Close()
		t.Fatalf("create stderr capture: %v", err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	var exitCode int
	func() {
		defer func() {
			os.Stdout, os.Stderr = originalStdout, originalStderr
		}()
		os.Stdout, os.Stderr = stdout, stderr
		exitCode = execute([]string{
			"--kubeconfig", filepath.Join(directory, "missing-kubeconfig"),
			"--namespace", testNamespace,
			"--job-name", testJobName,
			"--render-file", renderPath,
			"--log-file", logPath,
			"--status-file", statusPath,
			"--ready-file", readyPath,
			"--error-file", errorPath,
		})
	}()
	if err := stdout.Close(); err != nil {
		t.Fatalf("close stdout capture: %v", err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatalf("close stderr capture: %v", err)
	}
	if exitCode != 1 {
		t.Fatalf("execute exit code = %d, want 1", exitCode)
	}
	assertFileContents(t, logPath, "")
	assertFileContents(t, readyPath, "")
	assertFileContents(t, statusPath, "failed\n")
	assertFileContents(t, stdoutPath, "")
	assertFileContents(t, stderrPath, "")
	errorContents, err := os.ReadFile(errorPath)
	if err != nil {
		t.Fatalf("read private error: %v", err)
	}
	if !strings.Contains(string(errorContents), "build Kubernetes client configuration") {
		t.Fatalf("private error = %q", string(errorContents))
	}
	for _, path := range []string{logPath, statusPath, readyPath, errorPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("inspect %s: %v", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want regular 0600", path, info.Mode())
		}
	}
}

func TestParseOptionsReportsErrorsWithoutFlagOutput(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{"--error-file", filepath.Join(t.TempDir(), "error"), "--unknown"})
	if err == nil {
		t.Fatal("parseOptions accepted an unknown flag")
	}
	if opts.errorFile == "" {
		t.Fatal("parseOptions lost the error destination parsed before the invalid flag")
	}
}

func TestParseOptionsRequiresCandidateRender(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	_, err := parseOptions([]string{
		"--kubeconfig", filepath.Join(directory, "kubeconfig"),
		"--namespace", testNamespace,
		"--job-name", testJobName,
		"--log-file", filepath.Join(directory, "capture.log"),
		"--status-file", filepath.Join(directory, "capture.status"),
		"--ready-file", filepath.Join(directory, "capture.ready"),
		"--error-file", filepath.Join(directory, "capture.error"),
	})
	if err == nil || !strings.Contains(err.Error(), "--render-file is required") {
		t.Fatalf("parseOptions error = %v, want required candidate render", err)
	}
}
