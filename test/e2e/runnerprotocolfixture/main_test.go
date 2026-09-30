package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestTheOverlayChangesOnlyTheShippingProtocolLiteral(t *testing.T) {
	t.Parallel()
	const source = "package runner\n// ProtocolVersion = 1 stays in this comment.\nconst (\n ProtocolVersion = 1\n Other = 1\n)\nvar Label = `ProtocolVersion = 1`\n"
	updated, err := incompatibleProtocolSource([]byte(source), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != strings.Replace(source, "\n ProtocolVersion = 1", "\n ProtocolVersion = 2", 1) {
		t.Fatal("the overlay changed more than the one protocol literal")
	}
	for _, invalid := range []string{
		"package other\nconst ProtocolVersion = 1\n",
		"package runner\nvar ProtocolVersion = 1\n",
		"package runner\nconst Other = 1\n",
		"package runner\nconst ProtocolVersion = 2\n",
		"package runner\nconst ProtocolVersion = 1 + 0\n",
		"package runner\nconst ProtocolVersion = 0x1\n",
		"package runner\nconst ProtocolVersion, Other = 1, 1\n",
		"package runner\nconst ProtocolVersion = 1\nconst ProtocolVersion = 1\n",
		"invalid Go source",
	} {
		if _, err := incompatibleProtocolSource([]byte(invalid), 1); err == nil {
			t.Fatalf("an ambiguous or unrelated shipping declaration passed: %q", invalid)
		}
	}
}

func TestTheOverlayKeepsShippingSourceAndRecordsItsExactBytes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "protocol.go")
	source := []byte("package runner\nconst ProtocolVersion = 1\n")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "fixture")
	if err := writeOverlay(sourcePath, output, 1); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(source, retained) {
		t.Fatal("the build fixture overwrote shipping source")
	}
	var overlay struct{ Replace map[string]string }
	raw, err := os.ReadFile(filepath.Join(output, "overlay.json"))
	if err != nil || json.Unmarshal(raw, &overlay) != nil || len(overlay.Replace) != 1 || overlay.Replace[sourcePath] != filepath.Join(output, "protocol.go") {
		t.Fatal("the build overlay escaped the one source declaration")
	}
	updated, err := os.ReadFile(overlay.Replace[sourcePath])
	if err != nil {
		t.Fatal(err)
	}
	var record fixtureRecord
	raw, err = os.ReadFile(filepath.Join(output, "provenance.json"))
	if err != nil || json.Unmarshal(raw, &record) != nil || record.SupportedProtocol != 1 || record.RefusedProtocol != 2 ||
		record.SourceSHA256 != sourceSHA256(source) || record.OverlaySHA256 != sourceSHA256(updated) || record.Scope != fixtureScope {
		t.Fatal("fixture provenance did not bind the exact source and copied bytes")
	}
	if writeOverlay(sourcePath, directory, 1) == nil {
		t.Fatal("an overlay was allowed to overwrite its shipping input")
	}
}

// Build the actual CLI with this overlay. The native subprocess and complete
// result prove more than a hand-written foreign frame. The matching-protocol
// control must start its child; merely avoiding the mismatch code is not enough.
func TestTheCompiledFixtureRefusesEveryOperationBeforeStartingAChild(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	sourcePath := filepath.Join(root, "internal/runner/protocol.go")
	if err := writeOverlay(sourcePath, directory, runner.ProtocolVersion); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary := filepath.Join(directory, "runner")
	build := exec.CommandContext(ctx, "go", "build", "-overlay", filepath.Join(directory, "overlay.json"), "-o", binary, "./cmd/ptah-runner")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the native incompatible CLI: %v\n%s", err, output)
	}
	child, marker := filepath.Join(directory, "child"), filepath.Join(directory, "child-started")
	if err := os.WriteFile(child, []byte("#!/bin/sh\nprintf started > \"$RUNNER_FIXTURE_CHILD_MARKER\"\nexit 19\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []runner.Operation{runner.OperationResolve, runner.OperationVerify, runner.OperationObserve, runner.OperationPlan,
		runner.OperationApply, runner.OperationMigrationHistory, runner.OperationMigrationApply} {
		id := "native-protocol-" + string(operation)
		command := exec.CommandContext(ctx, binary, "--operation", string(operation), "--ptah-binary", child)
		command.Env = []string{runner.EnvOperationID + "=" + id, runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion),
			"RUNNER_FIXTURE_CHILD_MARKER=" + marker}
		logs, err := command.Output()
		if err != nil {
			t.Fatalf("the incompatible runner did not complete its refusal transport: %v", err)
		}
		result, err := runner.ParseResultFor(logs, operation, id)
		var mismatch *runner.ProtocolMismatchError
		if !errors.As(err, &mismatch) || mismatch.RunnerVersion != runner.ProtocolVersion+1 || !reflect.DeepEqual(result, runner.Result{}) ||
			mismatch.Message != fmt.Sprintf("the Job expects runner protocol %d; this runner speaks protocol %d", runner.ProtocolVersion, runner.ProtocolVersion+1) {
			t.Fatalf("the actual fixture did not return the exact bound foreign refusal for %s: %#v, %v", operation, result, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("the incompatible protocol started a child")
		}
	}
	// Migration Jobs run the copied runner's authority guard before fetching
	// files or starting Ptah. That mode has no result frame: its exit status
	// and exact diagnostic must carry the refusal on their own.
	guard := exec.CommandContext(ctx, binary, "--validate-oci-source", "oci://registry.example/migrations@sha256:"+strings.Repeat("a", 64))
	guard.Env = []string{runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion), "RUNNER_FIXTURE_CHILD_MARKER=" + marker}
	var diagnostic bytes.Buffer
	guard.Stderr = &diagnostic
	guardOutput, err := guard.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || len(guardOutput) != 0 || diagnostic.String() !=
		fmt.Sprintf("ptah-runner: runner_protocol_mismatch: the Job expects runner protocol %d; this runner speaks protocol %d\n", runner.ProtocolVersion, runner.ProtocolVersion+1) {
		t.Fatalf("the actual OCI guard lost its exact refusal: exit=%v stdout=%q stderr=%q", err, guardOutput, diagnostic.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the incompatible OCI guard started a child")
	}
	command := exec.CommandContext(ctx, binary, "--operation", "resolve", "--ptah-binary", child)
	command.Env = []string{runner.EnvOperationID + "=native-supported-control",
		runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion+1),
		runner.EnvRequestedReference + "=oci://registry.example/schema:main", "RUNNER_FIXTURE_CHILD_MARKER=" + marker}
	logs, err := command.Output()
	if err != nil {
		t.Fatalf("the matching-protocol control did not complete: %v", err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "started" {
		t.Fatalf("the matching protocol did not start its native child: %v\n%s", err, logs)
	}
	// Read the refusal shape from each operation with the production parser
	// above. A successful child uses the synthetic protocol and is deliberately
	// outside this manager's supported result contract.
	t.Logf("native fixture protocol %d refuses protocol %d for all seven operations; matching protocol starts child", runner.ProtocolVersion+1, runner.ProtocolVersion)
}

func TestFixtureArgumentsRequireAnExplicitOutputDirectory(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{nil, {"extra"}, {"--directory", t.TempDir(), "extra"}} {
		if run(arguments) == nil {
			t.Fatalf("invalid fixture arguments passed: %v", arguments)
		}
	}
}
