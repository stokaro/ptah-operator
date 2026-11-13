package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// fetchScript is a Ptah that records its arguments and environment, and exits
// with exitCode.
func fetchScript(t *testing.T, exitCode int) (binary, record string) {
	t.Helper()
	directory := t.TempDir()
	record = filepath.Join(directory, "record")
	binary = filepath.Join(directory, "ptah")
	script := "#!/bin/sh\n{ printf 'args=%s\\n' \"$*\"; env; } > '" + record + "'\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, record
}

// The OCI fetch container runs the runner in front of Ptah's pull: the
// registry credential reaches Ptah from its file, in the environment alone,
// and Ptah's exit status is the container's.
func TestRunFetchesAnOCISourceWithTheCredentialFromItsFile(t *testing.T) {
	t.Parallel()
	binary, record := fetchScript(t, 4)
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("registry-password-from-a-file"), 0o400); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exitCode := run(context.Background(),
		[]string{"--fetch-oci-source", "--ptah-binary", binary, "--", "schema", "pull", "oci://registry.example/acme/orders@sha256:" + strings.Repeat("a", 64)},
		&stdout, &stderr,
		[]string{runnerProtocol, runner.EnvOCIPasswordFile + "=" + password, "PATH=" + os.Getenv("PATH")},
		"",
	)
	if exitCode != 4 {
		t.Fatalf("run() exit code = %d, want the pull's own 4; stderr = %q", exitCode, stderr.String())
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the pull did not run: %v", err)
	}
	if !strings.Contains(string(recorded), "args=schema pull oci://registry.example/acme/orders@sha256:") {
		t.Fatalf("the pull ran with %q", recorded)
	}
	if !strings.Contains(string(recorded), "\n"+runner.EnvOCIPassword+"=registry-password-from-a-file\n") {
		t.Fatalf("the pull's environment carries no password from its file: %q", recorded)
	}
	if strings.Contains(string(recorded), runner.EnvOCIPasswordFile) {
		t.Fatalf("the pull was told where the password file is: %q", recorded)
	}
}

func TestRunRefusesAnOCIFetchItWasNotBuiltFor(t *testing.T) {
	t.Parallel()
	pull := []string{"schema", "pull", "oci://registry.example/acme/orders@sha256:" + strings.Repeat("a", 64)}
	for _, row := range []struct {
		name        string
		arguments   func(binary string) []string
		environment []string
		refusal     string
	}{
		{
			name: "another mode's flag beside it",
			arguments: func(binary string) []string {
				return append([]string{"--fetch-oci-source", "--ptah-binary", binary, "--operation", "resolve", "--"}, pull...)
			},
			environment: []string{runnerProtocol},
			refusal:     "OCI fetch mode takes only",
		},
		{
			name:        "no pull to run",
			arguments:   func(binary string) []string { return []string{"--fetch-oci-source", "--ptah-binary", binary} },
			environment: []string{runnerProtocol},
			refusal:     "OCI fetch mode takes only",
		},
		{
			name: "a Job built for another protocol",
			arguments: func(binary string) []string {
				return append([]string{"--fetch-oci-source", "--ptah-binary", binary, "--"}, pull...)
			},
			environment: []string{runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion+1)},
			refusal:     runner.CodeRunnerProtocolMismatch,
		},
		{
			name: "a command that is not a pull",
			arguments: func(binary string) []string {
				return []string{"--fetch-oci-source", "--ptah-binary", binary, "--", "schema", "apply"}
			},
			environment: []string{runnerProtocol},
			refusal:     runner.CodeOCIFetchRefused,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			binary, record := fetchScript(t, 0)
			var stdout, stderr bytes.Buffer
			exitCode := run(context.Background(), row.arguments(binary), &stdout, &stderr, row.environment, "")
			if exitCode != 2 || !strings.Contains(stderr.String(), row.refusal) {
				t.Fatalf("run() = exit %d, stderr %q, want 2 and a refusal naming %q", exitCode, stderr.String(), row.refusal)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("Ptah ran for a fetch the runner refused (stat error %v)", err)
			}
		})
	}
}
