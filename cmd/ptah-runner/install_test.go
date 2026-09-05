package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyExecutableInstallsAtomicallyWithReadExecutePermissions(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source")
	destinationPath := filepath.Join(directory, "installed")
	content := []byte("runner executable bytes")
	if err := os.WriteFile(sourcePath, content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(sourcePath, destinationPath); err != nil {
		t.Fatalf("copyExecutable() error = %v", err)
	}
	got, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("installed bytes = %q, want %q", got, content)
	}
	info, err := os.Stat(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("installed permissions = %o, want 555", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".ptah-runner-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary install files remain: %v", matches)
	}
}

func TestInstallSelfRejectsOtherDestinations(t *testing.T) {
	t.Parallel()

	if err := installSelf(filepath.Join(t.TempDir(), "ptah-runner")); err == nil {
		t.Fatal("installSelf() accepted a destination outside /runner")
	}
}

func TestValidateInstallDestinationRejectsSymlinks(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	realParent := filepath.Join(directory, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkParent := filepath.Join(directory, "link")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	destination := filepath.Join(symlinkParent, "ptah-runner")
	if err := validateInstallDestination(destination, destination); err == nil {
		t.Fatal("validateInstallDestination() accepted a symlink parent")
	}

	realDestination := filepath.Join(realParent, "ptah-runner")
	if err := os.WriteFile(filepath.Join(realParent, "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realParent, "target"), realDestination); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallDestination(realDestination, realDestination); err == nil {
		t.Fatal("validateInstallDestination() accepted a symlink destination")
	}
}
