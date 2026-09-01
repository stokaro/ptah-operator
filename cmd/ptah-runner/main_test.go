package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestRunUsesConfiguredBinaryAndResultLimit(t *testing.T) {
	t.Parallel()

	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo executable is unavailable")
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"--ptah-binary", echo, "--max-result-bytes", "7", "--operation", "resolve"},
		&stdout,
		&stderr,
		[]string{
			"PTAH_OPERATION_ID=resolve-flags",
			"PTAH_REQUESTED_REFERENCE=oci://registry.example/schema:main",
		},
	)
	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	result, err := runner.ParseResultFor(stdout.Bytes(), runner.OperationResolve, "resolve-flags")
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v, stdout = %q", err, stdout.String())
	}
	if result.Stdout != "" {
		t.Fatalf("stdout payload = %q, want no native Resolve output", result.Stdout)
	}
	if result.Error == nil || result.Error.Code != "output_truncated" || result.Truncation == nil || !result.Truncation.Stdout {
		t.Fatalf("result = %#v, want output truncation metadata", result)
	}
}

func TestRunRejectsIncoherentPlanFlags(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"--max-result-bytes", "7", "--max-plan-bytes", "8", "--operation", "plan"},
		&stdout,
		&stderr,
		nil,
	)
	if exitCode != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "must not exceed") {
		t.Fatalf("run() = exit %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunValidatesOCISourceWithoutStartingAChild(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		reference   string
		environment []string
		wantExit    int
	}{
		"matching grant": {
			reference: "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
			environment: []string{
				runner.EnvOCIAuthMode + "=Environment",
				"PTAH_OCI_REGISTRY=registry.example",
				runner.EnvOCIAuthRegistryGrant + "=registry.example",
			},
		},
		"mismatched grant": {
			reference: "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
			environment: []string{
				runner.EnvOCIAuthMode + "=Environment",
				"PTAH_OCI_REGISTRY=registry.example",
				runner.EnvOCIAuthRegistryGrant + "=attacker.example",
			},
			wantExit: 2,
		},
	} {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := run(
				context.Background(),
				[]string{"--validate-oci-source", test.reference},
				&stdout,
				&stderr,
				test.environment,
			)
			if exitCode != test.wantExit || stdout.Len() != 0 {
				t.Fatalf("run() = exit %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunSnapshotsValidatedOCICA(t *testing.T) {
	t.Parallel()

	content := []byte("-----BEGIN CERTIFICATE-----\ncli\n-----END CERTIFICATE-----\n")
	digest := sha256.Sum256(content)
	directory := t.TempDir()
	source := filepath.Join(directory, "source.pem")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnvironment := []string{
		runner.EnvOCIAuthMode + "=Environment",
		"PTAH_OCI_REGISTRY=registry.example",
		runner.EnvOCIAuthRegistryGrant + "=registry.example",
		runner.EnvOCIHasCA + "=true",
		runner.EnvOCICASourceFile + "=" + source,
	}
	for name, grant := range map[string]string{
		"matching":   fmt.Sprintf("sha256:%x", digest),
		"mismatched": "sha256:" + strings.Repeat("0", 64),
	} {
		name, grant := name, grant
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			destination := filepath.Join(directory, name+"-snapshot.pem")
			environment := append(append([]string(nil), baseEnvironment...), runner.EnvOCICASHA256Grant+"="+grant)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := run(
				context.Background(),
				[]string{
					"--validate-oci-source", "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
					"--snapshot-oci-ca-to", destination,
				},
				&stdout,
				&stderr,
				environment,
			)
			if name == "mismatched" {
				if exitCode != 2 || stdout.Len() != 0 {
					t.Fatalf("run() = exit %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("unauthorized CA created snapshot: %v", err)
				}
				return
			}
			if exitCode != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("run() = exit %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
			snapshot, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(snapshot, content) {
				t.Fatalf("snapshot = %q, want exact source bytes", snapshot)
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o400 {
				t.Fatalf("snapshot mode = %#o, want 0400", got)
			}
		})
	}
}

func TestRunRejectsCASnapshotFlagOutsideValidationMode(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"--snapshot-oci-ca-to", filepath.Join(t.TempDir(), "ca.pem"), "resolve"},
		&stdout,
		&stderr,
		nil,
	)
	if exitCode != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires --validate-oci-source") {
		t.Fatalf("run() = exit %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
}
