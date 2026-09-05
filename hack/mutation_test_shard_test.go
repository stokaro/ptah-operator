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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const mutationTestShardEnvironment = "PTAH_MUTATION_TEST_SHARD"

type mutationTestShard struct {
	index int
	total int
}

func activeMutationTestShard(t *testing.T) mutationTestShard {
	t.Helper()

	value, configured := os.LookupEnv(mutationTestShardEnvironment)
	if !configured {
		return mutationTestShard{total: 1}
	}
	shard, err := parseMutationTestShard(value)
	if err != nil {
		t.Fatalf("%s: %v", mutationTestShardEnvironment, err)
	}
	return shard
}

func parseMutationTestShard(value string) (mutationTestShard, error) {
	indexValue, totalValue, found := strings.Cut(value, "/")
	if !found || strings.Contains(totalValue, "/") {
		return mutationTestShard{}, fmt.Errorf("value %q must use index/total syntax", value)
	}
	index, err := strconv.Atoi(indexValue)
	if err != nil {
		return mutationTestShard{}, fmt.Errorf("index %q is not an integer", indexValue)
	}
	total, err := strconv.Atoi(totalValue)
	if err != nil {
		return mutationTestShard{}, fmt.Errorf("total %q is not an integer", totalValue)
	}
	if total <= 0 {
		return mutationTestShard{}, fmt.Errorf("total %d must be positive", total)
	}
	if index < 0 || index >= total {
		return mutationTestShard{}, fmt.Errorf("index %d must be between 0 and %d", index, total-1)
	}
	return mutationTestShard{index: index, total: total}, nil
}

func (shard mutationTestShard) includes(testIndex int) bool {
	return testIndex%shard.total == shard.index
}

func (shard mutationTestShard) requireNonemptyTable(t *testing.T, testCount int) {
	t.Helper()
	if testCount <= shard.index {
		t.Fatalf(
			"mutation shard %d/%d selects no cases from a %d-case table",
			shard.index,
			shard.total,
			testCount,
		)
	}
}

func TestParseMutationTestShard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value   string
		want    mutationTestShard
		wantErr bool
	}{
		{value: "0/1", want: mutationTestShard{total: 1}},
		{value: "3/8", want: mutationTestShard{index: 3, total: 8}},
		{value: "", wantErr: true},
		{value: "0", wantErr: true},
		{value: "0/1/2", wantErr: true},
		{value: "x/2", wantErr: true},
		{value: "0/x", wantErr: true},
		{value: "0/0", wantErr: true},
		{value: "-1/2", wantErr: true},
		{value: "2/2", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			got, err := parseMutationTestShard(test.value)
			if gotErr := err != nil; gotErr != test.wantErr {
				t.Fatalf("parseMutationTestShard(%q) error = %v, want error %t", test.value, err, test.wantErr)
			}
			if !test.wantErr && got != test.want {
				t.Fatalf("parseMutationTestShard(%q) = %+v, want %+v", test.value, got, test.want)
			}
		})
	}
}

func TestMutationTestShardPartition(t *testing.T) {
	t.Parallel()

	for _, total := range []int{1, 2, 8, 17} {
		for testIndex := 0; testIndex < 101; testIndex++ {
			selected := 0
			for index := 0; index < total; index++ {
				if (mutationTestShard{index: index, total: total}).includes(testIndex) {
					selected++
				}
			}
			if selected != 1 {
				t.Fatalf("test index %d selected by %d of %d shards, want exactly one", testIndex, selected, total)
			}
		}
	}
}

func TestRaceShardCountMakeValidation(t *testing.T) {
	t.Parallel()

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is required to exercise the race-shard validation target")
	}
	makefile := filepath.Join("..", makefilePath)
	for _, value := range []string{"1", "8"} {
		value := value
		t.Run("valid_"+value, func(t *testing.T) {
			t.Parallel()
			command := exec.Command(
				makePath,
				"--no-print-directory",
				"-f", makefile,
				"validate-race-shards",
				"RACE_MUTATION_SHARDS="+value,
			)
			if output, commandErr := command.CombinedOutput(); commandErr != nil {
				t.Fatalf("validate-race-shards rejected %q: %v\n%s", value, commandErr, output)
			}
		})
	}

	for _, value := range []string{
		"",
		"0",
		"x",
		"-1",
		"9",
		"01",
		"99999999999999999999999999999999999999999999999999",
	} {
		value := value
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Parallel()
			command := exec.Command(
				makePath,
				"--no-print-directory",
				"-f", makefile,
				"test-race",
				"GO=false",
				"RACE_MUTATION_SHARDS="+value,
			)
			output, commandErr := command.CombinedOutput()
			if commandErr == nil {
				t.Fatalf("test-race accepted invalid RACE_MUTATION_SHARDS=%q", value)
			}
			if !strings.Contains(string(output), "RACE_MUTATION_SHARDS must be an integer between 1 and 8") {
				t.Fatalf("test-race failure for RACE_MUTATION_SHARDS=%q did not come from validation: %v\n%s", value, commandErr, output)
			}
			if strings.Contains(string(output), "false test -race") {
				t.Fatalf("test-race began race execution before rejecting RACE_MUTATION_SHARDS=%q:\n%s", value, output)
			}
		})
	}
}

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
	aggregate := mustAuditedMakeRule(t, parsed, "test-race")
	base := mustAuditedMakeRule(t, parsed, "test-race-base")
	source := string(contents)

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "aggregate target is conditional",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					aggregate,
					"ifeq (1,1)\n"+aggregate+"\nendif",
				)
			},
		},
		{
			name: "aggregate target has whitespace before colon",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					"test-race: validate-race-shards test-race-base",
					"test-race : validate-race-shards test-race-base",
				)
			},
		},
		{
			name: "aggregate target is duplicated",
			mutate: func(source string) string {
				return source + "\n" + aggregate + "\n"
			},
		},
		{
			name: "dynamic target is injected",
			mutate: func(source string) string {
				return source + "\ntest-$$(RACE_TARGET):\n\t@true\n"
			},
		},
		{
			name: "aggregate omits shard validation",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					"test-race: validate-race-shards test-race-base",
					"test-race: test-race-base",
				)
			},
		},
		{
			name: "base race result can come from the Go test cache",
			mutate: func(source string) string {
				return replaceMakeSourceExactly(
					t,
					source,
					base,
					strings.Replace(base, " -count=1", "", 1),
				)
			},
		},
	}
	for _, test := range tests {
		test := test
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
