package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenPrivateDestinationCreatesAndTruncatesMode0600File(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	path := filepath.Join(t.TempDir(), "private")
	file, err := openPrivateDestination(path)
	if err != nil {
		t.Fatalf("create private destination: %v", err)
	}
	if _, err := file.WriteString("old data"); err != nil {
		t.Fatalf("write private destination: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close private destination: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("inspect private destination: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %#o, want 0600", info.Mode().Perm())
	}

	file, err = openPrivateDestination(path)
	if err != nil {
		t.Fatalf("reopen private destination: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close reopened destination: %v", err)
	}
	assertFileContents(t, path, "")
}

func TestOpenPrivateDestinationRefusesNon0600File(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	path := filepath.Join(t.TempDir(), "public")
	if err := os.WriteFile(path, []byte("must remain"), 0o644); err != nil {
		t.Fatalf("create public file: %v", err)
	}
	if _, err := openPrivateDestination(path); err == nil || !strings.Contains(err.Error(), "mode 0600") {
		t.Fatalf("openPrivateDestination error = %v", err)
	}
	assertFileContents(t, path, "must remain")
}

func TestOpenPrivateDestinationRefusesSymbolicLink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation requires additional Windows privileges")
	}

	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symbolic link: %v", err)
	}
	if _, err := openPrivateDestination(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("openPrivateDestination error = %v", err)
	}
	assertFileContents(t, target, "must remain")
}

func TestPrepareOutputsRefusesHardLinkAliases(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("hard-link behavior differs on Windows")
	}

	directory := t.TempDir()
	logPath := filepath.Join(directory, "log")
	statusPath := filepath.Join(directory, "status")
	if err := os.WriteFile(logPath, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("create log destination: %v", err)
	}
	if err := os.Link(logPath, statusPath); err != nil {
		t.Fatalf("create hard link: %v", err)
	}
	output, err := prepareOutputs(outputPaths{
		log:    logPath,
		status: statusPath,
		ready:  filepath.Join(directory, "ready"),
		error:  filepath.Join(directory, "error"),
	})
	if output != nil {
		_ = output.close()
	}
	if err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("prepareOutputs error = %v", err)
	}
	assertFileContents(t, logPath, "must remain")
	assertFileContents(t, statusPath, "must remain")
}

func TestPrivatePathAtomicWriteRefusesReplacementSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation requires additional Windows privileges")
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "status")
	file, err := openPrivateDestination(path)
	if err != nil {
		t.Fatalf("prepare status: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close status: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove status: %v", err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("replace status with symbolic link: %v", err)
	}
	private := &privatePath{path: path}
	if err := private.writeAtomic([]byte("captured\n")); err == nil {
		t.Fatal("writeAtomic accepted a replacement symbolic link")
	}
	assertFileContents(t, target, "must remain")
}

func TestOutputsRejectReplacedLogPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("removing an open file is not supported on Windows")
	}

	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	if err := os.Remove(output.logPath); err != nil {
		t.Fatalf("remove open log path: %v", err)
	}
	target := filepath.Join(filepath.Dir(output.logPath), "target")
	if err := os.WriteFile(target, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("create replacement target: %v", err)
	}
	if err := os.Symlink(target, output.logPath); err != nil {
		t.Fatalf("replace log path with symbolic link: %v", err)
	}
	if err := output.validateLogDestination(); err == nil {
		t.Fatal("validateLogDestination accepted a replacement symbolic link")
	}
	assertFileContents(t, target, "must remain")
}

func TestOutputsBoundStatusAndErrorContents(t *testing.T) {
	t.Parallel()

	output := newTestOutputs(t)
	defer func() { _ = output.close() }()
	if err := output.setStatus(captureStatus("unbounded arbitrary text")); err == nil {
		t.Fatal("setStatus accepted a value outside the status enum")
	}
	if err := output.writeError(errors.New(strings.Repeat("x", maxErrorBytes+100))); err != nil {
		t.Fatalf("writeError: %v", err)
	}
	contents, err := os.ReadFile(output.error.path)
	if err != nil {
		t.Fatalf("read error output: %v", err)
	}
	if len(contents) != maxErrorBytes+1 || contents[len(contents)-1] != '\n' {
		t.Fatalf("bounded error output length = %d", len(contents))
	}
}

func TestPrepareOutputsReportsPartialInitializationFailurePrivately(t *testing.T) {
	t.Parallel()
	requirePrivateModeSemantics(t)

	directory := t.TempDir()
	errorPath := filepath.Join(directory, "error")
	statusPath := filepath.Join(directory, "status")
	if err := os.WriteFile(errorPath, nil, 0o600); err != nil {
		t.Fatalf("create error destination: %v", err)
	}
	if err := os.WriteFile(statusPath, nil, 0o644); err != nil {
		t.Fatalf("create invalid status destination: %v", err)
	}
	output, err := prepareOutputs(outputPaths{
		log:    filepath.Join(directory, "log"),
		status: statusPath,
		ready:  filepath.Join(directory, "ready"),
		error:  errorPath,
	})
	if err == nil || output == nil {
		t.Fatalf("prepareOutputs = (%v, %v), want partial output and error", output, err)
	}
	output.reportFailure(err)
	if closeErr := output.close(); closeErr != nil {
		t.Fatalf("close partial output: %v", closeErr)
	}
	contents, readErr := os.ReadFile(errorPath)
	if readErr != nil {
		t.Fatalf("read private error: %v", readErr)
	}
	if !strings.Contains(string(contents), "prepare status destination") {
		t.Fatalf("private error = %q", string(contents))
	}
}

func requirePrivateModeSemantics(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper intentionally requires Unix mode 0600 semantics")
	}
}
