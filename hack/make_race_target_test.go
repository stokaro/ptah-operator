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

func TestVerifyMakeRaceTargetsRejectsRuleBypasses(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", makefilePath)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := verifyMakeRaceTargets(path); err != nil {
		t.Fatalf("verifyMakeRaceTargets() rejected the repository Makefile: %v", err)
	}

	parsed, err := parseAuditedMakefile(path, contents)
	if err != nil {
		t.Fatalf("parse repository Makefile: %v", err)
	}
	raceRule := mustAuditedMakeRule(t, parsed, "test-race")
	testRule := mustAuditedMakeRule(t, parsed, "test")
	source := string(contents)

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "race target is conditional",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, raceRule, "ifeq (1,1)\n"+raceRule+"\nendif")
			},
		},
		{
			name: "race target has whitespace before colon",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, "\ntest-race:\n", "\ntest-race :\n")
			},
		},
		{
			name: "race target is duplicated",
			mutate: func(source string) string {
				return source + "\n" + raceRule + "\n"
			},
		},
		{
			name: "dynamic target is injected",
			mutate: func(source string) string {
				return source + "\ntest-$$(RACE_TARGET):\n\t@true\n"
			},
		},
		{
			name: "race result can come from the Go test cache",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, raceRule, strings.Replace(raceRule, " -count=1", "", 1))
			},
		},
		// The skip list is what keeps a suite out of the race pass. One more
		// name there is a test the detector stops watching without a review.
		{
			name: "race pass skips a suite that is not a mutation table",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					"|TestVerifyE2EChildScriptsRejectCriticalMutations\n",
					"|TestVerifyE2EChildScriptsRejectCriticalMutations|TestVerifyE2EWiring\n",
				)
			},
		},
		{
			name: "skip list extended by a second assignment",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					"|TestVerifyE2EChildScriptsRejectCriticalMutations\n",
					"|TestVerifyE2EChildScriptsRejectCriticalMutations\nRACE_MUTATION_TESTS += TestVerifyE2EWiring\n",
				)
			},
		},
		// The race pass skips the mutation suites because the test target runs
		// them. A test target that skips them, or runs short, leaves them run
		// nowhere, and one that times out before ./hack finishes fails the
		// verify job on a healthy tree.
		{
			name: "test target skips the mutation suites",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					testRule,
					"test:\n\t$(GO) test -timeout=30m -skip '^($(RACE_MUTATION_TESTS))$$' ./...",
				)
			},
		},
		{
			name: "test target runs in short mode",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, testRule, "test:\n\t$(GO) test -timeout=30m -short ./...")
			},
		},
		{
			name: "test target leaves out ./hack",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, testRule, "test:\n\t$(GO) test -timeout=30m ./api/... ./cmd/... ./internal/...")
			},
		},
		// Go's default is ten minutes, which a loaded machine runs ./hack past.
		{
			name: "test target without a timeout",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, testRule, "test:\n\t$(GO) test ./...")
			},
		},
		// ./hack took 331 and 338 seconds in the verify job.
		{
			name: "test target times out before ./hack finishes",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, testRule, "test:\n\t$(GO) test -timeout=5m ./...")
			},
		},
		{
			name: "test target is conditional",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source, testRule, "ifeq (1,1)\n"+testRule+"\nendif")
			},
		},
		{
			name: "verify-source no longer runs the test target",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(t, source,
					" e2e-static vet build test test-envtest\n", " e2e-static vet build test-envtest\n")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := test.mutate(source)
			mutatedPath := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(mutatedPath, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write mutated Makefile: %v", err)
			}
			if err := verifyMakeRaceTargets(mutatedPath); err == nil {
				t.Fatal("verifyMakeRaceTargets() accepted a critical mutation")
			}
		})
	}
}

func TestMakeVerifiersRejectGlobalIgnoreBypasses(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", makefilePath)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := verifyMakeE2ETarget(path); err != nil {
		t.Fatalf("verifyMakeE2ETarget() rejected the repository Makefile: %v", err)
	}
	if err := verifyMakeRaceTargets(path); err != nil {
		t.Fatalf("verifyMakeRaceTargets() rejected the repository Makefile: %v", err)
	}
	source := string(contents)
	for _, directive := range []string{
		".IGNORE::",
		".IGNORE: # comment",
		".IGNORE : # comment",
	} {
		directive := directive
		t.Run(directive, func(t *testing.T) {
			t.Parallel()
			mutated := replaceMakeSourceExactly(
				t,
				source,
				"SHELL := /bin/sh\n",
				"SHELL := /bin/sh\n"+directive+"\n",
			)
			mutatedPath := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(mutatedPath, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write mutated Makefile: %v", err)
			}
			if err := verifyMakeE2ETarget(mutatedPath); err == nil {
				t.Fatal("verifyMakeE2ETarget() accepted a global .IGNORE mutation")
			}
			if err := verifyMakeRaceTargets(mutatedPath); err == nil {
				t.Fatal("verifyMakeRaceTargets() accepted a global .IGNORE mutation")
			}
		})
	}
}

func TestMakeVerifiersRejectRuleInjectionFunctions(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", makefilePath)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	source := string(contents)
	for _, injection := range []struct {
		name   string
		source string
	}{
		{
			name:   "parenthesized eval function",
			source: `$(eval test-race: ; @true)`,
		},
		{
			name:   "parenthesized file function",
			source: `$(file >injected.mk,test-race: ; @true)`,
		},
		{
			name:   "braced eval function",
			source: `${eval test-race: ; @true}`,
		},
		{
			name:   "braced file function",
			source: `${file >injected.mk,test-race: ; @true}`,
		},
	} {
		injection := injection
		t.Run(injection.name, func(t *testing.T) {
			t.Parallel()
			mutated := replaceMakeSourceExactly(
				t,
				source,
				"SHELL := /bin/sh\n",
				"SHELL := /bin/sh\n"+injection.source+"\n",
			)
			mutatedPath := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(mutatedPath, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write mutated Makefile: %v", err)
			}
			if err := verifyMakeE2ETarget(mutatedPath); err == nil {
				t.Fatal("verifyMakeE2ETarget() accepted rule injection")
			}
			if err := verifyMakeRaceTargets(mutatedPath); err == nil {
				t.Fatal("verifyMakeRaceTargets() accepted rule injection")
			}
		})
	}
}

func mustAuditedMakeRule(t *testing.T, parsed auditedMakefile, target string) string {
	t.Helper()
	rules := parsed.rules[target]
	if len(rules) != 1 {
		t.Fatalf("repository Makefile has %d %s rules, want 1", len(rules), target)
	}
	return exactMakeRule(parsed.lines, rules[0].line)
}

func replaceMakeSourceExactly(t *testing.T, source, old, replacement string) string {
	t.Helper()
	if count := strings.Count(source, old); count != 1 {
		t.Fatalf("Makefile mutation source count = %d, want 1 for %q", count, old)
	}
	return strings.Replace(source, old, replacement, 1)
}
