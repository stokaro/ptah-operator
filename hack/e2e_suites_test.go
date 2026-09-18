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

func repositoryFile(t *testing.T, path string) string {
	t.Helper()
	return filepath.Join("..", path)
}

// The catalog in the repository is the one CI runs, so it is the one under test.
func TestTheAcceptanceSuitesCoverEveryPhaseTheDriverRuns(t *testing.T) {
	t.Parallel()
	catalog, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatalf("load the acceptance suites: %v", err)
	}
	if err := verifyE2ESuiteCoverage(catalog, repositoryFile(t, e2eHarnessPath)); err != nil {
		t.Fatalf("the suites do not cover the driver: %v", err)
	}
	if len(catalog.Suites) < 2 {
		t.Fatalf("the catalog holds %d suites, which is not a partition", len(catalog.Suites))
	}
}

// writeSuiteCatalog writes a catalog with one thing changed, which is how a
// message it earns is attributable to that thing.
func writeSuiteCatalog(t *testing.T, old, new string) string {
	t.Helper()
	source, err := os.ReadFile(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatalf("read the acceptance suites: %v", err)
	}
	if count := strings.Count(string(source), old); count != 1 {
		t.Fatalf("the catalog holds %d instances of %q, want 1", count, old)
	}
	path := filepath.Join(t.TempDir(), "e2e-suites.json")
	if err := os.WriteFile(path, []byte(strings.Replace(string(source), old, new, 1)), 0o600); err != nil {
		t.Fatalf("write the mutated catalog: %v", err)
	}
	return path
}

func TestASuiteCatalogThatCannotBeExecutedIsRefused(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		old       string
		new       string
		wantError string
	}{
		"a phase claimed by two suites": {
			old:       `"phases": ["assert", "dataplane"],`,
			new:       `"phases": ["assert", "dataplane", "migrations-mysql"],`,
			wantError: `phase "migrations-mysql" is claimed by both`,
		},
		"a suite that runs nothing": {
			old:       `"phases": ["migrations-mysql", "reference-data-mysql"],`,
			new:       `"phases": [],`,
			wantError: `runs no phase`,
		},
		"a suite with no name": {
			old:       `"name": "data-plane",`,
			new:       `"name": "",`,
			wantError: `is not a usable suite name`,
		},
		"a slug that is not the name": {
			old:       `"slug": "data-plane",`,
			new:       `"slug": "dataplane",`,
			wantError: `a job, a run id and an artifact are named after the slug`,
		},
		"a suite that says nothing about itself": {
			old:       `"summary": "The control-plane contract, both engines end to end, restart identity, and fault injection",`,
			new:       `"summary": "  ",`,
			wantError: `says nothing about what it runs`,
		},
		"preparation standing in for coverage": {
			old: `"phases": ["migrations-mysql", "reference-data-mysql"],
      "prepare": ["dataplane"]`,
			new: `"phases": ["migrations-mysql", "reference-data-mysql"],
      "prepare": ["smoke"]`,
			wantError: `which no suite runs for its acceptance; preparation is not coverage`,
		},
		"a suite preparing with its own phase": {
			old: `"phases": ["migrations-mysql", "reference-data-mysql"],
      "prepare": ["dataplane"]`,
			new: `"phases": ["migrations-mysql", "reference-data-mysql"],
      "prepare": ["migrations-mysql"]`,
			wantError: `both runs and prepares with phase "migrations-mysql"`,
		},
		"an unknown field": {
			old:       `"summary": "The versioned migration rows and the declared reference data, on MySQL",`,
			new:       `"shard": 2,`,
			wantError: `unknown field "shard"`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeSuiteCatalog(t, test.old, test.new)
			_, err := loadE2ESuites(path)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("loadE2ESuites() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

// Sharding a suite is one edit away from a matrix that stopped running
// something, and this is the check that refuses it. The two directions are
// different failures: a phase no suite claims is coverage lost, and a suite
// claiming a phase the driver does not run is a catalog nobody can execute.
func TestAPartitionThatLostAPhaseIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("a mandatory suite removed", func(t *testing.T) {
		t.Parallel()
		path := writeSuiteCatalog(t,
			`"phases": ["migrations-mysql", "reference-data-mysql"],`,
			`"phases": ["migrations-mysql"],`)
		catalog, err := loadE2ESuites(path)
		if err != nil {
			t.Fatalf("load the mutated catalog: %v", err)
		}
		err = verifyE2ESuiteCoverage(catalog, repositoryFile(t, e2eHarnessPath))
		if err == nil || !strings.Contains(err.Error(), "reference-data-mysql, which no suite") {
			t.Fatalf("verifyE2ESuiteCoverage() error = %v, want the uncovered phase named", err)
		}
	})
	t.Run("a phase the driver does not run", func(t *testing.T) {
		t.Parallel()
		path := writeSuiteCatalog(t,
			`"phases": ["cert-rotation"],`,
			`"phases": ["cert-rotation", "smoke"],`)
		catalog, err := loadE2ESuites(path)
		if err != nil {
			t.Fatalf("load the mutated catalog: %v", err)
		}
		err = verifyE2ESuiteCoverage(catalog, repositoryFile(t, e2eHarnessPath))
		if err == nil || !strings.Contains(err.Error(), "smoke (in certificates)") {
			t.Fatalf("verifyE2ESuiteCoverage() error = %v, want the invented phase named", err)
		}
	})
	t.Run("a driver that runs no phase", func(t *testing.T) {
		t.Parallel()
		catalog, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
		if err != nil {
			t.Fatalf("load the acceptance suites: %v", err)
		}
		empty := filepath.Join(t.TempDir(), "e2e-kind.sh")
		if err := os.WriteFile(empty, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
			t.Fatalf("write the empty driver: %v", err)
		}
		err = verifyE2ESuiteCoverage(catalog, empty)
		if err == nil || !strings.Contains(err.Error(), "no lifecycle phase invocation was found") {
			t.Fatalf("verifyE2ESuiteCoverage() error = %v, want a refusal to check nothing", err)
		}
	})
}

// The acceptance matrix is the cross product, and every job in it has to be
// addressable: a name for the reader, a slug for the run id and the artifacts.
func TestTheAcceptanceMatrixIsEveryMinorAgainstEverySuite(t *testing.T) {
	t.Parallel()
	catalog, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatalf("load the acceptance suites: %v", err)
	}
	minors := []matrixEntry{
		{Minor: "1.36", MinorSlug: "1-36", KubernetesVersion: "1.36.4"},
		{Minor: "1.37", MinorSlug: "1-37", KubernetesVersion: "1.37.0"},
	}
	entries := e2eSuiteMatrix(minors, catalog)
	if len(entries) != len(minors)*len(catalog.Suites) {
		t.Fatalf("the matrix holds %d jobs, want %d", len(entries), len(minors)*len(catalog.Suites))
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Suite == "" || entry.SuiteSlug == "" || entry.SuiteSummary == "" {
			t.Fatalf("a matrix job does not identify its suite: %+v", entry)
		}
		if entry.KubernetesVersion == "" || entry.MinorSlug == "" {
			t.Fatalf("a matrix job does not identify its minor: %+v", entry)
		}
		key := entry.MinorSlug + "/" + entry.SuiteSlug
		if seen[key] {
			t.Fatalf("the matrix runs %s twice", key)
		}
		seen[key] = true
	}
	// The newest minor stays last, which is where the image build reads the
	// pair it validates out of the plain matrix.
	if entries[len(entries)-1].Minor != "1.37" {
		t.Fatalf("the matrix ends on minor %s", entries[len(entries)-1].Minor)
	}
}
