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
	"fmt"
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
	if err := verifyUpgradeHookProgressProofSource(repositoryE2EWiringFiles().crdUpgrade); err != nil {
		t.Fatalf("verifyUpgradeHookProgressProofSource() error = %v", err)
	}
}

func TestAPIServerEndpointInventoryFilterFixtures(t *testing.T) {
	t.Parallel()

	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to exercise the API server endpoint inventory filter")
	}
	port := func() map[string]any {
		return map[string]any{"name": "https", "protocol": "TCP", "port": 6443}
	}
	endpoint := func(address string) map[string]any {
		return map[string]any{
			"conditions": map[string]any{"ready": true, "serving": true, "terminating": false},
			"addresses":  []any{address},
		}
	}
	slice := func(address string, ports ...map[string]any) map[string]any {
		portValues := make([]any, 0, len(ports))
		for _, value := range ports {
			portValues = append(portValues, value)
		}
		return map[string]any{
			"addressType": "IPv4",
			"metadata": map[string]any{
				"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
			},
			"ports":     portValues,
			"endpoints": []any{endpoint(address)},
		}
	}
	node := func(name, role, address string) any {
		labels := map[string]any{}
		if role == "control-plane" {
			labels["node-role.kubernetes.io/control-plane"] = ""
		}
		return map[string]any{
			"metadata": map[string]any{"name": name, "labels": labels},
			"status": map[string]any{"addresses": []any{
				map[string]any{"type": "InternalIP", "address": address},
				map[string]any{"type": "Hostname", "address": name},
			}},
		}
	}
	nodeInventory := map[string]any{"items": []any{
		node("test-control-plane", "control-plane", "10.0.0.1"),
		node("test-control-plane2", "control-plane", "10.0.0.2"),
		node("test-control-plane3", "control-plane", "10.0.0.3"),
		node("test-worker", "worker", "10.0.0.4"),
	}}
	nodeBytes, err := json.Marshal(nodeInventory)
	if err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(nodePath, nodeBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		items []any
		want  bool
	}{
		{
			name: "one slice advertises three ready endpoints",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{port()},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.3"),
				},
			}},
			want: true,
		},
		{
			name: "arbitrary unbound addresses are rejected",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{port()},
				"endpoints": []any{
					endpoint("203.0.113.1"),
					endpoint("203.0.113.2"),
					endpoint("203.0.113.3"),
				},
			}},
		},
		{
			name: "worker InternalIP substitution is rejected",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{port()},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.4"),
				},
			}},
		},
		{
			name: "aggregate port count cannot mask per-slice distribution",
			items: []any{
				slice("10.0.0.1", port(), port()),
				slice("10.0.0.2", port()),
				slice("10.0.0.3"),
			},
		},
		{
			name: "invalid duplicate named port is rejected",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{
					port(),
					map[string]any{"name": "https", "protocol": "UDP", "port": 6443},
				},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.3"),
				},
			}},
		},
		{
			name: "extra non-https port is rejected",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{
					port(),
					map[string]any{"name": "metrics", "protocol": "TCP", "port": 10257},
				},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.3"),
				},
			}},
		},
		{
			name: "wrong API server port is rejected",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{
					map[string]any{"name": "https", "protocol": "TCP", "port": 443},
				},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.3"),
				},
			}},
		},
		{
			name: "terminating endpoint is not hidden from exact cardinality",
			items: []any{map[string]any{
				"addressType": "IPv4",
				"metadata": map[string]any{
					"labels": map[string]any{"kubernetes.io/service-name": "kubernetes"},
				},
				"ports": []any{port()},
				"endpoints": []any{
					endpoint("10.0.0.1"),
					endpoint("10.0.0.2"),
					endpoint("10.0.0.3"),
					map[string]any{
						"conditions": map[string]any{"ready": false, "serving": false, "terminating": true},
						"addresses":  []any{"10.0.0.4"},
					},
				},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture, marshalErr := json.Marshal(map[string]any{"items": test.items})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			command := exec.Command(
				jqPath,
				"-e",
				"--arg", "cluster", "test",
				"--slurpfile", "nodes", nodePath,
				"-f", repositoryE2EWiringFiles().apiServerEndpointFilter,
			)
			command.Stdin = strings.NewReader(string(fixture))
			output, runErr := command.CombinedOutput()
			if got := runErr == nil; got != test.want {
				t.Fatalf("API server endpoint inventory result = %t, want %t; jq output = %q", got, test.want, output)
			}
		})
	}
}

func TestVerifyAPIServerEndpointInventoryFilterRejectsMutation(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		old         string
		replacement string
	}{
		{
			name:        "per-slice port contract",
			old:         "(.ports | length) == 1",
			replacement: "(.ports | length) >= 0",
		},
		{
			name:        "control-plane InternalIP binding",
			old:         "($addresses | sort) == ($control_plane_addresses | sort)",
			replacement: "($addresses | length) == 3",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := repositoryE2EWiringFiles()
			source := readE2ESource(t, files.apiServerEndpointFilter)
			files.apiServerEndpointFilter = writeMutatedE2ESource(
				t,
				"api-server-endpoint-inventory.jq",
				source,
				test.old,
				test.replacement,
			)
			if err := verifyE2EWiring(files); err == nil || !strings.Contains(err.Error(), "API server endpoint inventory filter") {
				t.Fatalf("verifyE2EWiring() error = %v, want exact endpoint filter contract rejection", err)
			}
		})
	}
}

func TestAPIServerFeatureGateScopeFilterRejectsUnreadyStaticPods(t *testing.T) {
	t.Parallel()

	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to exercise the API server component readiness filter")
	}
	const cluster = "test-ha"
	controlPlaneNodes := []string{
		cluster + "-control-plane",
		cluster + "-control-plane2",
		cluster + "-control-plane3",
	}
	componentPod := func(component, node string) map[string]any {
		command := []any{"/usr/local/bin/" + component}
		if component == "kube-apiserver" {
			command = append(command, "--feature-gates=GenericWorkload=true", "--runtime-config=api/all=true")
		}
		return map[string]any{
			"metadata": map[string]any{
				"name":        component + "-" + node,
				"labels":      map[string]any{"component": component},
				"annotations": map[string]any{"kubernetes.io/config.mirror": "static-pod-hash"},
			},
			"spec": map[string]any{
				"nodeName": node,
				"containers": []any{map[string]any{
					"name": component, "command": command,
				}},
			},
			"status": map[string]any{
				"phase": "Running",
				"conditions": []any{map[string]any{
					"type": "Ready", "status": "True",
				}},
				"containerStatuses": []any{map[string]any{
					"name":  component,
					"ready": true,
					"state": map[string]any{"running": map[string]any{
						"startedAt": "2026-09-05T00:00:00Z",
					}},
				}},
			},
		}
	}
	fixture := func() []any {
		items := make([]any, 0, 9)
		for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
			for _, node := range controlPlaneNodes {
				items = append(items, componentPod(component, node))
			}
		}
		return items
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "all exact static Pods are running and ready", want: true},
		{
			name: "non-static API server Pod",
			mutate: func(pod map[string]any) {
				delete(pod["metadata"].(map[string]any), "annotations")
			},
		},
		{
			name: "misnamed API server Pod",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["name"] = "replacement-api-server"
			},
		},
		{
			name: "deleting API server Pod",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-05T00:01:00Z"
			},
		},
		{
			name: "non-running API server Pod",
			mutate: func(pod map[string]any) {
				pod["status"].(map[string]any)["phase"] = "Failed"
			},
		},
		{
			name: "API server Pod Ready is false",
			mutate: func(pod map[string]any) {
				conditions := pod["status"].(map[string]any)["conditions"].([]any)
				conditions[0].(map[string]any)["status"] = "False"
			},
		},
		{
			name: "API server container is not ready",
			mutate: func(pod map[string]any) {
				statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
				statuses[0].(map[string]any)["ready"] = false
			},
		},
		{
			name: "API server container is not running",
			mutate: func(pod map[string]any) {
				statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
				statuses[0].(map[string]any)["state"] = map[string]any{"terminated": map[string]any{"exitCode": 1}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			items := fixture()
			if test.mutate != nil {
				test.mutate(items[0].(map[string]any))
			}
			encoded, marshalErr := json.Marshal(map[string]any{"items": items})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			command := exec.Command(
				jqPath,
				"-e",
				"--arg", "expected", "GenericWorkload=true",
				"--arg", "cluster", cluster,
				apiServerFeatureGateScopeFilter(t),
			)
			command.Stdin = strings.NewReader(string(encoded))
			output, runErr := command.CombinedOutput()
			if got := runErr == nil; got != test.want {
				t.Fatalf("API server component readiness result = %t, want %t; jq output = %q", got, test.want, output)
			}
		})
	}
}

func TestVerifyKindHAConfig(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().kindConfig)
	if err := verifyKindHAConfig(repositoryE2EWiringFiles().kindConfig); err != nil {
		t.Fatalf("verifyKindHAConfig() error = %v", err)
	}
	workerBlock := `  - role: worker
    kubeadmConfigPatches:
      - |
        kind: KubeletConfiguration
        apiVersion: kubelet.config.k8s.io/v1beta1
        featureGates:
          KubeletInUserNamespace: true`
	for _, test := range []struct {
		name        string
		old         string
		replacement string
		want        string
	}{
		{
			name:        "worker omitted",
			old:         workerBlock,
			replacement: "",
			want:        "want exactly four",
		},
		{
			name:        "worker promoted",
			old:         "  - role: worker",
			replacement: "  - role: control-plane",
			want:        "role is",
		},
		{
			name:        "worker kubelet contract omitted",
			old:         workerBlock,
			replacement: "  - role: worker",
			want:        "exact kubelet feature-gate patch",
		},
		{
			name:        "API server exposed",
			old:         `  apiServerAddress: "127.0.0.1"`,
			replacement: `  apiServerAddress: "0.0.0.0"`,
			want:        "networking contract",
		},
		{
			name:        "unknown top-level field",
			old:         "networking:\n",
			replacement: "unexpected: true\nnetworking:\n",
			want:        "field unexpected not found",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedE2ESource(t, "kind.yaml.tmpl", source, test.old, test.replacement)
			err := verifyKindHAConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyKindHAConfig() error = %v, want substring %q", err, test.want)
			}
		})
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
		"--arg", "preflight_phase", "captured",
		"--arg", "reconcile_phase", "watching",
		"--arg", "preflight_failure_class", "job-watch",
		"--arg", "reconcile_failure_class", "log-start",
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
	if got := summary["preflightFailureClass"]; got != "job-watch" {
		t.Fatalf("preflightFailureClass = %v, want job-watch", got)
	}
	if got := summary["preflightCapturePhase"]; got != "captured" {
		t.Fatalf("preflightCapturePhase = %v, want captured", got)
	}
	if got := summary["reconcileCapturePhase"]; got != "watching" {
		t.Fatalf("reconcileCapturePhase = %v, want watching", got)
	}
	if got := summary["reconcileFailureClass"]; got != "log-start" {
		t.Fatalf("reconcileFailureClass = %v, want log-start", got)
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
		"--arg", "preflight_phase", "captured",
		"--arg", "reconcile_phase", "watching",
		"--arg", "preflight_failure_class", "job-watch",
		"--arg", "reconcile_failure_class", "log-start",
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

func TestLateActivationFailureClassSummaryAcceptsOnlyBoundedAllowlistedToken(t *testing.T) {
	t.Parallel()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is required to exercise the late-activation failure class summary")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	function := extractE2EShellFunction(t, source, "late_activation_failure_class_summary")
	directory := t.TempDir()
	scriptPath := filepath.Join(directory, "failure-class.sh")
	if err := os.WriteFile(scriptPath, []byte("set -eu\n"+function+"\nlate_activation_failure_class_summary \"$1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	valid := []string{
		"configuration", "output", "render", "kubernetes-client",
		"priority-inventory", "priority-watch", "job-inventory", "job-watch", "job-contract",
		"pod-inventory", "pod-watch", "pod-contract", "pod-owner", "log-start", "log-start-timeout",
		"log-read", "log-empty", "log-too-large", "deadline", "canceled", "internal",
	}
	for _, token := range valid {
		token := token
		t.Run("valid "+token, func(t *testing.T) {
			path := filepath.Join(directory, "valid-"+token)
			if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output, runErr := exec.Command(shPath, scriptPath, path).CombinedOutput()
			if runErr != nil || string(output) != token+"\n" {
				t.Fatalf("summary = %q, error = %v", output, runErr)
			}
		})
	}

	missingPath := filepath.Join(directory, "missing")
	output, err := exec.Command(shPath, scriptPath, missingPath).CombinedOutput()
	if err != nil || string(output) != "unavailable\n" {
		t.Fatalf("missing summary = %q, error = %v", output, err)
	}

	credentialMarker := "Authorization: Bearer credential-shaped-private-cause"
	invalid := []struct {
		name     string
		contents string
		mode     os.FileMode
		want     string
	}{
		{name: "empty", mode: 0o600, want: "unavailable\n"},
		{name: "unknown", contents: credentialMarker + "\n", mode: 0o600, want: "invalid\n"},
		{name: "multiline", contents: "job-watch\n" + credentialMarker + "\n", mode: 0o600, want: "invalid\n"},
		{name: "oversized", contents: strings.Repeat("x", 33), mode: 0o600, want: "invalid\n"},
		{name: "wrong mode", contents: "job-watch\n", mode: 0o644, want: "invalid\n"},
	}
	for _, test := range invalid {
		test := test
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, "invalid-"+strings.ReplaceAll(test.name, " ", "-"))
			if err := os.WriteFile(path, []byte(test.contents), test.mode); err != nil {
				t.Fatal(err)
			}
			output, runErr := exec.Command(shPath, scriptPath, path).CombinedOutput()
			if runErr != nil || string(output) != test.want {
				t.Fatalf("summary = %q, want %q, error = %v", output, test.want, runErr)
			}
			if strings.Contains(string(output), credentialMarker) {
				t.Fatalf("summary leaked private cause: %q", output)
			}
		})
	}

	targetPath := filepath.Join(directory, "symlink-target")
	linkPath := filepath.Join(directory, "failure-class-link")
	if err := os.WriteFile(targetPath, []byte("job-watch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatal(err)
	}
	output, err = exec.Command(shPath, scriptPath, linkPath).CombinedOutput()
	if err != nil || string(output) != "invalid\n" {
		t.Fatalf("symlink summary = %q, error = %v", output, err)
	}
}

func TestLateActivationReconcileDiagnosticEmitsBeforeMarkerFailure(t *testing.T) {
	t.Parallel()

	const diagnostic = "ptah-crd-manager: unexpected safe reconcile failure\n"
	output, err := runLateActivationReconcileDiagnostic(t, diagnostic)
	if err == nil {
		t.Fatal("emit_late_activation_reconcile_diagnostic() succeeded without blocker markers")
	}
	const wantFailure = "e2e crd: late activation reconcile log lacks exact blocker evidence: activation-phase blocker-webhook missing-service\n"
	if got, want := string(output), diagnostic+wantFailure; got != want {
		t.Fatalf("diagnostic output = %q, want %q", got, want)
	}
}

func TestLateActivationReconcileDiagnosticWithholdsUnsafeLog(t *testing.T) {
	t.Parallel()

	const protectedMarker = "DO_NOT_EMIT_CREDENTIAL"
	output, err := runLateActivationReconcileDiagnostic(t, "ptah-crd-manager: "+protectedMarker+"\n")
	if err == nil {
		t.Fatal("emit_late_activation_reconcile_diagnostic() accepted protected content")
	}
	if strings.Contains(string(output), protectedMarker) {
		t.Fatalf("unsafe diagnostic escaped the credential scanner: %q", output)
	}
	const want = "e2e crd: late activation reconcile log failed credential and format validation\n"
	if got := string(output); got != want {
		t.Fatalf("unsafe diagnostic output = %q, want %q", got, want)
	}
}

func runLateActivationReconcileDiagnostic(t *testing.T, diagnostic string) ([]byte, error) {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is required to exercise the late-activation diagnostic")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	var script strings.Builder
	script.WriteString("set -eu\n")
	script.WriteString("LATE_ACTIVATION_RECONCILE_CAPTURE_STATUS_FILE=$1\n")
	script.WriteString("LATE_ACTIVATION_RECONCILE_LOG_FILE=$2\n")
	script.WriteString("IDENTITY_HOOK_CREDENTIAL_PATTERNS_FILE=$3\n")
	for _, functionName := range []string{
		"fail",
		"require_mode_0600_regular_file",
		"hook_diagnostic_is_safe",
		"emit_late_activation_reconcile_diagnostic",
	} {
		script.WriteString(extractE2EShellFunction(t, source, functionName))
		script.WriteByte('\n')
	}
	script.WriteString("emit_late_activation_reconcile_diagnostic\n")

	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "diagnostic.sh")
	statusPath := filepath.Join(tempDir, "capture.status")
	logPath := filepath.Join(tempDir, "capture.log")
	patternsPath := filepath.Join(tempDir, "credential-patterns")
	for path, contents := range map[string]string{
		scriptPath:   script.String(),
		statusPath:   "captured\n",
		logPath:      diagnostic,
		patternsPath: "DO_NOT_EMIT_CREDENTIAL\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(shPath, scriptPath, statusPath, logPath, patternsPath)
	return command.CombinedOutput()
}

func TestHAResolveOperationFailureCounterParser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		metrics    string
		wantOutput string
		wantError  bool
	}{
		{name: "absent", metrics: "# unrelated\n", wantOutput: "0\n"},
		{name: "zero", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 0\n", wantOutput: "0\n"},
		{name: "positive exponent", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 3e+02\n", wantOutput: "3e+02\n"},
		{name: "overflowing exponent", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 1e999\n", wantError: true},
		{name: "duplicate", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 1\nptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 2\n", wantError: true},
		{name: "negative", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} -1\n", wantError: true},
		{name: "non-finite", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} NaN\n", wantError: true},
		{name: "timestamped", metrics: "ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 1 123\n", wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			output, err := runHAResolveMetricParser(t, test.metrics)
			if test.wantError {
				if err == nil {
					t.Fatalf("parser accepted invalid metrics and returned %q", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("parser failed: %v: %s", err, output)
			}
			if got := string(output); got != test.wantOutput {
				t.Fatalf("parser output = %q, want %q", got, test.wantOutput)
			}
		})
	}
}

func TestHACustomMetricValidatorRejectsOverflowingExponent(t *testing.T) {
	t.Parallel()

	metrics := strings.Join([]string{
		"# HELP ptah_operator_reconciliations_total Total reconciliations.",
		"# TYPE ptah_operator_reconciliations_total counter",
		"ptah_operator_reconciliations_total{result=\"success\"} 2",
		"# HELP ptah_operator_failures_total Total failures.",
		"# TYPE ptah_operator_failures_total counter",
		"ptah_operator_failures_total{category=\"operation\",stage=\"resolve\"} 1e999",
	}, "\n") + "\n"
	if output, err := runHACustomMetricValidator(t, metrics); err == nil {
		t.Fatalf("validator accepted an overflowing metric exponent and returned %q", output)
	}
}

func TestHACustomMetricsPollsUntilResolveFailureCounterIncreases(t *testing.T) {
	t.Parallel()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is required to exercise the HA metric delta proof")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().highAvailability)
	var script strings.Builder
	script.WriteString(`set -eu
OPERATOR_NAMESPACE=ptah-system
METRICS_TIMEOUT_SECONDS=3
SCRAPE_COUNT_FILE=$1
leader_pod_name() { printf '%s\n' leader; }
fail() { printf 'failure: %s\n' "$*" >&2; exit 1; }
sleep() { :; }
emit_metrics() {
  printf '%s\n' '# HELP ptah_operator_reconciliations_total Total reconciliations.'
  printf '%s\n' '# TYPE ptah_operator_reconciliations_total counter'
  printf '%s\n' 'ptah_operator_reconciliations_total{result="success"} 2'
  printf '%s\n' '# HELP ptah_operator_failures_total Total failures.'
  printf '%s\n' '# TYPE ptah_operator_failures_total counter'
  printf 'ptah_operator_failures_total{category="operation",stage="resolve"} %s\n' "$1"
}
k() {
  scrape_count=$(sed -n '1p' "$SCRAPE_COUNT_FILE")
  scrape_count=$((scrape_count + 1))
  printf '%s\n' "$scrape_count" >"$SCRAPE_COUNT_FILE"
  if [ "$scrape_count" -eq 1 ]; then
    emit_metrics 3
  else
    emit_metrics 4
  fi
}
`)
	for _, functionName := range []string{
		"validate_custom_operator_metrics",
		"resolve_operation_failure_counter_from_metrics",
		"assert_custom_operator_metrics",
	} {
		script.WriteString(extractE2EShellFunction(t, source, functionName))
		script.WriteByte('\n')
	}
	script.WriteString("assert_custom_operator_metrics holder 3\n")
	script.WriteString("cat \"$SCRAPE_COUNT_FILE\"\n")

	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "ha-metric-delta.sh")
	countPath := filepath.Join(tempDir, "scrape-count")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(countPath, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(shPath, scriptPath, countPath).CombinedOutput()
	if err != nil {
		t.Fatalf("HA metric delta proof failed: %v: %s", err, output)
	}
	if got, want := string(output), "2\n"; got != want {
		t.Fatalf("scrape count = %q, want %q", got, want)
	}
}

func runHAResolveMetricParser(t *testing.T, metrics string) ([]byte, error) {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is required to exercise the HA metric parser")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().highAvailability)
	script := "set -eu\n" +
		extractE2EShellFunction(t, source, "resolve_operation_failure_counter_from_metrics") +
		"\nresolve_operation_failure_counter_from_metrics\n"
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "ha-metric-parser.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shPath, scriptPath)
	command.Stdin = strings.NewReader(metrics)
	return command.CombinedOutput()
}

func runHACustomMetricValidator(t *testing.T, metrics string) ([]byte, error) {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is required to exercise the HA metric validator")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().highAvailability)
	script := "set -eu\n" +
		extractE2EShellFunction(t, source, "validate_custom_operator_metrics") +
		"\nvalidate_custom_operator_metrics\n"
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "ha-metric-validator.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shPath, scriptPath)
	command.Stdin = strings.NewReader(metrics)
	return command.CombinedOutput()
}

func TestSyntheticNextReleaseLineReplacementIsCountCheckedAndPortable(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().harness)
	script := "set -eu\n" +
		extractE2EShellFunction(t, source, "fail") + "\n" +
		extractE2EShellFunction(t, source, "replace_exact_line_once") + "\n" +
		`replace_exact_line_once "$1" 'release-sequence=1' 'release-sequence=2' 'portable sequence replacement'` + "\n"

	for _, shellName := range []string{"sh", "dash"} {
		shellName := shellName
		t.Run(shellName, func(t *testing.T) {
			shellPath, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is required to exercise the portable replacement helper", shellName)
			}
			directory := t.TempDir()
			scriptPath := filepath.Join(directory, "replace-line.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}

			tests := []struct {
				name      string
				contents  string
				want      string
				wantError string
				symlink   bool
			}{
				{
					name:     "one exact source line",
					contents: "before\nrelease-sequence=1\nafter\n",
					want:     "before\nrelease-sequence=2\nafter\n",
				},
				{
					name:      "source line absent",
					contents:  "release-sequence=0\n",
					wantError: "source line count is 0, expected exactly one",
				},
				{
					name:      "source line repeated",
					contents:  "release-sequence=1\nrelease-sequence=1\n",
					wantError: "source line count is 2, expected exactly one",
				},
				{
					name:      "symlink target",
					contents:  "release-sequence=1\n",
					wantError: "target must be a regular non-symlink file",
					symlink:   true,
				},
			}
			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					caseDirectory := t.TempDir()
					targetPath := filepath.Join(caseDirectory, "sequence.txt")
					writePath := targetPath
					if test.symlink {
						writePath = filepath.Join(caseDirectory, "sequence-target.txt")
					}
					if err := os.WriteFile(writePath, []byte(test.contents), 0o600); err != nil {
						t.Fatal(err)
					}
					if test.symlink {
						if err := os.Symlink(writePath, targetPath); err != nil {
							t.Fatal(err)
						}
					}
					output, runErr := exec.Command(shellPath, scriptPath, targetPath).CombinedOutput()
					if test.wantError != "" {
						if runErr == nil {
							t.Fatalf("replacement unexpectedly succeeded with output %q", output)
						}
						if !strings.Contains(string(output), test.wantError) {
							t.Fatalf("replacement error = %q, want substring %q", output, test.wantError)
						}
						return
					}
					if runErr != nil {
						t.Fatalf("replacement failed: %v: %s", runErr, output)
					}
					got, err := os.ReadFile(targetPath)
					if err != nil {
						t.Fatal(err)
					}
					if string(got) != test.want {
						t.Fatalf("replacement result = %q, want %q", got, test.want)
					}
					if _, err := os.Stat(targetPath + ".e2e-next"); !os.IsNotExist(err) {
						t.Fatalf("replacement temporary file remains: %v", err)
					}
				})
			}
		})
	}
}

func TestCurrentReleaseSequenceDerivationAndTransitionArePortable(t *testing.T) {
	t.Parallel()

	harnessSource := readE2ESource(t, repositoryE2EWiringFiles().harness)
	crdSource := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	deriveScript := "set -eu\n" +
		extractE2EShellFunction(t, harnessSource, "go_release_sequence_from_source") + "\n" +
		extractE2EShellFunction(t, harnessSource, "helm_release_sequence_from_source") + "\n" +
		"go_sequence=$(go_release_sequence_from_source \"$1\")\n" +
		"helm_sequence=$(helm_release_sequence_from_source \"$2\")\n" +
		"[ \"$go_sequence\" = \"$helm_sequence\" ]\n" +
		"printf '%s\\n' \"$go_sequence\"\n"
	transitionScript := "set -eu\n" +
		"E2E_CURRENT_RELEASE_SEQUENCE=$1\n" +
		"E2E_NEXT_RELEASE_SEQUENCE=$2\n" +
		extractE2EShellFunction(t, crdSource, "fail") + "\n" +
		extractE2EShellFunction(t, crdSource, "validate_release_sequence_transition") + "\n" +
		"validate_release_sequence_transition\n"

	for _, shellName := range []string{"sh", "dash"} {
		shellName := shellName
		t.Run(shellName, func(t *testing.T) {
			shellPath, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is required to exercise release sequence derivation", shellName)
			}
			directory := t.TempDir()
			derivePath := filepath.Join(directory, "derive-sequence.sh")
			transitionPath := filepath.Join(directory, "validate-transition.sh")
			if err := os.WriteFile(derivePath, []byte(deriveScript), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(transitionPath, []byte(transitionScript), 0o600); err != nil {
				t.Fatal(err)
			}

			writeSources := func(t *testing.T, goSource, helmSource string) (string, string) {
				t.Helper()
				caseDirectory := t.TempDir()
				goPath := filepath.Join(caseDirectory, "rollout.go")
				helmPath := filepath.Join(caseDirectory, "_helpers.tpl")
				if err := os.WriteFile(goPath, []byte(goSource), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(helmPath, []byte(helmSource), 0o600); err != nil {
					t.Fatal(err)
				}
				return goPath, helmPath
			}
			validGo := "package crdupgrade\n\nconst (\n\tCurrentReleaseSequence int32 = 37\n)\n"
			validHelm := "{{- define \"ptah-operator.releaseSequence\" -}}37{{- end -}}\n"

			t.Run("repository Go and Helm sources", func(t *testing.T) {
				output, runErr := exec.Command(
					shellPath,
					derivePath,
					filepath.Join("..", "internal", "crdupgrade", "rollout.go"),
					filepath.Join("..", "charts", "ptah-operator", "templates", "_helpers.tpl"),
				).CombinedOutput()
				if runErr != nil {
					t.Fatalf("repository release sequence derivation failed: %v: %s", runErr, output)
				}
				if !regexp.MustCompile(`^[1-9][0-9]*\n$`).Match(output) {
					t.Fatalf("repository release sequence = %q, want one positive integer", output)
				}
			})

			t.Run("matching independently derived sequence", func(t *testing.T) {
				goPath, helmPath := writeSources(t, validGo, validHelm)
				output, runErr := exec.Command(shellPath, derivePath, goPath, helmPath).CombinedOutput()
				if runErr != nil {
					t.Fatalf("release sequence derivation failed: %v: %s", runErr, output)
				}
				if got, want := string(output), "37\n"; got != want {
					t.Fatalf("derived sequence = %q, want %q", got, want)
				}
			})

			for _, test := range []struct {
				name       string
				goSource   string
				helmSource string
			}{
				{
					name:       "Go and Helm differ",
					goSource:   validGo,
					helmSource: "{{- define \"ptah-operator.releaseSequence\" -}}38{{- end -}}\n",
				},
				{
					name:       "duplicate Go declaration",
					goSource:   validGo + "\tCurrentReleaseSequence int32 = 37\n",
					helmSource: validHelm,
				},
				{
					name:       "duplicate Helm helper",
					goSource:   validGo,
					helmSource: validHelm + "{{- define \"ptah-operator.releaseSequence\" -}}37{{- end -}}\n",
				},
				{
					name:       "zero Go sequence",
					goSource:   "package crdupgrade\nconst (\n\tCurrentReleaseSequence int32 = 0\n)\n",
					helmSource: validHelm,
				},
				{
					name:       "zero Helm sequence",
					goSource:   validGo,
					helmSource: "{{- define \"ptah-operator.releaseSequence\" -}}0{{- end -}}\n",
				},
			} {
				test := test
				t.Run(test.name, func(t *testing.T) {
					goPath, helmPath := writeSources(t, test.goSource, test.helmSource)
					if output, runErr := exec.Command(shellPath, derivePath, goPath, helmPath).CombinedOutput(); runErr == nil {
						t.Fatalf("invalid release sequence sources were accepted with %q", output)
					}
				})
			}

			for _, test := range []struct {
				name    string
				current string
				next    string
				wantOK  bool
			}{
				{name: "ordinary successor", current: "37", next: "38", wantOK: true},
				{name: "maximum valid successor", current: "2147483646", next: "2147483647", wantOK: true},
				{name: "skipped sequence", current: "37", next: "39"},
				{name: "same sequence", current: "37", next: "37"},
				{name: "current overflow", current: "2147483647", next: "2147483648"},
				{name: "leading zero", current: "01", next: "2"},
				{name: "non-numeric", current: "current", next: "next"},
			} {
				test := test
				t.Run(test.name, func(t *testing.T) {
					output, runErr := exec.Command(
						shellPath, transitionPath, test.current, test.next,
					).CombinedOutput()
					if test.wantOK && runErr != nil {
						t.Fatalf("valid release transition failed: %v: %s", runErr, output)
					}
					if !test.wantOK && runErr == nil {
						t.Fatalf("invalid release transition was accepted with %q", output)
					}
				})
			}
		})
	}
}

func TestProductionControllerImageUsesOnlyProductionDigest(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to exercise production controller image extraction")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	if strings.Contains(source, `.repository + "@" + .testIdentityDigest`) {
		t.Fatal("production controller identity uses the mutually exclusive test-only digest")
	}
	if !strings.Contains(source, `((.testIdentityDigest // "") == "")`) {
		t.Fatal("production controller identity does not reject a test-only digest")
	}
	script := "set -eu\n" +
		extractE2EShellFunction(t, source, "fail") + "\n" +
		extractE2EShellFunction(t, source, "production_controller_image_from_values") + "\n" +
		"production_controller_image_from_values \"$1\"\n"
	lowerDigest := "sha256:" + strings.Repeat("a", 64)
	upperDigest := "sha256:" + strings.Repeat("A", 64)
	testDigest := "sha256:" + strings.Repeat("b", 64)

	for _, shellName := range []string{"sh", "dash"} {
		shellName := shellName
		t.Run(shellName, func(t *testing.T) {
			shellPath, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is required to exercise production image extraction", shellName)
			}
			directory := t.TempDir()
			scriptPath := filepath.Join(directory, "production-image.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}

			tests := []struct {
				name      string
				image     map[string]any
				want      string
				wantError bool
			}{
				{
					name: "production digest",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator",
						"digest":          lowerDigest,
						"allowMutableTag": false,
					},
					want: "registry.example/ptah/operator@" + lowerDigest + "\n",
				},
				{
					name: "explicitly empty test-only digest",
					image: map[string]any{
						"repository":         "registry.example/ptah/operator",
						"digest":             lowerDigest,
						"testIdentityDigest": "",
						"allowMutableTag":    false,
					},
					want: "registry.example/ptah/operator@" + lowerDigest + "\n",
				},
				{
					name: "mutually exclusive test-only digest",
					image: map[string]any{
						"repository":         "registry.example/ptah/operator",
						"digest":             lowerDigest,
						"testIdentityDigest": testDigest,
						"allowMutableTag":    false,
					},
					wantError: true,
				},
				{
					name: "uppercase digest",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator",
						"digest":          upperDigest,
						"allowMutableTag": false,
					},
					wantError: true,
				},
				{
					name: "missing digest",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator",
						"allowMutableTag": false,
					},
					wantError: true,
				},
				{
					name: "mutable tag enabled",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator",
						"digest":          lowerDigest,
						"allowMutableTag": true,
					},
					wantError: true,
				},
				{
					name: "repository already has a digest",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator@" + testDigest,
						"digest":          lowerDigest,
						"allowMutableTag": false,
					},
					wantError: true,
				},
			}
			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					values, err := json.Marshal(map[string]any{"image": test.image})
					if err != nil {
						t.Fatal(err)
					}
					valuesPath := filepath.Join(t.TempDir(), "values.json")
					if err := os.WriteFile(valuesPath, values, 0o600); err != nil {
						t.Fatal(err)
					}
					output, runErr := exec.Command(shellPath, scriptPath, valuesPath).CombinedOutput()
					if test.wantError {
						if runErr == nil {
							t.Fatalf("production identity unexpectedly succeeded with %q", output)
						}
						return
					}
					if runErr != nil {
						t.Fatalf("production identity failed: %v: %s", runErr, output)
					}
					if got := string(output); got != test.want {
						t.Fatalf("production identity = %q, want %q", got, test.want)
					}
				})
			}
		})
	}
}

func TestReleaseChartExportIsExactAtomicAndSafe(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().harness)
	script := "set -eu\n" +
		"WORK_DIR=$1\n" +
		"CHART_PACKAGE=$2\n" +
		"E2E_RELEASE_CHART_OUTPUT=$3\n" +
		"CURRENT_RELEASE_SEQUENCE=7\n" +
		"CHART_PACKAGE_DIGEST=sha256-test-digest\n" +
		"RELEASE_CHART_OUTPUT_PARENT=\n" +
		"RELEASE_CHART_OUTPUT_TEMP=\n" +
		extractE2EShellFunction(t, source, "fail") + "\n" +
		extractE2EShellFunction(t, source, "export_release_chart") + "\n" +
		"export_release_chart\n"

	for _, shellName := range []string{"sh", "dash"} {
		shellName := shellName
		t.Run(shellName, func(t *testing.T) {
			shellPath, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is required to exercise release chart export", shellName)
			}
			directory := t.TempDir()
			scriptPath := filepath.Join(directory, "export-chart.sh")
			workDirectory := filepath.Join(directory, "work")
			outputDirectory := filepath.Join(directory, "output")
			if err := os.Mkdir(workDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(outputDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			chartPath := filepath.Join(workDirectory, "sequence-1.tgz")
			chartBytes := []byte("exact sequence-1 chart bytes\x00\x01\xff")
			if err := os.WriteFile(chartPath, chartBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}

			t.Run("exact adjacent atomic publication", func(t *testing.T) {
				targetPath := filepath.Join(outputDirectory, "release-chart.tgz")
				output, runErr := exec.Command(
					shellPath, scriptPath, workDirectory, chartPath, targetPath,
				).CombinedOutput()
				if runErr != nil {
					t.Fatalf("release chart export failed: %v: %s", runErr, output)
				}
				got, err := os.ReadFile(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(chartBytes) {
					t.Fatalf("exported chart bytes = %q, want %q", got, chartBytes)
				}
				info, err := os.Stat(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				if gotMode := info.Mode().Perm(); gotMode != 0o600 {
					t.Fatalf("exported chart mode = %#o, want 0600", gotMode)
				}
				temporaryFiles, err := filepath.Glob(filepath.Join(outputDirectory, ".ptah-operator-release-chart.*"))
				if err != nil {
					t.Fatal(err)
				}
				if len(temporaryFiles) != 0 {
					t.Fatalf("atomic temporary files remain: %v", temporaryFiles)
				}
			})

			tests := []struct {
				name         string
				prepare      func(t *testing.T) string
				wantError    string
				wantContents string
			}{
				{
					name: "relative target",
					prepare: func(t *testing.T) string {
						return "relative-release-chart.tgz"
					},
					wantError: "must be an absolute path",
				},
				{
					name: "target inside task work directory",
					prepare: func(t *testing.T) string {
						return filepath.Join(workDirectory, "exported.tgz")
					},
					wantError: "must be outside the task work directory",
				},
				{
					name: "existing target",
					prepare: func(t *testing.T) string {
						path := filepath.Join(outputDirectory, "existing.tgz")
						if err := os.WriteFile(path, []byte("preserve me"), 0o600); err != nil {
							t.Fatal(err)
						}
						return path
					},
					wantError:    "refusing to replace existing",
					wantContents: "preserve me",
				},
				{
					name: "symlink target",
					prepare: func(t *testing.T) string {
						realPath := filepath.Join(outputDirectory, "real-target.tgz")
						if err := os.WriteFile(realPath, []byte("preserve real target"), 0o600); err != nil {
							t.Fatal(err)
						}
						linkPath := filepath.Join(outputDirectory, "target-link.tgz")
						if err := os.Symlink(realPath, linkPath); err != nil {
							t.Fatal(err)
						}
						return linkPath
					},
					wantError: "refusing to replace existing",
				},
				{
					name: "symlink parent",
					prepare: func(t *testing.T) string {
						realDirectory := filepath.Join(directory, "real-output")
						if err := os.Mkdir(realDirectory, 0o700); err != nil {
							t.Fatal(err)
						}
						linkDirectory := filepath.Join(directory, "output-link")
						if err := os.Symlink(realDirectory, linkDirectory); err != nil {
							t.Fatal(err)
						}
						return filepath.Join(linkDirectory, "release.tgz")
					},
					wantError: "parent must be an existing non-symlink directory",
				},
			}
			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					targetPath := test.prepare(t)
					output, runErr := exec.Command(
						shellPath, scriptPath, workDirectory, chartPath, targetPath,
					).CombinedOutput()
					if runErr == nil {
						t.Fatalf("unsafe release chart export unexpectedly succeeded with %q", output)
					}
					if !strings.Contains(string(output), test.wantError) {
						t.Fatalf("release chart export error = %q, want substring %q", output, test.wantError)
					}
					if test.wantContents != "" {
						got, err := os.ReadFile(targetPath)
						if err != nil {
							t.Fatal(err)
						}
						if string(got) != test.wantContents {
							t.Fatalf("existing target contents = %q, want %q", got, test.wantContents)
						}
					}
				})
			}

			t.Run("target creation race is not clobbered", func(t *testing.T) {
				realLink, err := exec.LookPath("ln")
				if err != nil {
					t.Skip("ln is required to exercise no-clobber chart publication")
				}
				wrapperDirectory := filepath.Join(directory, "race-bin")
				if err := os.Mkdir(wrapperDirectory, 0o700); err != nil {
					t.Fatal(err)
				}
				linkWrapper := filepath.Join(wrapperDirectory, "ln")
				wrapperSource := "#!/bin/sh\n" +
					"printf '%s' 'preserve racing target' >\"$E2E_RACE_TARGET\"\n" +
					"exec \"$E2E_REAL_LN\" \"$@\"\n"
				if err := os.WriteFile(linkWrapper, []byte(wrapperSource), 0o700); err != nil {
					t.Fatal(err)
				}
				targetPath := filepath.Join(outputDirectory, "racing-release-chart.tgz")
				command := exec.Command(
					shellPath, scriptPath, workDirectory, chartPath, targetPath,
				)
				command.Env = []string{
					"PATH=" + wrapperDirectory + string(os.PathListSeparator) + os.Getenv("PATH"),
					"E2E_RACE_TARGET=" + targetPath,
					"E2E_REAL_LN=" + realLink,
				}
				output, runErr := command.CombinedOutput()
				if runErr == nil {
					t.Fatalf("raced release chart export unexpectedly succeeded with %q", output)
				}
				if !strings.Contains(string(output), "could not publish the no-clobber atomic release chart output") {
					t.Fatalf("raced release chart export error = %q", output)
				}
				got, err := os.ReadFile(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != "preserve racing target" {
					t.Fatalf("racing target contents = %q, want preserved competitor bytes", got)
				}
				temporaryFiles, err := filepath.Glob(filepath.Join(outputDirectory, ".ptah-operator-release-chart.*"))
				if err != nil {
					t.Fatal(err)
				}
				if len(temporaryFiles) != 0 {
					t.Fatalf("atomic temporary files remain after raced publication: %v", temporaryFiles)
				}
			})
		})
	}
}

func extractE2EShellFunction(t *testing.T, source, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`)
	matches := pattern.FindAllString(source, -1)
	if len(matches) != 1 {
		t.Fatalf("%s function matches = %d, want 1", name, len(matches))
	}
	return matches[0]
}

func apiServerFeatureGateScopeFilter(t *testing.T) string {
	t.Helper()

	source := extractE2EShellFunction(
		t,
		readE2ESource(t, repositoryE2EWiringFiles().harness),
		"assert_api_server_feature_gate_scope",
	)
	const startMarker = `jq -e --arg expected "$expected_api_server_feature_gates" --arg cluster "$CLUSTER_NAME" '` + "\n"
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatal("API server component readiness filter start is missing")
	}
	start += len(startMarker)
	const endMarker = "\n\t' \"$control_plane_pods_file\" >/dev/null ||"
	end := strings.Index(source[start:], endMarker)
	if end < 0 {
		t.Fatal("API server component readiness filter end is missing")
	}
	return source[start : start+end]
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

	shard := activeMutationTestShard(t)
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
			name:        "daemon-side task claim omitted",
			old:         "acquire_task_claim\nif ! existing_clusters=$(kind get clusters); then",
			replacement: "if ! existing_clusters=$(kind get clusters); then",
			wantError:   "task claim before collision checks",
		},
		{
			name:        "task claim cleanup armed after create",
			old:         `TASK_CLAIM_CREATE_STARTED=1`,
			replacement: `TASK_CLAIM_CREATE_STARTED=0`,
			wantError:   "daemon-side task claim create latch",
		},
		{
			name: "daemon-side task claim nonce label omitted",
			old: "--label 'operator.ptah.dev/e2e-component=task-claim' \\\n\t\t" +
				`--label "operator.ptah.dev/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
			replacement: "--label 'operator.ptah.dev/e2e-component=task-claim' \\\n\t\t" +
				`--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \`,
			wantError: "atomic daemon-side task claim creation",
		},
		{
			name:        "task claim cleanup omitted",
			old:         `if ! docker --context "$DOCKER_CONTEXT" volume rm "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then`,
			replacement: `if false; then`,
			wantError:   "task claim cleanup removal",
		},
		{
			name: "operator image audit exports reusable name",
			old: "docker --context \"$DOCKER_CONTEXT\" export \"$IMAGE_AUDIT_CONTAINER_ID\" >\"$IMAGE_AUDIT_ARCHIVE\"\n" +
				`if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$'; then`,
			replacement: "docker --context \"$DOCKER_CONTEXT\" export \"$IMAGE_AUDIT_CONTAINER\" >\"$IMAGE_AUDIT_ARCHIVE\"\n" +
				`if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$'; then`,
			wantError: "operator image audit by captured ID",
		},
		{
			name:        "operator image audit removal omitted",
			old:         "remove_image_audit_container\n\ncreate_image_audit_container \"$FIXTURE_BUILD_IMAGE\"",
			replacement: `create_image_audit_container "$FIXTURE_BUILD_IMAGE"`,
			wantError:   "exactly two task-owned image-audit removals",
		},
		{
			name:        "image-audit cleanup removes reusable name",
			old:         `elif ! docker --context "$DOCKER_CONTEXT" container rm -f "$image_audit_cleanup_id" >/dev/null 2>&1; then`,
			replacement: `elif ! docker --context "$DOCKER_CONTEXT" container rm -f "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then`,
			wantError:   "task-owned image-audit cleanup removal",
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
			old:         `        (command_options(.; "--runtime-config=") | length) == 1`,
			replacement: `      true and`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "static component running status accepted as stale",
			old:         `        ($pod.status.phase == "Running") and`,
			replacement: `        ($pod.status.phase != "") and`,
			wantError:   "API-server feature gate contract",
		},
		{
			name:        "static component container readiness omitted",
			old:         `        ($pod.status.containerStatuses[0].ready == true) and`,
			replacement: `        true and`,
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
			name:        "exact node-count readiness guard bypassed",
			old:         `if ! jq -e '.items | length == 4' "$NODE_READINESS_FILE" >/dev/null; then`,
			replacement: `if ! jq -e 'true' "$NODE_READINESS_FILE" >/dev/null; then`,
			wantError:   "bounded hard node readiness wait",
		},
		{
			name:        "bounded node readiness wait bypassed",
			old:         `--for=condition=Ready nodes --all --timeout=2m; then`,
			replacement: `--for=condition=Ready nodes --all --timeout=2m || true; then`,
			wantError:   "bounded hard node readiness wait",
		},
		{
			name: "immediate readiness predicate accepts a partial cluster",
			old: "\t  ((.items | length) == 4) and\n" +
				"\t  all(.items[];\n" +
				"        any((.status.conditions // [])[];\n" +
				`          .type == "Ready" and .status == "True"`,
			replacement: "\t  ((.items | length) == 4) and\n" +
				"\t  all(.items[];\n" +
				"        any((.status.conditions // [])[];\n" +
				`          .status == "True"`,
			wantError: "immediate all-node readiness predicate",
		},
		{
			name:        "immediate readiness predicate accepts a partial topology",
			old:         `((.items | length) == 4) and`,
			replacement: `true and`,
			wantError:   "immediate all-node readiness predicate",
		},
		{
			name: "immediate readiness predicate masks node query failure",
			old: "nodes_ready_now() {\n" +
				"\tkubectl --kubeconfig \"$KUBECONFIG_FILE\" --request-timeout=15s \\\n" +
				"\t\tget nodes -o json >\"$NODE_READINESS_FILE\" &&",
			replacement: "nodes_ready_now() {\n" +
				"\tkubectl --kubeconfig \"$KUBECONFIG_FILE\" --request-timeout=15s \\\n" +
				"\t\tget nodes -o json |",
			wantError: "immediate all-node readiness predicate",
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
			name: "post-creation HA topology assertion omitted",
			old: "require_ready_nodes \"after kind cluster creation\"\n" +
				"assert_kind_ha_topology\n" +
				"assert_api_server_endpoint_inventory",
			replacement: "require_ready_nodes \"after kind cluster creation\"\n" +
				": # HA topology assertion omitted\n" +
				"assert_api_server_endpoint_inventory",
			wantError: "kind cluster creation",
		},
		{
			name: "third control-plane node omitted from topology proof",
			old: "\t\t\t\"${CLUSTER_NAME}-control-plane2\" \\\n" +
				"\t\t\t\"${CLUSTER_NAME}-control-plane3\" \\\n" +
				"\t\t\t\"${CLUSTER_NAME}-worker\"",
			replacement: "\t\t\t\"${CLUSTER_NAME}-control-plane2\" \\\n" +
				"\t\t\t\"${CLUSTER_NAME}-worker\"",
			wantError: "kind HA topology contract",
		},
		{
			name:        "control-plane node binding omitted from endpoint filter",
			old:         `jq -e --arg cluster "$CLUSTER_NAME" --slurpfile nodes "$NODE_READINESS_FILE" \`,
			replacement: `jq -e --arg cluster "$CLUSTER_NAME" --argjson nodes '[]' \`,
			wantError:   "API server endpoint inventory contract",
		},
		{
			name:        "direct API server endpoint probe omitted",
			old:         `			probe_api_server_endpoints; then`,
			replacement: `			true; then`,
			wantError:   "API server endpoint inventory contract",
		},
		{
			name:        "direct API server endpoint probe skips the third endpoint",
			old:         `	while IFS= read -r api_server_endpoint; do`,
			replacement: `	while IFS= read -r api_server_endpoint && [ "$api_server_endpoint_probe_count" -lt 2 ]; do`,
			wantError:   "API server direct endpoint probe contract",
		},
		{
			name:        "direct API server ready response acceptance weakened",
			old:         `		[ "$api_server_readyz" = ok ] || return 1`,
			replacement: `		[ -n "$api_server_readyz" ] || return 1`,
			wantError:   "API server direct endpoint probe contract",
		},
		{
			name:        "worker registry configuration omitted",
			old:         "\t\t\"${CLUSTER_NAME}-worker\"; do",
			replacement: "\t\t\"${CLUSTER_NAME}-control-plane3\"; do",
			wantError:   "all-node registry hosts contract",
		},
		{
			name:        "all-node registry configuration call omitted",
			old:         "configure_registry_hosts_on_kind_nodes\n\nprintf '%s\\n' 'e2e: mirroring immutable execution and database images into the isolated registry'",
			replacement: ": # all-node registry configuration omitted\n\nprintf '%s\\n' 'e2e: mirroring immutable execution and database images into the isolated registry'",
			wantError:   "all-node registry configuration call",
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
			name:        "synthetic next chart handoff omitted",
			old:         "E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_NEXT_CHART_PACKAGE= \\\n",
			wantError:   "uninstall lifecycle",
		},
		{
			name:        "current release sequence handoff omitted",
			old:         "E2E_CURRENT_RELEASE_SEQUENCE=$CURRENT_RELEASE_SEQUENCE \\\n",
			replacement: "E2E_CURRENT_RELEASE_SEQUENCE= \\\n",
			wantError:   "uninstall lifecycle",
		},
		{
			name: "current release values handoff omitted",
			old: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE= \\\n" +
				"E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			wantError: "uninstall lifecycle",
		},
		{
			name: "current release image handoff omitted",
			old: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CANDIDATE_IMAGE= \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			wantError: "uninstall lifecycle",
		},
		{
			name:        "installed chart export omitted",
			old:         "\nexport_release_chart\nprintf 'e2e: PASS Kubernetes=%s cluster=%s\\n'",
			replacement: "\n: # installed chart export omitted\nprintf 'e2e: PASS Kubernetes=%s cluster=%s\\n'",
			wantError:   "post-lifecycle installed chart export",
		},
		{
			name: "installed chart export moved after terminal evidence",
			old: "export_release_chart\n" +
				"printf 'e2e: PASS Kubernetes=%s cluster=%s\\n' \"$server_version\" \"$CLUSTER_NAME\"",
			replacement: "printf 'e2e: PASS Kubernetes=%s cluster=%s\\n' \"$server_version\" \"$CLUSTER_NAME\"\n" +
				"export_release_chart",
			wantError: "terminal Kubernetes lifecycle evidence",
		},
		{
			name:        "installed chart export uses synthetic next chart",
			old:         `cp "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"`,
			replacement: `cp "$NEXT_CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"`,
			wantError:   "export_release_chart digest",
		},
		{
			name:        "installed chart export clobbers a raced target",
			old:         `ln "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"`,
			replacement: `mv "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"`,
			wantError:   "export_release_chart digest",
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
	shard.requireNonemptyTable(t, len(tests))
	for index, test := range tests {
		if !shard.includes(index) {
			continue
		}
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

	shard := activeMutationTestShard(t)
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
			name:        "durable Job archive root permissions weakened",
			old:         `chmod 700 "$JOB_EVIDENCE_DIR"`,
			replacement: `chmod 755 "$JOB_EVIDENCE_DIR"`,
			wantError:   "private durable Job evidence root initialization",
		},
		{
			name:        "durable Job archive directory permissions unchecked",
			old:         `require_mode_0700_directory "$validated_archive" "Job evidence archive"`,
			replacement: `test -d "$validated_archive"`,
			wantError:   "durable Job archive private directory validation",
		},
		{
			name:        "durable Job archive accepts a sixth file",
			old:         `[ "$archive_entry_count" -eq 5 ] ||`,
			replacement: `[ "$archive_entry_count" -ge 5 ] ||`,
			wantError:   "durable Job archive exact entry count",
		},
		{
			name:        "durable Job archive omits one required file",
			old:         `"$validated_manifest_file:manifest"; do`,
			replacement: `"$validated_result_file:normalized result"; do`,
			wantError:   "durable Job archive exact five-file inventory",
		},
		{
			name:        "durable Job archive file permissions unchecked",
			old:         `require_mode_0600_regular_file "$validated_file" \`,
			replacement: `test -f "$validated_file" || \`,
			wantError:   "durable Job archive private file validation",
		},
		{
			name:        "durable Job archive credential scan omitted",
			old:         `scan_file_for_credentials "${validated_material%%:*}" "${validated_material#*:}"`,
			replacement: `true # archived material credential scan omitted`,
			wantError:   "durable Job archive complete credential scan",
		},
		{
			name:        "durable Job archive credential scanner stubbed",
			old:         `if grep -F -f "$CREDENTIAL_PATTERNS_FILE" "$scan_file" >/dev/null; then`,
			replacement: `if false; then`,
			wantError:   "credential scanner fail-closed implementation",
		},
		{
			name:        "durable Job archive staging directory permissions weakened",
			old:         `chmod 700 "$publish_stage"`,
			replacement: `chmod 755 "$publish_stage"`,
			wantError:   "durable Job archive private staging mode",
		},
		{
			name: "durable Job archive staged file permissions weakened",
			old: "chmod 600 \"$publish_stage/job.json\" \"$publish_stage/pod.json\" \\\n" +
				"\t\t\"$publish_stage/ptah.log\" \"$publish_stage/result.json\"",
			replacement: "chmod 644 \"$publish_stage/job.json\" \"$publish_stage/pod.json\" \\\n" +
				"\t\t\"$publish_stage/ptah.log\" \"$publish_stage/result.json\"",
			wantError: "durable Job archive private staged file modes",
		},
		{
			name:        "durable Job archive manifest permissions weakened",
			old:         `chmod 600 "$publish_stage/manifest.json"`,
			replacement: `chmod 644 "$publish_stage/manifest.json"`,
			wantError:   "durable Job archive private manifest mode",
		},
		{
			name:        "durable Job archive schema-owner manifest UID omitted",
			old:         `($expectedSchemaUID == "" or .job.owner.uid == $expectedSchemaUID) and`,
			replacement: `true and`,
			wantError:   "durable Job archive schema-owner manifest binding",
		},
		{
			name: "durable Job archive exact schema owner UID omitted",
			old: "([.metadata.ownerReferences[]? | select(\n" +
				"        .apiVersion == \"operator.ptah.dev/v1alpha1\" and .kind == \"PtahSchema\" and\n" +
				"        .name == $schema and .uid == $schemaUID and .controller == true)] | length) == 1 and",
			replacement: `([.metadata.ownerReferences[]? | select(.name == $schema)] | length) == 1 and`,
			wantError:   "durable Job archive exact schema ownerReference",
		},
		{
			name:        "durable Job archive schema-owner manifest persistence omitted",
			old:         `uid: $schemaUID,`,
			replacement: `uid: $jobUID,`,
			wantError:   "durable Job archive persisted schema-owner binding",
		},
		{
			name:        "durable Job archive UID-bounded log input omitted",
			old:         `publish_log_file=$3`,
			replacement: `publish_log_file=`,
			wantError:   "durable Job archive supplied UID-bounded log",
		},
		{
			name:        "durable Job archive UID-bounded log permissions unchecked",
			old:         `require_mode_0600_regular_file "$publish_log_file" "supplied UID-bounded ptah log"`,
			replacement: `test -f "$publish_log_file"`,
			wantError:   "durable Job archive supplied log private-file validation",
		},
		{
			name:        "durable Job archive schema-owner UID extraction weakened",
			old:         `(.uid | type) == "string" and (.uid | length) > 0)] |`,
			replacement: `true)] |`,
			wantError:   "durable Job archive supplied schema-owner UID extraction",
		},
		{
			name:        "durable Job archive refetches log by reusable Pod name",
			old:         `cp "$publish_log_file" "$publish_stage/ptah.log" ||`,
			replacement: `k -n "$TEST_NAMESPACE" logs pod/"$publish_pod_name" -c ptah >"$publish_stage/ptah.log" ||`,
			wantError:   "durable Job archive UID-bounded log copy",
		},
		{
			name:        "durable Job archive supplied identity validation omitted",
			old:         `validate_supplied_job_evidence_identity \`,
			replacement: `true # supplied identity validation omitted`,
			wantError:   "durable Job archive supplied identity validation",
		},
		{
			name:        "supplied Job archive operation ID binding omitted",
			old:         `$job.metadata.annotations["operator.ptah.dev/operation-id"] == $operationID and`,
			replacement: `true and`,
			wantError:   "supplied Job evidence exact Job identity binding",
		},
		{
			name: "supplied Job archive schema owner UID binding omitted",
			old: "([$job.metadata.ownerReferences[]? | select(\n" +
				"        .apiVersion == \"operator.ptah.dev/v1alpha1\" and .kind == \"PtahSchema\" and\n" +
				"        .name == $schema and .uid == $schemaUID and .controller == true)] | length) == 1 and",
			replacement: `([$job.metadata.ownerReferences[]? | select(.name == $schema)] | length) == 1 and`,
			wantError:   "supplied Job evidence exact schema ownerReference",
		},
		{
			name:        "supplied Job archive Pod UID binding omitted",
			old:         `$pod.metadata.uid == $podUID and $pod.metadata.name == $podName and`,
			replacement: `$pod.metadata.name == $podName and`,
			wantError:   "supplied Job evidence exact Pod identity binding",
		},
		{
			name: "supplied Job archive Pod owner binding omitted",
			old: "([$pod.metadata.ownerReferences[]? | select(\n" +
				"        .apiVersion == \"batch/v1\" and .kind == \"Job\" and\n" +
				"        .uid == $jobUID and .name == $jobName and .controller == true)] | length) == 1",
			replacement: `([$pod.metadata.ownerReferences[]?] | length) == 1`,
			wantError:   "supplied Job evidence exact Pod owner binding",
		},
		{
			name:        "existing durable Job archive acceptance skips supplied identity",
			old:         `assert_existing_job_evidence_matches_supplied \`,
			replacement: `validate_job_evidence_directory "$publish_archive" \`,
			wantError:   "existing durable Job archive exact identity acceptance",
		},
		{
			name:        "existing durable Job archive skips supplied schema-owner UID",
			old:         `[ "$VALIDATED_JOB_EVIDENCE_SCHEMA_UID" != "$existing_schema_uid" ] ||`,
			replacement: `if false ||`,
			wantError:   "existing Job evidence supplied identity comparison",
		},
		{
			name:        "existing durable Job archive skips supplied operation ID",
			old:         `[ "$VALIDATED_JOB_EVIDENCE_OPERATION_ID" != "$existing_operation_id" ] ||`,
			replacement: `if false ||`,
			wantError:   "existing Job evidence supplied identity comparison",
		},
		{
			name:        "existing durable Job archive skips supplied Job name",
			old:         `[ "$VALIDATED_JOB_EVIDENCE_JOB_NAME" != "$existing_job_name" ] ||`,
			replacement: `[ false = true ] ||`,
			wantError:   "existing Job evidence supplied identity comparison",
		},
		{
			name:        "existing durable Job archive skips supplied Pod UID",
			old:         `[ "$VALIDATED_JOB_EVIDENCE_POD_UID" != "$existing_pod_uid" ] ||`,
			replacement: `[ false = true ] ||`,
			wantError:   "existing Job evidence supplied identity comparison",
		},
		{
			name:        "existing durable Job archive skips supplied Pod name",
			old:         `[ "$VALIDATED_JOB_EVIDENCE_POD_NAME" != "$existing_pod_name" ]; then`,
			replacement: `[ false = true ]; then`,
			wantError:   "existing Job evidence supplied identity comparison",
		},
		{
			name: "existing durable Job archive skips supplied schema operation and UID",
			old: "validate_job_evidence_directory \"$existing_archive\" \\\n" +
				"\t\t\"$existing_schema\" \"$existing_operation\" \"$existing_job_uid\" \\\n" +
				"\t\t\"$existing_schema_uid\"",
			replacement: `validate_job_evidence_directory "$existing_archive" "" "" ""`,
			wantError:   "existing Job evidence schema-operation-UID validation",
		},
		{
			name: "durable Job archive live Job read drops exact NotFound distinction",
			old: "if live_evidence_job=$(k -n \"$TEST_NAMESPACE\" get job \"$live_evidence_job_name\" \\\n" +
				"\t\t-o json --ignore-not-found 2>\"$LIVE_JOB_EVIDENCE_ERROR_FILE\"); then",
			replacement: "if live_evidence_job=$(k -n \"$TEST_NAMESPACE\" get job \"$live_evidence_job_name\" \\\n" +
				"\t\t-o json 2>/dev/null); then",
			wantError: "durable Job evidence exact live Job read",
		},
		{
			name:        "durable Job archive live Job API failure accepted",
			old:         `fail "live Job consistency read failed before exact GC absence could be established"`,
			replacement: `: # live Job API failure treated as GC`,
			wantError:   "durable Job evidence fail-closed live Job API error",
		},
		{
			name:        "durable Job archive live Job identity weakened",
			old:         `.metadata.name == $name and .metadata.uid == $uid and`,
			replacement: `.metadata.name == $name and`,
			wantError:   "durable Job evidence exact live Job identity",
		},
		{
			name: "durable Job archive live Pod read drops exact NotFound distinction",
			old: "if live_evidence_pod=$(k -n \"$TEST_NAMESPACE\" get pod \"$live_evidence_pod_name\" \\\n" +
				"\t\t-o json --ignore-not-found 2>\"$LIVE_JOB_EVIDENCE_ERROR_FILE\"); then",
			replacement: "if live_evidence_pod=$(k -n \"$TEST_NAMESPACE\" get pod \"$live_evidence_pod_name\" \\\n" +
				"\t\t-o json 2>/dev/null); then",
			wantError: "durable Job evidence exact live Pod read",
		},
		{
			name:        "durable Job archive live Pod API failure accepted",
			old:         `fail "live Pod consistency read failed before exact GC absence could be established"`,
			replacement: `: # live Pod API failure treated as GC`,
			wantError:   "durable Job evidence fail-closed live Pod API error",
		},
		{
			name:        "durable Job archive live Pod identity weakened",
			old:         `.metadata.name == $podName and .metadata.uid == $podUID and`,
			replacement: `.metadata.name == $podName and`,
			wantError:   "durable Job evidence exact live Pod identity",
		},
		{
			name: "durable Job archive live Pod owner weakened",
			old: "([.metadata.ownerReferences[]? | select(\n" +
				"            .apiVersion == \"batch/v1\" and .kind == \"Job\" and\n" +
				"            .uid == $jobUID and .name == $jobName and .controller == true)] | length) == 1",
			replacement: `([.metadata.ownerReferences[]?] | length) == 1`,
			wantError:   "durable Job evidence exact live Pod owner",
		},
		{
			name: "durable Job archive omits UID-bounded audited log capture",
			old: "if [ \"$audit_managed_complete\" -eq 1 ] && [ \"$audit_container\" = ptah ]; then\n" +
				"\t\t\t\t\tcp \"$LOG_FILE\" \"$audit_evidence_log_file\" ||\n" +
				"\t\t\t\t\t\tfail \"could not retain UID-bounded ptah logs for exact Pod $audit_pod_name UID $audit_pod_uid\"\n" +
				"\t\t\t\t\tchmod 600 \"$audit_evidence_log_file\"",
			replacement: `if false; then :`,
			wantError:   "durable Job archive UID-bounded audited log capture",
		},
		{
			name: "durable Job archive drops post-log exact Pod UID read",
			old: "audit_pod_after=$(k -n \"$TEST_NAMESPACE\" get pod \"$audit_pod_name\" -o json 2>/dev/null) ||\n" +
				"\t\t\t\tfail \"exact Pod $audit_pod_name UID $audit_pod_uid disappeared during its log audit\"",
			replacement: `audit_pod_after=$audit_pod_object`,
			wantError:   "durable Job archive post-log exact Pod UID check",
		},
		{
			name:        "durable Job archive drops SHA-256 path binding",
			old:         `.archiveVersion == 1 and .pathKey == $key and`,
			replacement: `.archiveVersion == 1 and true and`,
			wantError:   "durable Job archive SHA-256 path binding",
		},
		{
			name: "durable Job archive drops schema-operation binding",
			old: ".schema == $schema and .operation == $operation and\n" +
				"        (.operationID | type) == \"string\" and",
			replacement: "true and\n" +
				"        (.operationID | type) == \"string\" and",
			wantError: "durable Job archive schema-operation binding",
		},
		{
			name:        "durable Job archive drops Pod owner binding",
			old:         `.pod.owner.name == .job.name and .pod.owner.uid == .job.uid and`,
			replacement: `true and`,
			wantError:   "durable Job archive Pod owner binding",
		},
		{
			name:        "durable Job archive drops transport digest binding",
			old:         `.digests.rawLogSHA256 == $logDigest and .digests.resultSHA256 == $resultDigest`,
			replacement: `true`,
			wantError:   "durable Job archive transport digest binding",
		},
		{
			name:        "durable Job archive omits manifest final marker",
			old:         `' >"$publish_stage/manifest.json"`,
			replacement: `' >"$WORK_DIR/unpublished-manifest.json"`,
			wantError:   "durable Job archive manifest-last staging",
		},
		{
			name: "full-audit ledger commits before durable archive",
			old: "publish_completed_job_evidence \\\n\t\t\t\t\"$audit_job_evidence_file\" \"$audit_pod_evidence_file\" \\\n" +
				"\t\t\t\t\"$audit_evidence_log_file\"",
			replacement: `true # durable archive publication omitted`,
			wantError:   "durable Job evidence publication before full-audit ledger",
		},
		{
			name:        "selected Job restores live-only result capture",
			old:         "validate_completed_job_evidence \\\n\t\t\"$selected_schema\" \"$selected_operation\" \"$selected_uid\"",
			replacement: `capture_one_new_job_result "$selected_schema" "$selected_operation" "$selected_uid" "$selected_output"`,
			wantError:   "selected Job archived result consumption",
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
			name:        "explicit apply policy input ignored",
			old:         `resource_apply=${11:-}`,
			replacement: `resource_apply=`,
			wantError:   "explicit optional apply-policy input",
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
			wantError:   "external lifecycle explicit Always source call",
		},
		{
			name:        "external lifecycle drops automatic apply policy",
			old:         `e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL" Always`,
			replacement: `e2e-verification-policy "$REGISTRY_AUTH_SECRET" Environment 45s "$QUIESCENT_INTERVAL"`,
			wantError:   "external lifecycle explicit Always source call",
		},
		{
			name:        "automatic lifecycle weakens exact Job history",
			old:         `($jobs | length) == 7 and`,
			replacement: `($jobs | length) >= 7 and`,
			wantError:   "automatic external PostgreSQL exact Job history",
		},
		{
			name:        "automatic lifecycle restores live-only Job materialization",
			old:         "materialize_archived_schema_jobs \"$automatic_schema\" \"$automatic_before\" 7 \\\n\t\t\"$automatic_observed_uids_file\" \"$automatic_jobs_file\"",
			replacement: `k -n "$TEST_NAMESPACE" get jobs -o json >"$automatic_jobs_file"`,
			wantError:   "automatic external PostgreSQL archived Job materialization",
		},
		{
			name:        "automatic lifecycle trusts only the expiring live Job list",
			old:         `([$jobs[].metadata.uid] | unique | sort) == $observed[0] and`,
			replacement: `true and`,
			wantError:   "automatic external PostgreSQL exact Job history",
		},
		{
			name: "automatic lifecycle accepts a post-snapshot historical Job UID",
			old: "        ($actual | length) == $expectedCount and\n" +
				"        $actual == $expected[0]",
			replacement: "        ($actual | length) >= $expectedCount and\n" +
				"        ($expected[0] - $actual | length) == 0",
			wantError: "automatic external PostgreSQL post-capture exact Job-boundary equality",
		},
		{
			name:        "automatic Apply drops captured Job UID binding",
			old:         `[ "$CAPTURED_JOB_UID" = "$automatic_apply_uid" ] ||`,
			replacement: `true ||`,
			wantError:   "automatic external PostgreSQL captured Apply Job UID binding",
		},
		{
			name:        "automatic Apply restores live-only Job read",
			old:         `cp "$CAPTURED_JOB_EVIDENCE_DIR/job.json" "$automatic_apply_job_file" ||`,
			replacement: `k -n "$TEST_NAMESPACE" get job "$CAPTURED_JOB_NAME" -o json >"$automatic_apply_job_file" ||`,
			wantError:   "automatic external PostgreSQL archived Apply workload evidence",
		},
		{
			name:        "automatic Apply drops plan fingerprint annotation binding",
			old:         `.["operator.ptah.dev/plan-fingerprint"] == $planFingerprint and`,
			replacement: `true and`,
			wantError:   "automatic external PostgreSQL Apply annotation bindings",
		},
		{
			name:        "automatic Apply drops archived Pod UID binding",
			old:         `$pod.metadata.name == $podName and $pod.metadata.uid == $podUID and`,
			replacement: `$pod.metadata.name == $podName and`,
			wantError:   "automatic external PostgreSQL Apply Pod UID identity",
		},
		{
			name:        "automatic Apply swaps plan content annotation binding",
			old:         `.["operator.ptah.dev/plan-content-digest"] == $contentDigest and`,
			replacement: `.["operator.ptah.dev/plan-content-digest"] == $planFingerprint and`,
			wantError:   "automatic external PostgreSQL Apply annotation bindings",
		},
		{
			name:        "automatic Apply swaps execution annotation binding",
			old:         `.["operator.ptah.dev/execution-binding-id"] == $executionBinding;`,
			replacement: `.["operator.ptah.dev/execution-binding-id"] == $contentDigest;`,
			wantError:   "automatic external PostgreSQL Apply annotation bindings",
		},
		{
			name:        "automatic Apply swaps install-runner image binding",
			old:         `select(.name == "install-runner" and .image == $runnerImage)] | length) == 1 and`,
			replacement: `select(.name == "install-runner" and .image == $executorImage)] | length) == 1 and`,
			wantError:   "automatic external PostgreSQL Apply runner image binding",
		},
		{
			name:        "automatic Apply swaps executor image binding",
			old:         `select(.name == "ptah" and .image == $executorImage)] | length) == 1 and`,
			replacement: `select(.name == "ptah" and .image == $runnerImage)] | length) == 1 and`,
			wantError:   "automatic external PostgreSQL Apply executor image binding",
		},
		{
			name:        "automatic Apply swaps expected database engine",
			old:         `.value == "PostgreSQL" and (.valueFrom // null) == null)] | length) == 1;`,
			replacement: `.value == "MySQL" and (.valueFrom // null) == null)] | length) == 1;`,
			wantError:   "automatic external PostgreSQL Apply database-engine binding",
		},
		{
			name:        "automatic lifecycle drops independent no-change Plan proof",
			old:         `.planOutcome == "NoChanges" and (.planContentDigest // "") == "" and`,
			replacement: `.planOutcome == "Changes" and (.planContentDigest // "") != "" and`,
			wantError:   "automatic external PostgreSQL no-change Plan evidence",
		},
		{
			name:        "automatic isolation restores live-only Job read",
			old:         "assert_job_isolation \"$automatic_schema\" \"$automatic_secret\" true \\\n\t\t\"$automatic_jobs_file\"",
			replacement: `assert_job_isolation "$automatic_schema" "$automatic_secret" true`,
			wantError:   "automatic external PostgreSQL archived isolation",
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
	shard.requireNonemptyTable(t, len(tests))
	for index, test := range tests {
		if !shard.includes(index) {
			continue
		}
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

	shard := activeMutationTestShard(t)
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
	shard.requireNonemptyTable(t, len(tests))
	for index, test := range tests {
		if !shard.includes(index) {
			continue
		}
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

	shard := activeMutationTestShard(t)
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
			old:         `fail "legacy Job bootstrap probe was refused before candidate activation"`,
			replacement: `true # legacy Job bootstrap admission removed`,
			wantError:   "legacy Job bootstrap admits before activation",
		},
		{
			name:        "CRD legacy Job bootstrap admission result is inverted",
			child:       "crd-upgrade",
			old:         `if ! controller_kube create --dry-run=server -o json -f "$legacy_job_probe" \`,
			replacement: `if controller_kube create --dry-run=server -o json -f "$legacy_job_probe" \`,
			wantError:   "legacy Job bootstrap admits before activation",
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
			old:         `fail "legacy plan bootstrap probe was refused before candidate activation"`,
			replacement: `true # legacy plan semantic boundary removed`,
			wantError:   "legacy plan bootstrap admits before activation",
		},
		{
			name:        "CRD legacy plan bootstrap admission result is inverted",
			child:       "crd-upgrade",
			old:         `if ! controller_kube create --dry-run=server -o json \`,
			replacement: `if controller_kube create --dry-run=server -o json \`,
			wantError:   "legacy plan bootstrap admits before activation",
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
			name:        "CRD late activation preflight failure class destination is omitted",
			child:       "crd-upgrade",
			old:         `--failure-class-file "$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE" \`,
			replacement: `--failure-class-file "$LATE_ACTIVATION_PREFLIGHT_CAPTURE_ERRORS_FILE" \`,
			wantError:   "exact dual resourceVersion-bound late activation hook capture arm contract",
		},
		{
			name:        "CRD late activation failure class loses its size bound",
			child:       "crd-upgrade",
			old:         `if [ "$failure_class_size" -gt 32 ]; then`,
			replacement: `if [ "$failure_class_size" -gt 320 ]; then`,
			wantError:   "late activation failure class size bound",
		},
		{
			name:        "CRD late activation failure class accepts arbitrary text",
			child:       "crd-upgrade",
			old:         `configuration | output | render | kubernetes-client | priority-inventory | priority-watch | job-inventory | job-watch | job-contract | pod-inventory | pod-watch | pod-contract | pod-owner | log-start | log-start-timeout | log-read | log-empty | log-too-large | deadline | canceled | internal)`,
			replacement: `*)`,
			wantError:   "exact bounded allowlisted late activation failure class summary contract",
		},
		{
			name:        "CRD late activation preflight failure class is synthesized",
			child:       "crd-upgrade",
			old:         `preflight_failure_class=$(late_activation_failure_class_summary "$LATE_ACTIVATION_PREFLIGHT_FAILURE_CLASS_FILE")`,
			replacement: `preflight_failure_class=unavailable`,
			wantError:   "late activation failure class synthesis",
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
			old:         `grep -F 'wait for release activation guard before persistence' \`,
			replacement: `grep -F 'unrelated failure' \`,
			wantError:   "late activation reconcile exact blocker evidence",
		},
		{
			name:        "CRD late activation reconcile diagnostic omits the blocker webhook",
			child:       "crd-upgrade",
			old:         `grep -F 'late-activation-blocker.operator.ptah.dev' \`,
			replacement: `grep -F 'unrelated-webhook.operator.ptah.dev' \`,
			wantError:   "late activation reconcile exact blocker evidence",
		},
		{
			name:        "CRD late activation reconcile diagnostic omits the missing service",
			child:       "crd-upgrade",
			old:         `grep -F 'service "ptah-operator-e2e-missing-blocker" not found' \`,
			replacement: `grep -F 'unrelated service failure' \`,
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
			name:  "CRD predecessor Apply diagnostic broadens guard ownership",
			child: "crd-upgrade",
			old: ".metadata.annotations[\"operator.ptah.dev/release-name\"] == $release and\n" +
				"\t\t\t    .metadata.annotations[\"operator.ptah.dev/release-namespace\"] == $namespace\n" +
				"\t\t\t  ) | {",
			replacement: ".metadata.annotations[\"operator.ptah.dev/release-name\"] == $release\n" +
				"\t\t\t  ) | {",
			wantError: "predecessor Apply diagnostic exact guard ownership",
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
			name:        "CRD predecessor metric sources remain active",
			child:       "crd-upgrade",
			old:         "quiesce_predecessor_metric_sources\n",
			replacement: "true # predecessor metric sources left active\n",
			wantError:   "predecessor metric source quiesce call",
		},
		{
			name:        "CRD predecessor Apply metric source remains active",
			child:       "crd-upgrade",
			old:         `for schema_name in "$PREDECESSOR_JOB_SCHEMA" "$PREDECESSOR_APPLY_SCHEMA"; do`,
			replacement: `for schema_name in "$PREDECESSOR_JOB_SCHEMA"; do`,
			wantError:   "predecessor metric source quiesce implementation",
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
			name:        "CRD exact released chart fresh install removed",
			child:       "crd-upgrade",
			old:         `helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \`,
			replacement: `true # exact released chart fresh install removed \`,
			wantError:   "exact released chart fresh install",
		},
		{
			name:        "CRD certificate Secret identity capture removed",
			child:       "crd-upgrade",
			old:         "capture_certificate_secret_names() {\n",
			replacement: "capture_certificate_secret_names_removed() {\n",
			wantError:   "certificate Secret identity capture implementation",
		},
		{
			name:  "CRD unlabeled certificate Secrets absence removed",
			child: "crd-upgrade",
			old: "\tremaining=$(kube -n \"$E2E_OPERATOR_NAMESPACE\" get \\\n" +
				"\t\t\"secret/$CERTIFICATE_SECRET_NAME\" --ignore-not-found=true -o name)\n" +
				"\t[ -z \"$remaining\" ] ||\n" +
				"\t\tfail \"unlabeled generated certificate Secret/$CERTIFICATE_SECRET_NAME survived uninstall\"\n" +
				"\tremaining=$(kube -n \"$E2E_OPERATOR_NAMESPACE\" get \\\n" +
				"\t\t\"secret/$CERTIFICATE_STAGING_SECRET_NAME\" --ignore-not-found=true -o name)\n" +
				"\t[ -z \"$remaining\" ] ||\n" +
				"\t\tfail \"unlabeled certificate staging Secret/$CERTIFICATE_STAGING_SECRET_NAME survived uninstall\"\n" +
				"\tCERTIFICATE_SECRET_NAME=\n" +
				"\tCERTIFICATE_STAGING_SECRET_NAME=\n",
			replacement: "\tCERTIFICATE_SECRET_NAME=\n\tCERTIFICATE_STAGING_SECRET_NAME=\n",
			wantError:   "unlabeled certificate Secrets exact uninstall absence",
		},
		{
			name:        "CRD upgraded release exact inventory absence removed",
			child:       "crd-upgrade",
			old:         "assert_inventory_resources_absent \\\n\t\t\"$next_sequence_inventory\" \"$next_sequence_marker_name\"",
			replacement: "true # upgraded release exact inventory absence removed",
			wantError:   "upgraded release exact uninstall absence",
		},
		{
			name:        "CRD reinstalled successor exact inventory absence removed",
			child:       "crd-upgrade",
			old:         "assert_inventory_resources_absent \\\n\t\t\"$reinstalled_next_inventory\" \"$reinstalled_next_marker_name\"",
			replacement: "true # reinstalled successor exact inventory absence removed",
			wantError:   "reinstalled successor exact uninstall absence",
		},
		{
			name:        "CRD released chart exact inventory absence removed",
			child:       "crd-upgrade",
			old:         "assert_inventory_resources_absent \\\n\t\t\"$fresh_current_inventory\" \"$fresh_current_marker_name\"",
			replacement: "true # released chart exact inventory absence removed",
			wantError:   "exact released chart inventory absence",
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
			name:        "HA custom metrics proof call removed",
			child:       "high-availability",
			old:         `assert_custom_operator_metrics "$second_holder" "$resolve_failure_counter_before"`,
			replacement: `: # post-failure custom metrics proof removed`,
			wantError:   "post-failure custom metrics proof",
		},
		{
			name:        "HA Resolve failure baseline removed",
			child:       "high-availability",
			old:         `resolve_failure_counter_before=$(read_resolve_operation_failure_counter "$second_holder")`,
			replacement: `resolve_failure_counter_before=0 # baseline proof removed`,
			wantError:   "pre-operation Resolve failure counter baseline",
		},
		{
			name:        "HA prior Resolve metric source exclusion removed",
			child:       "high-availability",
			old:         "assert_prior_resolve_metric_sources_quiesced\n",
			replacement: "true # prior Resolve metric source exclusion removed\n",
			wantError:   "prior Resolve metric source exclusion call",
		},
		{
			name:        "HA Resolve failure increase weakened",
			child:       "high-availability",
			old:         `'BEGIN { exit ! ((current + 0) > (baseline + 0)) }'; then`,
			replacement: `'BEGIN { exit ! ((current + 0) >= (baseline + 0)) }'; then`,
			wantError:   "Resolve failure counter increase proof",
		},
		{
			name:        "HA custom metrics exact families weakened",
			child:       "high-availability",
			old:         `reconciliation_sample == 1 && failure_sample == 1) {`,
			replacement: `reconciliation_sample == 1 || failure_sample == 1) {`,
			wantError:   "custom metrics exact two-family acceptance",
		},
		{
			name:        "HA terminal evidence removed",
			child:       "high-availability",
			old:         `printf '%s\n' 'e2e HA: PASS one Lease, exact RBAC, Pod failover, admitted operation, and custom metrics'`,
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
	shard.requireNonemptyTable(t, len(tests))
	for index, test := range tests {
		if !shard.includes(index) {
			continue
		}
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

func TestUpgradeHookProgressProofRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	shard := activeMutationTestShard(t)
	path := repositoryE2EWiringFiles().crdUpgrade
	source := readE2ESource(t, path)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "lifecycle call removed",
			old:         "\tprove_upgrade_hook_progress_guards\n",
			replacement: "\t: # hook progress proof removed\n",
			wantError:   "one implementation and one lifecycle call",
		},
		{
			name:        "candidate convergence removed",
			old:         "\trun_predecessor_upgrade_proof\n",
			replacement: "\t: # candidate convergence removed\n",
			wantError:   "candidate v2 convergence ordering",
		},
		{
			name:        "adversary UID omitted",
			old:         "\t\t--as-uid \"$HOOK_PROGRESS_ADVERSARY_UID\" \\\n",
			replacement: "",
			wantError:   "UID-bound adversary impersonation",
		},
		{
			name:        "release identity skips validation",
			old:         "\tfor hook_progress_identity in \"$E2E_OPERATOR_NAMESPACE\" \"$E2E_HELM_RELEASE\"; do\n",
			replacement: "\tfor hook_progress_identity in \"$E2E_OPERATOR_NAMESPACE\"; do\n",
			wantError:   "independent namespace and release identity validation",
		},
		{
			name:        "identity length bound removed",
			old:         "\t\t[ \"${#hook_progress_identity}\" -le 63 ] ||\n",
			replacement: "\t\ttrue ||\n",
			wantError:   "independent namespace and release identity validation",
		},
		{
			name:        "Pod main patch authorization removed",
			old:         "\texpect_hook_progress_authorization yes patch pods\n",
			replacement: "\texpect_hook_progress_authorization no patch pods\n",
			wantError:   "least-privilege adversary authorization",
		},
		{
			name:        "Pod identity attack removed",
			old:         "\t\tpatch pod \"$HOOK_PROGRESS_POD_NAME\" --type=json \\\n",
			replacement: "\t\tget pod \"$HOOK_PROGRESS_POD_NAME\" -o json \\\n",
			wantError:   "actual five-path adversary mutation set",
		},
		{
			name:        "temporary hold can satisfy denial",
			old:         "\tif grep -F \"$HOOK_PROGRESS_HOLD_MESSAGE\" \"$stdout\" \"$stderr\" >/dev/null; then\n",
			replacement: "\tif false; then\n",
			wantError:   "exact v2 denial classification",
		},
		{
			name:        "v2 denial polarity inverted",
			old:         "\tif ! grep -F \"$expected_message\" \"$stdout\" \"$stderr\" >/dev/null; then\n",
			replacement: "\tif grep -F \"$expected_message\" \"$stdout\" \"$stderr\" >/dev/null; then\n",
			wantError:   "exact v2 denial classification",
		},
		{
			name:        "hold stability reduced",
			old:         "HOOK_PROGRESS_HOLD_STABILITY_ATTEMPTS=5\n",
			replacement: "HOOK_PROGRESS_HOLD_STABILITY_ATTEMPTS=1\n",
			wantError:   "stable hold publication",
		},
		{
			name:        "next hook released early",
			old:         "\tset_hook_progress_hold_components '[\"crd-manager-preflight\",\"crd-manager\"]'\n",
			replacement: "\tset_hook_progress_hold_components '[\"crd-manager\"]'\n",
			wantError:   "monotonic four-hook release order",
		},
		{
			name:        "target wait loses API timeout",
			old:         "\t\tif kube -n \"$E2E_OPERATOR_NAMESPACE\" get jobs \\\n\t\t\t-l \"app.kubernetes.io/instance=$E2E_HELM_RELEASE,app.kubernetes.io/component=$component\" \\\n\t\t\t--request-timeout=15s \\\n",
			replacement: "\t\tif kube -n \"$E2E_OPERATOR_NAMESPACE\" get jobs \\\n\t\t\t-l \"app.kubernetes.io/instance=$E2E_HELM_RELEASE,app.kubernetes.io/component=$component\" \\\n",
			wantError:   "bounded target observation",
		},
		{
			name:        "background cleanup success latch removed",
			old:         "\tif [ \"$HOOK_PROGRESS_HELM_ACTIVE\" -eq 1 ] && [ -n \"$HOOK_PROGRESS_HELM_PID\" ]; then\n\t\t[ \"$status\" -ne 0 ] || status=1\n",
			replacement: "\tif [ \"$HOOK_PROGRESS_HELM_ACTIVE\" -eq 1 ] && [ -n \"$HOOK_PROGRESS_HELM_PID\" ]; then\n",
			wantError:   "background Helm cleanup",
		},
		{
			name:        "proof returns early",
			old:         "prove_upgrade_hook_progress_guards() {\n",
			replacement: "prove_upgrade_hook_progress_guards() {\n\treturn 0\n",
			wantError:   "early successful return",
		},
		{
			name:        "attack set returns early",
			old:         "exercise_hook_progress_attacks() {\n",
			replacement: "exercise_hook_progress_attacks() {\n\treturn 0\n",
			wantError:   "early successful return",
		},
		{
			name:        "denial classifier returns early",
			old:         "expect_hook_progress_guard_denial() {\n",
			replacement: "expect_hook_progress_guard_denial() {\n\treturn 0\n",
			wantError:   "early successful return",
		},
	}
	shard.requireNonemptyTable(t, len(tests))
	for index, test := range tests {
		if !shard.includes(index) {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := writeMutatedE2ESource(t, "e2e-crd-upgrade.sh", source, test.old, test.replacement)
			err := verifyUpgradeHookProgressProofSource(mutated)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyUpgradeHookProgressProofSource() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func verifyUpgradeHookProgressProofSource(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	criticalFunctions := []string{
		"hook_progress_adversary_kube",
		"wait_for_hook_progress_hold_ready",
		"probe_hook_progress_hold_state",
		"expect_hook_progress_hold_denial",
		"verify_hook_progress_hold_transition",
		"create_hook_progress_adversary_and_hold",
		"delete_hook_progress_adversary_and_hold",
		"wait_for_hook_progress_target",
		"assert_hook_progress_target_intact",
		"expect_hook_progress_guard_denial",
		"exercise_hook_progress_attacks",
		"assert_helm_stalled_on_hook_progress",
		"prove_upgrade_hook_progress_guards",
	}
	functions := make(map[string][]byte, len(criticalFunctions))
	for _, name := range criticalFunctions {
		body, bodyErr := exactHookProgressFunction(path, contents, name)
		if bodyErr != nil {
			return bodyErr
		}
		functions[name] = body
	}

	if count := strings.Count(string(contents), "prove_upgrade_hook_progress_guards"); count != 2 {
		return fmt.Errorf("%s: hook progress proof must have one implementation and one lifecycle call, found %d", path, count)
	}
	for _, marker := range []string{
		"image_check_matches=$(rendered_hook_job_name crd-manager-image-check -130)",
		".metadata.name == $expected_name and",
		"Ptah hook parent origin guard rejected an unauthorized Job",
		"Ptah hook Pod origin guard rejected an unauthorized Pod",
	} {
		if !strings.Contains(string(contents), marker) {
			return fmt.Errorf("%s: rendered hook name binding or exact v2 denial is missing: %s", path, marker)
		}
	}
	if !strings.Contains(string(contents), "HOOK_PROGRESS_HOLD_STABILITY_ATTEMPTS=5") {
		return fmt.Errorf("%s: stable hold publication must require five observations", path)
	}

	adversary := functions["hook_progress_adversary_kube"]
	if err := requireHookProgressMarkers(path, "UID-bound adversary impersonation", adversary, []string{
		`[ -n "$HOOK_PROGRESS_ADVERSARY_UID" ]`,
		`--as "system:serviceaccount:$E2E_OPERATOR_NAMESPACE:$HOOK_PROGRESS_ADVERSARY"`,
		`--as-uid "$HOOK_PROGRESS_ADVERSARY_UID"`,
	}); err != nil {
		return err
	}
	create := functions["create_hook_progress_adversary_and_hold"]
	if err := requireHookProgressMarkers(path, "independent namespace and release identity validation", create, []string{
		`for hook_progress_identity in "$E2E_OPERATOR_NAMESPACE" "$E2E_HELM_RELEASE"; do`,
		`grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'`,
		`[ "${#hook_progress_identity}" -le 63 ]`,
	}); err != nil {
		return err
	}
	if err := requireHookProgressMarkers(path, "least-privilege adversary authorization", create, []string{
		`resources: ["jobs"]`,
		`resources: ["jobs/status"]`,
		`resources: ["pods", "pods/status"]`,
		`expect_hook_progress_authorization yes delete jobs`,
		`expect_hook_progress_authorization yes patch jobs/status`,
		`expect_hook_progress_authorization yes patch pods`,
		`expect_hook_progress_authorization yes patch pods/status`,
		`expect_hook_progress_authorization no create jobs`,
		`expect_hook_progress_authorization no update jobs`,
		`expect_hook_progress_authorization no update pods`,
	}); err != nil {
		return err
	}
	if !regexp.MustCompile(`(?m)^[ \t]*expect_hook_progress_authorization yes patch pods[ \t]*$`).Match(create) {
		return fmt.Errorf("%s: least-privilege adversary authorization lacks Pod main-resource patch", path)
	}
	if err := requireHookProgressMarkers(path, "stable hold publication", create, []string{
		`resources: ["jobs/status"]`,
		`resources: ["pods/status"]`,
		`expression: 'request.userInfo.username == "$adversary_username"'`,
		`expect_hook_progress_hold_denial`,
		`--dry-run=server`,
	}); err != nil {
		return err
	}
	probe := functions["probe_hook_progress_hold_state"]
	if err := requireHookProgressMarkers(path, "stable hold publication", probe, []string{
		`--request-timeout=15s`,
		`[ "$expected_state" = admitted ]`,
		`[ "$expected_state" = denied ]`,
		`grep -F "$HOOK_PROGRESS_HOLD_MESSAGE"`,
	}); err != nil {
		return err
	}
	transition := functions["verify_hook_progress_hold_transition"]
	if err := requireHookProgressMarkers(path, "stable hold publication", transition, []string{
		`probe_hook_progress_hold_state admitted`,
		`probe_hook_progress_hold_state denied`,
		`[ "$stable_attempts" -eq "$HOOK_PROGRESS_HOLD_STABILITY_ATTEMPTS" ]`,
	}); err != nil {
		return err
	}

	classifier := functions["expect_hook_progress_guard_denial"]
	if err := requireHookProgressMarkers(path, "exact v2 denial classification", classifier, []string{
		`if "$@" >"$stdout" 2>"$stderr"; then`,
		`if grep -F "$HOOK_PROGRESS_HOLD_MESSAGE" "$stdout" "$stderr" >/dev/null; then`,
		`if ! grep -F "$expected_message" "$stdout" "$stderr" >/dev/null; then`,
	}); err != nil {
		return err
	}
	attacks := functions["exercise_hook_progress_attacks"]
	attackSource := string(attacks)
	if strings.Count(attackSource, "expect_hook_progress_guard_denial") != 5 ||
		strings.Count(attackSource, `"$HOOK_PROGRESS_JOB_DENIAL"`) != 3 ||
		strings.Count(attackSource, `"$HOOK_PROGRESS_POD_DENIAL"`) != 2 ||
		strings.Count(attackSource, "--subresource=status") != 3 ||
		strings.Count(attackSource, "--request-timeout=15s") != 5 ||
		strings.Contains(attackSource, "--dry-run") {
		return fmt.Errorf("%s: actual five-path adversary mutation set is incomplete or simulated", path)
	}
	if err := requireHookProgressMarkers(path, "actual five-path adversary mutation set", attacks, []string{
		`delete job "$HOOK_PROGRESS_JOB_NAME" --wait=false`,
		`"type":"Complete","status":"True"`,
		`"type":"Failed","status":"True"`,
		`/metadata/labels/app.kubernetes.io~1component`,
		`patch pod "$HOOK_PROGRESS_POD_NAME" --type=json`,
		`"phase":"Succeeded"`,
		`assert_hook_progress_target_intact "$component"`,
	}); err != nil {
		return err
	}

	waitTarget := functions["wait_for_hook_progress_target"]
	if strings.Count(string(waitTarget), "--request-timeout=15s") != 2 {
		return fmt.Errorf("%s: bounded target observation must time out Job and Pod reads", path)
	}
	if err := requireHookProgressMarkers(path, "rendered hook name binding", waitTarget, []string{
		`expected_name=$(expected_hook_progress_name "$component")`,
		`.metadata.name == $expected_name and`,
		`.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded,hook-failed"`,
		`.name == $job and .uid == $uid and .controller == true`,
	}); err != nil {
		return err
	}
	intact := functions["assert_hook_progress_target_intact"]
	if strings.Count(string(intact), "--request-timeout=15s") != 2 {
		return fmt.Errorf("%s: bounded target observation must time out intact Job and Pod reads", path)
	}
	if err := requireHookProgressMarkers(path, "target identity remains intact", intact, []string{
		`.metadata.uid == $uid and .metadata.deletionTimestamp == null`,
		`((.status.phase // "") != "Succeeded")`,
		`((.status.phase // "") != "Failed")`,
	}); err != nil {
		return err
	}

	proof := functions["prove_upgrade_hook_progress_guards"]
	proofContract := []sourceContractStep{
		exactSourceLine("progress resource setup", `create_hook_progress_adversary_and_hold`),
		exactSourceLine("background Helm PID capture", `HOOK_PROGRESS_HELM_PID=$!`),
		exactSourceLine("image-check observation", `wait_for_hook_progress_target crd-manager-image-check -130`),
		exactSourceLine("image-check attack", `exercise_hook_progress_attacks crd-manager-image-check`),
		exactSourceLine("image-check hold", `assert_helm_stalled_on_hook_progress crd-manager-image-check hook-identity-probe -105`),
		exactSourceLine("image-check hold shrink", `set_hook_progress_hold_components '["hook-identity-probe","crd-manager-preflight","crd-manager"]'`),
		exactSourceLine("image-check release", `verify_hook_progress_hold_transition crd-manager-image-check hook-identity-probe`),
		exactSourceLine("identity observation", `wait_for_hook_progress_target hook-identity-probe -105`),
		exactSourceLine("identity attack", `exercise_hook_progress_attacks hook-identity-probe`),
		exactSourceLine("identity hold", `assert_helm_stalled_on_hook_progress hook-identity-probe crd-manager-preflight -60`),
		exactSourceLine("identity hold shrink", `set_hook_progress_hold_components '["crd-manager-preflight","crd-manager"]'`),
		exactSourceLine("identity release", `verify_hook_progress_hold_transition hook-identity-probe crd-manager-preflight`),
		exactSourceLine("preflight observation", `wait_for_hook_progress_target crd-manager-preflight -60`),
		exactSourceLine("preflight attack", `exercise_hook_progress_attacks crd-manager-preflight`),
		exactSourceLine("preflight hold", `assert_helm_stalled_on_hook_progress crd-manager-preflight crd-manager 0`),
		exactSourceLine("preflight hold shrink", `set_hook_progress_hold_components '["crd-manager"]'`),
		exactSourceLine("preflight release", `verify_hook_progress_hold_transition crd-manager-preflight crd-manager`),
		exactSourceLine("reconcile observation", `wait_for_hook_progress_target crd-manager 0`),
		exactSourceLine("reconcile attack", `exercise_hook_progress_attacks crd-manager`),
		exactSourceLine("reconcile hold", `assert_helm_stalled_on_hook_progress crd-manager '' ''`),
		exactSourceLine("reconcile hold release", `set_hook_progress_hold_components '[]'`),
		exactSourceLine("reconcile release", `verify_hook_progress_hold_transition crd-manager ''`),
		exactSourceLine("background Helm join", `if wait "$HOOK_PROGRESS_HELM_PID"; then`),
		exactSourceLine("exact revision proof", `[ "$after_revision" -eq $((before_revision + 1)) ] ||`),
		exactSourceLine("progress resource teardown", `delete_hook_progress_adversary_and_hold`),
		exactSourceLine("terminal progress evidence", `printf '%s\n' 'e2e crd: retained v2 hook progress proof passed'`),
	}
	if err := verifyOrderedSourceContract(path+" hook progress lifecycle", proof, proofContract); err != nil {
		return fmt.Errorf("monotonic four-hook release order: %w", err)
	}

	runUpgrade, err := exactHookProgressFunction(path, contents, "run_upgrade_proof")
	if err != nil {
		return err
	}
	if err := verifyOrderedSourceContract(path+" upgrade lifecycle", runUpgrade, []sourceContractStep{
		exactSourceLine("candidate v2 convergence", `run_predecessor_upgrade_proof`),
		exactSourceLineSequence("installed controller image identity", []string{
			`helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \`,
			`-o json >"$WORK_DIR/release-values.json"`,
			`PROOF_CONTROLLER_IMAGE=$(production_controller_image_from_values \`,
			`"$WORK_DIR/release-values.json")`,
		}),
		exactSourceLine("hook progress proof", `prove_upgrade_hook_progress_guards`),
		exactSourceLine("later missing CRD case", `printf '%s\n' 'e2e crd: proving a missing CRD aborts Helm upgrade without recreation'`),
	}); err != nil {
		return fmt.Errorf("candidate v2 convergence ordering: %w", err)
	}

	cleanup, err := exactHookProgressFunction(path, contents, "cleanup")
	if err != nil {
		return err
	}
	if err := requireHookProgressMarkers(path, "background Helm cleanup", cleanup, []string{
		`if [ "$HOOK_PROGRESS_HELM_ACTIVE" -eq 1 ] && [ -n "$HOOK_PROGRESS_HELM_PID" ]; then`,
		`[ "$status" -ne 0 ] || status=1`,
		`kill "$HOOK_PROGRESS_HELM_PID"`,
		`wait "$HOOK_PROGRESS_HELM_PID"`,
	}); err != nil {
		return err
	}
	if strings.Count(string(cleanup), `[ "$status" -ne 0 ] || status=1`) != 2 {
		return fmt.Errorf("%s: background Helm cleanup and progress resource cleanup must fail leaked success latches", path)
	}
	if err := requireHookProgressMarkers(path, "exact progress resource cleanup", cleanup, []string{
		`validatingadmissionpolicybinding/$HOOK_PROGRESS_HOLD_POLICY`,
		`validatingadmissionpolicy/$HOOK_PROGRESS_HOLD_POLICY`,
		`job/$HOOK_PROGRESS_HOLD_PROBE`,
		`rolebinding/$HOOK_PROGRESS_ADVERSARY`,
		`role/$HOOK_PROGRESS_ADVERSARY`,
		`serviceaccount/$HOOK_PROGRESS_ADVERSARY`,
	}); err != nil {
		return err
	}
	for _, name := range []string{
		"expect_hook_progress_guard_denial",
		"exercise_hook_progress_attacks",
		"prove_upgrade_hook_progress_guards",
	} {
		if regexp.MustCompile(`(?m)^[ \t]*(?:return|exit)[ \t]+0(?:[ \t]|$)`).Match(functions[name]) {
			return fmt.Errorf("%s: %s contains an early successful return", path, name)
		}
	}
	if regexp.MustCompile(`(?m)^[^\n]*(?:cat|head|tail)[^\n]*hook-progress-(?:upgrade|denial|hold)`).Match(contents) {
		return fmt.Errorf("%s: private progress evidence is emitted", path)
	}
	return nil
}

func exactHookProgressFunction(path string, contents []byte, name string) ([]byte, error) {
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)[ \t]*\{\r?\n.*?^\}[ \t]*\r?$`)
	matches := pattern.FindAll(contents, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("%s: %s must have exactly one auditable function body, found %d", path, name, len(matches))
	}
	return matches[0], nil
}

func requireHookProgressMarkers(path, contract string, contents []byte, markers []string) error {
	for _, marker := range markers {
		if !strings.Contains(string(contents), marker) {
			return fmt.Errorf("%s: %s lacks %q", path, contract, marker)
		}
	}
	return nil
}

func repositoryE2EWiringFiles() e2eWiringFiles {
	return e2eWiringFiles{
		makefile:                   filepath.Join("..", makefilePath),
		harness:                    filepath.Join("..", e2eHarnessPath),
		kindConfig:                 filepath.Join("..", e2eKindConfigPath),
		apiServerEndpointFilter:    filepath.Join("..", apiServerEndpointFilterPath),
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
