package main

import (
	"bytes"
	"context"
	"os/exec"
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
	if result.Stdout != "oci res" {
		t.Fatalf("stdout payload = %q, want configured 7-byte capture", result.Stdout)
	}
	if result.Error == nil || result.Error.Code != "output_truncated" || result.Truncation == nil || !result.Truncation.Stdout {
		t.Fatalf("result = %#v, want output truncation metadata", result)
	}
}
