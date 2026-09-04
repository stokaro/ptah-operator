// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
)

func TestVerifyE2EWiring(t *testing.T) {
	t.Parallel()

	if err := verifyE2EWiring(repositoryE2EWiringFiles()); err != nil {
		t.Fatalf("verifyE2EWiring() error = %v", err)
	}
}

func TestLateActivationBlockerMatchesOnlySequenceChanges(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	expressionPattern := regexp.MustCompile(`(?m)^[\t ]*- name: active-release-sequence-change\r?\n[\t ]*expression: '([^'\r\n]+)'[\t ]*\r?$`)
	matches := expressionPattern.FindAllStringSubmatch(source, -1)
	if len(matches) != 1 {
		t.Fatalf("late activation blocker expression matches = %d, want 1", len(matches))
	}

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(matches[0][1])
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile late activation blocker: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build late activation blocker: %v", err)
	}

	activation := func(sequence string) map[string]any {
		return map[string]any{"data": map[string]any{"active-release-sequence": sequence}}
	}
	malformedProbe := activation("0")
	malformedProbe["data"].(map[string]any)["unexpected"] = "must-be-denied"
	for _, test := range []struct {
		name      string
		oldObject any
		object    any
		want      bool
	}{
		{
			name:      "same-sequence malformed guard probe skips blocker",
			oldObject: activation("0"),
			object:    malformedProbe,
			want:      false,
		},
		{
			name:      "candidate activation transition matches blocker",
			oldObject: activation("0"),
			object:    activation("1"),
			want:      true,
		},
		{
			name:      "unchanged activation skips blocker",
			oldObject: activation("1"),
			object:    activation("1"),
			want:      false,
		},
		{
			name:      "create skips blocker",
			oldObject: nil,
			object:    activation("1"),
			want:      false,
		},
		{
			name:      "missing old sequence skips blocker",
			oldObject: map[string]any{"data": map[string]any{}},
			object:    activation("1"),
			want:      false,
		},
		{
			name:      "missing new sequence skips blocker",
			oldObject: activation("0"),
			object:    map[string]any{"data": map[string]any{}},
			want:      false,
		},
		{
			name:      "missing old data skips blocker",
			oldObject: map[string]any{},
			object:    activation("1"),
			want:      false,
		},
		{
			name:      "missing new data skips blocker",
			oldObject: activation("0"),
			object:    map[string]any{},
			want:      false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, _, evalErr := program.Eval(map[string]any{
				"oldObject": test.oldObject,
				"object":    test.object,
			})
			if evalErr != nil {
				t.Fatalf("evaluate late activation blocker: %v", evalErr)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("late activation blocker result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("late activation blocker = %t, want %t", got, test.want)
			}
		})
	}
}

func TestLateActivationRevisionClassification(t *testing.T) {
	t.Parallel()

	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to exercise the embedded late-activation revision classifier")
	}
	filter := lateActivationRevisionFilter(t)
	const (
		expectedRevision      = 4
		expectedPreflightName = "ptah-operator-crd-manager-preflight"
		expectedReconcileName = "ptah-operator-crd-manager"
	)

	hook := func(name string, weight any, phase string) map[string]any {
		return map[string]any{
			"name":   name,
			"kind":   "Job",
			"weight": weight,
			"events": []any{"pre-upgrade"},
			"last_run": map[string]any{
				"phase":        phase,
				"started_at":   "2026-09-04T12:00:00Z",
				"completed_at": "2026-09-04T12:00:01Z",
			},
		}
	}
	fixture := func() map[string]any {
		return map[string]any{
			"version": expectedRevision,
			"info":    map[string]any{"status": "failed"},
			"hooks": []any{
				hook("ptah-operator-identity", -105, "Succeeded"),
				hook(expectedPreflightName, -60, "Succeeded"),
				hook(expectedReconcileName, 0, "Failed"),
			},
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "exact boundary", mutate: func(map[string]any) {}, want: true},
		{
			name: "preflight failed",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[1].(map[string]any)["last_run"].(map[string]any)["phase"] = "Failed"
			},
		},
		{
			name: "unknown failed hook",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[2].(map[string]any)["last_run"].(map[string]any)["phase"] = "Pending"
				status["hooks"] = append(status["hooks"].([]any), hook("unknown-hook", 0, "Failed"))
			},
		},
		{
			name: "multiple failed hooks",
			mutate: func(status map[string]any) {
				status["hooks"] = append(status["hooks"].([]any), hook("other-hook", 10, "Failed"))
			},
		},
		{
			name: "wrong reconcile identity",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[2].(map[string]any)["name"] = "other-reconcile"
			},
		},
		{
			name: "string reconcile weight",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[2].(map[string]any)["weight"] = "0"
			},
		},
		{
			name: "omitted zero reconcile weight",
			mutate: func(status map[string]any) {
				delete(status["hooks"].([]any)[2].(map[string]any), "weight")
			},
			want: true,
		},
		{
			name: "duplicate preflight",
			mutate: func(status map[string]any) {
				status["hooks"] = append(status["hooks"].([]any), hook(expectedPreflightName, -60, "Succeeded"))
			},
		},
		{
			name: "preflight has no completion time",
			mutate: func(status map[string]any) {
				delete(status["hooks"].([]any)[1].(map[string]any)["last_run"].(map[string]any), "completed_at")
			},
		},
		{
			name: "wrong revision",
			mutate: func(status map[string]any) {
				status["version"] = expectedRevision + 1
			},
		},
		{
			name: "release is not failed",
			mutate: func(status map[string]any) {
				status["info"].(map[string]any)["status"] = "deployed"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status := fixture()
			test.mutate(status)
			encoded, marshalErr := json.Marshal(status)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			command := exec.Command(
				jqPath,
				"-e",
				"--argjson", "expected_revision", "4",
				"--arg", "expected_preflight_name", expectedPreflightName,
				"--arg", "expected_reconcile_name", expectedReconcileName,
				filter,
			)
			command.Stdin = strings.NewReader(string(encoded))
			output, runErr := command.CombinedOutput()
			if got := runErr == nil; got != test.want {
				t.Fatalf("late activation revision classification = %t, want %t; jq output = %q", got, test.want, output)
			}
		})
	}
}

func lateActivationRevisionFilter(t *testing.T) string {
	t.Helper()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	commandMarker := `if jq -e --argjson expected_revision "$late_revision" \`
	commandOffset := strings.Index(source, commandMarker)
	if commandOffset < 0 {
		t.Fatal("late activation revision classifier command is missing")
	}
	filterMarker := `--arg expected_reconcile_name "$EXPECTED_RECONCILE_HOOK_NAME" '` + "\n"
	filterOffset := strings.Index(source[commandOffset:], filterMarker)
	if filterOffset < 0 {
		t.Fatal("late activation revision classifier filter start is missing")
	}
	filterOffset += commandOffset + len(filterMarker)
	endMarker := "\n        ' \"$late_status_file\" >/dev/null 2>&1; then"
	endOffset := strings.Index(source[filterOffset:], endMarker)
	if endOffset < 0 {
		t.Fatal("late activation revision classifier filter end is missing")
	}
	return source[filterOffset : filterOffset+endOffset]
}

func TestLateActivationFailureSummaryIsBoundedAndSynthesized(t *testing.T) {
	t.Parallel()

	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to exercise the embedded late-activation summary")
	}
	filter := lateActivationSummaryFilter(t)
	const rawEvidenceMarker = "RAW_PRIVATE_EVIDENCE_MUST_NOT_ESCAPE"
	status := map[string]any{
		"version": 4,
		"info": map[string]any{
			"status":      "failed",
			"description": rawEvidenceMarker,
		},
		"hooks": []any{
			map[string]any{
				"name":   "ptah-operator-crd-manager-preflight",
				"kind":   "Job",
				"weight": -60,
				"last_run": map[string]any{
					"phase": "Failed",
				},
			},
			map[string]any{
				"name": rawEvidenceMarker,
				"kind": "Job",
				"last_run": map[string]any{
					"phase": "Failed",
				},
			},
		},
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		jqPath,
		"-c",
		"--argjson", "expected_revision", "4",
		"--arg", "expected_preflight_name", "ptah-operator-crd-manager-preflight",
		"--arg", "expected_reconcile_name", "ptah-operator-crd-manager",
		"--arg", "preflight_capture", "captured",
		"--arg", "reconcile_capture", "canceled",
		"--arg", "preflight_exit", "0",
		"--arg", "reconcile_exit", "1",
		filter,
	)
	command.Stdin = strings.NewReader(string(encoded))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("execute late activation summary: %v", err)
	}
	if len(output) > 1024 {
		t.Fatalf("late activation summary length = %d, want at most 1024", len(output))
	}
	if strings.Count(string(output), "\n") != 1 {
		t.Fatalf("late activation summary is not exactly one line: %q", output)
	}
	if strings.Contains(string(output), rawEvidenceMarker) {
		t.Fatalf("late activation summary leaked raw revision evidence: %q", output)
	}
	var summary map[string]any
	if err := json.Unmarshal(output, &summary); err != nil {
		t.Fatalf("decode late activation summary: %v", err)
	}
	if got := summary["reconcileTarget"]; got != "not-reached" {
		t.Fatalf("reconcileTarget = %v, want not-reached", got)
	}
	status["hooks"] = append(status["hooks"].([]any), map[string]any{
		"name":   "ptah-operator-crd-manager",
		"kind":   "Job",
		"weight": 0,
		"last_run": map[string]any{
			"phase":      "Failed",
			"started_at": "2026-09-04T12:00:02Z",
		},
	})
	encoded, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	command = exec.Command(
		jqPath,
		"-c",
		"--argjson", "expected_revision", "4",
		"--arg", "expected_preflight_name", "ptah-operator-crd-manager-preflight",
		"--arg", "expected_reconcile_name", "ptah-operator-crd-manager",
		"--arg", "preflight_capture", "captured",
		"--arg", "reconcile_capture", "canceled",
		"--arg", "preflight_exit", "0",
		"--arg", "reconcile_exit", "1",
		filter,
	)
	command.Stdin = strings.NewReader(string(encoded))
	output, err = command.Output()
	if err != nil {
		t.Fatalf("execute late activation summary with reached reconcile: %v", err)
	}
	if err := json.Unmarshal(output, &summary); err != nil {
		t.Fatalf("decode late activation summary with reached reconcile: %v", err)
	}
	if got := summary["reconcileTarget"]; got != "reached" {
		t.Fatalf("reconcileTarget = %v, want reached despite canceled capture", got)
	}
}

func lateActivationSummaryFilter(t *testing.T) string {
	t.Helper()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	commandMarker := `if activation_summary=$(jq -c \`
	commandOffset := strings.Index(source, commandMarker)
	if commandOffset < 0 {
		t.Fatal("late activation summary command is missing")
	}
	filterMarker := `--arg reconcile_exit "$reconcile_capture_exit" '` + "\n"
	filterOffset := strings.Index(source[commandOffset:], filterMarker)
	if filterOffset < 0 {
		t.Fatal("late activation summary filter start is missing")
	}
	filterOffset += commandOffset + len(filterMarker)
	endMarker := "\n        ' \"$status_file\" 2>/dev/null); then"
	endOffset := strings.Index(source[filterOffset:], endMarker)
	if endOffset < 0 {
		t.Fatal("late activation summary filter end is missing")
	}
	return source[filterOffset : filterOffset+endOffset]
}

func TestVerifyMakeE2ETargetRejectsMutations(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, filepath.Join("..", makefilePath))
	tests := []struct {
		name        string
		old         string
		replacement string
	}{
		{
			name:        "Make shell is an unconditional success",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/true\n",
		},
		{
			name:        "Make ignores lifecycle failures",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/sh\n.IGNORE: e2e\n",
		},
		{
			name:        "Make shell flags bypass recipes",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/sh\n.SHELLFLAGS := -c 'true'\n",
		},
		{
			name:        "Make flags enable dry-run mode",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/sh\nMAKEFLAGS += --just-print\n",
		},
		{
			name:        "Make flags export dry-run mode",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/sh\nexport MAKEFLAGS := -n\n",
		},
		{
			name:        "Makefiles injects alternate rules",
			old:         "SHELL := /bin/sh\n",
			replacement: "SHELL := /bin/sh\nMAKEFILES := injected.mk\n",
		},
		{
			name:        "target is not phony",
			old:         " e2e-static e2e\n",
			replacement: " e2e-static\n",
		},
		{
			name:        "target is an unconditional success",
			old:         "\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n",
			replacement: "\t@true\n",
		},
		{
			name:        "target has leading whitespace",
			old:         "e2e:\n",
			replacement: " e2e:\n",
		},
		{
			name:        "target uses an overriding double-colon rule",
			old:         "e2e:\n",
			replacement: "e2e::\n",
		},
		{
			name: "target is overridden later",
			old:  "e2e:\n\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n",
			replacement: "e2e:\n\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n" +
				"\ne2e:\n\t@true\n",
		},
		{
			name: "target is hidden in a false Make branch",
			old:  "e2e:\n\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n",
			replacement: "ifeq (1,0)\n" +
				"e2e:\n\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\nendif\n",
		},
		{
			name:        "target invokes a different harness",
			old:         "\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n",
			replacement: "\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-static.sh\n",
		},
		{
			name:        "target ignores harness failure",
			old:         "\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh\n",
			replacement: "\tDOCKER_CONTEXT=\"$(DOCKER_CONTEXT)\" ./hack/e2e-kind.sh || true\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedE2ESource(t, "Makefile", source, test.old, test.replacement)
			if err := verifyMakeE2ETarget(path); err == nil {
				t.Fatal("verifyMakeE2ETarget() accepted a critical mutation")
			}
		})
	}
}

func TestVerifyE2EHarnessRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	harness := files.harness
	source := readE2ESource(t, harness)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "interpreter bypass",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "failure status trap bypass",
			old:         "trap cleanup EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "required Kubernetes version omitted",
			old:         `[ -n "$K8S_VERSION" ] || fail "K8S_VERSION is required (for example, 1.37.0)"`,
			replacement: `: # K8S_VERSION presence check omitted`,
			wantError:   "required Kubernetes version",
		},
		{
			name:        "exact Kubernetes version syntax omitted",
			old:         `printf '%s\n' "$K8S_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||`,
			replacement: `printf '%s\n' "$K8S_VERSION" | grep -Eq '.*' ||`,
			wantError:   "exact Kubernetes version syntax",
		},
		{
			name:        "supported Kubernetes minor binding omitted",
			old:         `K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`,
			replacement: `K8S_MAJOR_MINOR=1.37`,
			wantError:   "supported Kubernetes minor binding",
		},
		{
			name:        "support manifest image lookup omitted",
			old:         `if [ -z "$KIND_NODE_IMAGE" ]; then`,
			replacement: `if false; then`,
			wantError:   "support-manifest image selection",
		},
		{
			name:        "digest pin omitted",
			old:         `is_pinned_image "$KIND_NODE_IMAGE" ||`,
			replacement: `true ||`,
			wantError:   "digest-pinned node image",
		},
		{
			name:        "node image version binding omitted",
			old:         `kindest/node:v"$K8S_VERSION"@sha256:*) ;;`,
			replacement: `kindest/node:*@sha256:*) ;;`,
			wantError:   "node image version binding",
		},
		{
			name:        "kind version binding omitted",
			old:         `[ "$ACTUAL_KIND_VERSION" = "$EXPECTED_KIND_VERSION" ] ||`,
			replacement: `[ "$EXPECTED_KIND_VERSION" = "$EXPECTED_KIND_VERSION" ] ||`,
			wantError:   "kind version binding",
		},
		{
			name:        "runtime generated-name boundary fixture shortened",
			old:         `RUNTIME_FULLNAME=$(dns_name ptah-runtime-generated-name-prefix-boundary-proof "$identity" 60)`,
			replacement: `RUNTIME_FULLNAME=$(dns_name ptah-runtime "$identity" 35)`,
			wantError:   "runtime generated-name boundary fixture",
		},
		{
			name:        "runtime fullname omitted from release values",
			old:         `fullnameOverride: $fullnameOverride,`,
			replacement: `# fullname boundary omitted`,
			wantError:   "runtime fullname release-values binding",
		},
		{
			name:        "guarded API feature gate omitted",
			old:         `			printf '%s\n' '        value: EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true'`,
			replacement: `			printf '%s\n' '        value: EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true'`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "global kind feature gates restored",
			old:         `append_api_server_feature_gate_patch "$K8S_MAJOR_MINOR" "$KIND_CONFIG"`,
			replacement: "printf '%s\\n' 'featureGates:' >>\"$KIND_CONFIG\"\nappend_api_server_feature_gate_patch \"$K8S_MAJOR_MINOR\" \"$KIND_CONFIG\"",
			wantError:   "global kind featureGates",
		},
		{
			name:        "Kubernetes 1.35 kubeadm patch version changed",
			old:         `			printf '%s\n' '  version: v1beta3'`,
			replacement: `			printf '%s\n' '  version: v1beta4'`,
			wantError:   "API-server feature gate contract",
		},
		{
			name: "Kubernetes 1.37 patch targets the controller manager",
			old: "\t\t\tprintf '%s\\n' '  version: v1beta4'\n" +
				"\t\t\tprintf '%s\\n' '  kind: ClusterConfiguration'",
			replacement: "\t\t\tprintf '%s\\n' '  version: v1beta4'\n" +
				"\t\t\tprintf '%s\\n' '  kind: KubeletConfiguration'",
			wantError: "API-server feature gate contract",
		},
		{
			name:        "Kubernetes 1.35 patch replaces all API server arguments",
			old:         `			printf '%s\n' '      path: /apiServer/extraArgs/feature-gates'`,
			replacement: `			printf '%s\n' '      path: /apiServer/extraArgs'`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "Kubernetes 1.37 patch replaces the runtime config entry",
			old:         `			printf '%s\n' '      path: /apiServer/extraArgs/-'`,
			replacement: `			printf '%s\n' '      path: /apiServer/extraArgs/0'`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "Kubernetes 1.36 gains a feature gate patch",
			old:         `	1.36) ;;`,
			replacement: `	1.36) EXPECTED_API_SERVER_FEATURE_GATES=GenericWorkload=true ;;`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "runtime-config preservation assertion omitted",
			old:         `      ($api_server | map(select(startswith("--runtime-config="))) | length) == 1 and`,
			replacement: `      true and`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "kubelet and kube-proxy scope assertion omitted",
			old:         `		-n kube-system get configmaps kubelet-config kube-proxy -o json >"$component_configs_file"`,
			replacement: `		-n kube-system get configmaps kubelet-config -o json >"$component_configs_file"`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "user namespace kubelet assertion omitted",
			old:         `      (($kubelet_configs[0].data.kubelet // "") | contains("KubeletInUserNamespace: true")) and`,
			replacement: `      true and`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "kind cluster creation omitted",
			old:         "kind create cluster \\\n",
			replacement: "true # kind cluster creation omitted\n",
			wantError:   "kind cluster creation",
		},
		{
			name:        "kind cluster image binding omitted",
			old:         "\t--image \"$KIND_NODE_IMAGE\" \\\n",
			replacement: "\t--image kindest/node:latest \\\n",
			wantError:   "kind cluster creation",
		},
		{
			name:        "zero-node readiness guard bypassed",
			old:         `.items | length > 0`,
			replacement: `true`,
			wantError:   "bounded hard node readiness wait",
		},
		{
			name:        "bounded node readiness wait bypassed",
			old:         `--for=condition=Ready nodes --all --timeout=2m; then`,
			replacement: `--for=condition=Ready nodes --all --timeout=2m || true; then`,
			wantError:   "bounded hard node readiness wait",
		},
		{
			name:        "immediate readiness predicate accepts a partial cluster",
			old:         `.type == "Ready" and .status == "True"`,
			replacement: `.status == "True"`,
			wantError:   "immediate all-node readiness predicate",
		},
		{
			name:        "immediate readiness predicate accepts zero nodes",
			old:         `((.items | length) > 0) and`,
			replacement: `true and`,
			wantError:   "immediate all-node readiness predicate",
		},
		{
			name:        "immediate readiness predicate masks node query failure",
			old:         `get nodes -o json >"$NODE_READINESS_FILE" &&`,
			replacement: `get nodes -o json |`,
			wantError:   "immediate all-node readiness predicate",
		},
		{
			name:        "node warning diagnostics broadened beyond nodes",
			old:         `[.items[] | select(.involvedObject.kind == "Node")]`,
			replacement: `[.items[]]`,
			wantError:   "credential-safe node readiness diagnostics",
		},
		{
			name: "node condition diagnostics include free-form messages",
			old: ".status,\n" +
				`          (.reason // "-"),` + "\n" +
				`          (.lastTransitionTime // "-")`,
			replacement: ".status,\n" +
				`          (.message // "-"),` + "\n" +
				`          (.lastTransitionTime // "-")`,
			wantError: "credential-safe node readiness diagnostics",
		},
		{
			name:        "node diagnostics append raw workload YAML",
			old:         `printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2`,
			replacement: "kubectl --kubeconfig \"$KUBECONFIG_FILE\" get pods -A -o yaml >&2 || true\n\t" + `printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2`,
			wantError:   "credential-safe node readiness diagnostics",
		},
		{
			name:        "node diagnostics append broad describe output",
			old:         `printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2`,
			replacement: "kubectl --kubeconfig \"$KUBECONFIG_FILE\" describe pods -A >&2 || true\n\t" + `printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2`,
			wantError:   "credential-safe node readiness diagnostics",
		},
		{
			name: "node diagnostics are replaced by a later unsafe definition",
			old:  "    ' >&2 || true\n}\n\nwait_for_ready_nodes() {",
			replacement: "    ' >&2 || true\n}\n\n" +
				"collect_node_readiness_diagnostics() {\n" +
				"\tkubectl --kubeconfig \"$KUBECONFIG_FILE\" get pods -A -o yaml >&2 || true\n" +
				"}\n\nwait_for_ready_nodes() {",
			wantError: "exactly one function definition",
		},
		{
			name:        "bounded node readiness wait is replaced by a later no-op definition",
			old:         "collect_diagnostics() {",
			replacement: "wait_for_ready_nodes() { return 0; }\n\ncollect_diagnostics() {",
			wantError:   "wait_for_ready_nodes must have exactly one function definition",
		},
		{
			name:        "immediate node readiness predicate is replaced by a later no-op definition",
			old:         "collect_diagnostics() {",
			replacement: "nodes_ready_now() { return 0; }\n\ncollect_diagnostics() {",
			wantError:   "nodes_ready_now must have exactly one function definition",
		},
		{
			name:        "hard node readiness requirement is replaced by a later no-op definition",
			old:         "collect_diagnostics() {",
			replacement: "require_ready_nodes() { :; }\n\ncollect_diagnostics() {",
			wantError:   "require_ready_nodes must have exactly one function definition",
		},
		{
			name:        "hard node readiness requirement is replaced by a spaced multiline definition",
			old:         "collect_diagnostics() {",
			replacement: "require_ready_nodes ( )\n{\n\t:\n}\n\ncollect_diagnostics() {",
			wantError:   "require_ready_nodes must have exactly one function definition",
		},
		{
			name:        "hard node readiness requirement declarator is split across a continuation",
			old:         "collect_diagnostics() {",
			replacement: "require_ready_nodes \\\n() { :; }\n\ncollect_diagnostics() {",
			wantError:   "require_ready_nodes must have exactly one function definition",
		},
		{
			name:        "hard node readiness requirement is replaced by a subshell-body definition",
			old:         "collect_diagnostics() {",
			replacement: "require_ready_nodes() ( : )\n\ncollect_diagnostics() {",
			wantError:   "require_ready_nodes must have exactly one function definition",
		},
		{
			name:        "hard node readiness requirement is replaced by a keyword-body definition",
			old:         "collect_diagnostics() {",
			replacement: "require_ready_nodes() if false; then return 1; fi\n\ncollect_diagnostics() {",
			wantError:   "require_ready_nodes must have exactly one function definition",
		},
		{
			name:        "post-creation node readiness omitted",
			old:         `require_ready_nodes "after kind cluster creation"`,
			replacement: `: # post-creation node readiness omitted`,
			wantError:   "kind cluster creation",
		},
		{
			name:        "server version extraction omitted",
			old:         `server_version=$(kubectl --kubeconfig "$KUBECONFIG_FILE" version -o json |`,
			replacement: `server_version=v"$K8S_VERSION"`,
			wantError:   "API server version binding",
		},
		{
			name:        "server version verification omitted",
			old:         `v"$K8S_VERSION"*) ;;`,
			replacement: `v*) ;;`,
			wantError:   "API server version binding",
		},
		{
			name:        "admission OpenAPI endpoint omitted",
			old:         `/openapi/v3/apis/admissionregistration.k8s.io/v1 >"$ADMISSION_OPENAPI_FILE"`,
			replacement: `/openapi/v3 >"$ADMISSION_OPENAPI_FILE"`,
			wantError:   "live admission OpenAPI boundary",
		},
		{
			name:        "admission OpenAPI filter bypassed",
			old:         `"$ADMISSION_OPENAPI_FILE" >/dev/null ||`,
			replacement: `"$ADMISSION_OPENAPI_FILE" >/dev/null || true ||`,
			wantError:   "live admission OpenAPI boundary",
		},
		{
			name:        "predecessor install readiness gate omitted",
			old:         `require_ready_nodes "immediately before predecessor Helm install"`,
			replacement: `: # predecessor install readiness gate omitted`,
			wantError:   "immediate predecessor install readiness gate",
		},
		{
			name:        "predecessor install failure ignored",
			old:         `if command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			replacement: `command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			wantError:   "immediate predecessor install readiness gate",
		},
		{
			name:        "post-install failure readiness recheck is delayed",
			old:         `if nodes_ready_now; then`,
			replacement: `if wait_for_ready_nodes "after predecessor Helm install failed"; then`,
			wantError:   "immediate predecessor install readiness gate",
		},
		{
			name: "post-install readiness recheck is not immediate",
			old:  "\tpredecessor_install_status=$?\n\tif nodes_ready_now; then",
			replacement: "\tpredecessor_install_status=$?\n" +
				"\tsleep 30\n\tif nodes_ready_now; then",
			wantError: "immediate predecessor install readiness gate",
		},
		{
			name:        "Helm function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\nhelm() { command helm \"$@\" || command helm \"$@\"; }\n",
			wantError:   "Helm function override",
		},
		{
			name:        "command function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\ncommand() { /usr/bin/env \"$@\" || /usr/bin/env \"$@\"; }\n",
			wantError:   "command function override",
		},
		{
			name:        "multiline command function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\ncommand()\n{\n\t/usr/bin/env \"$@\" || /usr/bin/env \"$@\"\n}\n",
			wantError:   "command function override",
		},
		{
			name:        "spaced-parentheses command function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\ncommand ( ) { /usr/bin/env \"$@\" || /usr/bin/env \"$@\"; }\n",
			wantError:   "command function override",
		},
		{
			name:        "subshell-body command function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\ncommand() ( /usr/bin/env \"$@\" || /usr/bin/env \"$@\"; )\n",
			wantError:   "command function override",
		},
		{
			name:        "keyword-body command function override retries commands",
			old:         "set -eu\n",
			replacement: "set -eu\ncommand() if /usr/bin/env \"$@\"; then :; else /usr/bin/env \"$@\"; fi\n",
			wantError:   "command function override",
		},
		{
			name:        "command alias override replaces the builtin",
			old:         "set -eu\n",
			replacement: "set -eu\nalias command='retry_command'\n",
			wantError:   "command alias override",
		},
		{
			name:        "single-quoted command alias override replaces the builtin",
			old:         "set -eu\n",
			replacement: "set -eu\nalias 'command=retry_command'\n",
			wantError:   "command alias override",
		},
		{
			name:        "double-quoted Helm alias override replaces the executable",
			old:         "set -eu\n",
			replacement: "set -eu\nalias \"helm=retry_helm\"\n",
			wantError:   "Helm alias override",
		},
		{
			name: "predecessor install retried",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ncommand helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried in a subshell",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\n(command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\") || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install executable is split across a continuation",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ncommand hel\\\nm install retry-chart || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install subcommand is split across a continuation",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ncommand helm ins\\\ntall retry-chart || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried through quoted command substitution",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_result=\"$(command helm install retry-chart)\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried through an expandable here-document",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nsh <<EOF\ncommand helm install retry-chart\nEOF",
			wantError: "shell here-document syntax",
		},
		{
			name: "predecessor install retried through legacy command substitution",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_result=`helm install retry-chart` || true",
			wantError: "legacy backtick command substitution",
		},
		{
			name: "predecessor install retried in a command group",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\n{ command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"; } || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried as a pipeline command",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nprintf '%s\\n' retry | command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried after a background separator",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ntrue & command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried after an or-list boundary",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nfalse || command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried after a control keyword",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nif true; then command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"; fi",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried through a shell command string",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nsh -c 'command helm install retry-chart' || true",
			wantError: "Helm command-string launch",
		},
		{
			name: "predecessor install retried through eval",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\neval 'command helm install retry-chart' || true",
			wantError: "Helm command-string launch",
		},
		{
			name: "predecessor install retried through a variable-backed shell command string",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nenv sh -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "predecessor install retried through Bash after a long option",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nbash --noprofile -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "predecessor install retried through env and Bash after a long option",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nenv bash --noprofile -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "predecessor install retried through Bash after an option operand",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nbash -O extglob -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "predecessor install retried with environment prefix",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nHELM_DEBUG=1 command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "predecessor install retried through env",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nenv HELM_DEBUG=1 command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "predecessor install retried through env option operand",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nenv -u HELM_DEBUG helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "predecessor install retried through plain env",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nenv helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "predecessor install retried through multiline env",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\nenv -u HELM_DEBUG \\\n\thelm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "predecessor install retried through command env",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ncommand env HELM_DEBUG=1 helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "predecessor install retried through chained env",
			old: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)\"\n" +
				"fi\ntrue && env helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name:        "upgrade lifecycle omitted",
			old:         "E2E_PHASE=upgrade \\\n",
			replacement: "E2E_PHASE=upgrade-omitted \\\n",
			wantError:   "candidate upgrade lifecycle",
		},
		{
			name: "upgrade child call removed",
			old: "E2E_PHASE=upgrade \\\n" +
				"\t\"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
			replacement: `true # upgrade child call removed`,
			wantError:   "candidate upgrade lifecycle",
		},
		{
			name: "upgrade child call hidden in false branch",
			old: "E2E_PHASE=upgrade \\\n" +
				"\t\"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
			replacement: "if false; then\n\tE2E_PHASE=upgrade \\\n" +
				"\t\t\"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "high availability lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-ha.sh"`,
			replacement: `true # high availability lifecycle omitted`,
			wantError:   "high-availability lifecycle",
		},
		{
			name:        "high availability lifecycle hidden in false branch",
			old:         `"$ROOT_DIR/hack/e2e-ha.sh"`,
			replacement: "if false; then\n\t\"$ROOT_DIR/hack/e2e-ha.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "control plane lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-assert.sh"`,
			replacement: `true # control plane lifecycle omitted`,
			wantError:   "control-plane lifecycle",
		},
		{
			name:        "control plane lifecycle hidden in false branch",
			old:         `"$ROOT_DIR/hack/e2e-assert.sh"`,
			replacement: "if false; then\n\t\"$ROOT_DIR/hack/e2e-assert.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "certificate lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: `true # certificate lifecycle omitted`,
			wantError:   "certificate lifecycle",
		},
		{
			name:        "certificate lifecycle hidden in false branch",
			old:         `"$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: "if false; then\n\t\"$ROOT_DIR/hack/e2e-cert-rotation.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "data plane lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-dataplane.sh"`,
			replacement: `true # data plane lifecycle omitted`,
			wantError:   "data-plane and OCI lifecycle",
		},
		{
			name:        "data plane lifecycle hidden in false branch",
			old:         `"$ROOT_DIR/hack/e2e-dataplane.sh"`,
			replacement: "if false; then\n\t\"$ROOT_DIR/hack/e2e-dataplane.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "uninstall lifecycle omitted",
			old:         "E2E_PHASE=uninstall \\\n",
			replacement: "E2E_PHASE=uninstall-omitted \\\n",
			wantError:   "uninstall lifecycle",
		},
		{
			name:        "terminal evidence omitted",
			old:         `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`,
			replacement: `printf '%s\n' 'e2e lifecycle finished without evidence'`,
			wantError:   "terminal Kubernetes lifecycle evidence",
		},
		{
			name:        "early successful exit",
			old:         "set -eu\n",
			replacement: "set -eu\nexit 0\n",
			wantError:   "unconditional successful exit",
		},
		{
			name:        "top-level fail-fast mode disabled",
			old:         "set -eu\n",
			replacement: "set -eu\nset +e\n",
			wantError:   "top-level fail-fast mode is disabled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.harness = writeMutatedE2ESource(t, "e2e-kind.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyE2EDataPlaneRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	dataPlane := files.dataPlane
	source := readE2ESource(t, dataPlane)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "interpreter bypass",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "failure status trap bypass",
			old:         "trap cleanup EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "operation Pod create-origin proof omitted",
			old:         `printf '%s\n' 'e2e data plane: PASS operation Pod create-origin enforcement'`,
			replacement: `printf '%s\n' 'operation Pod clone check skipped'`,
			wantError:   "operation Pod create-origin evidence",
		},
		{
			name:        "operation Pod clone retains bypassable selector labels",
			old:         `.metadata.labels["app.kubernetes.io/managed-by"],`,
			replacement: `.metadata.labels["unrelated.example/label"],`,
			wantError:   "label-less operation Pod clone",
		},
		{
			name:        "operation Pod generated-name binding omitted",
			old:         `[ "$CAPTURED_POD_GENERATE_NAME" = "${CAPTURED_JOB_NAME}-" ] ||`,
			replacement: `true ||`,
			wantError:   "operation Pod generated-name binding",
		},
		{
			name:        "operation generated-name fixture shortened",
			old:         `EXTERNAL_PG_SCHEMA=e2e-postgresql-external-longpod`,
			replacement: `EXTERNAL_PG_SCHEMA=e2e-postgresql-external`,
			wantError:   "operation Job generated-name boundary fixture",
		},
		{
			name:        "operation generated-name boundary proof weakened",
			old:         `[ "${#CAPTURED_POD_GENERATE_NAME}" -eq 59 ] ||`,
			replacement: `[ "${#CAPTURED_POD_GENERATE_NAME}" -gt 0 ] ||`,
			wantError:   "operation generated-name boundary proof",
		},
		{
			name:        "OCI lifecycle implementation omitted",
			old:         `run_engine_lifecycle() {`,
			replacement: `run_engine_lifecycle_omitted() {`,
			wantError:   "OCI lifecycle implementation",
		},
		{
			name:        "safe-default persistence proof omitted",
			old:         `' >/dev/null || fail "$resource_schema did not persist the safe apply-policy defaults"`,
			replacement: `' >/dev/null || true # safe defaults not proven`,
			wantError:   "safe-default persistence proof",
		},
		{
			name:        "immutable plan-storage proof call omitted",
			old:         `assert_plan_storage_immutable "$plan_schema" "$CURRENT_PLAN" "$CURRENT_PLAN_UID"`,
			replacement: `true # immutable plan storage not proven`,
			wantError:   "immutable plan-storage proof call",
		},
		{
			name:        "external lifecycle does not select its published digest",
			old:         `external_reference="${external_publish_reference%:stable}@${external_digest}"`,
			replacement: `external_reference="$external_publish_reference"`,
			wantError:   "external digest-selected OCI source",
		},
		{
			name:        "external lifecycle does not consume its digest-selected source",
			old:         `"$external_reference" "$EXTERNAL_PG_COORDINATION_KEY" \`,
			replacement: `"$external_publish_reference" "$EXTERNAL_PG_COORDINATION_KEY" \`,
			wantError:   "external lifecycle digest-selected source call",
		},
		{
			name:        "external lifecycle returns before consuming its source",
			old:         "run_external_postgresql_lifecycle() {\n",
			replacement: "run_external_postgresql_lifecycle() {\n\treturn 0\n",
			wantError:   "unconditional successful return",
		},
		{
			name:        "OCI reference construction omitted",
			old:         `lifecycle_reference="oci://${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000/schemas/${lifecycle_slug}:stable"`,
			replacement: `lifecycle_reference="file:///tmp/${lifecycle_slug}"`,
			wantError:   "OCI reference construction",
		},
		{
			name:        "OCI publication omitted",
			old:         `digest_v1=$(publish_schema "$lifecycle_slug" v1 "$lifecycle_dialect" "$lifecycle_reference")`,
			replacement: `digest_v1=sha256:omitted`,
			wantError:   "OCI publication",
		},
		{
			name:        "OCI lifecycle returns before exercising sources",
			old:         "run_engine_lifecycle() {\n",
			replacement: "run_engine_lifecycle() {\n\treturn 0\n",
			wantError:   "unconditional successful return",
		},
		{
			name:        "registry fixture omitted",
			old:         "create_registry_service\n",
			replacement: "true # registry fixture omitted\n",
			wantError:   "registry fixture",
		},
		{
			name:        "authenticated OCI fixture omitted",
			old:         "create_authenticated_tls_proxy\n",
			replacement: "true # authenticated OCI fixture omitted\n",
			wantError:   "authenticated OCI fixture",
		},
		{
			name:        "PostgreSQL lifecycle omitted",
			old:         `run_engine_lifecycle postgresql PostgreSQL postgres "$PG_SECRET"`,
			replacement: `true # PostgreSQL lifecycle omitted`,
			wantError:   "PostgreSQL lifecycle",
		},
		{
			name:        "external PostgreSQL lifecycle omitted",
			old:         "run_external_postgresql_lifecycle\n",
			replacement: "true # external PostgreSQL lifecycle omitted\n",
			wantError:   "external PostgreSQL lifecycle",
		},
		{
			name:        "MySQL lifecycle omitted",
			old:         `run_engine_lifecycle mysql MySQL mysql "$MYSQL_SECRET"`,
			replacement: `true # MySQL lifecycle omitted`,
			wantError:   "MySQL lifecycle",
		},
		{
			name:        "fault lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-faults.sh"`,
			replacement: `true # fault lifecycle omitted`,
			wantError:   "fault lifecycle",
		},
		{
			name:        "fault lifecycle hidden in false branch",
			old:         `"$ROOT_DIR/hack/e2e-faults.sh"`,
			replacement: "if false; then\n\t\"$ROOT_DIR/hack/e2e-faults.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "operation audit omitted",
			old:         "assert_observed_jobs_audited\n",
			replacement: "true # operation audit omitted\n",
			wantError:   "audited operation evidence",
		},
		{
			name:        "terminal evidence omitted",
			old:         `printf '%s\n' 'e2e data plane: PASS PostgreSQL, external PostgreSQL, MySQL, OCI, restart, and fault lifecycle'`,
			replacement: `printf '%s\n' 'e2e data plane finished without evidence'`,
			wantError:   "terminal data-plane lifecycle evidence",
		},
		{
			name:        "early successful exit",
			old:         "set -eu\n",
			replacement: "set -eu\nexec /usr/bin/true\n",
			wantError:   "unconditional successful exit",
		},
		{
			name:        "top-level fail-fast mode disabled",
			old:         "set -eu\n",
			replacement: "set -eu\nset +o errexit\n",
			wantError:   "top-level fail-fast mode is disabled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.dataPlane = writeMutatedE2ESource(t, "e2e-dataplane.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyFailedUpgradeEvidenceRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	source := readE2ESource(t, files.crdUpgrade)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "next revision is not bound to current revision",
			old:         `failed_revision=$((before_revision + 1))`,
			replacement: `failed_revision=$before_revision`,
			wantError:   "current and next failed revision binding",
		},
		{
			name: "hook name is not derived from rendered identity",
			old: "[ -n \"$EXPECTED_IDENTITY_HOOK_NAME\" ] || fail \"rendered identity hook name is unavailable\"\n" +
				"\t[ -n \"$EXPECTED_PREFLIGHT_HOOK_NAME\" ] || fail \"rendered preflight hook name is unavailable\"",
			replacement: "[ -n \"$EXPECTED_IDENTITY_HOOK_NAME\" ] || fail \"rendered identity hook name is unavailable\"\n" +
				"\tEXPECTED_PREFLIGHT_HOOK_NAME=ptah-crd-preflight",
			wantError: "rendered hook identity binding",
		},
		{
			name:        "failed revision is not selected explicitly",
			old:         `--revision "$failed_revision" -o json >"$status_file"; then`,
			replacement: `-o json >"$status_file"; then`,
			wantError:   "explicit revision retrieval",
		},
		{
			name:        "evidence is checked against previous revision",
			old:         `--argjson expected_revision "$failed_revision" \`,
			replacement: `--argjson expected_revision "$before_revision" \`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "evidence omits exact hook name",
			old:         `--arg expected_name "$EXPECTED_PREFLIGHT_HOOK_NAME" \`,
			replacement: `--arg expected_name "" \`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "evidence omits exact hook weight",
			old:         `--argjson expected_weight -60 \`,
			replacement: `--argjson expected_weight -50 \`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "evidence omits successful identity hook",
			old:         `--arg expected_identity_name "$EXPECTED_IDENTITY_HOOK_NAME" \`,
			replacement: `--arg expected_identity_name "" \`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "evidence omits identity hook weight",
			old:         `--argjson expected_identity_weight -105 \`,
			replacement: `--argjson expected_identity_weight -104 \`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "identity capture is not armed before Helm",
			old:         "\tarm_identity_hook_log_capture\n",
			replacement: "\t: # identity capture omitted\n",
			wantError:   "rendered hook identity binding",
		},
		{
			name: "identity capture is not stopped on unexpected success",
			old: "\t\tfinish_identity_hook_log_capture\n" +
				"\t\tfail \"$description unexpectedly succeeded\"",
			replacement: "\t\tfail \"$description unexpectedly succeeded\"",
			wantError:   "failed upgrade execution and explicit revision retrieval",
		},
		{
			name: "safe identity diagnostic is omitted",
			old: "\t\temit_identity_hook_diagnostic >&2 ||\n" +
				"\t\t\tfail \"$description identity-hook diagnostic failed closed\"",
			replacement: "\t\ttrue",
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name:        "evidence filter result is ignored",
			old:         `-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null; then`,
			replacement: `-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null || true; then`,
			wantError:   "exact failed preflight evidence evaluation",
		},
		{
			name: "stderr is parsed as hook evidence",
			old:  `status_file=$WORK_DIR/failed-upgrade-status.json`,
			replacement: "status_file=$WORK_DIR/failed-upgrade-status.json\n" +
				`grep -F preflight "$WORK_DIR/failed-upgrade.err" >/dev/null || true`,
			wantError: "stderr may only be captured once",
		},
		{
			name: "structured revision evidence is overwritten",
			old:  `status_file=$WORK_DIR/failed-upgrade-status.json`,
			replacement: "status_file=$WORK_DIR/failed-upgrade-status.json\n" +
				`printf '%s\n' '{}' >"$status_file"`,
			wantError: "must flow only from the explicitly retrieved structured revision status",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.crdUpgrade = writeMutatedE2ESource(t, "e2e-crd-upgrade.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyAdmissionSchemaAssetsRejectCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	t.Run("filter accepts added fields", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.admissionSchemaContract)
		mutated := files
		mutated.admissionSchemaContract = writeMutatedE2ESource(
			t,
			"admission-schema-contract.jq",
			source,
			`if $actual == $expected then true`,
			`if ($expected - $actual | length) == 0 then true`,
		)
		if err := verifyAdmissionSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "exact configuration, metadata, client, and webhook field inventories") {
			t.Fatalf("verifyAdmissionSchemaAssets() error = %v, want exact inventory rejection", err)
		}
	})

	t.Run("configuration negative removed", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.admissionSchemaSelftest)
		mutated := files
		mutated.admissionSchemaSelftest = writeMutatedE2ESource(
			t,
			"admission-schema-contract-selftest.sh",
			source,
			`if jq -e -f "$FILTER" "$top_level" >/dev/null 2>&1; then`,
			`if false; then`,
		)
		if err := verifyAdmissionSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "added configuration field refusal") {
			t.Fatalf("verifyAdmissionSchemaAssets() error = %v, want configuration negative rejection", err)
		}
	})

	t.Run("static invocation removed", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.staticChecks)
		mutated := files
		mutated.staticChecks = writeMutatedE2ESource(
			t,
			"e2e-static.sh",
			source,
			`"$(dirname -- "$0")/admission-schema-contract-selftest.sh"`,
			`: # admission schema self-test removed`,
		)
		if err := verifyAdmissionSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "admission schema self-test wiring") {
			t.Fatalf("verifyAdmissionSchemaAssets() error = %v, want static wiring rejection", err)
		}
	})
}

func TestVerifyControllerObjectSchemaAssetsRejectCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	t.Run("reviewed field inventory changed", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.controllerSchemaContract)
		mutated := files
		mutated.controllerSchemaContract = writeMutatedE2ESource(
			t,
			"controller-object-schema-contract.jq",
			source,
			`    "scheduling",`,
			`    "schedulingGroup",`,
		)
		if err := verifyControllerObjectSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "field inventory digest") {
			t.Fatalf("verifyControllerObjectSchemaAssets() error = %v, want inventory digest rejection", err)
		}
	})

	t.Run("unreviewed minor negative removed", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.controllerSchemaSelftest)
		mutated := files
		mutated.controllerSchemaSelftest = writeMutatedE2ESource(
			t,
			"controller-object-schema-contract-selftest.sh",
			source,
			`if evaluate 1.38 "$batch_fixture" "$core_fixture" 2>/dev/null; then`,
			`if false; then`,
		)
		if err := verifyControllerObjectSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "unreviewed minor refusal") {
			t.Fatalf("verifyControllerObjectSchemaAssets() error = %v, want unreviewed-minor rejection", err)
		}
	})

	t.Run("static invocation removed", func(t *testing.T) {
		t.Parallel()
		source := readE2ESource(t, files.staticChecks)
		mutated := files
		mutated.staticChecks = writeMutatedE2ESource(
			t,
			"e2e-static.sh",
			source,
			`"$(dirname -- "$0")/controller-object-schema-contract-selftest.sh"`,
			`: # controller object schema self-test removed`,
		)
		if err := verifyControllerObjectSchemaAssets(mutated); err == nil || !strings.Contains(err.Error(), "controller object schema self-test wiring") {
			t.Fatalf("verifyControllerObjectSchemaAssets() error = %v, want static wiring rejection", err)
		}
	})
}

func TestVerifyFailedHookEvidenceFilterRejectsContractMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	source := readE2ESource(t, files.failedHookEvidence)
	tests := []struct {
		name        string
		old         string
		replacement string
	}{
		{name: "revision", old: `(.version == $expected_revision)`, replacement: `(.version >= $expected_revision)`},
		{name: "release status", old: `(.info.status == "failed")`, replacement: `(.info.status != "deployed")`},
		{name: "single failed hook", old: `($failed | length == 1)`, replacement: `($failed | length >= 1)`},
		{name: "hook name", old: `.name == $expected_name`, replacement: `.name != ""`},
		{name: "hook kind", old: ".name == $expected_name and\n  .kind == \"Job\"", replacement: ".name == $expected_name and\n  .kind != \"\""},
		{name: "weight default", old: `if .weight == null then 0 else (.weight | tonumber) end;`, replacement: `if .weight == null then -1 else (.weight | tonumber) end;`},
		{name: "hook weight", old: `hook_weight == $expected_weight`, replacement: `hook_weight <= $expected_weight`},
		{name: "hook event", old: "hook_weight == $expected_weight and\n  ((.events // []) | index(\"pre-upgrade\") != null)", replacement: "hook_weight == $expected_weight and\n  ((.events // []) | length > 0)"},
		{name: "started timestamp", old: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0)", replacement: "((.events // []) | index(\"pre-upgrade\") != null) and\n  true"},
		{name: "completed timestamp", old: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0) and\n  ((.last_run.completed_at // \"\") | length > 0))", replacement: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0) and\n  true)"},
		{name: "identity name", old: `.name == $expected_identity_name`, replacement: `.name != ""`},
		{name: "identity weight", old: `hook_weight == $expected_identity_weight`, replacement: `hook_weight <= $expected_identity_weight`},
		{name: "identity success", old: `hook_phase == "Succeeded"`, replacement: `hook_phase != "Failed"`},
		{name: "later hook cutoff", old: `(hook_weight > $expected_weight)`, replacement: `(hook_weight >= $expected_weight)`},
		{name: "later hook exclusion", old: `hook_phase == ""`, replacement: `hook_phase != "Failed"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.failedHookEvidence = writeMutatedE2ESource(t, "failed-hook-evidence.jq", source, test.old, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), "failed Helm hook evidence filter") {
				t.Fatalf("verifyE2EWiring() error = %v, want exact filter contract rejection", err)
			}
		})
	}
}

func TestVerifyFailedHookEvidenceSelftestRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	source := readE2ESource(t, files.failedHookEvidenceSelftest)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "wrong filter is evaluated",
			old:         `-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$1" >/dev/null`,
			replacement: `-f "$ROOT_DIR/hack/other-filter.jq" "$1" >/dev/null`,
			wantError:   "revision-bound failed-hook evaluator",
		},
		{
			name:        "valid fixture evaluation is removed",
			old:         `evaluate "$WORK_DIR/valid.json"`,
			replacement: `: # valid fixture evaluation removed`,
			wantError:   "valid fixture evaluation",
		},
		{
			name:        "revision negative is removed",
			old:         `expect_rejected wrong-revision '.version = 8'`,
			replacement: `: # wrong revision accepted`,
			wantError:   "wrong revision refusal",
		},
		{
			name:        "name negative is removed",
			old:         `expect_rejected wrong-name '.hooks[1].name = "other-preflight"'`,
			replacement: `: # wrong name accepted`,
			wantError:   "wrong hook name refusal",
		},
		{
			name:        "weight negative is removed",
			old:         `expect_rejected wrong-weight '.hooks[1].weight = -59'`,
			replacement: `: # wrong weight accepted`,
			wantError:   "wrong hook weight refusal",
		},
		{
			name:        "later hook negative is removed",
			old:         `expect_rejected later-hook-ran '.hooks[2].last_run = .hooks[0].last_run'`,
			replacement: `: # later hook execution accepted`,
			wantError:   "later hook execution refusal",
		},
		{
			name:        "negative checker returns successfully",
			old:         "expect_rejected() {\n",
			replacement: "expect_rejected() {\n\treturn 0\n",
			wantError:   "unconditional successful return",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.failedHookEvidenceSelftest = writeMutatedE2ESource(t, "failed-hook-evidence-selftest.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyFailedHookEvidenceStaticWiringRejectsMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	source := readE2ESource(t, files.staticChecks)
	tests := []struct {
		name        string
		replacement string
		wantError   string
	}{
		{
			name:        "self-test invocation removed",
			replacement: `: # failed hook evidence self-test removed`,
			wantError:   "failed-hook evidence self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$(dirname -- "$0")/failed-hook-evidence-selftest.sh" || true`,
			wantError:   "failed-hook evidence self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$(dirname -- \"$0\")/failed-hook-evidence-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$(dirname -- "$0")/failed-hook-evidence-selftest.sh"`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.staticChecks = writeMutatedE2ESource(t, "e2e-static.sh", source, invocation, test.replacement)
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyE2EChildScriptsRejectCriticalMutations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		child       string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "assertions interpreter bypass",
			child:       "assertions",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "assertions fail-fast bypass",
			child:       "assertions",
			old:         "set -eu\n",
			replacement: "set +e\n",
			wantError:   "enable set -eu",
		},
		{
			name:        "assertions trap discards failure",
			child:       "assertions",
			old:         "trap cleanup_files EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "assertions proof call removed",
			child:       "assertions",
			old:         `printf '%s\n' 'e2e assertions: checking approval stamping and exact binding'`,
			replacement: `true # approval binding proof removed`,
			wantError:   "approval binding proof",
		},
		{
			name:  "assertions proof call hidden in false branch",
			child: "assertions",
			old:   `printf '%s\n' 'e2e assertions: checking approval stamping and exact binding'`,
			replacement: "if false; then\n\tprintf '%s\\n' " +
				"'e2e assertions: checking approval stamping and exact binding'\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "assertions terminal evidence removed",
			child:       "assertions",
			old:         `printf '%s\n' 'e2e assertions: PASS control-plane contract'`,
			replacement: `printf '%s\n' 'e2e assertions finished'`,
			wantError:   "terminal control-plane lifecycle evidence",
		},
		{
			name:        "assertions early successful exit",
			child:       "assertions",
			old:         "set -eu\n",
			replacement: "set -eu\nexit 0\n",
			wantError:   "unconditional successful exit",
		},
		{
			name:        "CRD interpreter bypass",
			child:       "crd-upgrade",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "CRD trap discards failure",
			child:       "crd-upgrade",
			old:         "trap cleanup EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "CRD predecessor Apply schema fixture path changed",
			child:       "crd-upgrade",
			old:         `cp "$ROOT_DIR/testdata/e2e/postgresql-v1.sql" "$predecessor_plan_source"`,
			replacement: `cp "$ROOT_DIR/testdata/e2e/postgres-v1.sql" "$predecessor_plan_source"`,
			wantError:   "predecessor Apply schema fixture",
		},
		{
			name:        "CRD legacy plan activation probe manifest removed",
			child:       "crd-upgrade",
			old:         `PREDECESSOR_PLAN_GUARD_PROBE_FILE=$WORK_DIR/predecessor-plan-guard-probe.json`,
			replacement: `PREDECESSOR_PLAN_GUARD_PROBE_FILE=`,
			wantError:   "legacy plan activation probe manifest",
		},
		{
			name:        "CRD legacy Job activation probe source removed",
			child:       "crd-upgrade",
			old:         `legacy_job_source=$WORK_DIR/predecessor-read-only-job-terminal.json`,
			replacement: `legacy_job_source=`,
			wantError:   "legacy Job activation probe source",
		},
		{
			name:        "CRD legacy Job bootstrap boundary is bypassed",
			child:       "crd-upgrade",
			old:         `fail "legacy Job bootstrap probe did not reach the semantic controller-write boundary"`,
			replacement: `true # legacy Job semantic boundary removed`,
			wantError:   "legacy Job bootstrap semantic boundary",
		},
		{
			name:        "CRD legacy Job active denial is bypassed",
			child:       "crd-upgrade",
			old:         `fail "legacy Job post-activation probe lacked the exact structural guard denial"`,
			replacement: `true # legacy Job active denial removed`,
			wantError:   "legacy Job active structural denial",
		},
		{
			name:        "CRD legacy plan bootstrap boundary is bypassed",
			child:       "crd-upgrade",
			old:         `fail "legacy plan bootstrap probe did not reach the semantic controller-write boundary"`,
			replacement: `true # legacy plan semantic boundary removed`,
			wantError:   "legacy plan bootstrap semantic boundary",
		},
		{
			name:        "CRD legacy plan active denial is bypassed",
			child:       "crd-upgrade",
			old:         `fail "legacy plan post-activation probe lacked the exact structural guard denial"`,
			replacement: `true # legacy plan active denial removed`,
			wantError:   "legacy plan active structural denial",
		},
		{
			name:        "CRD late activation hook identity is hard-coded",
			child:       "crd-upgrade",
			old:         `reconcile_matches=$(rendered_hook_job_name crd-manager 0)`,
			replacement: `reconcile_matches=ptah-operator-crd-manager`,
			wantError:   "exact rendered reconcile hook identity",
		},
		{
			name:        "CRD late activation hook identity assignment is hard-coded",
			child:       "crd-upgrade",
			old:         `EXPECTED_RECONCILE_HOOK_NAME=$reconcile_matches`,
			replacement: `EXPECTED_RECONCILE_HOOK_NAME=ptah-operator-crd-manager`,
			wantError:   "rendered reconcile hook identity assignment",
		},
		{
			name:        "CRD late activation hook uniqueness checks the wrong render",
			child:       "crd-upgrade",
			old:         `[ "$(printf '%s\n' "$reconcile_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||`,
			replacement: `[ "$(printf '%s\n' "$identity_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||`,
			wantError:   "unique rendered reconcile hook identity",
		},
		{
			name:        "CRD late activation blocker broadens its target",
			child:       "crd-upgrade",
			old:         `expression: 'request.namespace == "$E2E_OPERATOR_NAMESPACE" && request.name == "ptah-operator-release-activation"'`,
			replacement: `expression: 'true'`,
			wantError:   "late activation exact update blocker",
		},
		{
			name:        "CRD late activation blocker loses same-sequence probe exclusion",
			child:       "crd-upgrade",
			old:         `expression: 'oldObject != null && has(oldObject.data) && has(object.data) && "active-release-sequence" in oldObject.data && "active-release-sequence" in object.data && object.data["active-release-sequence"] != oldObject.data["active-release-sequence"]'`,
			replacement: `expression: 'true'`,
			wantError:   "late activation exact update blocker",
		},
		{
			name:        "CRD late activation dual captures are not armed",
			child:       "crd-upgrade",
			old:         "\tarm_late_activation_hook_log_captures\n",
			replacement: "\t: # late activation hook captures omitted\n",
			wantError:   "late activation dual capture arming",
		},
		{
			name:        "CRD late activation dual captures are not finished",
			child:       "crd-upgrade",
			old:         "\tif finish_late_activation_hook_log_captures; then\n\t\tlate_activation_captures_succeeded=true\n\tfi\n",
			replacement: "\tif true; then\n\t\tlate_activation_captures_succeeded=true\n\tfi\n",
			wantError:   "late activation dual capture completion",
		},
		{
			name:        "CRD late activation helper build target is replaced",
			child:       "crd-upgrade",
			old:         `-o "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ./hack/hooklogcapture`,
			replacement: `-o "$LATE_ACTIVATION_HOOK_CAPTURE_BINARY" ./hack`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation preflight helper Job target is hard-coded",
			child:       "crd-upgrade",
			old:         `--job-name "$EXPECTED_PREFLIGHT_HOOK_NAME" \`,
			replacement: `--job-name ptah-operator-crd-manager-preflight \`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation reconcile helper Job target is hard-coded",
			child:       "crd-upgrade",
			old:         `--job-name "$EXPECTED_RECONCILE_HOOK_NAME" \`,
			replacement: `--job-name ptah-operator-crd-manager \`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation preflight mode is omitted",
			child:       "crd-upgrade",
			old:         `--hook-mode preflight \`,
			replacement: `--hook-mode reconcile \`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation reconcile mode is omitted",
			child:       "crd-upgrade",
			old:         `--hook-mode reconcile \`,
			replacement: `--hook-mode preflight \`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation preflight helper PID is not retained",
			child:       "crd-upgrade",
			old:         `LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID=$!`,
			replacement: `LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID=`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation reconcile helper PID is not retained",
			child:       "crd-upgrade",
			old:         `LATE_ACTIVATION_RECONCILE_CAPTURE_PID=$!`,
			replacement: `LATE_ACTIVATION_RECONCILE_CAPTURE_PID=`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation preflight helper is not cleaned up",
			child:       "crd-upgrade",
			old:         `wait "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_PID" >/dev/null 2>&1 || true`,
			replacement: `true # preflight capture is not joined during cleanup`,
			wantError:   "late activation preflight capture cleanup",
		},
		{
			name:        "CRD late activation reconcile helper is not cleaned up",
			child:       "crd-upgrade",
			old:         `wait "$LATE_ACTIVATION_RECONCILE_CAPTURE_PID" >/dev/null 2>&1 || true`,
			replacement: `true # reconcile capture is not joined during cleanup`,
			wantError:   "late activation reconcile capture cleanup",
		},
		{
			name:        "CRD late activation helper readiness accepts a non-watching process",
			child:       "crd-upgrade",
			old:         `[ "$(sed -n '1p' "$capture_status_file" 2>/dev/null)" != watching ] ||`,
			replacement: `[ ! -s "$capture_status_file" ] ||`,
			wantError:   "late activation readiness requires watching state",
		},
		{
			name:        "CRD late activation helper completion does not join the process",
			child:       "crd-upgrade",
			old:         `wait "$capture_pid" >/dev/null 2>&1 || capture_exit_status=$?`,
			replacement: `true # capture helper left unjoined`,
			wantError:   "exact bounded late activation hook capture completion contract",
		},
		{
			name:  "CRD late activation blocker is deleted before dual capture completion",
			child: "crd-upgrade",
			old: "\tlate_activation_captures_succeeded=false\n" +
				"\tif finish_late_activation_hook_log_captures; then\n" +
				"\t\tlate_activation_captures_succeeded=true\n" +
				"\tfi\n",
			replacement: "\tdelete_late_activation_blocker\n" +
				"\tlate_activation_captures_succeeded=false\n" +
				"\tif finish_late_activation_hook_log_captures; then\n" +
				"\t\tlate_activation_captures_succeeded=true\n" +
				"\tfi\n",
			wantError: "late activation dual capture completion",
		},
		{
			name:  "CRD late activation hook diagnostic drops the credential scan",
			child: "crd-upgrade",
			old: "if grep -F -f \"$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE\" \"$diagnostic_file\" >/dev/null; then\n" +
				"\t\treturn 1\n" +
				"\telse\n" +
				"\t\tdiagnostic_scan_status=$?\n" +
				"\t\t[ \"$diagnostic_scan_status\" -eq 1 ] || return 1\n" +
				"\tfi",
			replacement: `: # protected credential scan removed`,
			wantError:   "late activation hook diagnostic credential scan",
		},
		{
			name:        "CRD late activation hook diagnostic removes its size bound",
			child:       "crd-upgrade",
			old:         `[ "$diagnostic_size" -gt 0 ] && [ "$diagnostic_size" -le 8192 ] &&`,
			replacement: `[ "$diagnostic_size" -gt 0 ] &&`,
			wantError:   "late activation hook diagnostic bounded format",
		},
		{
			name:        "CRD late activation reconcile diagnostic accepts an unbound capture",
			child:       "crd-upgrade",
			old:         `fail "late activation reconcile log was not captured"`,
			replacement: `true # missing capture accepted`,
			wantError:   "exact reconcile blocker diagnostic emission contract",
		},
		{
			name:        "CRD late activation reconcile diagnostic omits the activation phase",
			child:       "crd-upgrade",
			old:         `'wait for release activation guard before persistence' \`,
			replacement: `'unrelated failure' \`,
			wantError:   "late activation reconcile exact blocker evidence",
		},
		{
			name:        "CRD late activation reconcile diagnostic omits the blocker webhook",
			child:       "crd-upgrade",
			old:         `'late-activation-blocker.operator.ptah.dev' \`,
			replacement: `'unrelated-webhook.operator.ptah.dev' \`,
			wantError:   "late activation reconcile exact blocker evidence",
		},
		{
			name:        "CRD late activation reconcile diagnostic omits the missing service",
			child:       "crd-upgrade",
			old:         `'service "ptah-operator-e2e-missing-blocker" not found'; do`,
			replacement: `'unrelated service failure'; do`,
			wantError:   "late activation reconcile exact blocker evidence",
		},
		{
			name:        "CRD late activation preflight diagnostic emits before safety checks",
			child:       "crd-upgrade",
			old:         "emit_late_activation_preflight_diagnostic_if_available() {\n",
			replacement: "emit_late_activation_preflight_diagnostic_if_available() {\n\tcat \"$LATE_ACTIVATION_PREFLIGHT_LOG_FILE\" >&2\n",
			wantError:   "exact optional preflight diagnostic emission contract",
		},
		{
			name:        "CRD late activation reconcile diagnostic emits before safety checks",
			child:       "crd-upgrade",
			old:         "emit_late_activation_reconcile_diagnostic() {\n",
			replacement: "emit_late_activation_reconcile_diagnostic() {\n\tcat \"$LATE_ACTIVATION_RECONCILE_LOG_FILE\" >&2\n",
			wantError:   "late activation reconcile safe diagnostic emission",
		},
		{
			name:        "CRD late activation preflight is not name-bound",
			child:       "crd-upgrade",
			old:         ".name == $expected_preflight_name and\n            .kind == \"Job\"",
			replacement: ".name != \"\" and\n            .kind == \"Job\"",
			wantError:   "late activation failed revision evidence",
		},
		{
			name:        "CRD late activation reconcile is not name-bound",
			child:       "crd-upgrade",
			old:         ".name == $expected_reconcile_name and\n            .kind == \"Job\"",
			replacement: ".name != \"\" and\n            .kind == \"Job\"",
			wantError:   "late activation reconcile identity evidence",
		},
		{
			name:        "CRD late activation accepts multiple failed hooks",
			child:       "crd-upgrade",
			old:         `($failed | length == 1) and`,
			replacement: `($failed | length >= 1) and`,
			wantError:   "late activation exact failed reconcile evidence",
		},
		{
			name:        "CRD late activation accepts a failed preflight",
			child:       "crd-upgrade",
			old:         `.last_run.phase == "Succeeded" and`,
			replacement: `.last_run.phase != "" and`,
			wantError:   "late activation exact preflight success evidence",
		},
		{
			name:  "CRD late activation coerces a reconcile hook weight",
			child: "crd-upgrade",
			old: "($reconcile[0] |\n" +
				`            (.weight == null or ((.weight | type) == "number" and .weight == 0)) and`,
			replacement: "($reconcile[0] |\n" +
				`            (.weight == null or (.weight | tonumber) == 0) and`,
			wantError: "late activation exact failed reconcile evidence",
		},
		{
			name:        "CRD late activation failed status jq is not fail-closed",
			child:       "crd-upgrade",
			old:         `if jq -e --argjson expected_revision "$late_revision" \`,
			replacement: `if jq --argjson expected_revision "$late_revision" \`,
			wantError:   "late activation failed status fail-closed exact hook identity jq",
		},
		{
			name:        "CRD late activation exact reconcile diagnostic is skipped",
			child:       "crd-upgrade",
			old:         "\temit_late_activation_reconcile_diagnostic\n",
			replacement: "\t: # exact blocker diagnostic skipped\n",
			wantError:   "late activation capture evidence only after revision classification",
		},
		{
			name:        "CRD late activation leaks raw Helm stderr",
			child:       "crd-upgrade",
			old:         "\tlate_activation_expected_failure=false\n",
			replacement: "\tcat \"$WORK_DIR/late-activation-failure.err\" >&2\n\tlate_activation_expected_failure=false\n",
			wantError:   "late activation raw Helm evidence must remain write-only",
		},
		{
			name:        "CRD late activation leaks raw helper errors",
			child:       "crd-upgrade",
			old:         "\tlate_activation_expected_failure=false\n",
			replacement: "\tcat \"$LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE\" >&2\n\tlate_activation_expected_failure=false\n",
			wantError:   "late activation helper errors must not be emitted as evidence",
		},
		{
			name:  "CRD late activation requires captures before revision query",
			child: "crd-upgrade",
			old: "\tif ! helm_e2e status \"$E2E_HELM_RELEASE\" --namespace \"$E2E_OPERATOR_NAMESPACE\" \\\n" +
				"\t\t--revision \"$late_revision\" -o json >\"$late_status_file\" 2>/dev/null; then\n",
			replacement: "\tif [ \"$late_activation_captures_succeeded\" != true ]; then\n" +
				"\t\tfail \"capture failed too early\"\n" +
				"\tfi\n" +
				"\tif ! helm_e2e status \"$E2E_HELM_RELEASE\" --namespace \"$E2E_OPERATOR_NAMESPACE\" \\\n" +
				"\t\t--revision \"$late_revision\" -o json >\"$late_status_file\" 2>/dev/null; then\n",
			wantError: "revision classification must precede capture-success enforcement",
		},
		{
			name:        "CRD late failure skips the activation marker check",
			child:       "crd-upgrade",
			old:         `fail "late failure advanced the release activation marker"`,
			replacement: `true # activation marker check removed`,
			wantError:   "late activation marker remains uncommitted",
		},
		{
			name:        "CRD late failure skips predecessor Deployment restore",
			child:       "crd-upgrade",
			old:         `restore_runtime_deployment_snapshot "$CONTROLLER_DEPLOYMENT" "$controller_snapshot"`,
			replacement: `true # predecessor restore removed`,
			wantError:   "predecessor Deployment restore",
		},
		{
			name:        "CRD predecessor upgrade skips late-failure recovery",
			child:       "crd-upgrade",
			old:         "\tprove_late_activation_failure_recovery\n",
			replacement: "\ttrue # late-failure recovery removed\n",
			wantError:   "predecessor late activation recovery call",
		},
		{
			name:        "CRD predecessor upgrade skips legacy Job bootstrap proof",
			child:       "crd-upgrade",
			old:         "\tprove_legacy_job_activation_boundary bootstrap\n",
			replacement: "\ttrue # legacy Job bootstrap proof removed\n",
			wantError:   "legacy Job bootstrap boundary call",
		},
		{
			name:        "CRD controller proof skips legacy Job active boundary",
			child:       "crd-upgrade",
			old:         "\tprove_legacy_job_activation_boundary active\n",
			replacement: "\ttrue # legacy Job active proof removed\n",
			wantError:   "legacy Job active boundary call",
		},
		{
			name:        "CRD predecessor fixture completion polling rejects missing conditions",
			child:       "crd-upgrade",
			old:         `(.status.conditions // []) | any(.type == "Complete" and .status == "True")`,
			replacement: `.status.conditions | any(.type == "Complete" and .status == "True")`,
			wantError:   "predecessor fixture Job nil-safe completion polling",
		},
		{
			name:        "CRD predecessor fixture failure polling rejects missing conditions",
			child:       "crd-upgrade",
			old:         `(.status.conditions // []) | any(.type == "Failed" and .status == "True")`,
			replacement: `.status.conditions | any(.type == "Failed" and .status == "True")`,
			wantError:   "predecessor fixture Job nil-safe failure polling",
		},
		{
			name:        "CRD predecessor Apply diagnostic broadens guard ownership",
			child:       "crd-upgrade",
			old:         `.metadata.annotations["operator.ptah.dev/release-namespace"] == $namespace`,
			replacement: `true`,
			wantError:   "predecessor Apply diagnostic exact guard ownership",
		},
		{
			name:  "CRD predecessor Apply diagnostic drops the credential scan",
			child: "crd-upgrade",
			old: "if grep -F -f \"$IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE\" \"$diagnostic_file\" >/dev/null; then\n" +
				"\t\tfail \"predecessor Apply diagnostic contained a protected task credential\"\n" +
				"\telse\n" +
				"\t\tdiagnostic_scan_status=$?\n" +
				"\t\t[ \"$diagnostic_scan_status\" -eq 1 ] || fail \"predecessor Apply credential scan failed closed\"\n" +
				"\tfi",
			replacement: `: # credential scan removed`,
			wantError:   "predecessor Apply diagnostic credential scan",
		},
		{
			name:        "CRD predecessor Apply failure waits through outcome unknown",
			child:       "crd-upgrade",
			old:         `.status.pendingObservation.outcome == "OutcomeUnknown" or`,
			replacement: `false or`,
			wantError:   "predecessor Apply terminal failure fast path",
		},
		{
			name:        "CRD predecessor terminal fixture bypasses Job controller",
			child:       "crd-upgrade",
			old:         `type: "FailureTarget", status: "True",`,
			replacement: `type: "Failed", status: "True",`,
			wantError:   "predecessor read-only Job controller-owned failure staging",
		},
		{
			name:        "CRD predecessor terminal fixture alters active status",
			child:       "crd-upgrade",
			old:         "status: {\n\t    conditions: [{",
			replacement: "status: {\n\t    active: 0,\n\t    conditions: [{",
			wantError:   "predecessor read-only Job controller-owned failure staging",
		},
		{
			name:        "CRD predecessor terminal fixture loses native wait",
			child:       "crd-upgrade",
			old:         `fail "Job controller did not retire the predecessor read-only Job after FailureTarget staging"`,
			replacement: `true # native terminal wait removed`,
			wantError:   "predecessor read-only Job native terminal wait",
		},
		{
			name:        "CRD predecessor terminal fixture accepts partial invariant",
			child:       "crd-upgrade",
			old:         "predecessor_job_terminal=1\n\t\t\tbreak",
			replacement: "break",
			wantError:   "predecessor read-only Job full terminal invariant latch",
		},
		{
			name:        "CRD predecessor terminal fixture ignores active Pod accounting",
			child:       "crd-upgrade",
			old:         `((.status.active // 0) == 0) and`,
			replacement: `true and`,
			wantError:   "predecessor read-only Job complete native terminal predicate",
		},
		{
			name:  "CRD predecessor Pod webhook failure policy is not restored",
			child: "crd-upgrade",
			old: "set_predecessor_pod_webhook_failure_policy Fail Ignore\n" +
				"\tstage_predecessor_read_only_job_completion\n" +
				"\tset_predecessor_pod_webhook_failure_policy Ignore Fail",
			replacement: "set_predecessor_pod_webhook_failure_policy Fail Ignore\n" +
				"\tstage_predecessor_read_only_job_completion",
			wantError: "predecessor read-only Job bounded webhook outage bridge",
		},
		{
			name:  "CRD runtime Deployment deletion loses its timeout",
			child: "crd-upgrade",
			old: `"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" \
		--cascade=foreground --wait=true --timeout=2m >/dev/null`,
			replacement: `"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" \
		--cascade=foreground --wait=true >/dev/null`,
			wantError: "bounded runtime Deployment deletion",
		},
		{
			name:  "CRD controller Deployment deletion loses its timeout",
			child: "crd-upgrade",
			old: `kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_DEPLOYMENT" \
		--cascade=foreground --wait=true --timeout=2m >/dev/null`,
			replacement: `kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_DEPLOYMENT" \
		--cascade=foreground --wait=true >/dev/null`,
			wantError: "bounded controller Deployment deletion",
		},
		{
			name:        "CRD proof call removed",
			child:       "crd-upgrade",
			old:         "prove_runtime_singleton_guard\n",
			replacement: "true # singleton proof removed\n",
			wantError:   "runtime singleton proof call",
		},
		{
			name:        "CRD predecessor Job cleanup proof removed",
			child:       "crd-upgrade",
			old:         "wait_for_predecessor_read_only_job_cleanup\n",
			replacement: "true # predecessor Job cleanup proof removed\n",
			wantError:   "predecessor read-only Job cleanup proof",
		},
		{
			name:        "CRD predecessor Apply cleanup proof removed",
			child:       "crd-upgrade",
			old:         "wait_for_predecessor_apply_job_cleanup\n",
			replacement: "true # predecessor Apply cleanup proof removed\n",
			wantError:   "predecessor Apply cleanup proof",
		},
		{
			name:        "CRD live predecessor Apply overlap proof removed",
			child:       "crd-upgrade",
			old:         "assert_predecessor_apply_remains_exclusive_while_running\n",
			replacement: "true # predecessor Apply overlap proof removed\n",
			wantError:   "predecessor Apply upgrade overlap proof",
		},
		{
			name:        "CRD controller guarded-field proof removed",
			child:       "crd-upgrade",
			old:         "prove_controller_object_supported_window_guard\n",
			replacement: "true # controller guarded-field proof removed\n",
			wantError:   "controller guarded-field proof call",
		},
		{
			name:        "CRD upgrade proof returns immediately",
			child:       "crd-upgrade",
			old:         "run_upgrade_proof() {\n",
			replacement: "run_upgrade_proof() {\n\treturn 0\n",
			wantError:   "unconditional successful return",
		},
		{
			name:        "CRD proof call hidden in false branch",
			child:       "crd-upgrade",
			old:         "prove_runtime_singleton_guard\n",
			replacement: "if false; then\n\tprove_runtime_singleton_guard\nfi\n",
			wantError:   "always-false wrapper",
		},
		{
			name:        "CRD phase call removed",
			child:       "crd-upgrade",
			old:         "upgrade) run_upgrade_proof ;;",
			replacement: "upgrade) true ;;",
			wantError:   "phase dispatch",
		},
		{
			name:        "CRD terminal evidence removed",
			child:       "crd-upgrade",
			old:         `printf 'e2e crd: PASS phase=%s\n' "$E2E_PHASE"`,
			replacement: `printf 'e2e crd: phase=%s finished\n' "$E2E_PHASE"`,
			wantError:   "terminal CRD lifecycle evidence",
		},
		{
			name:        "fault interpreter bypass",
			child:       "faults",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "fault trap discards failure",
			child:       "faults",
			old:         "trap cleanup EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "fault proof call removed",
			child:       "faults",
			old:         "start_watches\n",
			replacement: "true # watch proof removed\n",
			wantError:   "resourceVersion watch proof call",
		},
		{
			name:        "fault principal proof returns immediately",
			child:       "faults",
			old:         "run_credential_principal_refusal() {\n",
			replacement: "run_credential_principal_refusal() {\n\treturn 0\n",
			wantError:   "unconditional successful return",
		},
		{
			name:        "fault proof call hidden in false branch",
			child:       "faults",
			old:         "start_watches\n",
			replacement: "if false; then\n\tstart_watches\nfi\n",
			wantError:   "always-false wrapper",
		},
		{
			name:        "fault terminal evidence removed",
			child:       "faults",
			old:         `printf '%s\n' 'e2e faults: PASS watches, Kubernetes deadline recovery, stale-plan preflight, native lock barriers, restart identity, uncertain recovery, deletion, Pod serialization, credential audit, and coordination realms'`,
			replacement: `printf '%s\n' 'e2e faults finished'`,
			wantError:   "terminal fault lifecycle evidence",
		},
		{
			name:        "HA interpreter bypass",
			child:       "high-availability",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "HA trap discards failure",
			child:       "high-availability",
			old:         "trap cleanup EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "HA proof call removed",
			child:       "high-availability",
			old:         `initial_holder=$(wait_for_leader "")`,
			replacement: `initial_holder=omitted`,
			wantError:   "initial leader proof",
		},
		{
			name:        "HA proof call hidden in false branch",
			child:       "high-availability",
			old:         `initial_holder=$(wait_for_leader "")`,
			replacement: "if false; then\n\tinitial_holder=$(wait_for_leader \"\")\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "HA terminal evidence removed",
			child:       "high-availability",
			old:         `printf '%s\n' 'e2e HA: PASS one Lease, exact RBAC, Pod failover, and admitted post-failover operation'`,
			replacement: `printf '%s\n' 'e2e HA finished'`,
			wantError:   "terminal high-availability lifecycle evidence",
		},
		{
			name:        "certificate interpreter bypass",
			child:       "certificate-rotation",
			old:         "#!/bin/sh\n",
			replacement: "#!/bin/true\n",
			wantError:   "must execute with #!/bin/sh",
		},
		{
			name:        "certificate trap discards failure",
			child:       "certificate-rotation",
			old:         "trap cleanup_upgrade_files EXIT\n",
			replacement: "trap 'exit 0' EXIT\n",
			wantError:   "failure-preserving trap",
		},
		{
			name:        "certificate proof call removed",
			child:       "certificate-rotation",
			old:         `assert_approval_admission_callable "before the Helm upgrade"`,
			replacement: `true # pre-upgrade admission proof removed`,
			wantError:   "pre-upgrade admission proof call",
		},
		{
			name:  "certificate proof call hidden in false branch",
			child: "certificate-rotation",
			old:   `assert_approval_admission_callable "before the Helm upgrade"`,
			replacement: "if false; then\n\t" +
				"assert_approval_admission_callable \"before the Helm upgrade\"\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "certificate terminal evidence removed",
			child:       "certificate-rotation",
			old:         `printf '%s\n' 'e2e certificate rotation: PASS live Helm lookup, corrupt-CA recovery, and exact guarded recreation'`,
			replacement: `printf '%s\n' 'e2e certificate rotation finished'`,
			wantError:   "terminal certificate lifecycle evidence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := repositoryE2EWiringFiles()
			path := e2eChildPath(files, test.child)
			source := readE2ESource(t, path)
			mutated := writeMutatedE2ESource(t, filepath.Base(path), source, test.old, test.replacement)
			setE2EChildPath(&files, test.child, mutated)
			err := verifyE2EWiring(files)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func repositoryE2EWiringFiles() e2eWiringFiles {
	return e2eWiringFiles{
		makefile:                   filepath.Join("..", makefilePath),
		harness:                    filepath.Join("..", e2eHarnessPath),
		staticChecks:               filepath.Join("..", e2eStaticPath),
		dataPlane:                  filepath.Join("..", e2eDataPlanePath),
		assertions:                 filepath.Join("..", e2eAssertPath),
		crdUpgrade:                 filepath.Join("..", e2eCRDUpgradePath),
		faults:                     filepath.Join("..", e2eFaultsPath),
		highAvailability:           filepath.Join("..", e2eHAPath),
		certRotation:               filepath.Join("..", e2eCertRotationPath),
		failedHookEvidence:         filepath.Join("..", failedHookEvidencePath),
		failedHookEvidenceSelftest: filepath.Join("..", failedHookEvidenceSelftestPath),
		admissionSchemaContract:    filepath.Join("..", admissionSchemaContractPath),
		admissionSchemaSelftest:    filepath.Join("..", admissionSchemaSelftestPath),
		controllerSchemaContract:   filepath.Join("..", controllerSchemaContractPath),
		controllerSchemaSelftest:   filepath.Join("..", controllerSchemaSelftestPath),
	}
}

func e2eChildPath(files e2eWiringFiles, child string) string {
	switch child {
	case "assertions":
		return files.assertions
	case "crd-upgrade":
		return files.crdUpgrade
	case "faults":
		return files.faults
	case "high-availability":
		return files.highAvailability
	case "certificate-rotation":
		return files.certRotation
	default:
		panic("unknown E2E child fixture: " + child)
	}
}

func setE2EChildPath(files *e2eWiringFiles, child, path string) {
	switch child {
	case "assertions":
		files.assertions = path
	case "crd-upgrade":
		files.crdUpgrade = path
	case "faults":
		files.faults = path
	case "high-availability":
		files.highAvailability = path
	case "certificate-rotation":
		files.certRotation = path
	default:
		panic("unknown E2E child fixture: " + child)
	}
}

func readE2ESource(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func writeMutatedE2ESource(t *testing.T, name, source, old, replacement string) string {
	t.Helper()
	if count := strings.Count(source, old); count != 1 {
		t.Fatalf("source fixture contains %d instances of %q, want 1", count, old)
	}
	path := filepath.Join(t.TempDir(), name)
	mutated := strings.Replace(source, old, replacement, 1)
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
