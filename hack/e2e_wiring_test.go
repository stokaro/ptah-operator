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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

func TestKubernetesSupportImageResolverFollowsShiftedManifest(t *testing.T) {
	t.Parallel()

	digest := func(character string) string { return strings.Repeat(character, 64) }
	manifest := supportManifest{
		SchemaVersion: 1,
		Policy:        "upstream-active-minors",
		WindowSize:    3,
		LastVerified:  "2026-09-05",
		KindVersion:   "v0.33.0",
		Releases: []release{
			{Minor: "1.36", NodeImage: "kindest/node:v1.36.7@sha256:" + digest("6")},
			{Minor: "1.37", NodeImage: "kindest/node:v1.37.3@sha256:" + digest("7")},
			{Minor: "1.38", NodeImage: "kindest/node:v1.38.0@sha256:" + digest("8")},
		},
	}
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "kubernetes.json")
	if err := os.WriteFile(manifestPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := repositoryE2EWiringFiles().supportImageResolver

	output, err := exec.Command(resolver, manifestPath, "1.38.0").Output()
	if err != nil {
		t.Fatalf("resolve shifted-window newest release: %v", err)
	}
	want := "kindest/node:v1.38.0@sha256:" + digest("8") + "\n"
	if string(output) != want {
		t.Fatalf("resolved image = %q, want %q", output, want)
	}

	if err := exec.Command(resolver, manifestPath, "1.35.9").Run(); err == nil {
		t.Fatal("resolver accepted a release outside the shifted support manifest")
	}
}

func TestVerifyKubernetesSupportWindowWiringRejectsHardCodedAllowlists(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	tests := []struct {
		name     string
		path     string
		marker   string
		variable string
		apply    func(*e2eWiringFiles, string)
	}{
		{
			name:     "root harness",
			path:     files.harness,
			marker:   `K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`,
			variable: "K8S_MAJOR_MINOR",
			apply:    func(files *e2eWiringFiles, path string) { files.harness = path },
		},
		{
			name:     "CRD upgrade child",
			path:     files.crdUpgrade,
			marker:   `KUBERNETES_MAJOR_MINOR=$(printf '%s\n' "$E2E_KUBERNETES_VERSION" | cut -d. -f1,2)`,
			variable: "KUBERNETES_MAJOR_MINOR",
			apply:    func(files *e2eWiringFiles, path string) { files.crdUpgrade = path },
		},
	}
	mutants := []struct {
		name      string
		allowlist func(string) string
		wantError string
	}{
		{
			name: "joined case label",
			allowlist: func(variable string) string {
				return fmt.Sprintf("case \"$%s\" in\n1.35 | 1.36 | 1.37) ;;\nesac", variable)
			},
			wantError: "Kubernetes minor case blocks",
		},
		{
			name: "split case labels",
			allowlist: func(variable string) string {
				return fmt.Sprintf("case \"$%s\" in\n1.35) ;;\n1.36) ;;\n1.37) ;;\nesac", variable)
			},
			wantError: "Kubernetes minor case blocks",
		},
		{
			name: "extended grep expression",
			allowlist: func(variable string) string {
				return fmt.Sprintf("printf '%%s\\n' \"$%s\" | grep -Eq '^(1\\.35|1\\.36|1\\.37)$'", variable)
			},
			wantError: "extended-regexp grep",
		},
	}
	for _, script := range tests {
		for _, mutant := range mutants {
			t.Run(script.name+"/"+mutant.name, func(t *testing.T) {
				t.Parallel()
				source := readE2ESource(t, script.path)
				replacement := script.marker + "\n" + mutant.allowlist(script.variable)
				mutatedPath := writeMutatedE2ESource(t, filepath.Base(script.path), source, script.marker, replacement)
				mutated := files
				script.apply(&mutated, mutatedPath)
				err := verifyKubernetesSupportWindowWiring(mutated)
				if err == nil || !strings.Contains(err.Error(), mutant.wantError) {
					t.Fatalf("verifyKubernetesSupportWindowWiring() error = %v, want substring %q", err, mutant.wantError)
				}
			})
		}
	}
}

func TestVerifyE2ESourceSnapshotRejectsLivePathMutations(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().harness)
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "chart metadata read from live checkout",
			old:         `chart_version=$(sed -n 's/^version: //p' "$ROOT_DIR/charts/ptah-operator/Chart.yaml")`,
			replacement: `chart_version=$(sed -n 's/^version: //p' "$SOURCE_REPOSITORY_ROOT/charts/ptah-operator/Chart.yaml")`,
			wantError:   "live checkout path escapes",
		},
		{
			name:        "operator build context read from live checkout",
			old:         `--tag "$OPERATOR_IMAGE" "$ROOT_DIR"`,
			replacement: `--tag "$OPERATOR_IMAGE" "$SOURCE_REPOSITORY_ROOT"`,
			wantError:   "original checkout must have only",
		},
		{
			name:        "admission contract read from live checkout",
			old:         `jq -e -f "$ROOT_DIR/hack/admission-schema-contract.jq" \`,
			replacement: `jq -e -f "$SOURCE_REPOSITORY_ROOT/hack/admission-schema-contract.jq" \`,
			wantError:   "live checkout path escapes",
		},
		{
			name:        "child evidence script read from live checkout",
			old:         `run_recorded_phase cert-rotation "$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: `"$SOURCE_REPOSITORY_ROOT/hack/e2e-cert-rotation.sh"`,
			wantError:   "live checkout path escapes",
		},
		{
			name:        "snapshot archive replaced by live copy",
			old:         `git -C "$BOOTSTRAP_ROOT_DIR" archive --format=tar \`,
			replacement: `tar -cf "$SOURCE_SNAPSHOT_ARCHIVE" -C "$BOOTSTRAP_ROOT_DIR" .`,
			wantError:   "exact source archive",
		},
		{
			name:        "caller-controlled environment bypass",
			old:         `if [ "${1:-}" != --source-snapshot ]; then`,
			replacement: `if [ "${E2E_SOURCE_SNAPSHOT_ACTIVE:-0}" -eq 0 ]; then`,
			wantError:   "outer snapshot branch",
		},
		{
			name:        "snapshot comparison omitted",
			old:         "\nverify_snapshot_source\n",
			replacement: "\n: snapshot verification skipped\n",
			wantError:   "snapshot content verification",
		},
		{
			name:        "Docker access before snapshot comparison",
			old:         "\nverify_snapshot_source\n",
			replacement: "\ndocker --context \"$DOCKER_CONTEXT\" info >/dev/null\nverify_snapshot_source\n",
			wantError:   "snapshot must be active before first Docker access",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := writeMutatedE2ESource(t, "e2e-kind.sh", source, test.old, test.replacement)
			err := verifyE2ESourceSnapshot(mutated, []byte(readE2ESource(t, mutated)))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2ESourceSnapshot() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestE2ESnapshotComparisonRejectsUncommittedInputs(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().harness)
	start := strings.Index(source, "verify_snapshot_source() (\n")
	if start < 0 {
		t.Fatal("snapshot comparison function is missing")
	}
	end := strings.Index(source[start:], "\n)\n")
	if end < 0 {
		t.Fatal("snapshot comparison function terminator is missing")
	}
	comparison := source[start : start+end+3]
	run := func(t *testing.T, dir, name string, args ...string) []byte {
		t.Helper()
		command := exec.Command(name, args...)
		command.Dir = dir
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
		return output
	}
	repository := t.TempDir()
	run(t, repository, "git", "init", "--quiet", "--object-format=sha1")
	for path, content := range map[string]string{
		".gitignore": "ignored\n",
		"input":      "committed source\n",
		"entry.sh":   "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(repository, "entry.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, repository, "git", "add", ".")
	run(t, repository, "git", "-c", "user.name=Snapshot Test", "-c", "user.email=snapshot@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "Test fixture")
	revision := strings.TrimSpace(string(run(t, repository, "git", "rev-parse", "HEAD")))
	archive := filepath.Join(t.TempDir(), "source.tar")
	run(t, repository, "git", "archive", "--format=tar", "--output="+archive, revision)

	tests := []struct {
		name   string
		mutate func(string) error
	}{
		{name: "exact archive"},
		{name: "modified tracked source", mutate: func(root string) error {
			return os.WriteFile(filepath.Join(root, "input"), []byte("uncommitted source\n"), 0o600)
		}},
		{name: "ignored build input", mutate: func(root string) error {
			return os.WriteFile(filepath.Join(root, "ignored"), []byte("not in the commit\n"), 0o600)
		}},
		{name: "executable mode changed", mutate: func(root string) error {
			return os.Chmod(filepath.Join(root, "entry.sh"), 0o600)
		}},
		{name: "same-content external symlink", mutate: func(root string) error {
			path := filepath.Join(root, "input")
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(repository, "input"), path)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			run(t, root, "tar", "-xf", archive, "-C", root)
			if test.mutate != nil {
				if err := test.mutate(root); err != nil {
					t.Fatal(err)
				}
			}
			temporary := t.TempDir()
			command := exec.Command("sh", "-eu", "-c", "fail() { printf '%s\\n' \"$*\" >&2; exit 1; }\n"+comparison+"\nverify_snapshot_source\n")
			command.Env = append(os.Environ(), "ROOT_DIR="+root, "SOURCE_REPOSITORY_ROOT="+repository,
				"CONTROLLER_REVISION="+revision, "TMPDIR="+temporary)
			output, err := command.CombinedOutput()
			if test.mutate == nil && err != nil {
				t.Fatalf("exact source was rejected: %v\n%s", err, output)
			}
			if test.mutate != nil && (err == nil || !strings.Contains(string(output), "snapshot differs from the exact operator commit")) {
				t.Fatalf("uncommitted input was not rejected: %v\n%s", err, output)
			}
			leftovers, err := filepath.Glob(filepath.Join(temporary, "ptah-operator-e2e-source-verification.*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("snapshot comparison left temporary files: entries=%v err=%v", leftovers, err)
			}
		})
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

// The filter decides two things at once: whether the control plane has reached
// the shape the bootstrap waits for, and whether it is one waiting cannot
// repair. A reading sorted into the wrong one of those costs either a real
// refusal or ninety minutes of a lifecycle, so each is pinned here.
func TestControlPlaneShapeFilterSeparatesUnreadyFromWrong(t *testing.T) {
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
		want   string
	}{
		{name: "all exact static Pods are running and ready", want: "ready"},
		// A control plane that is still joining holds fewer pods than this
		// asserts. Every one of these is a cluster the bootstrap waits for.
		{
			name: "one control plane has not started its static Pods",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["labels"] = map[string]any{"component": "etcd"}
			},
			want: "incomplete",
		},
		{
			name: "deleting API server Pod",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-05T00:01:00Z"
			},
			want: "incomplete",
		},
		{
			name: "non-running API server Pod",
			mutate: func(pod map[string]any) {
				pod["status"].(map[string]any)["phase"] = "Failed"
			},
			want: "incomplete",
		},
		{
			name: "API server Pod Ready is false",
			mutate: func(pod map[string]any) {
				conditions := pod["status"].(map[string]any)["conditions"].([]any)
				conditions[0].(map[string]any)["status"] = "False"
			},
			want: "incomplete",
		},
		{
			name: "API server container is not ready",
			mutate: func(pod map[string]any) {
				statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
				statuses[0].(map[string]any)["ready"] = false
			},
			want: "incomplete",
		},
		{
			name: "API server container is not running",
			mutate: func(pod map[string]any) {
				statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
				statuses[0].(map[string]any)["state"] = map[string]any{"terminated": map[string]any{"exitCode": 1}}
			},
			want: "incomplete",
		},
		// Waiting repairs none of these, so each is a refusal on the reading
		// that shows it.
		{
			name: "non-static API server Pod",
			mutate: func(pod map[string]any) {
				delete(pod["metadata"].(map[string]any), "annotations")
			},
			want: "wrong",
		},
		{
			name: "misnamed API server Pod",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["name"] = "replacement-api-server"
			},
			want: "wrong",
		},
		{
			name: "API server Pod off the control plane",
			mutate: func(pod map[string]any) {
				pod["metadata"].(map[string]any)["name"] = "kube-apiserver-" + cluster + "-worker"
				pod["spec"].(map[string]any)["nodeName"] = cluster + "-worker"
			},
			want: "wrong",
		},
		{
			name: "API server runtime-config replaced",
			mutate: func(pod map[string]any) {
				container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
				container["command"] = []any{"/usr/local/bin/kube-apiserver", "--feature-gates=GenericWorkload=true"}
			},
			want: "wrong",
		},
		{
			name: "API server feature gates replaced",
			mutate: func(pod map[string]any) {
				container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
				container["command"] = []any{
					"/usr/local/bin/kube-apiserver",
					"--feature-gates=ExpandedDNSConfig=true",
					"--runtime-config=api/all=true",
				}
			},
			want: "wrong",
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
				"-r",
				"--arg", "expected", "GenericWorkload=true",
				"--arg", "cluster", cluster,
				apiServerFeatureGateScopeFilter(t),
			)
			command.Stdin = strings.NewReader(string(encoded))
			output, runErr := command.CombinedOutput()
			if runErr != nil {
				t.Fatalf("control-plane shape filter failed: %v; jq output = %q", runErr, output)
			}
			verdict, detail, _ := strings.Cut(strings.TrimSpace(string(output)), " ")
			if verdict != test.want {
				t.Fatalf("control-plane shape verdict = %q, want %q; detail = %q", verdict, test.want, detail)
			}
			if detail == "" {
				t.Fatalf("control-plane shape verdict %q carries nothing a reader can act on", verdict)
			}
		})
	}
}

func TestVerifyKindHAConfig(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	source := readE2ESource(t, files.kindConfig)
	if err := verifyKindHAConfig(files.kindConfig, files.kindIsolationWorker); err != nil {
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
			// A label on the worker everything else runs on is a selector a
			// Pod meant for the isolation worker could land on instead.
			name:        "worker labelled",
			old:         "  - role: worker\n",
			replacement: "  - role: worker\n    labels:\n      operator.ptah.run/e2e-isolation: \"true\"\n",
			want:        "only the isolation worker is labelled",
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
			err := verifyKindHAConfig(path, files.kindIsolationWorker)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyKindHAConfig() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

// The isolation worker is audited the way the driver builds it: appended to the
// template, as one more node. Each refusal is a worker that would take more
// than the one Apply down with it, or let that Apply land somewhere else.
func TestVerifyKindHAConfigRefusesAnIsolationWorkerThatIsNotIsolated(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	fragment := readE2ESource(t, files.kindIsolationWorker)
	joinPatch := `      - |
        kind: JoinConfiguration
        nodeRegistration:
          taints:
            - key: operator.ptah.run/e2e-isolation
              value: "true"
              effect: NoSchedule
`
	kubeletPatch := `      - |
        kind: KubeletConfiguration
        apiVersion: kubelet.config.k8s.io/v1beta1
        featureGates:
          KubeletInUserNamespace: true
`
	for _, test := range []struct {
		name        string
		old         string
		replacement string
		want        string
	}{
		{
			name:        "untainted, so the scheduler places anything on it",
			old:         joinPatch,
			replacement: "",
			want:        "exactly the isolation taint",
		},
		{
			name:        "a taint the scheduler may ignore",
			old:         "              effect: NoSchedule\n",
			replacement: "              effect: PreferNoSchedule\n",
			want:        "exactly the isolation taint",
		},
		{
			name:        "a taint that evicts what already runs there",
			old:         "              effect: NoSchedule\n",
			replacement: "              effect: NoExecute\n",
			want:        "exactly the isolation taint",
		},
		{
			name:        "unlabelled, so nothing can select it",
			old:         "    labels:\n      operator.ptah.run/e2e-isolation: \"true\"\n",
			replacement: "",
			want:        "exactly the label",
		},
		{
			name:        "labelled with another key",
			old:         "      operator.ptah.run/e2e-isolation: \"true\"\n",
			replacement: "      operator.ptah.run/e2e-isolated: \"true\"\n",
			want:        "exactly the label",
		},
		{
			name:        "a second label",
			old:         "      operator.ptah.run/e2e-isolation: \"true\"\n",
			replacement: "      operator.ptah.run/e2e-isolation: \"true\"\n      operator.ptah.run/e2e-apply-gate: open\n",
			want:        "exactly the label",
		},
		{
			name:        "promoted to a control plane",
			old:         "  - role: worker\n",
			replacement: "  - role: control-plane\n",
			want:        "isolation worker's role",
		},
		{
			name:        "kubelet contract omitted",
			old:         kubeletPatch,
			replacement: "",
			want:        "kubelet feature-gate patch and exactly the isolation taint",
		},
		{
			name:        "two nodes appended",
			old:         fragment,
			replacement: fragment + fragment,
			want:        "want exactly five",
		},
		{
			name:        "not a node of the list",
			old:         "  - role: worker\n",
			replacement: "nodes:\n  - role: worker\n",
			want:        "decode",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedE2ESource(t, "kind-isolation-worker.yaml.tmpl", fragment, test.old, test.replacement)
			err := verifyKindHAConfig(files.kindConfig, path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyKindHAConfig() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

// The node inventory filter in assert_kind_ha_topology, run as the driver runs
// it. It is taken out of the audited contract rather than out of the driver,
// so what passes here is the program the audit holds the driver to.
func TestKindHATopologyFilterHoldsTheIsolationWorkerToItsSuite(t *testing.T) {
	t.Parallel()
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal(err)
	}
	const opening = "--arg key \"$ISOLATION_NODE_KEY\" '\n"
	const closing = "\n    ' \"$NODE_READINESS_FILE\""
	start := strings.Index(kindHATopologyContract, opening)
	end := strings.Index(kindHATopologyContract, closing)
	if start < 0 || end <= start {
		t.Fatal("the node inventory filter could not be found in the kind HA topology contract")
	}
	filter := kindHATopologyContract[start+len(opening) : end]
	key := strings.TrimPrefix(isolationNodeKeyDeclaration, "ISOLATION_NODE_KEY=")

	type node struct {
		name, label  string
		controlPlane bool
		taints       []map[string]string
		ready        string
	}
	isolationTaint := []map[string]string{{"key": key, "value": "true", "effect": "NoSchedule"}}
	inventory := func(nodes ...node) string {
		items := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			labels := map[string]string{"kubernetes.io/hostname": "c-" + n.name}
			if n.controlPlane {
				labels["node-role.kubernetes.io/control-plane"] = ""
			}
			if n.label != "" {
				labels[key] = n.label
			}
			item := map[string]any{
				"metadata": map[string]any{"name": "c-" + n.name, "labels": labels},
				"status":   map[string]any{"conditions": []map[string]string{{"type": "Ready", "status": n.ready}}},
			}
			if len(n.taints) > 0 {
				item["spec"] = map[string]any{"taints": n.taints}
			}
			items = append(items, item)
		}
		encoded, err := json.Marshal(map[string]any{"items": items})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	controlPlanes := []node{
		{name: "control-plane", controlPlane: true, ready: "True"},
		{name: "control-plane2", controlPlane: true, ready: "True"},
		{name: "control-plane3", controlPlane: true, ready: "True"},
	}
	worker := node{name: "worker", ready: "True"}
	isolated := node{name: "worker2", label: "true", taints: isolationTaint, ready: "True"}
	with := func(nodes ...node) string {
		return inventory(append(append([]node(nil), controlPlanes...), nodes...)...)
	}

	for _, test := range []struct {
		name      string
		isolation bool
		nodes     string
		accept    bool
	}{
		{name: "four nodes where no suite declared the worker", isolation: false, nodes: with(worker), accept: true},
		{name: "five nodes where the suite declared it", isolation: true, nodes: with(worker, isolated), accept: true},
		{name: "the worker missing where the suite declared it", isolation: true, nodes: with(worker)},
		{name: "the worker present where no suite declared it", isolation: false, nodes: with(worker, isolated)},
		{name: "the worker without its taint", isolation: true, nodes: with(worker, node{name: "worker2", label: "true", ready: "True"})},
		{name: "the worker without its label", isolation: true, nodes: with(worker, node{name: "worker2", taints: isolationTaint, ready: "True"})},
		{name: "a taint the scheduler may ignore", isolation: true, nodes: with(worker, node{name: "worker2", label: "true",
			taints: []map[string]string{{"key": key, "value": "true", "effect": "PreferNoSchedule"}}, ready: "True"})},
		{name: "the label on the worker everything runs on", isolation: true,
			nodes: with(node{name: "worker", label: "true", ready: "True"}, isolated)},
		{name: "the taint on the worker everything runs on", isolation: true,
			nodes: with(node{name: "worker", taints: isolationTaint, ready: "True"}, isolated)},
		{name: "the worker not ready", isolation: true, nodes: with(worker, node{name: "worker2", label: "true", taints: isolationTaint, ready: "False"})},
		{name: "the worker under another name", isolation: true, nodes: with(worker, node{name: "worker3", label: "true", taints: isolationTaint, ready: "True"})},
		{name: "the worker made a control plane", isolation: true,
			nodes: with(worker, node{name: "worker2", label: "true", taints: isolationTaint, controlPlane: true, ready: "True"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "nodes.json")
			if err := os.WriteFile(path, []byte(test.nodes), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(jqPath, "-e", "--arg", "cluster", "c",
				"--argjson", "isolation", strconv.FormatBool(test.isolation),
				"--arg", "key", key, filter, path)
			output, err := command.CombinedOutput()
			if accepted := err == nil; accepted != test.accept {
				t.Fatalf("filter accepted = %v, want %v; output %s", accepted, test.accept, output)
			}
		})
	}
}

func TestLateFailureRetryRejectsChangedCandidate(t *testing.T) {
	t.Parallel()
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	functions := strings.Replace(extractE2EShellFunction(t, source, "file_sha256"), "file_sha256() {", "real_file_sha256() {", 1) + "\n" +
		"file_sha256() { real_file_sha256 \"$1\"; if [ \"$checksum_failure_path\" = \"$1\" ]; then return 73; fi; }\n" +
		extractE2EShellFunction(t, source, "assert_late_failure_candidate_unchanged")
	for _, mutation := range []string{"none", "chart", "values", "image", "chart checksum failure", "values checksum failure"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			chartPath := filepath.Join(directory, "candidate.tgz")
			valuesPath := filepath.Join(directory, "candidate-values.json")
			chart, values := []byte("immutable chart fixture"), []byte(`{"image":"candidate"}`)
			chartDigest, valuesDigest := sha256.Sum256(chart), sha256.Sum256(values)
			image := "candidate-image"
			checksumFailurePath := ""
			switch mutation {
			case "chart":
				chart = []byte("replacement chart")
			case "values":
				values = []byte(`{"image":"replacement"}`)
			case "image":
				image = "replacement-image"
			case "chart checksum failure":
				checksumFailurePath = chartPath
			case "values checksum failure":
				checksumFailurePath = valuesPath
			}
			for path, contents := range map[string][]byte{chartPath: chart, valuesPath: values} {
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(shPath, "-c", "set -eu\nfail() { printf '%s\\n' \"$*\" >&2; exit 1; }\n"+functions+"\nassert_late_failure_candidate_unchanged\n")
			command.Env = append(os.Environ(), "E2E_NEXT_CHART_PACKAGE="+chartPath, "E2E_NEXT_VALUES_FILE="+valuesPath,
				"E2E_NEXT_CONTROLLER_IMAGE="+image,
				fmt.Sprintf("late_candidate_chart_sha256=%x", chartDigest), fmt.Sprintf("late_candidate_values_sha256=%x", valuesDigest),
				"late_candidate_image=candidate-image", "checksum_failure_path="+checksumFailurePath)
			output, err := command.CombinedOutput()
			if got, want := err == nil, mutation == "none"; got != want {
				t.Fatalf("candidate retry accepted = %t, want %t: %s", got, want, output)
			}
		})
	}
}

// The blocker stands in for whatever fails after the hook stopped the runtime,
// so it has to let the hook's own scale-down through and refuse only Helm's
// write of the candidate. Both match conditions must hold for the webhook to
// be called, as the API server evaluates them.
func TestLateFailureBlockerMatchesOnlyTheCandidateDeployments(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	substitute := strings.NewReplacer(
		"$E2E_OPERATOR_NAMESPACE", "ptah-system",
		"$CONTROLLER_DEPLOYMENT", "ptah-operator",
		"$ROTATOR_DEPLOYMENT", "ptah-operator-cert-rotator",
		"$E2E_NEXT_CONTROLLER_IMAGE", "registry.invalid/operator@sha256:candidate",
	)
	environment, err := celgo.NewEnv(
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("object", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	var programs []celgo.Program
	for _, condition := range []string{"exact-runtime-deployment", "candidate-image"} {
		pattern := regexp.MustCompile(`(?m)^[\t ]*- name: ` + regexp.QuoteMeta(condition) + `\r?\n[\t ]*expression: '([^'\r\n]+)'[\t ]*\r?$`)
		matches := pattern.FindAllStringSubmatch(source, -1)
		if len(matches) != 1 {
			t.Fatalf("late failure blocker %s condition matches = %d, want 1", condition, len(matches))
		}
		ast, issues := environment.Compile(substitute.Replace(matches[0][1]))
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile late failure blocker %s: %v", condition, issues.Err())
		}
		program, err := environment.Program(ast)
		if err != nil {
			t.Fatalf("build late failure blocker %s: %v", condition, err)
		}
		programs = append(programs, program)
	}

	deployment := func(images ...string) map[string]any {
		containers := make([]any, 0, len(images))
		for _, image := range images {
			containers = append(containers, map[string]any{"image": image})
		}
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}}
	}
	request := func(namespace, name string) map[string]any {
		return map[string]any{"namespace": namespace, "name": name}
	}
	const (
		candidate   = "registry.invalid/operator@sha256:candidate"
		predecessor = "registry.invalid/operator@sha256:predecessor"
	)
	for _, test := range []struct {
		name    string
		request map[string]any
		object  any
		want    bool
	}{
		{name: "Helm writes the candidate controller", request: request("ptah-system", "ptah-operator"), object: deployment(candidate), want: true},
		{name: "Helm writes the candidate rotator", request: request("ptah-system", "ptah-operator-cert-rotator"), object: deployment(candidate), want: true},
		{name: "the hook scales the predecessor to zero", request: request("ptah-system", "ptah-operator"), object: deployment(predecessor)},
		{name: "another Deployment carries the candidate", request: request("ptah-system", "other"), object: deployment(candidate)},
		{name: "the controller name in another namespace", request: request("other", "ptah-operator"), object: deployment(candidate)},
		{name: "a request without an object", request: request("ptah-system", "ptah-operator"), object: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matched := true
			for _, program := range programs {
				result, _, evalErr := program.Eval(map[string]any{"request": test.request, "object": test.object})
				if evalErr != nil {
					t.Fatalf("evaluate late failure blocker: %v", evalErr)
				}
				value, ok := result.Value().(bool)
				if !ok {
					t.Fatalf("late failure blocker result = %T(%v), want bool", result.Value(), result.Value())
				}
				matched = matched && value
			}
			if matched != test.want {
				t.Fatalf("late failure blocker matched = %t, want %t", matched, test.want)
			}
		})
	}
}

// The late failure is told apart from a refusal by its hooks: the reconcile
// hook of the failed revision ran and succeeded, and no hook failed.
func TestLateFailureRevisionClassification(t *testing.T) {
	t.Parallel()

	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required to exercise the embedded late-failure revision classifier")
	}
	filter := lateFailureRevisionFilter(t)
	const (
		expectedRevision      = 4
		expectedReconcileName = "ptah-operator-crd-v1-0123456789ab"
	)
	hook := func(name, kind string, weight any, phase string) map[string]any {
		return map[string]any{
			"name":   name,
			"kind":   kind,
			"weight": weight,
			"events": []any{"pre-install", "pre-upgrade", "pre-rollback"},
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
				hook(expectedReconcileName, "ServiceAccount", -110, "Succeeded"),
				hook(expectedReconcileName, "Job", nil, "Succeeded"),
			},
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "the reconcile hook succeeded and the release failed after it", mutate: func(map[string]any) {}, want: true},
		{
			name: "the reconcile hook failed",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[1].(map[string]any)["last_run"].(map[string]any)["phase"] = "Failed"
			},
		},
		{
			name: "another hook failed",
			mutate: func(status map[string]any) {
				status["hooks"] = append(status["hooks"].([]any), hook("other-hook", "Job", 5, "Failed"))
			},
		},
		{
			name: "the reconcile hook never ran",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[1].(map[string]any)["last_run"].(map[string]any)["phase"] = ""
			},
		},
		{
			name: "another release's reconcile hook",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[1].(map[string]any)["name"] = "other-reconcile"
			},
		},
		{
			name: "two reconcile hooks",
			mutate: func(status map[string]any) {
				status["hooks"] = append(status["hooks"].([]any), hook(expectedReconcileName, "Job", nil, "Succeeded"))
			},
		},
		{
			name: "the reconcile hook is not a pre-upgrade hook",
			mutate: func(status map[string]any) {
				status["hooks"].([]any)[1].(map[string]any)["events"] = []any{"pre-install"}
			},
		},
		{
			name: "another revision",
			mutate: func(status map[string]any) {
				status["version"] = expectedRevision + 1
			},
		},
		{
			name: "the release did not fail",
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
				"--argjson", "expected_revision", strconv.Itoa(expectedRevision),
				"--arg", "expected_reconcile_name", expectedReconcileName,
				filter,
			)
			command.Stdin = strings.NewReader(string(encoded))
			output, runErr := command.CombinedOutput()
			if got := runErr == nil; got != test.want {
				t.Fatalf("late failure revision classification = %t, want %t; jq output = %q", got, test.want, output)
			}
		})
	}
}

func lateFailureRevisionFilter(t *testing.T) string {
	t.Helper()
	source := extractE2EShellFunction(t, readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade), "prove_late_failure_recovery")
	const startMarker = `--arg expected_reconcile_name "$EXPECTED_RECONCILE_HOOK_NAME" '` + "\n"
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatal("late failure revision classifier filter start is missing")
	}
	start += len(startMarker)
	const endMarker = "\n        ' \"$late_status_file\" >/dev/null; then"
	end := strings.Index(source[start:], endMarker)
	if end < 0 {
		t.Fatal("late failure revision classifier filter end is missing")
	}
	return source[start : start+end]
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
		{name: "zero", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 0\n", wantOutput: "0\n"},
		{name: "positive exponent", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 3e+02\n", wantOutput: "3e+02\n"},
		{name: "overflowing exponent", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 1e999\n", wantError: true},
		{name: "duplicate", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 1\nptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 2\n", wantError: true},
		{name: "negative", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} -1\n", wantError: true},
		{name: "non-finite", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} NaN\n", wantError: true},
		{name: "timestamped", metrics: "ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 1 123\n", wantError: true},
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
		"ptah_operator_reconciliations_total{family=\"schema\",result=\"success\"} 2",
		"# HELP ptah_operator_failures_total Total failures.",
		"# TYPE ptah_operator_failures_total counter",
		"ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"} 1e999",
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
  printf '%s\n' 'ptah_operator_reconciliations_total{family="schema",result="success"} 2'
  printf '%s\n' '# HELP ptah_operator_failures_total Total failures.'
  printf '%s\n' '# TYPE ptah_operator_failures_total counter'
  printf 'ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} %s\n' "$1"
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

func TestProductionControllerImageUsesOnlyProductionDigest(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to exercise production controller image extraction")
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	if strings.Contains(source, `.repository + "@" + .testIdentityDigest`) {
		t.Fatal("production controller identity uses the mutually exclusive test-only digest")
	}
	if !strings.Contains(source, `(has("testIdentityDigest") | not)`) {
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
						"repository": "registry.example/ptah/operator",
						"digest":     lowerDigest,
					},
					want: "registry.example/ptah/operator@" + lowerDigest + "\n",
				},
				{
					name: "retired empty test-only digest key",
					image: map[string]any{
						"repository":         "registry.example/ptah/operator",
						"digest":             lowerDigest,
						"testIdentityDigest": "",
					},
					wantError: true,
				},
				{
					name: "retired test-only digest",
					image: map[string]any{
						"repository":         "registry.example/ptah/operator",
						"digest":             lowerDigest,
						"testIdentityDigest": testDigest,
					},
					wantError: true,
				},
				{
					name: "uppercase digest",
					image: map[string]any{
						"repository": "registry.example/ptah/operator",
						"digest":     upperDigest,
					},
					wantError: true,
				},
				{
					name: "missing digest",
					image: map[string]any{
						"repository": "registry.example/ptah/operator",
					},
					wantError: true,
				},
				{
					name: "retired mutable tag key",
					image: map[string]any{
						"repository":      "registry.example/ptah/operator",
						"digest":          lowerDigest,
						"allowMutableTag": false,
					},
					wantError: true,
				},
				{
					name: "repository already has a digest",
					image: map[string]any{
						"repository": "registry.example/ptah/operator@" + testDigest,
						"digest":     lowerDigest,
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
		"chart_version=0.1.0-test\n" +
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
			chartPath := filepath.Join(workDirectory, "release.tgz")
			chartBytes := []byte("exact release chart bytes\x00\x01\xff")
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
		"wait_for_control_plane_component_shape",
	)
	const startMarker = `control_plane_shape_reading=$(jq -r --arg expected "$expected_api_server_feature_gates" --arg cluster "$CLUSTER_NAME" '` + "\n"
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatal("API server component readiness filter start is missing")
	}
	start += len(startMarker)
	const endMarker = "\n\t' \"$control_plane_pods_file\"); then"
	end := strings.Index(source[start:], endMarker)
	if end < 0 {
		t.Fatal("API server component readiness filter end is missing")
	}
	return source[start : start+end]
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
			name:        "Kubernetes minor behavior selector omitted",
			old:         `K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)`,
			replacement: `K8S_MAJOR_MINOR=1.37`,
			wantError:   "Kubernetes minor behavior selector",
		},
		{
			name:        "support manifest resolver bypassed",
			old:         `SUPPORTED_KIND_NODE_IMAGE=$("$ROOT_DIR/hack/e2e-kubernetes-support-image.sh" \`,
			replacement: `SUPPORTED_KIND_NODE_IMAGE=$(printf '%s\n' "$KIND_NODE_IMAGE" \`,
			wantError:   "manifest-backed Kubernetes support membership",
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
			name:        "clean source guard ignores untracked files",
			old:         `E2E_SOURCE_STATUS=$(git -C "$BOOTSTRAP_ROOT_DIR" status --porcelain=v1 --untracked-files=all) ||`,
			replacement: `E2E_SOURCE_STATUS=$(git -C "$BOOTSTRAP_ROOT_DIR" status --porcelain=v1 --untracked-files=no) ||`,
			wantError:   "clean checkout preflight",
		},
		{
			name:        "clean source guard result ignored",
			old:         `[ -z "$E2E_SOURCE_STATUS" ] ||`,
			replacement: `: ||`,
			wantError:   "clean checkout preflight",
		},
		{
			name: "Docker accessed before source snapshot activation",
			old:  `ROOT_DIR=$BOOTSTRAP_ROOT_DIR`,
			replacement: "docker --context \"$DOCKER_CONTEXT\" version >/dev/null\n" +
				`ROOT_DIR=$BOOTSTRAP_ROOT_DIR`,
			wantError: "snapshot must be active before first Docker access",
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
			old: "--label 'operator.ptah.run/e2e-component=task-claim' \\\n\t\t" +
				`--label "operator.ptah.run/e2e-claim-token=${TASK_CLAIM_TOKEN}" \`,
			replacement: "--label 'operator.ptah.run/e2e-component=task-claim' \\\n\t\t" +
				`--label "operator.ptah.run/e2e-owner=${CLUSTER_NAME}" \`,
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
			old:         `        elif $component == "kube-apiserver" and (command_options($pod; "--runtime-config=") | length) != 1 then`,
			replacement: `        elif false then`,
			wantError:   "control-plane component shape contract",
		},
		{
			name:        "static component running status accepted as stale",
			old:         `        ($pod.status.phase == "Running") and`,
			replacement: `        ($pod.status.phase != "") and`,
			wantError:   "control-plane component shape contract",
		},
		{
			name:        "static component container readiness omitted",
			old:         `        ($pod.status.containerStatuses[0].ready == true) and`,
			replacement: `        true and`,
			wantError:   "control-plane component shape contract",
		},
		// A wrong control plane that is waited for instead of refused turns a
		// real refusal into a timeout with a vague message, which is the worse
		// gate this change had to avoid.
		{
			name:        "a wrong control plane is waited for",
			old:         "\t\t\t\t\"wrong \"*)",
			replacement: "\t\t\t\t\"never \"*)",
			wantError:   "control-plane component shape contract",
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
			old:         `if ! jq -e --argjson count "$KIND_NODE_COUNT" '.items | length == $count' "$NODE_READINESS_FILE" >/dev/null; then`,
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
			old: "\t  ((.items | length) == $count) and\n" +
				"\t  all(.items[];\n" +
				"        any((.status.conditions // [])[];\n" +
				`          .type == "Ready" and .status == "True"`,
			replacement: "\t  ((.items | length) == $count) and\n" +
				"\t  all(.items[];\n" +
				"        any((.status.conditions // [])[];\n" +
				`          .status == "True"`,
			wantError: "immediate all-node readiness predicate",
		},
		{
			name:        "immediate readiness predicate accepts a partial topology",
			old:         `((.items | length) == $count) and`,
			replacement: `true and`,
			wantError:   "immediate all-node readiness predicate",
		},
		{
			name:        "immediate readiness predicate counts four nodes whatever the suite declared",
			old:         `jq -e --argjson count "$KIND_NODE_COUNT" '` + "\n\t  ((.items | length) == $count) and",
			replacement: `jq -e '` + "\n\t  ((.items | length) == 4) and",
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
			name:        "current-release Helm values use local tag instead of registry digest",
			old:         `"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET"`,
			replacement: `"" "$MANAGER_PULL_SECRET"`,
			wantError:   "digest-pinned current-release Helm values",
		},
		{
			name:        "current-release Helm values exempt an assumed group from the apply-policy guard",
			old:         `"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET" "$APPLY_POLICY_EXEMPT_GROUPS"`,
			replacement: `"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET" '["system:masters"]'`,
			wantError:   "digest-pinned current-release Helm values",
		},
		{
			name:        "apply-policy guard exempt groups keep every authenticated identity",
			old:         `jq -ce '[.status.userInfo.groups[] | select(. != "system:authenticated")] | select(length > 0)') ||`,
			replacement: `jq -ce '.status.userInfo.groups') ||`,
			wantError:   "apply-policy guard exempt groups read from the harness identity",
		},
		{
			name:        "apply-policy guard exempt groups tolerate a harness with none",
			old:         `fail "the harness identity carries no group the apply-policy guard could exempt"`,
			replacement: `APPLY_POLICY_EXEMPT_GROUPS='[]'`,
			wantError:   "apply-policy guard exempt groups read from the harness identity",
		},
		{
			name:        "release image-pull namespace bootstrap omitted",
			old:         `kubectl --kubeconfig "$KUBECONFIG_FILE" create namespace "$OPERATOR_NAMESPACE" >/dev/null`,
			replacement: `: # namespace bootstrap omitted`,
			wantError:   "release namespace and image-pull bootstrap",
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
			old:         "\t\t\"${CLUSTER_NAME}-worker\" \\\n\t\t\"${CLUSTER_NAME}-worker2\"; do",
			replacement: "\t\t\"${CLUSTER_NAME}-worker2\"; do",
			wantError:   "all-node registry hosts contract",
		},
		{
			name:        "isolation worker registry configuration omitted",
			old:         "\t\t\"${CLUSTER_NAME}-worker\" \\\n\t\t\"${CLUSTER_NAME}-worker2\"; do",
			replacement: "\t\t\"${CLUSTER_NAME}-worker\"; do",
			wantError:   "all-node registry hosts contract",
		},
		{
			name:        "isolation worker registry configuration skipped where it exists",
			old:         `if [ "$kind_node_container" = "${CLUSTER_NAME}-worker2" ] && [ "$ISOLATION_WORKER" != true ]; then`,
			replacement: `if [ "$kind_node_container" = "${CLUSTER_NAME}-worker2" ]; then`,
			wantError:   "all-node registry hosts contract",
		},
		{
			name: "isolation worker left out of the kind inventory",
			old: "\t\tif [ \"$ISOLATION_WORKER\" = true ]; then\n" +
				"\t\t\tprintf '%s\\n' \"${CLUSTER_NAME}-worker2\"\n" +
				"\t\tfi\n",
			replacement: "",
			wantError:   "kind HA topology contract",
		},
		{
			name:        "isolation worker accepted where no suite declared it",
			old:         `(if $isolation then [$cluster + "-worker2"] else [] end) as $isolated |`,
			replacement: `[$cluster + "-worker2"] as $isolated |`,
			wantError:   "kind HA topology contract",
		},
		{
			name:        "isolation taint no longer required of the isolation worker",
			old:         `$label == "true" and $taints == [{key: $key, value: "true", effect: "NoSchedule"}]`,
			replacement: `$label == "true"`,
			wantError:   "kind HA topology contract",
		},
		{
			name:        "isolation key allowed on every other node",
			old:         `$label == null and $taints == []`,
			replacement: `true`,
			wantError:   "kind HA topology contract",
		},
		{
			name:        "isolation worker key renamed",
			old:         "ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation\n",
			replacement: "ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolated\n",
			wantError:   "isolation worker key",
		},
		{
			name:        "isolation worker declared for every suite",
			old:         `'any(.suites[]; .name == $suite and .isolationWorker == true)' "$SUITE_CATALOG"`,
			replacement: `'any(.suites[]; .isolationWorker == true)' "$SUITE_CATALOG"`,
			wantError:   "isolation worker declared by the suite catalog",
		},
		{
			name: "isolation worker given to a bootstrap that runs no phase",
			old: "\tif [ \"$E2E_STOP_AFTER\" = bootstrap ]; then\n" +
				"\t\tprintf '%s\\n' false\n" +
				"\telif [ \"$E2E_SUITE\" = all ]; then\n",
			replacement: "\tif [ \"$E2E_SUITE\" = all ]; then\n",
			wantError:   "isolation worker declared by the suite catalog",
		},
		{
			name:        "node count fixed at four",
			old:         "\t\tKIND_NODE_COUNT=5\n",
			replacement: "\t\tKIND_NODE_COUNT=4\n",
			wantError:   "isolation worker declared by the suite catalog",
		},
		{
			name: "isolation worker appended whatever the suite declared",
			old: "if [ \"$ISOLATION_WORKER\" = true ]; then\n" +
				"\tcat \"$ROOT_DIR/testdata/e2e/kind-isolation-worker.yaml.tmpl\" >>\"$KIND_CONFIG\"\n" +
				"fi\n",
			replacement: "cat \"$ROOT_DIR/testdata/e2e/kind-isolation-worker.yaml.tmpl\" >>\"$KIND_CONFIG\"\n",
			wantError:   "isolation worker appended to the node list",
		},
		{
			name: "isolation worker appended after the node list ended",
			old: "if [ \"$ISOLATION_WORKER\" = true ]; then\n" +
				"\tcat \"$ROOT_DIR/testdata/e2e/kind-isolation-worker.yaml.tmpl\" >>\"$KIND_CONFIG\"\n" +
				"fi\n" +
				"EXPECTED_API_SERVER_FEATURE_GATES=\n",
			replacement: "EXPECTED_API_SERVER_FEATURE_GATES=\n",
			wantError:   "isolation worker appended to the node list",
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
			name:        "current-release install readiness gate omitted",
			old:         `require_ready_nodes "immediately before current-release Helm install"`,
			replacement: `: # current-release install readiness gate omitted`,
			wantError:   "immediate current-release install readiness gate",
		},
		{
			name:        "current-release install failure ignored",
			old:         `if command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			replacement: `command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \`,
			wantError:   "immediate current-release install readiness gate",
		},
		{
			name:        "post-install failure readiness recheck is delayed",
			old:         `if nodes_ready_now; then`,
			replacement: `if wait_for_ready_nodes "after current-release Helm install failed"; then`,
			wantError:   "immediate current-release install readiness gate",
		},
		{
			name: "post-install readiness recheck is not immediate",
			old:  "\tcurrent_install_status=$?\n\tif nodes_ready_now; then",
			replacement: "\tcurrent_install_status=$?\n" +
				"\tsleep 30\n\tif nodes_ready_now; then",
			wantError: "immediate current-release install readiness gate",
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
			name: "current-release install retried",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ncommand helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried in a subshell",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\n(command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\") || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install executable is split across a continuation",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ncommand hel\\\nm install retry-chart || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install subcommand is split across a continuation",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ncommand helm ins\\\ntall retry-chart || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried through quoted command substitution",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_result=\"$(command helm install retry-chart)\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried through an expandable here-document",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nsh <<EOF\ncommand helm install retry-chart\nEOF",
			wantError: "shell here-document syntax",
		},
		{
			name: "current-release install retried through legacy command substitution",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_result=`helm install retry-chart` || true",
			wantError: "legacy backtick command substitution",
		},
		{
			name: "current-release install retried in a command group",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\n{ command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"; } || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried as a pipeline command",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nprintf '%s\\n' retry | command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried after a background separator",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ntrue & command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried after an or-list boundary",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nfalse || command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried after a control keyword",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nif true; then command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\"; fi",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried through a shell command string",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nsh -c 'command helm install retry-chart' || true",
			wantError: "Helm command-string launch",
		},
		{
			name: "current-release install retried through eval",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\neval 'command helm install retry-chart' || true",
			wantError: "Helm command-string launch",
		},
		{
			name: "current-release install retried through a variable-backed shell command string",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nenv sh -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "current-release install retried through Bash after a long option",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nbash --noprofile -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "current-release install retried through env and Bash after a long option",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nenv bash --noprofile -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "current-release install retried through Bash after an option operand",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nretry_command='command helm install retry-chart'\nbash -O extglob -c \"$retry_command\" || true",
			wantError: "host shell command-string launch",
		},
		{
			name: "current-release install retried with environment prefix",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nHELM_DEBUG=1 command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "exactly one semantic install attempt",
		},
		{
			name: "current-release install retried through env",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nenv HELM_DEBUG=1 command helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "current-release install retried through env option operand",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nenv -u HELM_DEBUG helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "current-release install retried through plain env",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nenv helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "current-release install retried through multiline env",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\nenv -u HELM_DEBUG \\\n\thelm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "current-release install retried through command env",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ncommand env HELM_DEBUG=1 helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name: "current-release install retried through chained env",
			old: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi",
			replacement: "\tfail \"infrastructure readiness loss: current-release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $current_install_status)\"\n" +
				"fi\ntrue && env helm install retry-chart --kubeconfig \"$KUBECONFIG_FILE\" >/dev/null 2>&1 || true",
			wantError: "env-launched Helm command",
		},
		{
			name:        "upgrade lifecycle omitted",
			old:         "E2E_PHASE=upgrade \\\n",
			replacement: "E2E_PHASE=upgrade-omitted \\\n",
			wantError:   `upgrade phase must bind E2E_PHASE to "upgrade", and binds "upgrade-omitted"`,
		},
		{
			name: "upgrade child call removed",
			old: "E2E_PHASE=upgrade \\\n" +
				"\trun_recorded_phase upgrade \"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
			replacement: `true # upgrade child call removed`,
			wantError:   "candidate upgrade lifecycle",
		},
		{
			name: "upgrade child call hidden in false branch",
			old: "E2E_PHASE=upgrade \\\n" +
				"\trun_recorded_phase upgrade \"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"",
			replacement: "if false; then\n\tE2E_PHASE=upgrade \\\n" +
				"\t\trun_recorded_phase upgrade \"$ROOT_DIR/hack/e2e-crd-upgrade.sh\"\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "high availability lifecycle omitted",
			old:         `run_recorded_phase ha "$ROOT_DIR/hack/e2e-ha.sh"`,
			replacement: `true # high availability lifecycle omitted`,
			wantError:   "high-availability lifecycle",
		},
		{
			name:        "high availability lifecycle hidden in false branch",
			old:         `run_recorded_phase ha "$ROOT_DIR/hack/e2e-ha.sh"`,
			replacement: "if false; then\n\trun_recorded_phase ha \"$ROOT_DIR/hack/e2e-ha.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "control plane lifecycle omitted",
			old:         `run_recorded_phase assert "$ROOT_DIR/hack/e2e-assert.sh"`,
			replacement: `true # control plane lifecycle omitted`,
			wantError:   "control-plane lifecycle",
		},
		{
			name:        "control plane lifecycle hidden in false branch",
			old:         `run_recorded_phase assert "$ROOT_DIR/hack/e2e-assert.sh"`,
			replacement: "if false; then\n\trun_recorded_phase assert \"$ROOT_DIR/hack/e2e-assert.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "certificate lifecycle omitted",
			old:         `run_recorded_phase cert-rotation "$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: `true # certificate lifecycle omitted`,
			wantError:   "certificate lifecycle",
		},
		{
			name:        "certificate lifecycle hidden in false branch",
			old:         `run_recorded_phase cert-rotation "$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: "if false; then\n\trun_recorded_phase cert-rotation \"$ROOT_DIR/hack/e2e-cert-rotation.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "data plane lifecycle omitted",
			old:         `run_recorded_phase dataplane "$ROOT_DIR/hack/e2e-dataplane.sh"`,
			replacement: `true # data plane lifecycle omitted`,
			wantError:   "data-plane and OCI lifecycle",
		},
		{
			name:        "data plane lifecycle hidden in false branch",
			old:         `run_recorded_phase dataplane "$ROOT_DIR/hack/e2e-dataplane.sh"`,
			replacement: "if false; then\n\trun_recorded_phase dataplane \"$ROOT_DIR/hack/e2e-dataplane.sh\"\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "PostgreSQL migration lifecycle omitted",
			old:         `run_recorded_phase migrations-postgresql "$ROOT_DIR/hack/e2e-migrations.sh"`,
			replacement: `true # migration lifecycle omitted`,
			wantError:   "PostgreSQL migration lifecycle",
		},
		{
			name:        "MySQL migration lifecycle omitted",
			old:         `run_recorded_phase migrations-mysql "$ROOT_DIR/hack/e2e-migrations.sh"`,
			replacement: `true # migration lifecycle omitted`,
			wantError:   "MySQL migration lifecycle",
		},
		{
			name:        "migration lifecycle call separated from its environment",
			old:         `run_recorded_phase migrations-postgresql "$ROOT_DIR/hack/e2e-migrations.sh"`,
			replacement: "true\n\trun_recorded_phase migrations-postgresql \"$ROOT_DIR/hack/e2e-migrations.sh\"",
			wantError:   `migrations-postgresql phase must bind E2E_KUBECONFIG to "$KUBECONFIG_FILE", and binds nothing`,
		},
		{
			name:        "PostgreSQL reference-data lifecycle omitted",
			old:         `run_recorded_phase reference-data-postgresql "$ROOT_DIR/hack/e2e-reference-data.sh"`,
			replacement: `true # reference-data lifecycle omitted`,
			wantError:   "PostgreSQL reference-data lifecycle",
		},
		{
			name:        "MySQL reference-data lifecycle omitted",
			old:         `run_recorded_phase reference-data-mysql "$ROOT_DIR/hack/e2e-reference-data.sh"`,
			replacement: `true # reference-data lifecycle omitted`,
			wantError:   "MySQL reference-data lifecycle",
		},
		{
			name:        "reference-data lifecycle call separated from its environment",
			old:         `run_recorded_phase reference-data-mysql "$ROOT_DIR/hack/e2e-reference-data.sh"`,
			replacement: "true\n\trun_recorded_phase reference-data-mysql \"$ROOT_DIR/hack/e2e-reference-data.sh\"",
			wantError:   `reference-data-mysql phase must bind E2E_KUBECONFIG to "$KUBECONFIG_FILE", and binds nothing`,
		},
		{
			name: "migration lifecycle hidden in false branch",
			old:  "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql",
			replacement: "if false; then\nE2E_ENGINE=postgresql \\\n" +
				"\trun_recorded_phase migrations-postgresql",
			wantError: "always-false wrapper",
		},
		{
			name:        "migration lifecycle loses the controller identity",
			old:         "E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\nE2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\nE2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\nE2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n",
			replacement: "E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\nE2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\nE2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n",
			wantError:   `migrations-postgresql phase must bind E2E_CONTROLLER_REVISION to "$CONTROLLER_REVISION", and binds nothing`,
		},
		{
			// A phase that ran the engine its name does not say would cover one
			// engine twice and leave the other unproven, with both jobs green.
			name:        "a migration phase bound to the other engine",
			old:         "E2E_ENGINE=mysql \\\n\trun_recorded_phase migrations-mysql",
			replacement: "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-mysql",
			wantError:   `migrations-mysql phase must bind E2E_ENGINE to "mysql", and binds "postgresql"`,
		},
		{
			name:        "a reference-data phase bound to the other engine",
			old:         "E2E_ENGINE=postgresql \\\n\trun_recorded_phase reference-data-postgresql",
			replacement: "E2E_ENGINE=mysql \\\n\trun_recorded_phase reference-data-postgresql",
			wantError:   `reference-data-postgresql phase must bind E2E_ENGINE to "postgresql", and binds "mysql"`,
		},
		{
			name:        "uninstall lifecycle omitted",
			old:         "E2E_PHASE=uninstall \\\n",
			replacement: "E2E_PHASE=uninstall-omitted \\\n",
			wantError:   `uninstall phase must bind E2E_PHASE to "uninstall", and binds "uninstall-omitted"`,
		},
		{
			name:        "synthetic next chart handoff omitted",
			old:         "E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_NEXT_CHART_PACKAGE= \\\n",
			wantError:   `uninstall phase must bind E2E_NEXT_CHART_PACKAGE to "$NEXT_CHART_PACKAGE", and binds ""`,
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
			wantError: `uninstall phase must bind E2E_CANDIDATE_VALUES_FILE to "$CANDIDATE_VALUES_FILE", and binds ""`,
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
			wantError: `uninstall phase must bind E2E_CANDIDATE_IMAGE to "$CANDIDATE_OPERATOR_IMAGE", and binds ""`,
		},
		{
			name:        "installed chart export omitted",
			old:         "\nexport_release_chart\nPHASE_COMPLETED=1\nif [ -n \"$SKIPPED_PHASES\" ]; then",
			replacement: "\n: # installed chart export omitted\nPHASE_COMPLETED=1\nif [ -n \"$SKIPPED_PHASES\" ]; then",
			wantError:   "post-lifecycle installed chart export",
		},
		{
			name: "installed chart export moved after terminal evidence",
			old: "export_release_chart\n" +
				"PHASE_COMPLETED=1\n" +
				"if [ -n \"$SKIPPED_PHASES\" ]; then",
			replacement: "PHASE_COMPLETED=1\n" +
				"printf 'e2e: PASS Kubernetes=%s cluster=%s\\n' \"$server_version\" \"$CLUSTER_NAME\"\n" +
				"export_release_chart\n" +
				"if [ -n \"$SKIPPED_PHASES\" ]; then",
			wantError: "terminal Kubernetes lifecycle evidence",
		},
		{
			name:        "installed chart export uses synthetic next chart",
			old:         `cp "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"`,
			replacement: `cp "$NEXT_CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"`,
			wantError:   "export_release_chart is missing its installed chart export source",
		},
		{
			name:        "installed chart export clobbers a raced target",
			old:         `ln "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"`,
			replacement: `mv "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"`,
			wantError:   "export_release_chart is missing its installed chart export without replacement",
		},
		{
			name:        "terminal evidence omitted",
			old:         `printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"`,
			replacement: `printf '%s\n' 'e2e lifecycle finished without evidence'`,
			wantError:   "terminal Kubernetes lifecycle evidence",
		},
		{
			name:        "diagnosis run reaches the pass line",
			old:         `if [ -n "$SKIPPED_PHASES" ]; then`,
			replacement: `if false; then`,
			wantError:   "diagnosis-only terminal evidence",
		},
		{
			name:        "phase left out without a record",
			old:         "\t\t\tSKIPPED_PHASES=\"$SKIPPED_PHASES $recorded_phase\"\n",
			replacement: "",
			wantError:   "diagnosis phase record",
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
				"        .apiVersion == \"operator.ptah.run/v1alpha1\" and .kind == \"PtahSchema\" and\n" +
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
			old:         `$job.metadata.annotations["operator.ptah.run/operation-id"] == $operationID and`,
			replacement: `true and`,
			wantError:   "supplied Job evidence exact Job identity binding",
		},
		{
			name: "supplied Job archive schema owner UID binding omitted",
			old: "([$job.metadata.ownerReferences[]? | select(\n" +
				"        .apiVersion == \"operator.ptah.run/v1alpha1\" and .kind == \"PtahSchema\" and\n" +
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
			name: "durable Job archive omits settled UID-bounded audited log capture",
			old: "if [ \"$audit_managed_complete\" -eq 1 ] && [ \"$audit_container\" = ptah ]; then\n" +
				"\t\t\t\t\tread_result_transport \"$audit_pod_name\" \"$audit_evidence_log_file\" \\\n" +
				"\t\t\t\t\t\t\"$audit_operation\" \"$audit_operation_id\" \"$audit_evidence_result_file\"\n" +
				"\t\t\t\t\tchmod 600 \"$audit_evidence_log_file\" \"$audit_evidence_result_file\"",
			replacement: `if false; then :`,
			wantError:   "durable Job archive settled UID-bounded audited log capture",
		},
		{
			name: "durable Job archive retains an unsettled audited log",
			old: "read_result_transport \"$audit_pod_name\" \"$audit_evidence_log_file\" \\\n" +
				"\t\t\t\t\t\t\"$audit_operation\" \"$audit_operation_id\" \"$audit_evidence_result_file\"",
			replacement: `cp "$LOG_FILE" "$audit_evidence_log_file" || true`,
			wantError:   "durable Job archive settled UID-bounded audited log capture",
		},
		{
			name:        "durable Job archive accepts an unreadable result frame",
			old:         `fail "the $publish_operation result frame for Job UID $publish_job_uid cannot be archived"`,
			replacement: `: # an unreadable result frame is archived unreported`,
			wantError:   "durable Job archive reported result refusal",
		},
		{
			name:        "durable Job archive drops the refused result frame reason",
			old:         `sed 's/^/e2e data plane:   /' "$PUBLISH_RESULT_ERROR_FILE" >&2`,
			replacement: `true`,
			wantError:   "durable Job archive reported result refusal",
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
			old:         `.["operator.ptah.run/plan-fingerprint"] == $planFingerprint and`,
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
			old:         `.["operator.ptah.run/plan-content-digest"] == $contentDigest and`,
			replacement: `.["operator.ptah.run/plan-content-digest"] == $planFingerprint and`,
			wantError:   "automatic external PostgreSQL Apply annotation bindings",
		},
		{
			name:        "automatic Apply swaps execution annotation binding",
			old:         `.["operator.ptah.run/execution-binding-id"] == $executionBinding;`,
			replacement: `.["operator.ptah.run/execution-binding-id"] == $contentDigest;`,
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
			name: "fault lifecycle hidden in false branch",
			old: "\"$ROOT_DIR/hack/e2e-faults.sh\" ||\n" +
				"\tfail \"the restart and fault-injection phase failed; its reason is above\"",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-faults.sh\" ||\n" +
				"\t\tfail \"the restart and fault-injection phase failed; its reason is above\"\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "operation audit omitted",
			old:         "assert_observed_jobs_audited\n",
			replacement: "true # operation audit omitted\n",
			wantError:   "audited operation evidence",
		},
		{
			name:        "declared Pod metadata evidence omitted",
			old:         `printf '%s\n' 'e2e data plane: PASS declared Pod metadata reaches every operation Pod under a namespace admission policy, and a Pod the policy refuses is reported as PodAdmissionRefused'`,
			replacement: `printf '%s\n' 'e2e data plane: Pod metadata row skipped'`,
			wantError:   "declared Pod metadata evidence",
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
			wantError:   "rendered reconcile hook and failed revision binding",
		},
		{
			name: "hook name is not derived from the render",
			old: "[ -n \"$UPGRADE_VALUES_FILE\" ] || fail \"upgrade values file is not configured\"\n" +
				"\t[ -n \"$EXPECTED_RECONCILE_HOOK_NAME\" ] || fail \"rendered reconcile hook name is unavailable\"",
			replacement: "[ -n \"$UPGRADE_VALUES_FILE\" ] || fail \"upgrade values file is not configured\"\n" +
				"\tEXPECTED_RECONCILE_HOOK_NAME=ptah-crd-reconcile",
			wantError: "rendered reconcile hook and failed revision binding",
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
			wantError:   "exact failed reconcile evidence evaluation",
		},
		{
			name:        "evidence omits exact hook name",
			old:         `--arg expected_name "$EXPECTED_RECONCILE_HOOK_NAME" \`,
			replacement: `--arg expected_name "" \`,
			wantError:   "exact failed reconcile evidence evaluation",
		},
		{
			name:        "evidence filter result is ignored",
			old:         `-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null; then`,
			replacement: `-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null || true; then`,
			wantError:   "exact failed reconcile evidence evaluation",
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
		{name: "hook kind", old: `.kind == "Job" and`, replacement: `.kind != "" and`},
		{name: "weight default", old: `if .weight == null then 0 else (.weight | tonumber) end;`, replacement: `if .weight == null then -1 else (.weight | tonumber) end;`},
		{name: "hook weight", old: `hook_weight == 0 and`, replacement: `hook_weight <= 0 and`},
		{name: "hook event", old: "hook_weight == 0 and\n  ((.events // []) | index(\"pre-upgrade\") != null)", replacement: "hook_weight == 0 and\n  ((.events // []) | length > 0)"},
		{name: "started timestamp", old: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0)", replacement: "((.events // []) | index(\"pre-upgrade\") != null) and\n  true"},
		{name: "completed timestamp", old: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0) and\n  ((.last_run.completed_at // \"\") | length > 0))", replacement: "((.events // []) | index(\"pre-upgrade\") != null) and\n  ((.last_run.started_at // \"\") | length > 0) and\n  true)"},
		{name: "later hook cutoff", old: `(hook_weight > 0)`, replacement: `(hook_weight >= 0)`},
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
			old:         `expect_rejected wrong-name '.hooks[1].name = "other-reconcile"'`,
			replacement: `: # wrong name accepted`,
			wantError:   "wrong hook name refusal",
		},
		{
			name:        "weight negative is removed",
			old:         `expect_rejected wrong-weight '.hooks[1].weight = -60'`,
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

// The stopwatch measures the phases that decide whether the operator works, so
// the risk it carries is a measurement that swallows a failure. Its self-test
// is what refuses that, and this refuses a static gate that stopped running it.
func TestVerifyTimingSelftestWiringRejectsMutations(t *testing.T) {
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
			replacement: `: # timing self-test removed`,
			wantError:   "timing self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$ROOT_DIR/hack/e2e-timing-selftest.sh" || true`,
			wantError:   "timing self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-timing-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$ROOT_DIR/hack/e2e-timing-selftest.sh"`
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

// The prepared images a matrix loads are only as good as the refusals that
// check them, and those are shell. This refuses a static gate that stopped
// running the self-test which measures them.
func TestVerifySharedImageSelftestWiringRejectsMutations(t *testing.T) {
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
			replacement: `: # shared-image self-test removed`,
			wantError:   "shared-image self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$ROOT_DIR/hack/e2e-shared-images-selftest.sh" || true`,
			wantError:   "shared-image self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-shared-images-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$ROOT_DIR/hack/e2e-shared-images-selftest.sh"`
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

// A suite that stopped running one of its phases is a green job that proves
// less, and the shell that selects them is what this measures. So the gate has
// to keep running the self-test that measures it.
func TestVerifyAcceptanceSuiteSelftestWiringRejectsMutations(t *testing.T) {
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
			replacement: `: # acceptance suite self-test removed`,
			wantError:   "acceptance suite self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$ROOT_DIR/hack/e2e-suites-selftest.sh" || true`,
			wantError:   "acceptance suite self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-suites-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$ROOT_DIR/hack/e2e-suites-selftest.sh"`
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

// The bootstrap waits for a control plane that is still joining and refuses one
// that is wrong, in the same loop. A loop that stopped refusing still looks
// like it waits, so the gate has to keep running the self-test that separates
// them.
func TestVerifyControlPlaneShapeSelftestWiringRejectsMutations(t *testing.T) {
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
			replacement: `: # control-plane shape self-test removed`,
			wantError:   "control-plane shape self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$ROOT_DIR/hack/e2e-control-plane-shape-selftest.sh" || true`,
			wantError:   "control-plane shape self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-control-plane-shape-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$ROOT_DIR/hack/e2e-control-plane-shape-selftest.sh"`
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

// A guard that reads the trim instead of the exec skips the phase's own setup
// in silence, and the phase then blames the operator for a state it never
// received. The split that fixes it is shell, so the gate has to keep running
// the self-test that measures it.
func TestVerifySQLStatementSelftestWiringRejectsMutations(t *testing.T) {
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
			replacement: `: # SQL statement self-test removed`,
			wantError:   "SQL statement self-test wiring",
		},
		{
			name:        "self-test failure ignored",
			replacement: `"$ROOT_DIR/hack/e2e-sql-selftest.sh" || true`,
			wantError:   "SQL statement self-test wiring",
		},
		{
			name: "self-test hidden in false branch",
			replacement: "if false; then\n" +
				"\t\"$ROOT_DIR/hack/e2e-sql-selftest.sh\"\n" +
				"fi",
			wantError: "always-false wrapper",
		},
	}
	const invocation = `"$ROOT_DIR/hack/e2e-sql-selftest.sh"`
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

// The self-test proves the helpers; this proves the call sites still reach
// them. Both phases surround their two guarded statements with thirty-odd
// value queries that differ by one word, so the way the defect comes back is a
// copy of the neighbor.
func TestVerifySQLStatementGuardsRejectValueHelperCallSites(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	const wantError = "must run through the statement helper"
	tests := []struct {
		name        string
		fixture     string
		path        string
		assign      func(*e2eWiringFiles, string)
		old         string
		replacement string
	}{
		{
			name:    "undone column restored to the value helper",
			fixture: "e2e-migrations.sh",
			path:    files.migrations,
			assign:  func(mutated *e2eWiringFiles, path string) { mutated.migrations = path },
			old: `	migration_statement "ALTER TABLE e2e_migration_widgets DROP COLUMN weight" >/dev/null ||
		fail "could not undo the column the $ENGINE partial migration committed"`,
			replacement: `	migration_query "ALTER TABLE e2e_migration_widgets DROP COLUMN weight" >/dev/null ||
		fail "could not undo the column the $ENGINE partial migration committed"`,
		},
		{
			name:    "external edit restored to the value helper",
			fixture: "e2e-reference-data.sh",
			path:    files.referenceData,
			assign:  func(mutated *e2eWiringFiles, path string) { mutated.referenceData = path },
			old: `	reference_statement "UPDATE countries SET name = 'Edited outside the operator' WHERE code = 'US'" >/dev/null ||
		fail "the external edit could not be made"`,
			replacement: `	reference_query "UPDATE countries SET name = 'Edited outside the operator' WHERE code = 'US'" >/dev/null ||
		fail "the external edit could not be made"`,
		},
		{
			name:    "revision row deleted through a continued value call",
			fixture: "e2e-migrations.sh",
			path:    files.migrations,
			assign:  func(mutated *e2eWiringFiles, path string) { mutated.migrations = path },
			old: `	migration_statement "DELETE FROM schema_migrations WHERE state <> 'applied'" >/dev/null ||
		fail "could not take the unfinished $ENGINE revision out of the history"`,
			replacement: `	migration_query \
		"DELETE FROM schema_migrations WHERE state <> 'applied'" >/dev/null ||
		fail "could not take the unfinished $ENGINE revision out of the history"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := readE2ESource(t, test.path)
			mutatedFiles := files
			test.assign(&mutatedFiles, writeMutatedE2ESource(t, test.fixture, source, test.old, test.replacement))
			err := verifyE2EWiring(mutatedFiles)
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, wantError)
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
			name:        "CRD reconcile hook identity is hard-coded",
			child:       "crd-upgrade",
			old:         `reconcile_matches=$(rendered_hook_job_name crd-manager 0)`,
			replacement: `reconcile_matches=ptah-operator-crd-manager`,
			wantError:   "exact rendered reconcile hook identity",
		},
		{
			name:        "CRD reconcile hook identity assignment is hard-coded",
			child:       "crd-upgrade",
			old:         `EXPECTED_RECONCILE_HOOK_NAME=$reconcile_matches`,
			replacement: `EXPECTED_RECONCILE_HOOK_NAME=ptah-operator-crd-manager`,
			wantError:   "rendered reconcile hook identity assignment",
		},
		{
			name:        "CRD reconcile hook uniqueness checks the wrong render",
			child:       "crd-upgrade",
			old:         `[ "$(printf '%s\n' "$reconcile_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||`,
			replacement: `[ "$(printf '%s\n' "$other_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||`,
			wantError:   "unique rendered reconcile hook identity",
		},
		{
			name:        "CRD late failure blocker broadens its target",
			child:       "crd-upgrade",
			old:         `expression: 'request.namespace == "$E2E_OPERATOR_NAMESPACE" && (request.name == "$CONTROLLER_DEPLOYMENT" || request.name == "$ROTATOR_DEPLOYMENT")'`,
			replacement: `expression: 'true'`,
			wantError:   "late failure blocker refuses only the candidate Deployments",
		},
		{
			name:        "CRD late failure blocker also refuses the hook's scale-down",
			child:       "crd-upgrade",
			old:         `expression: 'object != null && object.spec.template.spec.containers.exists(container, container.image == "$E2E_NEXT_CONTROLLER_IMAGE")'`,
			replacement: `expression: 'object != null'`,
			wantError:   "late failure blocker refuses only the candidate Deployments",
		},
		{
			name:        "CRD late failure installs no blocker",
			child:       "crd-upgrade",
			old:         "\tcreate_late_failure_blocker\n",
			replacement: "\t: # blocker omitted\n",
			wantError:   "late failure blocker before the candidate",
		},
		{
			name:        "CRD late failure applies another chart",
			child:       "crd-upgrade",
			old:         "\"$E2E_NEXT_CHART_PACKAGE\" \\\n\t\t--namespace \"$E2E_OPERATOR_NAMESPACE\" --values \"$E2E_NEXT_VALUES_FILE\" \\\n\t\t--force-conflicts \\\n\t\t--wait --timeout 7m >\"$WORK_DIR/late-failure.out\"",
			replacement: "\"$E2E_CHART_PACKAGE\" \\\n\t\t--namespace \"$E2E_OPERATOR_NAMESPACE\" --values \"$E2E_NEXT_VALUES_FILE\" \\\n\t\t--force-conflicts \\\n\t\t--wait --timeout 7m >\"$WORK_DIR/late-failure.out\"",
			wantError:   "late failure Helm execution",
		},
		{
			name:        "CRD late failure reads another revision",
			child:       "crd-upgrade",
			old:         `--revision "$late_revision" -o json >"$late_status_file" 2>/dev/null ||`,
			replacement: `-o json >"$late_status_file" 2>/dev/null ||`,
			wantError:   "late failure structured revision retrieval",
		},
		{
			name:        "CRD late failure accepts a failed hook",
			child:       "crd-upgrade",
			old:         `([$hooks[] | select(.last_run.phase == "Failed")] | length == 0) and`,
			replacement: `true and`,
			wantError:   "late failure after a reconcile hook that succeeded",
		},
		{
			name:        "CRD late failure accepts a running runtime",
			child:       "crd-upgrade",
			old:         `.spec.replicas == 0 and`,
			replacement: `.spec.replicas >= 0 and`,
			wantError:   "late failure stopped runtime",
		},
		{
			name:        "CRD late failure accepts a runtime Pod",
			child:       "crd-upgrade",
			old:         `' >/dev/null || fail "the late failure left a runtime Pod after the runtime stop"`,
			replacement: `' >/dev/null || true`,
			wantError:   "late failure runtime Pod absence",
		},
		{
			name:        "CRD recovery permits changed candidate image",
			child:       "crd-upgrade",
			old:         `[ "$E2E_NEXT_CONTROLLER_IMAGE" != "$late_candidate_image" ]; then`,
			replacement: `[ -z "$E2E_NEXT_CONTROLLER_IMAGE" ]; then`,
			wantError:   "late failure immutable candidate retry inputs",
		},
		{
			name:        "CRD recovery permits changed candidate package",
			child:       "crd-upgrade",
			old:         `if [ "$late_retry_chart_sha256" != "$late_candidate_chart_sha256" ] ||`,
			replacement: `if [ ! -f "$E2E_NEXT_CHART_PACKAGE" ] ||`,
			wantError:   "late failure immutable candidate retry inputs",
		},
		{
			name:        "CRD recovery ignores candidate chart checksum failure",
			child:       "crd-upgrade",
			old:         `fail "could not checksum the late-failure candidate chart"`,
			replacement: `: # checksum failure ignored`,
			wantError:   "late failure immutable candidate retry inputs",
		},
		{
			name:        "CRD recovery ignores candidate values checksum failure",
			child:       "crd-upgrade",
			old:         `fail "could not checksum the late-failure candidate values"`,
			replacement: `: # checksum failure ignored`,
			wantError:   "late failure immutable candidate retry inputs",
		},
		{
			name:        "CRD recovery skips candidate identity recheck",
			child:       "crd-upgrade",
			old:         "\tassert_late_failure_candidate_unchanged\n",
			replacement: "\t: # changed candidate allowed\n",
			wantError:   "successor read-only Job dispatch before the late failure",
		},
		{
			name:        "CRD recovery removes blocker before staging the UID gap",
			child:       "crd-upgrade",
			old:         "\tstage_read_only_job_uid_gap\n\tassert_late_failure_candidate_unchanged\n\tdelete_late_failure_blocker\n",
			replacement: "\tdelete_late_failure_blocker\n\tstage_read_only_job_uid_gap\n\tassert_late_failure_candidate_unchanged\n",
			wantError:   "successor read-only Job dispatch before the late failure",
		},
		{
			name:        "CRD recovery permits extra Helm revisions",
			child:       "crd-upgrade",
			old:         `[ "$after_revision" -eq $((late_revision + 1)) ] ||`,
			replacement: `[ "$after_revision" -gt "$late_revision" ] ||`,
			wantError:   "same-candidate recovery exactly one retry revision",
		},
		{
			name:        "CRD recovery accepts a replaced controller ServiceAccount",
			child:       "crd-upgrade",
			old:         `[ "$next_service_account_uid" = "$current_service_account_uid" ] ||`,
			replacement: `[ -n "$next_service_account_uid" ] ||`,
			wantError:   "same-candidate recovery kept the controller identity",
		},
		{
			name:        "CRD recovery skips candidate readiness",
			child:       "crd-upgrade",
			old:         "\tretry_same_candidate\n\twait_runtime_ready\n",
			replacement: "\tretry_same_candidate\n",
			wantError:   "same-candidate retry, read-only Job cleanup and running Apply adoption",
		},
		{
			name:        "CRD recovery returns successfully before doing any work",
			child:       "crd-upgrade",
			old:         "run_next_release_upgrade_proof() {\n",
			replacement: "run_next_release_upgrade_proof() {\n\treturn 0\n",
			wantError:   "successful return",
		},
		{
			name:        "CRD recovery candidate helper returns before its assertions",
			child:       "crd-upgrade",
			old:         "assert_late_failure_candidate_unchanged() {\n",
			replacement: "assert_late_failure_candidate_unchanged() {\n\treturn 0\n",
			wantError:   "successful return",
		},
		{
			name:        "CRD late failure restarts the runtime by hand",
			child:       "crd-upgrade",
			old:         "\tprintf '%s\\n' 'e2e crd: the late failure left the runtime stopped on the predecessor template'\n",
			replacement: "\tstart_runtime_deployments\n\tprintf '%s\\n' 'e2e crd: the late failure left the runtime stopped on the predecessor template'\n",
			wantError:   "must leave the runtime to the hook",
		},
		{
			name:        "CRD recovery retries a different chart",
			child:       "crd-upgrade",
			old:         "retry_same_candidate() {\n\tif ! helm_e2e upgrade \"$E2E_HELM_RELEASE\" \"$E2E_NEXT_CHART_PACKAGE\" \\\n",
			replacement: "retry_same_candidate() {\n\tif ! helm_e2e upgrade \"$E2E_HELM_RELEASE\" \"$E2E_CHART_PACKAGE\" \\\n",
			wantError:   "same-candidate retry",
		},
		{
			name:        "CRD refused rollback is not attempted",
			child:       "crd-upgrade",
			old:         "\tprove_rollback_refused_over_future_state \"$current_release_revision\"\n",
			replacement: "\t: # refused rollback omitted\n",
			wantError:   "refused rollback, then the rollback it leaves pending",
		},
		{
			name:        "CRD refused rollback may be admitted",
			child:       "crd-upgrade",
			old:         `fail "a rollback over stored state newer than the release it rolls back to was admitted"`,
			replacement: `true`,
			wantError:   "refused rollback execution",
		},
		{
			name:        "CRD refused rollback may be refused before its hook",
			child:       "crd-upgrade",
			old:         `fail "the refused rollback did not reach its pre-rollback hook"`,
			replacement: `true`,
			wantError:   "refused rollback reached its hook",
		},
		{
			name:        "CRD refused rollback may change a Deployment",
			child:       "crd-upgrade",
			old:         `cmp "$before" "$after" || fail "the refused rollback changed a runtime Deployment"`,
			replacement: `true`,
			wantError:   "refused rollback left the runtime alone",
		},
		{
			name:        "CRD refused rollback returns before its assertions",
			child:       "crd-upgrade",
			old:         "prove_rollback_refused_over_future_state() {\n",
			replacement: "prove_rollback_refused_over_future_state() {\n\treturn 0\n",
			wantError:   "successful return",
		},
		{
			name:        "CRD rollback may end anywhere",
			child:       "crd-upgrade",
			old:         `fail "the rollback to revision $rollback_revision did not end deployed"`,
			replacement: `true`,
			wantError:   "rollback ends deployed",
		},
		{
			name:        "CRD read-only Job terminal fixture bypasses Job controller",
			child:       "crd-upgrade",
			old:         `type: "FailureTarget", status: "True",`,
			replacement: `type: "Failed", status: "True",`,
			wantError:   "read-only Job controller-owned failure staging",
		},
		{
			name:        "CRD read-only Job terminal fixture alters active status",
			child:       "crd-upgrade",
			old:         "status: {\n\t    conditions: [{",
			replacement: "status: {\n\t    active: 0,\n\t    conditions: [{",
			wantError:   "read-only Job controller-owned failure staging",
		},
		{
			name:        "CRD read-only Job terminal fixture loses native wait",
			child:       "crd-upgrade",
			old:         `fail "Job controller did not retire the read-only Job after FailureTarget staging"`,
			replacement: `true # native terminal wait removed`,
			wantError:   "read-only Job native terminal wait",
		},
		{
			name:        "CRD current-release read-only Job staging skips the controller stop",
			child:       "crd-upgrade",
			old:         "\tdispatch_read_only_job_fixture\n\tstop_runtime_deployments\n\tset_pod_webhook_failure_policy Fail Ignore\n\tstage_read_only_job_completion\n\tset_pod_webhook_failure_policy Ignore Fail\n\tstart_runtime_deployments\n",
			replacement: "\tdispatch_read_only_job_fixture\n\tset_pod_webhook_failure_policy Fail Ignore\n\tstage_read_only_job_completion\n\tset_pod_webhook_failure_policy Ignore Fail\n\tstart_runtime_deployments\n",
			wantError:   "current-release read-only Job cleanup staging",
		},
		{
			name:        "CRD next-release upgrade skips late-failure recovery",
			child:       "crd-upgrade",
			old:         "\tprove_late_failure_recovery \"$CURRENT_RELEASE_CONTROLLER_IMAGE\"\n",
			replacement: "\t: # late failure recovery removed\n",
			wantError:   "successor read-only Job dispatch before the late failure",
		},
		{
			name:        "CRD successor read-only Job cleanup proof removed",
			child:       "crd-upgrade",
			old:         "\twait_for_read_only_job_cleanup\n\tquiesce_read_only_job_schema\n\tassert_predecessor_apply_remains_exclusive_while_running\n\trelease_running_apply_barrier\n\twait_for_predecessor_apply_job_terminal\n\twait_for_predecessor_apply_job_cleanup\n\tafter_revision=",
			replacement: "\tquiesce_read_only_job_schema\n\tassert_predecessor_apply_remains_exclusive_while_running\n\trelease_running_apply_barrier\n\twait_for_predecessor_apply_job_terminal\n\twait_for_predecessor_apply_job_cleanup\n\tafter_revision=",
			wantError:   "same-candidate retry, read-only Job cleanup and running Apply adoption",
		},
		{
			name:        "CRD read-only Job terminal fixture accepts partial invariant",
			child:       "crd-upgrade",
			old:         "read_only_job_terminal=1\n\t\t\tbreak",
			replacement: "break",
			wantError:   "read-only Job full terminal invariant latch",
		},
		{
			name:        "CRD read-only Job terminal fixture ignores active Pod accounting",
			child:       "crd-upgrade",
			old:         `((.status.active // 0) == 0) and`,
			replacement: `true and`,
			wantError:   "read-only Job complete native terminal predicate",
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
			name:        "CRD runtime Deployment recovery proof call removed",
			child:       "crd-upgrade",
			old:         "\tprove_runtime_deployment_recovery\n",
			replacement: "\ttrue # recovery proof removed\n",
			wantError:   "runtime deployment recovery proof call",
		},
		{
			name:        "CRD running Apply exclusivity proof call removed",
			child:       "crd-upgrade",
			old:         "\tassert_predecessor_apply_remains_exclusive_while_running\n",
			replacement: "\ttrue # running Apply exclusivity proof removed\n",
			wantError:   "same-candidate retry, read-only Job cleanup and running Apply adoption",
		},
		{
			name:        "CRD manager-only upgrade stops holding the schema unchanged",
			child:       "crd-upgrade",
			old:         "retires nothing.\n\tfor resource in ptahschema ptahschemaplan ptahschemaapproval; do\n",
			replacement: "retires nothing.\n\tfor resource in ptahschemaplan ptahschemaapproval; do\n",
			wantError:   "manager-only upgrade leaves the schema, its plan and its approval unchanged",
		},
		{
			name:        "CRD running Apply barrier released before the proof",
			child:       "crd-upgrade",
			old:         "\tassert_predecessor_apply_remains_exclusive_while_running\n\trelease_running_apply_barrier\n",
			replacement: "\trelease_running_apply_barrier\n\tassert_predecessor_apply_remains_exclusive_while_running\n",
			wantError:   "same-candidate retry, read-only Job cleanup and running Apply adoption",
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
			name:        "CRD rolled-back release uninstall may fail",
			child:       "crd-upgrade",
			old:         `fail "the uninstall of the rolled-back release failed; Helm's own error is above"`,
			replacement: `true`,
			wantError:   "rolled-back release uninstall",
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

// TestPhaseEnvironmentContractsRejectCriticalMutations measures what the audit
// refuses, because an audit that only passes proves nothing about the source it
// read. Each case takes away one property the lifecycle depends on and expects
// the failure to name it: the block match this replaced could say only that a
// block of text had moved.
//
// The last case is the regression that motivated the change. Reordering the
// bindings takes nothing away, so the audit has to accept it -- an addition in
// the middle of the block is what removed every lifecycle verdict from master.
func TestPhaseEnvironmentContractsRejectCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	harness := readE2ESource(t, files.harness)
	migrations := readE2ESource(t, files.migrations)
	referenceData := readE2ESource(t, files.referenceData)
	// Between the credentials and the engine, in every migration phase's call.
	isolationBindings := "E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n"
	tests := []struct {
		name          string
		script        bool
		referenceData bool
		old           string
		replacement   string
		wantError     string
	}{
		{
			name:        "binding removed",
			old:         "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n",
			replacement: "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			wantError:   `migrations-postgresql phase must bind E2E_REGISTRY_HOST_ADDRESS to "$REMOTE_REGISTRY", and binds nothing`,
		},
		{
			name: "candidate controller image redirected",
			old: "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\n" +
				"E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			replacement: "E2E_CONTROLLER_IMAGE=$PRODUCTION_OPERATOR_IMAGE \\\n" +
				"E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\n" +
				"E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			wantError: `migrations-postgresql phase must bind E2E_CONTROLLER_IMAGE to "$CANDIDATE_OPERATOR_IMAGE", and binds "$PRODUCTION_OPERATOR_IMAGE"`,
		},
		{
			// The version the phases assert against is read out of the Makefile
			// that stamps the chart. Writing the number here instead is the
			// mistake this case measures: it reads correctly, it agrees with
			// the chart on the day it is written, and it stops agreeing the
			// first time the contract moves.
			name: "state version pinned to a literal",
			old: "E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			replacement: "E2E_CONTROLLER_STATE_VERSION=1 \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			wantError: `migrations-postgresql phase must bind E2E_CONTROLLER_STATE_VERSION to "$CONTROLLER_STATE_VERSION", and binds "1"`,
		},
		{
			name:        "undeclared binding added",
			old:         "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n",
			replacement: "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_MIGRATION_INTERVAL=1s \\\n" + "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n",
			wantError:   "migrations-postgresql phase binds E2E_MIGRATION_INTERVAL, which no environment contract declares",
		},
		{
			name:        "phase left out",
			old:         "\trun_recorded_phase migrations-postgresql \"$ROOT_DIR/hack/e2e-migrations.sh\"\n",
			replacement: "\t:\n",
			wantError:   `lifecycle phase "migrations-postgresql" is never invoked`,
		},
		{
			name:        "phase pointed at another script",
			old:         "\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-migrations.sh\"\n",
			replacement: "\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-assert.sh\"\n",
			wantError:   `lifecycle phase "migrations-mysql" must run hack/e2e-migrations.sh, not hack/e2e-assert.sh`,
		},
		{
			name:        "phase invoked twice",
			old:         "\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-migrations.sh\"\n",
			replacement: "\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-migrations.sh\"\n\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-migrations.sh\"\n",
			wantError:   `lifecycle phase "migrations-mysql" is invoked more than once`,
		},
		{
			name:        "script grows an input nobody passes",
			script:      true,
			old:         "INTERVAL=${E2E_MIGRATION_INTERVAL:-5m}\n",
			replacement: "INTERVAL=${E2E_MIGRATION_INTERVAL:-5m}\nSOURCE_AUTHORITY=$E2E_SOURCE_AUTHORITY\n",
			wantError:   "hack/e2e-migrations.sh reads E2E_SOURCE_AUTHORITY without a default, and the migrations-postgresql phase binds nothing to it",
		},
		{
			name:        "script stops reading what it is passed",
			script:      true,
			old:         "REGISTRY_HOST_ADDRESS=${E2E_REGISTRY_HOST_ADDRESS:-}\n",
			replacement: "REGISTRY_HOST_ADDRESS=\n",
			wantError:   "migrations-postgresql phase binds E2E_REGISTRY_HOST_ADDRESS, which hack/e2e-migrations.sh never reads",
		},
		{
			name: "bindings reordered",
			old: "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" +
				isolationBindings + "E2E_ENGINE=postgresql \\\n",
			replacement: "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\n" +
				"E2E_ENGINE=postgresql \\\n",
			wantError: "",
		},
		{
			name:        "isolation worker container unbound",
			old:         "E2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=mysql \\\n",
			replacement: "E2E_ENGINE=mysql \\\n",
			wantError:   `migrations-mysql phase must bind E2E_KIND_CLUSTER_NAME to "$CLUSTER_NAME", and binds nothing`,
		},
		{
			name: "isolation worker reached through another Docker daemon",
			old: "E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n" +
				"E2E_ENGINE=postgresql \\\n",
			replacement: "E2E_DOCKER_CONTEXT=$SELECTED_DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n" +
				"E2E_ENGINE=postgresql \\\n",
			wantError: `migrations-postgresql phase must bind E2E_DOCKER_CONTEXT to "$DOCKER_CONTEXT", and binds "$SELECTED_DOCKER_CONTEXT"`,
		},
		{
			name:        "script stops reading the cluster it isolates a node of",
			script:      true,
			old:         "KIND_CLUSTER_NAME=${E2E_KIND_CLUSTER_NAME:-}\n",
			replacement: "KIND_CLUSTER_NAME=kind\n",
			wantError:   "migrations-postgresql phase binds E2E_KIND_CLUSTER_NAME, which hack/e2e-migrations.sh never reads",
		},
		{
			// A phase that isolates a node, in a suite the audit no longer
			// knows to give one: the declaration is what the suite check reads.
			name:        "script stops declaring the isolation key",
			script:      true,
			old:         "ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation\n",
			replacement: "ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolated\n",
			wantError:   "the migrations-postgresql phase isolates a node, and hack/e2e-migrations.sh does not declare ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation",
		},
		{
			name:          "another phase's script declares the isolation key",
			referenceData: true,
			old:           "PHASE_ENGINE=${E2E_ENGINE:-}\n",
			replacement:   "PHASE_ENGINE=${E2E_ENGINE:-}\nISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation\n",
			wantError:     "hack/e2e-reference-data.sh declares ISOLATION_NODE_KEY=operator.ptah.run/e2e-isolation, and the reference-data-postgresql phase is not one that isolates a node",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			switch {
			case test.script:
				mutatedFiles.migrations = writeMutatedE2ESource(
					t, "e2e-migrations.sh", migrations, test.old, test.replacement)
			case test.referenceData:
				mutatedFiles.referenceData = writeMutatedE2ESource(
					t, "e2e-reference-data.sh", referenceData, test.old, test.replacement)
			default:
				mutatedFiles.harness = writeMutatedE2ESource(
					t, "e2e-kind.sh", harness, test.old, test.replacement)
			}
			err := verifyPhaseEnvironmentContracts(mutatedFiles)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("verifyPhaseEnvironmentContracts() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyPhaseEnvironmentContracts() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func repositoryE2EWiringFiles() e2eWiringFiles {
	return e2eWiringFiles{
		makefile:                   filepath.Join("..", makefilePath),
		harness:                    filepath.Join("..", e2eHarnessPath),
		supportImageResolver:       filepath.Join("..", e2eSupportImageResolverPath),
		kindConfig:                 filepath.Join("..", e2eKindConfigPath),
		kindIsolationWorker:        filepath.Join("..", e2eKindIsolationWorkerPath),
		apiServerEndpointFilter:    filepath.Join("..", apiServerEndpointFilterPath),
		staticChecks:               filepath.Join("..", e2eStaticPath),
		dataPlane:                  filepath.Join("..", e2eDataPlanePath),
		assertions:                 filepath.Join("..", e2eAssertPath),
		crdUpgrade:                 filepath.Join("..", e2eCRDUpgradePath),
		faults:                     filepath.Join("..", e2eFaultsPath),
		highAvailability:           filepath.Join("..", e2eHAPath),
		certRotation:               filepath.Join("..", e2eCertRotationPath),
		migrations:                 filepath.Join("..", e2eMigrationsPath),
		referenceData:              filepath.Join("..", e2eReferenceDataPath),
		alerting:                   filepath.Join("..", e2eAlertingPath),
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

// TestPublishedPortsStayBelowTheEphemeralFloor measures the rule rather than
// pinning the literals: a future edit that moves either range back over the
// kernel's ephemeral floor reintroduces the race that failed master with
//
//	failed to bind host port for 127.0.0.1:47966:172.18.0.6:6443/tcp:
//	address already in use
//
// A port at or above the floor can be handed to an outgoing connection as its
// source port, and this suite pulls images immediately before kind binds the
// cluster's published port.
func TestPublishedPortsStayBelowTheEphemeralFloor(t *testing.T) {
	t.Parallel()

	source := readE2ESource(t, repositoryE2EWiringFiles().harness)

	// The lowest floor Linux is configured with in practice. The harness reads
	// the running kernel's own value; this is the bound the ranges are written
	// against, so the test does not depend on the machine it runs on.
	const ephemeralFloor = 32768

	for _, test := range []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			name:    "api server",
			pattern: regexp.MustCompile(`E2E_API_SERVER_PORT=\$\(\((\d+) \+ port_seed % (\d+)\)\)`),
		},
		{
			name:    "registry",
			pattern: regexp.MustCompile(`E2E_REGISTRY_PORT=\$\(\((\d+) \+ registry_seed % (\d+)\)\)`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			match := test.pattern.FindStringSubmatch(source)
			if match == nil {
				t.Fatalf("no port derivation found for %s in %s", test.name, repositoryE2EWiringFiles().harness)
			}
			base, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("base: %v", err)
			}
			span, err := strconv.Atoi(match[2])
			if err != nil {
				t.Fatalf("span: %v", err)
			}
			highest := base + span - 1
			if highest >= ephemeralFloor {
				t.Fatalf("%s derives up to %d, which reaches the ephemeral floor %d; keep the range below it",
					test.name, highest, ephemeralFloor)
			}
		})
	}

	// The derived ranges are only half the rule: an operator may supply either
	// port, and the race does not care who chose it.
	if !strings.Contains(source, `if [ "$published_port" -ge "$EPHEMERAL_PORT_FLOOR" ]; then`) {
		t.Fatal("the harness no longer refuses a supplied port at or above the ephemeral floor")
	}
	if !strings.Contains(source, "/proc/sys/net/ipv4/ip_local_port_range") {
		t.Fatal("the harness no longer reads the running kernel's ephemeral floor")
	}
}
