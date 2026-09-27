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
	"time"
)

// The envtest suites decide what an API server does with the chart's policies
// and the CRDs, so the API server they start is part of the claim: a release
// inside the support window, fetched by a setup-envtest and an index that are
// pinned rather than named by branch.
func TestTheEnvtestPinsHoldTheSupportWindow(t *testing.T) {
	t.Parallel()

	_, releases, err := loadAndValidateManifest(repositoryFile(t, manifestPath), time.Now().UTC())
	if err != nil {
		t.Fatalf("load the support window: %v", err)
	}
	makefile, err := os.ReadFile(repositoryFile(t, makefilePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyEnvtestPins(repositoryFile(t, makefilePath), releases, false); err != nil {
		t.Fatalf("verifyEnvtestPins() refused the repository Makefile: %v", err)
	}

	const (
		version = "ENVTEST_KUBERNETES_VERSION ?= 1.37.0\n"
		setup   = "SETUP_ENVTEST_VERSION ?= v0.25.1\n"
		index   = "ENVTEST_INDEX ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml\n"
		suites  = "PTAH_REQUIRE_ENVTEST=1 $(GO) test -count=1 -timeout=10m ./test/envtest/...\n"
	)
	tests := []struct {
		name        string
		old, new    string
		proposalErr bool
	}{
		{name: "a release the window no longer supports", old: version, new: "ENVTEST_KUBERNETES_VERSION ?= 1.34.2\n"},
		{name: "a release the window does not yet support", old: version, new: "ENVTEST_KUBERNETES_VERSION ?= 1.38.0\n"},
		{name: "a minor with no patch", old: version, new: "ENVTEST_KUBERNETES_VERSION ?= 1.37\n", proposalErr: true},
		{name: "a wildcard release", old: version, new: "ENVTEST_KUBERNETES_VERSION ?= 1.37.x\n", proposalErr: true},
		{name: "no release at all", old: version, new: "", proposalErr: true},
		{name: "two releases", old: version, new: version + version, proposalErr: true},
		{name: "setup-envtest named by its release branch", old: setup, new: "SETUP_ENVTEST_VERSION ?= release-0.25\n", proposalErr: true},
		{name: "setup-envtest at latest", old: setup, new: "SETUP_ENVTEST_VERSION ?= latest\n", proposalErr: true},
		{
			name: "an index read from HEAD", old: index, proposalErr: true,
			new: "ENVTEST_INDEX ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/HEAD/envtest-releases.yaml\n",
		},
		{
			name: "an index from another repository", old: index, proposalErr: true,
			new: "ENVTEST_INDEX ?= https://raw.githubusercontent.com/someone/controller-tools/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml\n",
		},
		{name: "no index", old: index, new: "ENVTEST_INDEX ?=\n", proposalErr: true},
		{
			name: "a suite timeout the verify job is not budgeted for", old: suites, proposalErr: true,
			new: strings.Replace(suites, "-timeout=10m", "-timeout=20m", 1),
		},
		{
			name: "the suites under go test's default timeout", old: suites, proposalErr: true,
			new: strings.Replace(suites, " -timeout=10m", "", 1),
		},
		{
			name: "verify-source without the suites", old: " build test test-envtest\n", proposalErr: true,
			new: " build test\n",
		},
		{
			name: "verify-source running the suites before the unit tests", old: " build test test-envtest\n", proposalErr: true,
			new: " build test-envtest test\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if count := strings.Count(string(makefile), test.old); count != 1 {
				t.Fatalf("the Makefile holds %d copies of %q, want 1", count, test.old)
			}
			path := filepath.Join(t.TempDir(), "Makefile")
			mutated := strings.Replace(string(makefile), test.old, test.new, 1)
			if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyEnvtestPins(path, releases, false); err == nil {
				t.Fatal("verifyEnvtestPins() accepted the mutated Makefile")
			}
			// A proposal moves the window before anyone reviewed the envtest
			// release, so only the pins' shape is held there.
			err := verifyEnvtestPins(path, releases, true)
			if test.proposalErr && err == nil {
				t.Fatal("verifyEnvtestPins() accepted the mutated Makefile in a proposal")
			}
			if !test.proposalErr && err != nil {
				t.Fatalf("verifyEnvtestPins() refused a window move in a proposal: %v", err)
			}
		})
	}
}

// The verify job runs the envtest suites, so the store they read and the cache
// that fills it are part of the audited workflow. Each mutation is a way the
// cache and the Makefile stop agreeing, or the suites stop being required.
func TestTheVerifyJobKeepsTheEnvtestStoreItCaches(t *testing.T) {
	t.Parallel()

	workflow := readWorkflowFixture(t)
	if err := verifyCIWorkflowSemanticsAtPath(repositoryFile(t, workflowPath)); err != nil {
		t.Fatalf("verifyCIWorkflowSemantics() refused the repository workflow: %v", err)
	}
	tests := map[string]struct{ old, new string }{
		"no envtest store for make verify-source": {
			old: "          ENVTEST_BIN_DIR: ${{ runner.temp }}/envtest\n",
			new: "",
		},
		"make verify-source reads a store the cache does not fill": {
			old: "          ENVTEST_BIN_DIR: ${{ runner.temp }}/envtest\n",
			new: "          ENVTEST_BIN_DIR: ${{ runner.temp }}/other-envtest\n",
		},
		"the cache fills a store make verify-source does not read": {
			old: "          path: ${{ runner.temp }}/envtest\n",
			new: "          path: ~/.local/share/kubebuilder-envtest\n",
		},
		"a cache key that ignores the pins": {
			old: "          key: envtest-${{ runner.os }}-${{ hashFiles('Makefile') }}\n",
			new: "          key: envtest-${{ runner.os }}\n",
		},
		"a conditional cache": {
			old: "        id: envtest-assets\n",
			new: "        id: envtest-assets\n        if: github.event_name == 'push'\n",
		},
		"the cache step removed": {
			old: "      - name: Cache the envtest control plane\n        id: envtest-assets\n",
			new: "      - name: Cache the envtest control plane\n        id: envtest-assets-renamed\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeMutatedWorkflow(t, workflow, test.old, test.new)
			if err := verifyCIWorkflowSemanticsAtPath(path); err == nil {
				t.Fatal("verifyCIWorkflowSemantics() accepted a mutation of the envtest store")
			}
		})
	}
}

func readWorkflowFixture(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(repositoryFile(t, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
