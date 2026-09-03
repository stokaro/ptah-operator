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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLoadAndValidateManifestFreshness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lastVerified string
		now          string
		wantError    string
	}{
		{
			name:         "exact maximum age is accepted",
			lastVerified: "2026-08-31",
			now:          "2026-10-05",
		},
		{
			name:         "older verification is stale",
			lastVerified: "2026-08-31",
			now:          "2026-10-06",
			wantError:    "maximum age is 35 days",
		},
		{
			name:         "future verification is rejected",
			lastVerified: "2026-08-31",
			now:          "2026-08-30",
			wantError:    "is after validation date",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "kubernetes.json")
			manifest := fmt.Sprintf(`{
  "schemaVersion": 1,
  "policy": "upstream-active-minors",
  "windowSize": 3,
  "lastVerified": %q,
  "kindVersion": "v0.32.0",
  "releases": [
    {"minor": "1.35", "nodeImage": "kindest/node:v1.35.5@sha256:%s"},
    {"minor": "1.36", "nodeImage": "kindest/node:v1.36.1@sha256:%s"},
    {"minor": "1.37", "nodeImage": "kindest/node:v1.37.0@sha256:%s"}
  ]
}
`, test.lastVerified, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64))
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			now, err := time.Parse("2006-01-02", test.now)
			if err != nil {
				t.Fatalf("parse test date: %v", err)
			}
			_, _, err = loadAndValidateManifest(path, now)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("loadAndValidateManifest() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("loadAndValidateManifest() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidationDateRejectsNonDate(t *testing.T) {
	t.Parallel()
	if _, err := validationDate("2026-8-31"); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Fatalf("validationDate() error = %v, want strict date error", err)
	}
}

func TestVerifyKubernetesDependencyWindow(t *testing.T) {
	t.Parallel()

	releases := []parsedRelease{
		{release: release{Minor: "1.35"}, major: 1, minor: 35},
		{release: release{Minor: "1.36"}, major: 1, minor: 36},
		{release: release{Minor: "1.37"}, major: 1, minor: 37},
	}
	moduleFile := func(versionByModule map[string]string) string {
		var contents strings.Builder
		contents.WriteString("module example.test/operator\n\nrequire (\n")
		for _, module := range []string{
			"k8s.io/api",
			"k8s.io/apiextensions-apiserver",
			"k8s.io/apimachinery",
			"k8s.io/client-go",
		} {
			version := versionByModule[module]
			if version != "" {
				fmt.Fprintf(&contents, "\t%s %s\n", module, version)
			}
		}
		contents.WriteString(")\n")
		return contents.String()
	}
	all := func(version string) map[string]string {
		return map[string]string{
			"k8s.io/api":                     version,
			"k8s.io/apiextensions-apiserver": version,
			"k8s.io/apimachinery":            version,
			"k8s.io/client-go":               version,
		}
	}
	tests := []struct {
		name      string
		versions  map[string]string
		wantError string
	}{
		{name: "same minor", versions: all("v0.37.0")},
		{name: "one-minor forward compatibility", versions: all("v0.36.1")},
		{
			name:      "support advanced by two minors",
			versions:  all("v0.35.9"),
			wantError: "2 minors ahead",
		},
		{
			name: "mixed Kubernetes module minors",
			versions: map[string]string{
				"k8s.io/api":                     "v0.36.1",
				"k8s.io/apiextensions-apiserver": "v0.36.1",
				"k8s.io/apimachinery":            "v0.37.0",
				"k8s.io/client-go":               "v0.36.1",
			},
			wantError: "must share one API minor",
		},
		{
			name:      "prerelease dependency",
			versions:  all("v0.37.0-beta.0"),
			wantError: "must have one stable",
		},
		{
			name: "missing direct dependency",
			versions: map[string]string{
				"k8s.io/api":                     "v0.36.1",
				"k8s.io/apiextensions-apiserver": "v0.36.1",
				"k8s.io/apimachinery":            "v0.36.1",
			},
			wantError: "k8s.io/client-go must have one stable",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "go.mod")
			if err := os.WriteFile(path, []byte(moduleFile(test.versions)), 0o600); err != nil {
				t.Fatalf("write go.mod fixture: %v", err)
			}
			_, err := verifyKubernetesDependencyWindow(path, releases)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("verifyKubernetesDependencyWindow() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyKubernetesDependencyWindow() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestVerifyKubernetesDependencyWindowProposalMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "go.mod")
	contents := `module example.test/operator

require (
	k8s.io/api v0.36.1
	k8s.io/apiextensions-apiserver v0.36.1
	k8s.io/apimachinery v0.36.1
	k8s.io/client-go v0.36.1
)
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write go.mod fixture: %v", err)
	}
	nextWindow := []parsedRelease{
		{release: release{Minor: "1.36"}, major: 1, minor: 36},
		{release: release{Minor: "1.37"}, major: 1, minor: 37},
		{release: release{Minor: "1.38"}, major: 1, minor: 38},
	}
	if _, err := verifyKubernetesDependencyWindow(path, nextWindow); err == nil || !strings.Contains(err.Error(), "2 minors ahead") {
		t.Fatalf("strict dependency verification error = %v, want two-minor skew rejection", err)
	}
	if _, err := verifyKubernetesDependencyWindowForMode(path, nextWindow, true); err != nil {
		t.Fatalf("proposal dependency verification rejected the immediate next window: %v", err)
	}

	skippedWindow := []parsedRelease{
		{release: release{Minor: "1.37"}, major: 1, minor: 37},
		{release: release{Minor: "1.38"}, major: 1, minor: 38},
		{release: release{Minor: "1.39"}, major: 1, minor: 39},
	}
	if _, err := verifyKubernetesDependencyWindowForMode(path, skippedWindow, true); err == nil ||
		!strings.Contains(err.Error(), "3 minors ahead") {
		t.Fatalf("proposal dependency verification error = %v, want skipped-window rejection", err)
	}
}

func TestVerifyReviewedJobAPIBoundary(t *testing.T) {
	t.Parallel()

	actualDigest := controllerJobAPISurfaceDigest()
	if err := verifyReviewedJobAPIBoundary(
		reviewedKubernetesAPIMinor,
		reviewedKubernetesSupportMaximum,
		actualDigest,
	); err != nil {
		t.Fatalf("verifyReviewedJobAPIBoundary() rejected the compiled profile: %v", err)
	}
	if err := verifyReviewedJobAPIBoundary(
		reviewedKubernetesAPIMinor,
		reviewedKubernetesSupportMaximum+1,
		actualDigest,
	); err == nil || !strings.Contains(err.Error(), "differs from reviewed Job API boundary") {
		t.Fatalf("verifyReviewedJobAPIBoundary() support error = %v", err)
	}
	if err := verifyReviewedJobAPIBoundary(
		reviewedKubernetesAPIMinor,
		reviewedKubernetesSupportMaximum,
		strings.Repeat("0", 64),
	); err == nil || !strings.Contains(err.Error(), "reachable Job API surface digest") {
		t.Fatalf("verifyReviewedJobAPIBoundary() digest error = %v", err)
	}
}

func TestVerifyJobAPIBoundaryProposalMode(t *testing.T) {
	t.Parallel()

	actualDigest := controllerJobAPISurfaceDigest()
	if err := verifyJobAPIBoundaryForMode(
		reviewedKubernetesAPIMinor,
		reviewedKubernetesSupportMaximum+1,
		actualDigest,
		true,
	); err != nil {
		t.Fatalf("proposal boundary rejected the immediate next minor: %v", err)
	}

	tests := []struct {
		name             string
		compiledMinor    int
		supportedMaximum int
		digest           string
		proposal         bool
	}{
		{
			name:          "ordinary verification cannot bypass review",
			compiledMinor: reviewedKubernetesAPIMinor, supportedMaximum: reviewedKubernetesSupportMaximum + 1,
			digest: actualDigest,
		},
		{
			name:          "proposal cannot skip a support minor",
			compiledMinor: reviewedKubernetesAPIMinor, supportedMaximum: reviewedKubernetesSupportMaximum + 2,
			digest: actualDigest, proposal: true,
		},
		{
			name:          "proposal cannot conceal dependency drift",
			compiledMinor: reviewedKubernetesAPIMinor + 1, supportedMaximum: reviewedKubernetesSupportMaximum + 1,
			digest: actualDigest, proposal: true,
		},
		{
			name:          "proposal cannot conceal reachable API drift",
			compiledMinor: reviewedKubernetesAPIMinor, supportedMaximum: reviewedKubernetesSupportMaximum + 1,
			digest: strings.Repeat("0", 64), proposal: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := verifyJobAPIBoundaryForMode(
				test.compiledMinor,
				test.supportedMaximum,
				test.digest,
				test.proposal,
			); err == nil {
				t.Fatal("verification accepted an unreviewed API boundary")
			}
		})
	}
}

func TestVerifyWorkflowRejectsSupportGateMutations(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", workflowPath)
	workflow := readTestWorkflow(t, path)
	if err := verifyWorkflow(path); err != nil {
		t.Fatalf("verifyWorkflow(valid) error = %v", err)
	}
	tests := map[string]struct {
		old string
		new string
	}{
		"workflow default shell": {
			old: "env:\n  GOFLAGS: -mod=readonly\n\njobs:\n",
			new: "env:\n  GOFLAGS: -mod=readonly\n\ndefaults:\n  run:\n    shell: 'true {0}'\n\njobs:\n",
		},
		"workflow default working directory": {
			old: "env:\n  GOFLAGS: -mod=readonly\n\njobs:\n",
			new: "env:\n  GOFLAGS: -mod=readonly\n\ndefaults:\n  run:\n    working-directory: /tmp\n\njobs:\n",
		},
		"support matrix output": {
			old: "      matrix: ${{ steps.matrix.outputs.matrix }}\n",
			new: "      matrix: '[]'\n",
		},
		"conditional matrix export": {
			old: "        id: matrix\n",
			new: "        id: matrix\n        if: ${{ false }}\n",
		},
		"shallow CRD history checkout": {
			old: "      - name: Check out repository\n        id: checkout\n        uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5\n        with:\n          fetch-depth: 0\n          persist-credentials: false\n",
			new: "      - name: Check out repository\n        id: checkout\n        uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5\n        with:\n          fetch-depth: 1\n          persist-credentials: false\n",
		},
		"pull request baseline from candidate": {
			old: "          PULL_REQUEST_BASE_SHA: ${{ github.event.pull_request.base.sha }}\n",
			new: "          PULL_REQUEST_BASE_SHA: ${{ github.sha }}\n",
		},
		"push baseline from candidate": {
			old: "          EVENT_BEFORE_SHA: ${{ github.event.before }}\n",
			new: "          EVENT_BEFORE_SHA: ${{ github.sha }}\n",
		},
		"scheduled fallback without parent": {
			old: "            schedule|workflow_dispatch)\n              baseline=\"$(resolve_exact_commit \"${CURRENT_SHA}^\")\"\n",
			new: "            schedule|workflow_dispatch)\n              baseline=\"$(resolve_exact_commit \"${CURRENT_SHA}\")\"\n",
		},
		"forged baseline output": {
			old: "          printf 'baseline=%s\\n' \"$baseline\" >> \"$GITHUB_OUTPUT\"\n",
			new: "          printf 'baseline=%s\\n' \"$CURRENT_SHA\" >> \"$GITHUB_OUTPUT\"\n",
		},
		"skipped CRD baseline selection": {
			old: "        id: crd-baseline\n",
			new: "        id: crd-baseline\n        if: ${{ false }}\n",
		},
		"project verification without explicit baseline": {
			old: "          CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE: \"true\"\n",
			new: "          CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE: \"false\"\n",
		},
		"project verification with unbound baseline": {
			old: "          CRD_SCHEMA_BASELINE_REF: ${{ steps.crd-baseline.outputs.baseline }}\n",
			new: "          CRD_SCHEMA_BASELINE_REF: ${{ github.sha }}\n",
		},
		"E2E dependencies": {
			old: "    needs: [support-matrix, verify]\n",
			new: "    needs: [support-matrix]\n",
		},
		"static E2E matrix": {
			old: "        include: ${{ fromJSON(needs.support-matrix.outputs.matrix) }}\n",
			new: "        include: []\n",
		},
		"conditional lifecycle": {
			old: "      - name: Run complete operator lifecycle\n        env:\n",
			new: "      - name: Run complete operator lifecycle\n        if: ${{ false }}\n        env:\n",
		},
		"job default shell": {
			old: "    timeout-minutes: 90\n    strategy:\n",
			new: "    timeout-minutes: 90\n    defaults:\n      run:\n        shell: 'true {0}'\n    strategy:\n",
		},
		"job default working directory": {
			old: "    timeout-minutes: 90\n    strategy:\n",
			new: "    timeout-minutes: 90\n    defaults:\n      run:\n        working-directory: /tmp\n    strategy:\n",
		},
		"bypassed lifecycle shell": {
			old: "        shell: bash\n        run: make e2e\n",
			new: "        shell: 'true {0}'\n        run: make e2e\n",
		},
		"make dry-run environment": {
			old: "        env:\n          DOCKER_CONTEXT: ${{ steps.docker-context.outputs.name }}\n",
			new: "        env:\n          MAKEFLAGS: --just-print\n          DOCKER_CONTEXT: ${{ steps.docker-context.outputs.name }}\n",
		},
		"wrong node image binding": {
			old: "          KIND_NODE_IMAGE: ${{ matrix.node_image }}\n",
			new: "          KIND_NODE_IMAGE: kindest/node:latest\n",
		},
		"duplicate lifecycle": {
			old: "        run: make e2e\n      - name: Remove explicit Docker context\n",
			new: "        run: make e2e\n      - name: Duplicate lifecycle\n        run: make e2e\n      - name: Remove explicit Docker context\n",
		},
		"verify timeout drift": {
			old: "    timeout-minutes: 20\n",
			new: "    timeout-minutes: 25\n",
		},
		"matrix timeout drift": {
			old: "    timeout-minutes: 10\n",
			new: "    timeout-minutes: 30\n",
		},
		"E2E timeout drift": {
			old: "    timeout-minutes: 90\n",
			new: "    timeout-minutes: 95\n",
		},
		"gate timeout drift": {
			old: "    timeout-minutes: 5\n",
			new: "    timeout-minutes: 4\n",
		},
		"unstable check name": {
			old: "    name: Kubernetes support gate\n",
			new: "    name: Kubernetes support window 1.35-1.37\n",
		},
		"conditional gate": {
			old: "    if: ${{ always() }}\n",
			new: "    if: ${{ success() }}\n",
		},
		"missing lifecycle dependency": {
			old: "    needs: [support-matrix, verify, kubernetes-e2e]\n",
			new: "    needs: [support-matrix, verify]\n",
		},
		"unbound lifecycle result": {
			old: "          KUBERNETES_E2E_RESULT: ${{ needs.kubernetes-e2e.result }}\n",
			new: "          KUBERNETES_E2E_RESULT: success\n",
		},
		"successful failure branch": {
			old: "              echo \"required Kubernetes support job concluded: $result\" >&2\n              exit 1\n",
			new: "              echo \"required Kubernetes support job concluded: $result\" >&2\n              exit 0\n",
		},
		"continue on error": {
			old: "  kubernetes-support-gate:\n    name: Kubernetes support gate\n",
			new: "  kubernetes-support-gate:\n    name: Kubernetes support gate\n    continue-on-error: true\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedWorkflow(t, workflow, test.old, test.new)
			if err := verifyCIWorkflowSemanticsAtPath(path); err == nil {
				t.Fatal("verifyCIWorkflowSemantics() accepted a critical mutation")
			}
		})
	}
}

func TestVerifyWorkflowDigestRejectsSetupEnvironmentMutation(t *testing.T) {
	t.Parallel()

	workflow := readTestWorkflow(t, filepath.Join("..", workflowPath))
	path := writeMutatedWorkflow(
		t,
		workflow,
		"          set -euo pipefail\n          sudo apt-get update\n",
		"          set -euo pipefail\n          echo 'MAKEFLAGS=--just-print' >> \"$GITHUB_ENV\"\n          sudo apt-get update\n",
	)
	if err := verifyCIWorkflowSemanticsAtPath(path); err != nil {
		t.Fatalf("semantic verifier unexpectedly caught whole-workflow mutation: %v", err)
	}
	if err := verifyWorkflow(path); err == nil || !strings.Contains(err.Error(), "workflow digest") {
		t.Fatalf("verifyWorkflow() error = %v, want whole-workflow digest rejection", err)
	}
}

func TestVerifyUpdateWorkflowRejectsDeliveryMutations(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", updateWorkflowPath)
	workflow := readTestWorkflow(t, path)
	if err := verifyUpdateWorkflow(path); err != nil {
		t.Fatalf("verifyUpdateWorkflow(valid) error = %v", err)
	}
	tests := map[string]struct {
		old string
		new string
	}{
		"skipped preparation job": {
			old: "  prepare:\n    name: Discover and validate maintained Kubernetes minors\n",
			new: "  prepare:\n    name: Discover and validate maintained Kubernetes minors\n    if: ${{ false }}\n",
		},
		"skipped delivery job": {
			old: "    if: needs.prepare.outputs.changed == 'true'\n",
			new: "    if: ${{ false }}\n",
		},
		"combined proposal and dispatch authority": {
			old: "    permissions:\n      contents: write\n      pull-requests: write\n    outputs:\n",
			new: "    permissions:\n      actions: write\n      contents: write\n      pull-requests: write\n    outputs:\n",
		},
		"dispatch content write authority": {
			old: "    permissions:\n      actions: write\n      contents: read\n      pull-requests: read\n    steps:\n",
			new: "    permissions:\n      actions: write\n      contents: write\n      pull-requests: read\n    steps:\n",
		},
		"no-op discovery": {
			old: "          go run ./hack/updatekubernetessupport\n",
			new: "          true # discovery omitted\n",
		},
		"no-op deterministic verification": {
			old: "          go test ./hack ./hack/updatekubernetessupport\n",
			new: "          true # deterministic tests omitted\n",
		},
		"proposal validation replaced by strict verification": {
			old: "          go run ./hack/verify-kubernetes-support.go -output=proposal -now \"$today\"\n",
			new: "          go run ./hack/verify-kubernetes-support.go -now \"$today\"\n",
		},
		"forced unchanged output": {
			old: "            'changed=true' \\\n",
			new: "            'changed=false' \\\n",
		},
		"unchecked status producer": {
			old: "          git diff --cached --quiet --exit-code\n\n          status_file=\"$RUNNER_TEMP/kubernetes-support-status\"\n          git status --porcelain=v1 --untracked-files=all > \"$status_file\"\n          mapfile -t status_lines < \"$status_file\"\n",
			new: "          git diff --cached --quiet --exit-code\n\n          mapfile -t status_lines < <(git status --porcelain=v1 --untracked-files=all)\n",
		},
		"review commit ancestry guard omitted": {
			old: "            git merge-base --is-ancestor \"$remote_parent\" \"$BASE_SHA\"\n",
			new: "            true # ancestry guard omitted\n",
		},
		"merged support branch classification omitted": {
			old: "            if ! git merge-base --is-ancestor \"$remote_oid\" \"$BASE_SHA\"; then\n",
			new: "            if true; then # every remote branch is treated as replaceable\n",
		},
		"review commit count guard omitted": {
			old: "            commits_ahead=\"$(git rev-list --count \"$BASE_SHA..$remote_oid\")\"\n",
			new: "            commits_ahead=1 # review commits ignored\n",
		},
		"review committer identity guard omitted": {
			old: "            [[ \"$(git show -s --format=%ce \"$remote_oid\")\" == '41898282+github-actions[bot]@users.noreply.github.com' ]]\n",
			new: "            true # committer identity guard omitted\n",
		},
		"prior support path audit omitted": {
			old: "            git diff-tree --no-commit-id --name-status -r \"$remote_oid\" > \"$prior_status_file\"\n",
			new: "            : > \"$prior_status_file\" # prior path audit omitted\n",
		},
		"skipped delivery step": {
			old: "        id: support-window-pr\n",
			new: "        id: support-window-pr\n        if: ${{ false }}\n",
		},
		"skipped dispatch evidence": {
			old: "        id: dispatch-evidence\n",
			new: "        id: dispatch-evidence\n        if: ${{ false }}\n",
		},
		"unbound pushed SHA": {
			old: "      pushed-sha: ${{ steps.support-window-pr.outputs.pushed-sha }}\n",
			new: "      pushed-sha: ${{ github.sha }}\n",
		},
		"unqualified pull request lookup": {
			old: "            -f head=\"$repository_owner:$support_branch\" \\\n",
			new: "            -f head=\"$support_branch\" \\\n",
		},
		"cross-repository pull request selection": {
			old: "                .head.repo.full_name == $repo\n",
			new: "                .head.repo.full_name != $repo\n",
		},
		"ambiguous same-repository pull request selection": {
			old: "          case \"${#same_repo_pr_numbers[@]}\" in\n",
			new: "          case 0 in\n",
		},
		"pull request head SHA": {
			old: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n",
			new: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid != $sha and\n",
		},
		"proposal cross-repository flag": {
			old: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n",
			new: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == true and\n",
		},
		"proposal head repository": {
			old: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n                .headRepository.nameWithOwner == $repo\n",
			new: "              --arg sha \"$pushed_sha\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n                .headRepository.nameWithOwner != $repo\n",
		},
		"dispatch cross-repository flag": {
			old: "              --arg sha \"$EXPECTED_SHA\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n",
			new: "              --arg sha \"$EXPECTED_SHA\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == true and\n",
		},
		"dispatch head repository": {
			old: "              --arg sha \"$EXPECTED_SHA\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n                .headRepository.nameWithOwner == $repo\n",
			new: "              --arg sha \"$EXPECTED_SHA\" '\n                .state == \"OPEN\" and\n                .baseRefName == $base and\n                .headRefName == $head and\n                .headRefOid == $sha and\n                .isCrossRepository == false and\n                .headRepository.nameWithOwner != $repo\n",
		},
		"dispatched run SHA": {
			old: "              -f head_sha=\"$EXPECTED_SHA\" \\\n              -f per_page=100)\"\n            before_ids=",
			new: "              -f per_page=100 \\\n              -f per_page=100)\"\n            before_ids=",
		},
		"CI dispatch": {
			old: "          require_dispatched_run ci.yml\n",
			new: "          true # CI dispatch omitted\n",
		},
		"release dispatch": {
			old: "          require_dispatched_run release.yml\n",
			new: "          true # Release dispatch omitted\n",
		},
		"continue on error": {
			old: "        id: support-window-pr\n",
			new: "        id: support-window-pr\n        continue-on-error: true\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedWorkflow(t, workflow, test.old, test.new)
			if err := verifyUpdateWorkflowSemanticsAtPath(path); err == nil {
				t.Fatal("verifyUpdateWorkflowSemantics() accepted a critical mutation")
			}
		})
	}
}

func TestVerifyUpdateWorkflowDigestRejectsSemanticNoOp(t *testing.T) {
	t.Parallel()

	workflow := readTestWorkflow(t, filepath.Join("..", updateWorkflowPath))
	path := writeMutatedWorkflow(
		t,
		workflow,
		"      - name: Check out the default branch\n",
		"      - name: Check out the default branch # audited policy changed\n",
	)
	if err := verifyUpdateWorkflowSemanticsAtPath(path); err != nil {
		t.Fatalf("semantic verifier unexpectedly rejected inert policy text: %v", err)
	}
	if err := verifyUpdateWorkflow(path); err == nil || !strings.Contains(err.Error(), "workflow digest") {
		t.Fatalf("verifyUpdateWorkflow() error = %v, want whole-workflow digest rejection", err)
	}
}

func TestVerifyReleaseWorkflowRejectsSupportEvidenceMutations(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", releaseWorkflowPath)
	workflow := readTestWorkflow(t, path)
	if err := verifyReleaseWorkflow(path); err != nil {
		t.Fatalf("verifyReleaseWorkflow(valid) error = %v", err)
	}
	tests := map[string]struct {
		old string
		new string
	}{
		"manual smoke trigger": {
			old: "  workflow_dispatch:\n",
			new: "  # workflow_dispatch removed\n",
		},
		"manual smoke guard": {
			old: "github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch'",
			new: "github.event_name == 'pull_request'",
		},
		"privileged preflight": {
			old: "      actions: read\n",
			new: "      actions: write\n",
		},
		"short preflight job": {
			old: "    timeout-minutes: 130\n",
			new: "    timeout-minutes: 120\n",
		},
		"short support poll": {
			old: "          SUPPORT_POLL_TIMEOUT_MINUTES: \"120\"\n",
			new: "          SUPPORT_POLL_TIMEOUT_MINUTES: \"100\"\n",
		},
		"default branch binding": {
			old: "          DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}\n",
			new: "          DEFAULT_BRANCH: master\n",
		},
		"fresh support verification": {
			old: "          go run ./hack/verify-kubernetes-support.go -now \"$today\"\n",
			new: "          true # freshness omitted\n",
		},
		"release contract verification": {
			old: "          go run ./hack/releaseverify\n",
			new: "          true # release contract omitted\n",
		},
		"exact CI workflow": {
			old: "actions/workflows/ci.yml/runs",
			new: "actions/runs",
		},
		"exact source SHA": {
			old: "            -f head_sha=\"$GITHUB_SHA\" \\\n",
			new: "            -f per_page=100 \\\n",
		},
		"push event": {
			old: ".event == \"push\"",
			new: ".event == \"pull_request\"",
		},
		"unchecked CI run decoding": {
			old: "            run_ids_file=\"$RUNNER_TEMP/successful-support-run-ids\"\n            jq -r \\\n",
			new: "            mapfile -t run_ids < <(jq -r \\\n",
		},
		"stable gate": {
			old: ".name == \"Kubernetes support gate\"",
			new: ".name == \"Verify source and generated files\"",
		},
		"preflight output": {
			old: "      source-sha: ${{ steps.support-evidence.outputs.source-sha }}\n",
			new: "      source-sha: ${{ github.sha }}\n",
		},
		"publish dependency": {
			old: "    needs: [support-preflight]\n",
			new: "    needs: []\n",
		},
		"publish output binding": {
			old: "needs.support-preflight.outputs.source-sha == github.sha",
			new: "github.sha == github.sha",
		},
		"continue on error": {
			old: "        id: support-evidence\n",
			new: "        id: support-evidence\n        continue-on-error: true\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedWorkflow(t, workflow, test.old, test.new)
			if err := verifyReleaseWorkflow(path); err == nil {
				t.Fatal("verifyReleaseWorkflow() accepted a critical mutation")
			}
		})
	}
}

func TestRejectEarlySuccessfulExitCannotBeHiddenByHeredocPayloadQuote(t *testing.T) {
	t.Parallel()

	contents := []byte(`#!/bin/sh
set -eu
: <<'PAYLOAD'
'
PAYLOAD
exit 0
printf '%s\n' 'LIFECYCLE_COMPLETE'
`)
	completion := regexp.MustCompile(`(?m)^printf '%s\\n' 'LIFECYCLE_COMPLETE'$`)
	err := rejectEarlySuccessfulExit("lifecycle.sh", contents, completion)
	if err == nil || !strings.Contains(err.Error(), "lifecycle.sh:6: unconditional successful exit") {
		t.Fatalf("rejectEarlySuccessfulExit() error = %v, want line 6 early-exit rejection", err)
	}
}

func TestRejectEarlySuccessfulExitIgnoresHeredocPayloadCommands(t *testing.T) {
	t.Parallel()

	contents := []byte(`#!/bin/sh
set -eu
cat <<-'PAYLOAD'
	set +eu
	exit 0
	PAYLOAD
printf '%s\n' 'LIFECYCLE_COMPLETE'
`)
	completion := regexp.MustCompile(`(?m)^printf '%s\\n' 'LIFECYCLE_COMPLETE'$`)
	if err := rejectEarlySuccessfulExit("lifecycle.sh", contents, completion); err != nil {
		t.Fatalf("rejectEarlySuccessfulExit() rejected here-document data: %v", err)
	}
}

func TestRejectEarlySuccessfulExitVariants(t *testing.T) {
	t.Parallel()

	completion := regexp.MustCompile(`(?m)^printf '%s\\n' 'LIFECYCLE_COMPLETE'$`)
	tests := map[string]struct {
		command   string
		wantError bool
	}{
		"zero padded status":         {command: "exit 00", wantError: true},
		"builtin without status":     {command: "builtin exit", wantError: true},
		"builtin zero status":        {command: "builtin exit 0", wantError: true},
		"builtin zero padded status": {command: "builtin exit 000", wantError: true},
		"command zero status":        {command: "command exit 0", wantError: true},
		"command zero padded status": {command: "command exit 000", wantError: true},
		"nonzero status":             {command: "exit 1"},
		"builtin nonzero status":     {command: "builtin exit 17"},
		"command nonzero status":     {command: "command exit 17"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contents := []byte("#!/bin/bash\nset -eu\n" + test.command + "\nprintf '%s\\n' 'LIFECYCLE_COMPLETE'\n")
			err := rejectEarlySuccessfulExit("lifecycle.sh", contents, completion)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unconditional successful exit")) {
				t.Fatalf("rejectEarlySuccessfulExit(%q) error = %v, want early-exit rejection", test.command, err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectEarlySuccessfulExit(%q) error = %v, want acceptance", test.command, err)
			}
		})
	}
}

func TestRejectEarlySuccessfulExitUsesExecutableShellCode(t *testing.T) {
	t.Parallel()

	completion := regexp.MustCompile(`(?m)^printf '%s\\n' 'LIFECYCLE_COMPLETE'$`)
	tests := map[string]struct {
		prefix    string
		wantError bool
	}{
		"heredoc cannot forge completion": {
			prefix:    "cat <<'PAYLOAD'\nprintf '%s\\n' 'LIFECYCLE_COMPLETE'\nPAYLOAD\nexit 00\n",
			wantError: true,
		},
		"quoted completion cannot forge completion": {
			prefix:    "payload='\nprintf '%s\\n' 'LIFECYCLE_COMPLETE'\n'\nexit 00\n",
			wantError: true,
		},
		"quoted exit is data": {
			prefix: "payload='\nexit 00\n'\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contents := []byte("#!/bin/bash\n" + test.prefix + "printf '%s\\n' 'LIFECYCLE_COMPLETE'\n")
			err := rejectEarlySuccessfulExit("lifecycle.sh", contents, completion)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unconditional successful exit")) {
				t.Fatalf("rejectEarlySuccessfulExit() error = %v, want early-exit rejection", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectEarlySuccessfulExit() error = %v, want shell-data acceptance", err)
			}
		})
	}
}

func TestRejectEarlySuccessfulReturnVariants(t *testing.T) {
	t.Parallel()

	start := regexp.MustCompile(`(?m)^run_lifecycle\(\) \{$`)
	completion := regexp.MustCompile(`(?m)^\tprintf '%s\\n' 'ENGINE_COMPLETE'$`)
	tests := map[string]struct {
		command   string
		wantError bool
	}{
		"zero padded status":         {command: "return 00", wantError: true},
		"builtin without status":     {command: "builtin return", wantError: true},
		"builtin zero status":        {command: "builtin return 0", wantError: true},
		"builtin zero padded status": {command: "builtin return 000", wantError: true},
		"command zero status":        {command: "command return 0", wantError: true},
		"command zero padded status": {command: "command return 000", wantError: true},
		"nonzero status":             {command: "return 1"},
		"builtin nonzero status":     {command: "builtin return 17"},
		"command nonzero status":     {command: "command return 17"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contents := []byte("#!/bin/bash\nrun_lifecycle() {\n\t" + test.command + "\n\tprintf '%s\\n' 'ENGINE_COMPLETE'\n}\n")
			err := rejectEarlySuccessfulReturn("lifecycle.sh", contents, start, completion)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unconditional successful return")) {
				t.Fatalf("rejectEarlySuccessfulReturn(%q) error = %v, want early-return rejection", test.command, err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectEarlySuccessfulReturn(%q) error = %v, want acceptance", test.command, err)
			}
		})
	}
}

func TestRejectEarlySuccessfulReturnUsesExecutableShellCode(t *testing.T) {
	t.Parallel()

	start := regexp.MustCompile(`(?m)^run_lifecycle\(\) \{$`)
	completion := regexp.MustCompile(`(?m)^\tprintf '%s\\n' 'ENGINE_COMPLETE'$`)
	tests := map[string]struct {
		body      string
		wantError bool
	}{
		"heredoc cannot forge completion": {
			body:      "\tcat <<'PAYLOAD'\n\tprintf '%s\\n' 'ENGINE_COMPLETE'\nPAYLOAD\n\treturn 00\n",
			wantError: true,
		},
		"heredoc return is data": {
			body: "\tcat <<'PAYLOAD'\n\treturn 00\nPAYLOAD\n",
		},
		"quoted completion cannot forge completion": {
			body:      "\tpayload='\n\tprintf '%s\\n' 'ENGINE_COMPLETE'\n'\n\treturn 00\n",
			wantError: true,
		},
		"quoted return is data": {
			body: "\tpayload='\n\treturn 00\n'\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contents := []byte("#!/bin/bash\nrun_lifecycle() {\n" + test.body + "\tprintf '%s\\n' 'ENGINE_COMPLETE'\n}\n")
			err := rejectEarlySuccessfulReturn("lifecycle.sh", contents, start, completion)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unconditional successful return")) {
				t.Fatalf("rejectEarlySuccessfulReturn() error = %v, want early-return rejection", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectEarlySuccessfulReturn() error = %v, want shell-data acceptance", err)
			}
		})
	}
}

func TestRejectStaticControlFlowBypassUsesExecutableShellCode(t *testing.T) {
	t.Parallel()

	completion := regexp.MustCompile(`(?m)^printf '%s\\n' 'LIFECYCLE_COMPLETE'$`)
	tests := map[string]struct {
		prefix    string
		wantError bool
	}{
		"heredoc cannot forge completion": {
			prefix:    "cat <<'PAYLOAD'\nprintf '%s\\n' 'LIFECYCLE_COMPLETE'\nPAYLOAD\nfalse && run_lifecycle\n",
			wantError: true,
		},
		"heredoc command is data": {
			prefix: "cat <<'PAYLOAD'\nfalse && run_lifecycle\nPAYLOAD\n",
		},
		"quoted completion cannot forge completion": {
			prefix:    "payload='\nprintf '%s\\n' 'LIFECYCLE_COMPLETE'\n'\nfalse && run_lifecycle\n",
			wantError: true,
		},
		"quoted command is data": {
			prefix: "payload='\nfalse && run_lifecycle\n'\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			contents := []byte("#!/bin/bash\n" + test.prefix + "printf '%s\\n' 'LIFECYCLE_COMPLETE'\n")
			err := rejectStaticControlFlowBypass("lifecycle.sh", contents, completion)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "can bypass audited lifecycle work")) {
				t.Fatalf("rejectStaticControlFlowBypass() error = %v, want bypass rejection", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectStaticControlFlowBypass() error = %v, want shell-data acceptance", err)
			}
		})
	}
}

func readTestWorkflow(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func verifyCIWorkflowSemanticsAtPath(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	return verifyCIWorkflowSemantics(path, workflow, contents)
}

func verifyUpdateWorkflowSemanticsAtPath(path string) error {
	workflow, contents, err := readWorkflow(path)
	if err != nil {
		return err
	}
	return verifyUpdateWorkflowSemantics(path, workflow, contents)
}

func writeMutatedWorkflow(t *testing.T, workflow, old, replacement string) string {
	t.Helper()
	if count := strings.Count(workflow, old); count != 1 {
		t.Fatalf("workflow fixture contains %d instances of %q, want 1", count, old)
	}
	path := filepath.Join(t.TempDir(), "workflow.yml")
	mutated := strings.Replace(workflow, old, replacement, 1)
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
