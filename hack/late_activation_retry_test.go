package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSameCandidateRetryPreservesFailureAndCaptureOrder(t *testing.T) {
	t.Parallel()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	function := extractE2EShellFunction(t, source, "retry_same_candidate_with_diagnostics")
	for _, test := range []struct {
		name                                  string
		helmExit, captureExit, diagnosticExit int
		wantExit                              int
	}{
		{name: "successful retry"},
		{name: "Helm failure", helmExit: 73, wantExit: 73},
		{name: "both fail", helmExit: 73, captureExit: 19, wantExit: 73},
		{name: "diagnostic refusal preserves Helm failure", helmExit: 73, diagnosticExit: 29, wantExit: 73},
		{name: "missing capture cannot pass", captureExit: 19, wantExit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			script := `set -eu
WORK_DIR=$1
helm_exit=$2
capture_exit=$3
diagnostic_exit=$4
E2E_HELM_RELEASE=exact-release
E2E_OPERATOR_NAMESPACE=exact-namespace
E2E_NEXT_CHART_PACKAGE=exact-chart.tgz
E2E_NEXT_VALUES_FILE=exact-values.yaml
fail() { printf '%s\n' "$*" >&2; exit 1; }
arm_late_activation_hook_log_captures() {
  printf 'arm\n' >>"$WORK_DIR/order"
  printf '%s\n' "$LATE_ACTIVATION_PREFLIGHT_LOG_FILE" "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_STATUS_FILE" \
    "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE" "$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE" \
    "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_READY_FILE" "$LATE_ACTIVATION_RECONCILE_LOG_FILE" \
    "$LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE" "$LATE_ACTIVATION_RECONCILE_CAPTURE_ERRORS_FILE" \
    "$LATE_ACTIVATION_RECONCILE_FAILURE_CLASS_FILE" "$LATE_ACTIVATION_RECONCILE_CAPTURE_READY_FILE" >"$WORK_DIR/destinations"
}
helm_e2e() {
  printf 'helm\n' >>"$WORK_DIR/order"
  printf '%s\n' "$@" >"$WORK_DIR/arguments"
  printf 'PRIVATE_HELM_OUTPUT\n'
  printf 'PRIVATE_HELM_ERROR\n' >&2
  return "$helm_exit"
}
finish_late_activation_hook_log_captures() {
  printf 'finish\n' >>"$WORK_DIR/order"
  return "$capture_exit"
}
emit_late_activation_preflight_diagnostic_if_available() {
  printf 'preflight-diagnostic\n' >>"$WORK_DIR/order"
  exit "$diagnostic_exit"
}
emit_same_candidate_retry_reconcile_diagnostic_if_available() {
  printf 'reconcile-diagnostic\n' >>"$WORK_DIR/order"
  exit "$diagnostic_exit"
}
late_activation_capture_status_summary() { printf 'captured\n'; }
verify_late_activation_preflight_capture() { printf 'verify\n' >>"$WORK_DIR/order"; }
` + function + "\nretry_same_candidate_with_diagnostics\n"
			command := exec.Command("sh", "-c", script, "retry-test", directory,
				fmt.Sprint(test.helmExit), fmt.Sprint(test.captureExit), fmt.Sprint(test.diagnosticExit))
			output, err := command.CombinedOutput()
			exitCode := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatal(err)
				}
				exitCode = exitError.ExitCode()
			}
			if exitCode != test.wantExit {
				t.Fatalf("exit = %d, want %d: %s", exitCode, test.wantExit, output)
			}
			if strings.Contains(string(output), "PRIVATE_HELM_") {
				t.Fatalf("raw Helm output escaped private files: %s", output)
			}
			wantOrder := "arm\nhelm\nfinish\n"
			if test.helmExit != 0 {
				wantOrder += "preflight-diagnostic\nreconcile-diagnostic\n"
			} else if test.captureExit == 0 {
				wantOrder += "verify\n"
			}
			if got := readRetryTestFile(t, directory, "order"); got != wantOrder {
				t.Fatalf("order = %q, want %q", got, wantOrder)
			}
			if got, want := readRetryTestFile(t, directory, "arguments"), "upgrade\nexact-release\nexact-chart.tgz\n--namespace\nexact-namespace\n--values\nexact-values.yaml\n--force-conflicts\n--wait\n--timeout\n7m\n"; got != want {
				t.Fatalf("Helm arguments = %q, want %q", got, want)
			}
			destinations := strings.Fields(readRetryTestFile(t, directory, "destinations"))
			seen := make(map[string]bool)
			for _, destination := range destinations {
				if !strings.HasPrefix(destination, filepath.Join(directory, "retry-")) || seen[destination] {
					t.Fatalf("capture destination is not private, fresh, and unique: %q", destination)
				}
				seen[destination] = true
			}
			if len(seen) != 10 {
				t.Fatalf("capture destinations = %d, want 10", len(seen))
			}
		})
	}
}

func readRetryTestFile(t *testing.T, directory, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestSameCandidateRetryDiagnosticWithholdsUnsafeContent(t *testing.T) {
	t.Parallel()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	var functions strings.Builder
	for _, name := range []string{"fail", "require_mode_0600_regular_file", "late_activation_capture_status_summary", "hook_diagnostic_is_safe", "emit_same_candidate_retry_reconcile_diagnostic_if_available"} {
		functions.WriteString(extractE2EShellFunction(t, source, name))
		functions.WriteByte('\n')
	}
	for _, test := range []struct {
		name, diagnostic, status string
		wantEmission             bool
	}{
		{name: "safe exact failure", diagnostic: "ptah-crd-manager: exact retry failure\n", status: "captured", wantEmission: true},
		{name: "credential", diagnostic: "ptah-crd-manager: DO_NOT_EMIT_CREDENTIAL\n", status: "captured"},
		{name: "multiple lines", diagnostic: "ptah-crd-manager: line one\nline two\n", status: "captured"},
		{name: "bearer token", diagnostic: "ptah-crd-manager: Bearer protected-value\n", status: "captured"},
		{name: "untrusted format", diagnostic: "arbitrary log contents\n", status: "captured"},
		{name: "incomplete capture", diagnostic: "ptah-crd-manager: partial failure\n", status: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			for name, contents := range map[string]string{"capture.log": test.diagnostic, "status": test.status + "\n", "patterns": "DO_NOT_EMIT_CREDENTIAL\n"} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			script := "set -eu\nLATE_ACTIVATION_RECONCILE_LOG_FILE=$1/capture.log\nLATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE=$1/status\nIDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE=$1/patterns\n" + functions.String() + "\nemit_same_candidate_retry_reconcile_diagnostic_if_available\n"
			output, err := exec.Command("sh", "-c", script, "diagnostic-test", directory).CombinedOutput()
			if err != nil {
				t.Fatalf("diagnostic failed: %v: %s", err, output)
			}
			if got := strings.Contains(string(output), test.diagnostic); got != test.wantEmission {
				t.Fatalf("raw diagnostic emitted = %t, want %t: %q", got, test.wantEmission, output)
			}
		})
	}
}

func TestSameCandidateRetryStaticContractRejectsBypasses(t *testing.T) {
	t.Parallel()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	if err := verifySameCandidateRetryDiagnostics("fixture", []byte(source)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, old, replacement string }{
		{"early success", "retry_same_candidate_with_diagnostics() {", "retry_same_candidate_with_diagnostics() {\n\treturn 0"},
		{"capture omitted", "\tarm_late_activation_hook_log_captures\n\tretry_helm_status=0", "\tretry_helm_status=0"},
		{"Helm failure hidden", "return \"$retry_helm_status\"", "return 0"},
		{"capture not joined", "finish_late_activation_hook_log_captures || retry_capture_status=$?", ":"},
		{"original evidence overwritten", "LATE_ACTIVATION_RECONCILE_LOG_FILE=$WORK_DIR/retry-reconcile.log", "LATE_ACTIVATION_RECONCILE_LOG_FILE=$WORK_DIR/late-activation-reconcile.log"},
		{"raw diagnostic bypass", "if hook_diagnostic_is_safe \"$LATE_ACTIVATION_RECONCILE_LOG_FILE\"; then", "if true; then"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if strings.Count(source, test.old) != 1 {
				t.Fatalf("mutation %q is not unique", test.old)
			}
			mutated := strings.Replace(source, test.old, test.replacement, 1)
			if err := verifySameCandidateRetryDiagnostics("fixture", []byte(mutated)); err == nil {
				t.Fatal("contract accepted retry bypass")
			}
		})
	}
}
