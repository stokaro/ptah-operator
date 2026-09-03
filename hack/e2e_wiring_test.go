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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyE2EWiring(t *testing.T) {
	t.Parallel()

	if err := verifyE2EWiring(
		filepath.Join("..", makefilePath),
		filepath.Join("..", e2eHarnessPath),
		filepath.Join("..", e2eDataPlanePath),
	); err != nil {
		t.Fatalf("verifyE2EWiring() error = %v", err)
	}
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

	makefile := filepath.Join("..", makefilePath)
	harness := filepath.Join("..", e2eHarnessPath)
	dataPlane := filepath.Join("..", e2eDataPlanePath)
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
			name:        "upgrade lifecycle omitted",
			old:         "E2E_PHASE=upgrade \\\n",
			replacement: "E2E_PHASE=upgrade-omitted \\\n",
			wantError:   "candidate upgrade lifecycle",
		},
		{
			name:        "high availability lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-ha.sh"`,
			replacement: `true # high availability lifecycle omitted`,
			wantError:   "high-availability lifecycle",
		},
		{
			name:        "control plane lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-assert.sh"`,
			replacement: `true # control plane lifecycle omitted`,
			wantError:   "control-plane lifecycle",
		},
		{
			name:        "certificate lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-cert-rotation.sh"`,
			replacement: `true # certificate lifecycle omitted`,
			wantError:   "certificate lifecycle",
		},
		{
			name:        "data plane lifecycle omitted",
			old:         `"$ROOT_DIR/hack/e2e-dataplane.sh"`,
			replacement: `true # data plane lifecycle omitted`,
			wantError:   "data-plane and OCI lifecycle",
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
			mutatedHarness := writeMutatedE2ESource(t, "e2e-kind.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(makefile, mutatedHarness, dataPlane)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyE2EDataPlaneRejectsCriticalMutations(t *testing.T) {
	t.Parallel()

	makefile := filepath.Join("..", makefilePath)
	harness := filepath.Join("..", e2eHarnessPath)
	dataPlane := filepath.Join("..", e2eDataPlanePath)
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
			name:        "OCI lifecycle implementation omitted",
			old:         `run_engine_lifecycle() {`,
			replacement: `run_engine_lifecycle_omitted() {`,
			wantError:   "OCI lifecycle implementation",
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
			mutatedDataPlane := writeMutatedE2ESource(t, "e2e-dataplane.sh", source, test.old, test.replacement)
			err := verifyE2EWiring(makefile, harness, mutatedDataPlane)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyE2EWiring() error = %v, want substring %q", err, test.wantError)
			}
		})
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
