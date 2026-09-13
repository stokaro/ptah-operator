package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exemption has to be narrow: every other early exit is still refused, and
// the one that is permitted is permitted for the reasons the block states.
func TestAuditBootstrapHandoff(t *testing.T) {
	t.Parallel()
	sound := []byte(strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		// Both names the hand-off prints are assigned where every path reaches
		// them, which is what keeps set -u from ending the run there.
		"E2E_ENVIRONMENT_FILE=",
		"KUBECONFIG_FILE=/dev/null",
		bootstrapHandoffOpener,
		"\t[ -n \"$E2E_ENVIRONMENT_FILE\" ] || fail \"name a file\"",
		"\t{ printf 'E2E_KUBECONFIG=%s\\n' \"$KUBECONFIG_FILE\"; } >\"$E2E_ENVIRONMENT_FILE\"",
		"\ttrap - EXIT HUP INT TERM",
		"\texit 0",
		"fi",
		"printf 'e2e: complete\\n'",
		"",
	}, "\n"))

	masked, err := auditBootstrapHandoff("harness", sound)
	if err != nil {
		t.Fatalf("auditBootstrapHandoff refused a sound hand-off: %v", err)
	}
	if bytes.Contains(masked, []byte("exit 0")) {
		t.Fatal("the audited block was not masked, so the scan still sees its exit")
	}
	if bytes.Count(masked, []byte("\n")) != bytes.Count(sound, []byte("\n")) {
		t.Fatal("masking changed the line count, so a reported line number would name the wrong line")
	}

	tests := []struct {
		name    string
		mutate  func(string) string
		problem string
	}{
		{
			name:    "the trap is not released",
			mutate:  func(s string) string { return strings.Replace(s, "\ttrap - EXIT HUP INT TERM\n", "", 1) },
			problem: "trap - EXIT HUP INT TERM",
		},
		{
			name:    "nothing is written for the caller",
			mutate:  func(s string) string { return strings.Replace(s, ">\"$E2E_ENVIRONMENT_FILE\"", ">/dev/null", 1) },
			problem: "E2E_ENVIRONMENT_FILE",
		},
		{
			name:    "the block does not end with its exit",
			mutate:  func(s string) string { return strings.Replace(s, "\texit 0\nfi", "\texit 0\n\tls\nfi", 1) },
			problem: "ends with",
		},
		{
			// The Ptah build context is assigned only when the executor is
			// built from source, so a caller who supplies an executor image
			// used to lose the whole lab at this line.
			name: "a name assigned only on one path",
			mutate: func(s string) string {
				return strings.Replace(s, "KUBECONFIG_FILE=/dev/null\n", "", 1)
			},
			problem: "nothing assigns at the top level",
		},
		{
			name:    "a second exit hides inside it",
			mutate:  func(s string) string { return strings.Replace(s, "\ttrap -", "\texit 1\n\ttrap -", 1) },
			problem: "holds 2 exits",
		},
		{
			name:    "the opener is gone",
			mutate:  func(s string) string { return strings.Replace(s, bootstrapHandoffOpener, "if true; then", 1) },
			problem: "audited opener",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := auditBootstrapHandoff("harness", []byte(test.mutate(string(sound)))); err == nil {
				t.Fatalf("auditBootstrapHandoff accepted %s", test.name)
			} else if !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("auditBootstrapHandoff said %q, which does not carry %q", err, test.problem)
			}
		})
	}
}

// The harness itself, which is the only file the exemption is written for.
func TestHarnessBootstrapHandoffIsAudited(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", e2eHarnessPath)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := auditBootstrapHandoff(e2eHarnessPath, contents); err != nil {
		t.Fatalf("the harness hand-off is not audited: %v", err)
	}
}
