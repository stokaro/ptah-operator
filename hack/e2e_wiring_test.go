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

	if err := verifyE2EWiring(repositoryE2EWiringFiles()); err != nil {
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
			name:        "CRD proof call removed",
			child:       "crd-upgrade",
			old:         "prove_runtime_singleton_guard\n",
			replacement: "true # singleton proof removed\n",
			wantError:   "runtime singleton proof call",
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
		makefile:         filepath.Join("..", makefilePath),
		harness:          filepath.Join("..", e2eHarnessPath),
		dataPlane:        filepath.Join("..", e2eDataPlanePath),
		assertions:       filepath.Join("..", e2eAssertPath),
		crdUpgrade:       filepath.Join("..", e2eCRDUpgradePath),
		faults:           filepath.Join("..", e2eFaultsPath),
		highAvailability: filepath.Join("..", e2eHAPath),
		certRotation:     filepath.Join("..", e2eCertRotationPath),
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
