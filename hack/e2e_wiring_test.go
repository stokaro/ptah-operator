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
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
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
			name:        "Go phases built from live checkout",
			old:         `go -C "$ROOT_DIR" test -tags e2e -c -o "$GO_PHASE_BINARY" ./test/e2e ||`,
			replacement: `go -C "$SOURCE_REPOSITORY_ROOT" test -tags e2e -c -o "$GO_PHASE_BINARY" ./test/e2e ||`,
			wantError:   "original checkout must have only",
		},
		{
			name:        "Go phase run from live checkout",
			old:         `(cd "$ROOT_DIR/test/e2e" &&`,
			replacement: `(cd "$SOURCE_REPOSITORY_ROOT/test/e2e" &&`,
			wantError:   "live checkout path escapes",
		},
		{
			name:        "Go phases built from somewhere else",
			old:         `go -C "$ROOT_DIR" test -tags e2e -c -o "$GO_PHASE_BINARY" ./test/e2e ||`,
			replacement: `go -C "$ROOT_DIR/.." test -tags e2e -c -o "$GO_PHASE_BINARY" ./test/e2e ||`,
			wantError:   "exact snapshot path",
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
			name:        "control-plane memory setup omitted",
			old:         "\tconfigure_control_plane_memory\n",
			replacement: "\t: # control-plane memory setup omitted\n",
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
				"assert_kubelet_log_budget\n" +
				"assert_api_server_endpoint_inventory",
			replacement: "require_ready_nodes \"after kind cluster creation\"\n" +
				": # HA topology assertion omitted\n" +
				"assert_kubelet_log_budget\n" +
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
			old:         `run_recorded_phase upgrade run_go_phase upgrade`,
			replacement: `true # upgrade lifecycle omitted`,
			wantError:   "candidate upgrade lifecycle",
		},
		{
			name: "upgrade lifecycle hidden in false branch",
			old:  "E2E_KUBERNETES_VERSION=$K8S_VERSION \\\n\trun_recorded_phase upgrade run_go_phase upgrade",
			replacement: "if false; then\nE2E_KUBERNETES_VERSION=$K8S_VERSION \\\n" +
				"\trun_recorded_phase upgrade run_go_phase upgrade\nfi",
			wantError: "always-false wrapper",
		},
		{
			name:        "upgrade lifecycle call separated from its environment",
			old:         `run_recorded_phase upgrade run_go_phase upgrade`,
			replacement: "true\n\trun_recorded_phase upgrade run_go_phase upgrade",
			wantError:   `upgrade phase must bind E2E_KUBECONFIG to "$KUBECONFIG_FILE", and binds nothing`,
		},
		{
			name:        "high availability lifecycle omitted",
			old:         `run_recorded_phase ha run_go_phase ha`,
			replacement: `true # high availability lifecycle omitted`,
			wantError:   "high-availability lifecycle",
		},
		{
			name:        "high availability lifecycle hidden in false branch",
			old:         `run_recorded_phase ha run_go_phase ha`,
			replacement: "if false; then\n\trun_recorded_phase ha run_go_phase ha\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "control plane lifecycle omitted",
			old:         `run_recorded_phase assert run_go_phase assert`,
			replacement: `true # control plane lifecycle omitted`,
			wantError:   "control-plane lifecycle",
		},
		{
			name:        "control plane lifecycle hidden in false branch",
			old:         `run_recorded_phase assert run_go_phase assert`,
			replacement: "if false; then\n\trun_recorded_phase assert run_go_phase assert\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "certificate lifecycle omitted",
			old:         `run_recorded_phase cert-rotation run_go_phase cert-rotation`,
			replacement: `true # certificate lifecycle omitted`,
			wantError:   "certificate lifecycle",
		},
		{
			name:        "certificate lifecycle hidden in false branch",
			old:         `run_recorded_phase cert-rotation run_go_phase cert-rotation`,
			replacement: "if false; then\n\trun_recorded_phase cert-rotation run_go_phase cert-rotation\nfi",
			wantError:   "always-false wrapper",
		},
		{
			// Every Go phase passes through the runner, so a runner that
			// returns without running the binary passes all of them.
			name:        "Go phase runner that returns first",
			old:         "run_go_phase() {\n",
			replacement: "run_go_phase() {\n\treturn 0\n",
			wantError:   "Go phase runner contract",
		},
		{
			name:        "Go phase runner asked for a fixed phase",
			old:         `"$GO_PHASE_BINARY" -test.v -e2e.phase="$1" -e2e.completed="$go_phase_record") || return 1`,
			replacement: `"$GO_PHASE_BINARY" -test.v -e2e.phase=cert-rotation -e2e.completed="$go_phase_record") || return 1`,
			wantError:   "Go phase runner contract",
		},
		{
			// The record is the evidence a phase passed; a runner that trusts
			// the exit status alone passes a program that ran nothing.
			name:        "Go phase runner that ignores the completion record",
			old:         "\t[ \"$(cat -- \"$go_phase_record\" 2>/dev/null)\" = \"$1\" ] || {\n",
			replacement: "\t[ -n \"$1\" ] || {\n",
			wantError:   "Go phase runner contract",
		},
		{
			name:        "Go phase runner defined twice",
			old:         "run_go_phase() {\n",
			replacement: "run_go_phase() { :; }\nrun_go_phase() {\n",
			wantError:   "run_go_phase must have exactly one",
		},
		{
			name:        "Go phase binary pointed at another program after the build",
			old:         "\t\tfail \"the Go acceptance phases do not compile\"\nfi\n",
			replacement: "\t\tfail \"the Go acceptance phases do not compile\"\nfi\nGO_PHASE_BINARY=/usr/bin/true\n",
			wantError:   "GO_PHASE_BINARY must appear exactly three times",
		},
		{
			name:        "Go phase binary assigned another program",
			old:         "GO_PHASE_BINARY=$WORK_DIR/ptah-e2e.test\n",
			replacement: "GO_PHASE_BINARY=/usr/bin/true\n",
			wantError:   "must be assigned exactly once, found 0",
		},
		{
			name:        "Go phase binary overwritten after the build",
			old:         "\t\tfail \"the Go acceptance phases do not compile\"\nfi\n",
			replacement: "\t\tfail \"the Go acceptance phases do not compile\"\nfi\ncp /usr/bin/true \"$GO_PHASE_BINARY\"\n",
			wantError:   "GO_PHASE_BINARY must appear exactly three times",
		},
		{
			name:        "schema faults omitted",
			old:         `run_recorded_phase schema-faults run_go_phase schema-faults`,
			replacement: `true # schema faults omitted`,
			wantError:   "schema fault recovery",
		},
		{
			name:        "schema faults changed to preparation",
			old:         "E2E_DATAPLANE_MODE=full \\\n\trun_recorded_phase schema-faults",
			replacement: "E2E_DATAPLANE_MODE=prepare \\\n\trun_recorded_phase schema-faults",
			wantError:   `schema-faults phase must bind E2E_DATAPLANE_MODE to "full", and binds "prepare"`,
		},
		{
			name:        "data plane lifecycle omitted",
			old:         `run_recorded_phase dataplane run_go_phase dataplane`,
			replacement: `true # data plane lifecycle omitted`,
			wantError:   "data-plane and OCI lifecycle",
		},
		{
			name:        "data plane lifecycle hidden in false branch",
			old:         `run_recorded_phase dataplane run_go_phase dataplane`,
			replacement: "if false; then\n\trun_recorded_phase dataplane run_go_phase dataplane\nfi",
			wantError:   "always-false wrapper",
		},
		{
			name:        "PostgreSQL migration lifecycle omitted",
			old:         `run_recorded_phase migrations-postgresql run_go_phase migrations-postgresql`,
			replacement: `true # migration lifecycle omitted`,
			wantError:   "PostgreSQL migration lifecycle",
		},
		{
			name:        "MySQL migration lifecycle omitted",
			old:         `run_recorded_phase migrations-mysql run_go_phase migrations-mysql`,
			replacement: `true # migration lifecycle omitted`,
			wantError:   "MySQL migration lifecycle",
		},
		{
			name:        "migration lifecycle call separated from its environment",
			old:         `run_recorded_phase migrations-postgresql run_go_phase migrations-postgresql`,
			replacement: "true\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql",
			wantError:   `migrations-postgresql phase must bind E2E_KUBECONFIG to "$KUBECONFIG_FILE", and binds nothing`,
		},
		{
			name:        "PostgreSQL reference-data lifecycle omitted",
			old:         `run_recorded_phase reference-data-postgresql run_go_phase reference-data-postgresql`,
			replacement: `true # reference-data lifecycle omitted`,
			wantError:   "PostgreSQL reference-data lifecycle",
		},
		{
			name:        "MySQL reference-data lifecycle omitted",
			old:         `run_recorded_phase reference-data-mysql run_go_phase reference-data-mysql`,
			replacement: `true # reference-data lifecycle omitted`,
			wantError:   "MySQL reference-data lifecycle",
		},
		{
			name:        "reference-data lifecycle call separated from its environment",
			old:         `run_recorded_phase reference-data-mysql run_go_phase reference-data-mysql`,
			replacement: "true\n\trun_recorded_phase reference-data-mysql run_go_phase reference-data-mysql",
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
			old:         "E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\nE2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\nE2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\nE2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\nE2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\nE2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
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
			old:         `run_recorded_phase uninstall run_go_phase uninstall`,
			replacement: `true # uninstall lifecycle omitted`,
			wantError:   "uninstall lifecycle",
		},
		{
			name:        "synthetic next chart handoff omitted",
			old:         "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" + "E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" + "E2E_NEXT_CHART_PACKAGE= \\\n",
			wantError:   `uninstall phase must bind E2E_NEXT_CHART_PACKAGE to "$NEXT_CHART_PACKAGE", and binds ""`,
		},
		{
			name: "current release values handoff omitted",
			old: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE= \\\n" +
				"E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			wantError: `uninstall phase must bind E2E_CANDIDATE_VALUES_FILE to "$CANDIDATE_VALUES_FILE", and binds ""`,
		},
		{
			name: "current release image handoff omitted",
			old: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" +
				"E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \\\n" +
				"E2E_CONTROLLER_IMAGE= \\\n" +
				"E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			wantError: `uninstall phase must bind E2E_CONTROLLER_IMAGE to "$CANDIDATE_OPERATOR_IMAGE", and binds ""`,
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

// TestPhaseEnvironmentContractsRejectCriticalMutations measures what the audit
// refuses, because an audit that only passes proves nothing about the source it
// read. Each case takes away one property the lifecycle depends on and expects
// the failure to name it: the block match this replaced could say only that a
// block of text had moved.
//
// The last case is the regression that motivated the change. Reordering the
// bindings takes nothing away, so the audit has to accept it -- an addition in
// the middle of the block is what removed every lifecycle verdict from master.
// certRotationCall is the driver's call of the Go certificate phase.
const certRotationCall = "\trun_recorded_phase cert-rotation run_go_phase cert-rotation\n"

// Every input a Go phase declares has a driver variable to be bound to, and
// every line of that table feeds some phase: a binding nothing reads is a
// line that goes on reading as a contract.
func TestGoPhaseBindingsCoverTheDeclaredInputs(t *testing.T) {
	t.Parallel()
	read := map[string]bool{}
	for _, phase := range phases.All() {
		for _, name := range phase.Inputs() {
			read[name] = true
			if _, bound := goPhaseBinding(phase.Name, name); !bound {
				t.Errorf("Go phase %s reads %s, and goPhaseBindings does not say which driver variable feeds it", phase.Name, name)
			}
		}
	}
	if len(read) == 0 {
		t.Fatal("no Go phase declares an input, so the check above checked nothing")
	}
	for name := range goPhaseBindings {
		if !read[name] {
			t.Errorf("goPhaseBindings binds %s, which no Go phase reads", name)
		}
	}
}

func TestPhaseEnvironmentContractsRejectCriticalMutations(t *testing.T) {
	t.Parallel()

	files := repositoryE2EWiringFiles()
	harness := readE2ESource(t, files.harness)
	// Between the credentials and the engine, in every migration phase's call.
	isolationBindings := "E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n"
	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "binding removed",
			old:         "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			wantError:   `migrations-postgresql phase must bind E2E_REGISTRY_HOST_ADDRESS to "$REMOTE_REGISTRY", and binds nothing`,
		},
		{
			name: "candidate controller image redirected",
			old: "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" +
				"E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\n" +
				"E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_CONTROLLER_IMAGE=$PRODUCTION_OPERATOR_IMAGE \\\n" +
				"E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \\\n" +
				"E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
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
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_CONTROLLER_STATE_VERSION=1 \\\n" +
				"E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			wantError: `migrations-postgresql phase must bind E2E_CONTROLLER_STATE_VERSION to "$CONTROLLER_STATE_VERSION", and binds "1"`,
		},
		{
			name:        "undeclared binding added",
			old:         "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\nE2E_MIGRATION_INTERVAL=1s \\\n" + "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" + isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			wantError:   "migrations-postgresql phase binds E2E_MIGRATION_INTERVAL, which test/e2e/phases does not declare it reads",
		},
		{
			name:        "phase left out",
			old:         "\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "\t:\n",
			wantError:   `Go phase "migrations-postgresql" is never invoked`,
		},
		{
			name:        "phase pointed at a script",
			old:         "\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n",
			replacement: "\trun_recorded_phase migrations-mysql \"$ROOT_DIR/hack/e2e-phase.sh\"\n",
			wantError:   "migrations-mysql is a Go phase and must run through run_go_phase migrations-mysql",
		},
		{
			name:        "phase invoked twice",
			old:         "\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n",
			replacement: "\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n",
			wantError:   `lifecycle phase "migrations-mysql" is invoked more than once`,
		},
		{
			// Each migration phase runs the engine its name ends in, which is
			// the one binding goPhaseBindings cannot hold for every phase alike.
			name:        "PostgreSQL phase bound to the other engine",
			old:         "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql",
			replacement: "E2E_ENGINE=mysql \\\n\trun_recorded_phase migrations-postgresql",
			wantError:   `migrations-postgresql phase must bind E2E_ENGINE to "postgresql", and binds "mysql"`,
		},
		{
			name:        "migration phase left without an engine",
			old:         "E2E_ENGINE=mysql \\\n\trun_recorded_phase migrations-mysql",
			replacement: "\trun_recorded_phase migrations-mysql",
			wantError:   `migrations-mysql phase must bind E2E_ENGINE to "mysql", and binds nothing`,
		},
		// A Go phase declares what it reads in test/e2e/phases, and the call
		// is held to exactly that declaration.
		{
			name:        "Go phase input unbound",
			old:         "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" + certRotationCall,
			replacement: certRotationCall,
			wantError:   `cert-rotation phase must bind E2E_CHART_PACKAGE to "$CHART_PACKAGE", and binds nothing`,
		},
		{
			name:        "Go phase input redirected",
			old:         "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" + certRotationCall,
			replacement: "E2E_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n" + certRotationCall,
			wantError:   `cert-rotation phase must bind E2E_CHART_PACKAGE to "$CHART_PACKAGE", and binds "$NEXT_CHART_PACKAGE"`,
		},
		{
			name:        "Go phase handed an input it does not read",
			old:         "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" + certRotationCall,
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\nE2E_ENGINE=postgresql \\\n" + certRotationCall,
			wantError:   "cert-rotation phase binds E2E_ENGINE, which test/e2e/phases does not declare it reads",
		},
		{
			name:        "Go phase input bound twice",
			old:         "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" + certRotationCall,
			replacement: "E2E_CHART_PACKAGE=$CHART_PACKAGE \\\nE2E_CHART_PACKAGE=$CHART_PACKAGE \\\n" + certRotationCall,
			wantError:   "cert-rotation phase binds E2E_CHART_PACKAGE twice",
		},
		{
			name:        "Go phase recorded under one name and run under another",
			old:         certRotationCall,
			replacement: "\trun_recorded_phase cert-rotation run_go_phase dataplane\n",
			wantError:   "run_recorded_phase cert-rotation runs the Go phase dataplane",
		},
		{
			name:        "Go phase run as a script",
			old:         certRotationCall,
			replacement: "\trun_recorded_phase cert-rotation \"$ROOT_DIR/hack/e2e-phase.sh\"\n",
			wantError:   "cert-rotation is a Go phase and must run through run_go_phase cert-rotation",
		},
		{
			// Every phase is a Go phase, so a script call is refused even for
			// a phase the catalog has never heard of.
			name:        "undeclared phase run as a script",
			old:         certRotationCall,
			replacement: certRotationCall + "\trun_recorded_phase legacy \"$ROOT_DIR/hack/e2e-legacy.sh\"\n",
			wantError:   `lifecycle phase "legacy" runs hack/e2e-legacy.sh; every phase is a Go phase`,
		},
		{
			name:        "undeclared Go phase invoked",
			old:         certRotationCall,
			replacement: certRotationCall + "\trun_recorded_phase legacy run_go_phase legacy\n",
			wantError:   `lifecycle phase "legacy" is invoked but test/e2e/phases does not declare it`,
		},
		{
			// The upgrade phase reads no database and no registry; the script
			// was handed both, and the Go phase must not be.
			name:        "upgrade phase handed the running Apply's database",
			old:         "E2E_KUBERNETES_VERSION=$K8S_VERSION \\\n\trun_recorded_phase upgrade run_go_phase upgrade",
			replacement: "E2E_KUBERNETES_VERSION=$K8S_VERSION \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\n\trun_recorded_phase upgrade run_go_phase upgrade",
			wantError:   "upgrade phase binds E2E_DOCKER_CONTEXT, which test/e2e/phases does not declare it reads",
		},
		{
			name:        "upgrade phase still told which script path to take",
			old:         "E2E_KUBERNETES_VERSION=$K8S_VERSION \\\n\trun_recorded_phase upgrade run_go_phase upgrade",
			replacement: "E2E_KUBERNETES_VERSION=$K8S_VERSION \\\nE2E_PHASE=upgrade \\\n\trun_recorded_phase upgrade run_go_phase upgrade",
			wantError:   "upgrade phase binds E2E_PHASE, which test/e2e/phases does not declare it reads",
		},
		{
			name:        "uninstall phase handed a synthetic next chart it cannot trust",
			old:         "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" + "E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \\\n",
			replacement: "E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \\\n" + "E2E_NEXT_CHART_PACKAGE=$CHART_PACKAGE \\\n",
			wantError:   `uninstall phase must bind E2E_NEXT_CHART_PACKAGE to "$NEXT_CHART_PACKAGE", and binds "$CHART_PACKAGE"`,
		},
		{
			name:        "high availability phase test namespace unbound",
			old:         "E2E_HA_TEST_NAMESPACE=$HA_TEST_NAMESPACE \\\n",
			replacement: "",
			wantError:   `ha phase must bind E2E_HA_TEST_NAMESPACE to "$HA_TEST_NAMESPACE", and binds nothing`,
		},
		{
			name:        "Go phase left out",
			old:         certRotationCall,
			replacement: "\t:\n",
			wantError:   `Go phase "cert-rotation" is never invoked`,
		},
		{
			name: "bindings reordered",
			old: "E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" +
				isolationBindings + "E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \\\n" +
				"E2E_REGISTRY_HOST_ADDRESS=$REMOTE_REGISTRY \\\n" +
				"E2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\n" +
				"E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			wantError: "",
		},
		{
			name:        "isolation worker container unbound",
			old:         "E2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\nE2E_ENGINE=mysql \\\n\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n",
			replacement: "E2E_ENGINE=mysql \\\n\trun_recorded_phase migrations-mysql run_go_phase migrations-mysql\n",
			wantError:   `migrations-mysql phase must bind E2E_KIND_CLUSTER_NAME to "$CLUSTER_NAME", and binds nothing`,
		},
		{
			name: "isolation worker reached through another Docker daemon",
			old: "E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n" +
				"E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			replacement: "E2E_DOCKER_CONTEXT=$SELECTED_DOCKER_CONTEXT \\\nE2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n" +
				"E2E_ENGINE=postgresql \\\n\trun_recorded_phase migrations-postgresql run_go_phase migrations-postgresql\n",
			wantError: `migrations-postgresql phase must bind E2E_DOCKER_CONTEXT to "$DOCKER_CONTEXT", and binds "$SELECTED_DOCKER_CONTEXT"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutatedFiles := files
			mutatedFiles.harness = writeMutatedE2ESource(t, "e2e-kind.sh", harness, test.old, test.replacement)
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
		makefile:                 filepath.Join("..", makefilePath),
		harness:                  filepath.Join("..", e2eHarnessPath),
		supportImageResolver:     filepath.Join("..", e2eSupportImageResolverPath),
		kindConfig:               filepath.Join("..", e2eKindConfigPath),
		kindIsolationWorker:      filepath.Join("..", e2eKindIsolationWorkerPath),
		apiServerEndpointFilter:  filepath.Join("..", apiServerEndpointFilterPath),
		staticChecks:             filepath.Join("..", e2eStaticPath),
		admissionSchemaContract:  filepath.Join("..", admissionSchemaContractPath),
		admissionSchemaSelftest:  filepath.Join("..", admissionSchemaSelftestPath),
		controllerSchemaContract: filepath.Join("..", controllerSchemaContractPath),
		controllerSchemaSelftest: filepath.Join("..", controllerSchemaSelftestPath),
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
