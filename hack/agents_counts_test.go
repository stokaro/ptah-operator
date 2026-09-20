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
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AGENTS.md counts things, and a count in prose is the one part of it nothing
// recomputes. Both counts below had already drifted the same way: a program and
// a suite were added, and the sentence that totals them was not touched. An
// agent reading "the four binaries" goes looking for the fifth, or worse, stops
// at four.
//
// Only the counts are checked here. The rest of the file is judgment, and a
// gate over judgment refuses a rewording rather than an error.
const agentsGuidePath = "AGENTS.md"

// The names the guide spells out. It writes numbers as words, so the check has
// to as well, and a count past this table is a signal to write more of it
// rather than to give up: the failure says which number it could not name.
var numberWords = map[int]string{
	1: "one", 2: "two", 3: "three", 4: "four", 5: "five", 6: "six",
	7: "seven", 8: "eight", 9: "nine", 10: "ten", 11: "eleven", 12: "twelve",
}

func TestAgentsGuideCountsTheProgramsThatShip(t *testing.T) {
	t.Parallel()

	guide := readAgentsGuide(t)
	programs := mainPackagesUnderCmd(t)

	// The intro sentence and the build comment count the same thing, and they
	// have to agree with cmd/ and with each other.
	assertCountedAs(t, guide, programs,
		regexp.MustCompile(`(?i)([a-z]+)\s+programs ship from `+"`cmd/`"), "programs shipped from cmd/")
	assertCountedAs(t, guide, programs,
		regexp.MustCompile(`(?m)^make build\s+# the ([a-z]+) binaries$`), "binaries make build writes")
}

func TestAgentsGuideCountsTheAcceptanceSuites(t *testing.T) {
	t.Parallel()

	guide := readAgentsGuide(t)
	suites := acceptanceSuiteCount(t)

	assertCountedAs(t, guide, suites,
		regexp.MustCompile(`the same\s+([a-z]+)\s+suites`), "acceptance suites the matrix fans out over")
}

// assertCountedAs reads the number the guide spells in one sentence and
// compares it with the number the repository actually holds.
// assertCountedAs is the guide checks' entry point, and it delegates rather
// than repeating the comparison. A second copy would be the thing the
// mutations below measure, and the copy is not what runs against AGENTS.md: a
// production assertion that stopped refusing would leave them green.
func assertCountedAs(t *testing.T, guide string, want int, pattern *regexp.Regexp, subject string) {
	t.Helper()

	if err := checkCountedAs(guide, want, pattern); err != nil {
		t.Fatalf("%s, %s: %v", agentsGuidePath, subject, err)
	}
}

// mainPackagesUnderCmd counts the programs, by reading the package clause
// rather than by counting directories: a directory under cmd/ that is not a
// main package builds no binary, and would make the guide wrong the other way.
func mainPackagesUnderCmd(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir(repositoryFile(t, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}
	programs := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(repositoryFile(t, "cmd"), entry.Name())
		packages, parseErr := parser.ParseDir(token.NewFileSet(), directory, nil, parser.PackageClauseOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", directory, parseErr)
		}
		if _, isProgram := packages["main"]; isProgram {
			programs++
		}
	}
	if programs == 0 {
		t.Fatal("cmd/ holds no main package, which cannot be right")
	}
	return programs
}

// acceptanceSuiteCount reads the catalog, which the guide itself calls the only
// place the suites are written down.
func acceptanceSuiteCount(t *testing.T) int {
	t.Helper()

	document, err := os.ReadFile(repositoryFile(t, "support/e2e-suites.json"))
	if err != nil {
		t.Fatalf("read the suite catalog: %v", err)
	}
	var catalog struct {
		Suites []struct {
			Name string `json:"name"`
		} `json:"suites"`
	}
	if err := json.Unmarshal(document, &catalog); err != nil {
		t.Fatalf("parse the suite catalog: %v", err)
	}
	if len(catalog.Suites) == 0 {
		t.Fatal("the suite catalog is empty, which cannot be right")
	}
	return len(catalog.Suites)
}

func readAgentsGuide(t *testing.T) string {
	t.Helper()

	guide, err := os.ReadFile(repositoryFile(t, agentsGuidePath))
	if err != nil {
		t.Fatalf("read %s: %v", agentsGuidePath, err)
	}
	return string(guide)
}

// A gate that has never refused anything has not been measured. Each mutation
// is the drift that actually happened: a thing was added, and the sentence that
// counts it stayed as it was. The last one is the other direction -- a sentence
// reworded until it no longer says a number at all, which must fail loudly
// rather than quietly stop checking.
func TestTheAgentsGuideCountCheckRefusesDrift(t *testing.T) {
	t.Parallel()

	guide := readAgentsGuide(t)

	tests := []struct {
		name        string
		guide       string
		want        int
		pattern     *regexp.Regexp
		subject     string
		mutatesText bool
	}{
		{
			name:    "a program was added and the build comment was not",
			guide:   guide,
			want:    mainPackagesUnderCmd(t) + 1,
			pattern: regexp.MustCompile(`(?m)^make build\s+# the ([a-z]+) binaries$`),
			subject: "binaries",
		},
		{
			name:    "a suite was added and the sentence was not",
			guide:   guide,
			want:    acceptanceSuiteCount(t) + 1,
			pattern: regexp.MustCompile(`the same\s+([a-z]+)\s+suites`),
			subject: "suites",
		},
		{
			name:        "the sentence stopped naming a number",
			guide:       strings.Replace(guide, "the same\nfive suites", "the same suites", 1),
			want:        acceptanceSuiteCount(t),
			pattern:     regexp.MustCompile(`the same\s+([a-z]+)\s+suites`),
			subject:     "suites",
			mutatesText: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// A mutation that did not change anything proves nothing, and the
			// two count mutations change the expected number rather than the
			// text, so only the reworded one is checked here.
			if test.mutatesText && test.guide == guide {
				t.Fatal("the mutation changed nothing; it is asserting against the guide as written")
			}
			if err := checkCountedAs(test.guide, test.want, test.pattern); err == nil {
				t.Fatalf("the check accepted a guide that says the wrong number of %s", test.subject)
			}
		})
	}

	// And the sound guide passes, so the mutations above prove the check and
	// not a check that refuses everything.
	if err := checkCountedAs(guide, acceptanceSuiteCount(t),
		regexp.MustCompile(`the same\s+([a-z]+)\s+suites`)); err != nil {
		t.Fatalf("the check refused the guide as it stands: %v", err)
	}
}

// checkCountedAs is assertCountedAs as a function that returns its verdict, so
// the mutations above can assert a refusal instead of taking the test down with
// them.
func checkCountedAs(guide string, want int, pattern *regexp.Regexp) error {
	word, ok := numberWords[want]
	if !ok {
		return fmt.Errorf("no word for %d", want)
	}
	match := pattern.FindStringSubmatch(guide)
	if match == nil {
		return fmt.Errorf("the guide no longer says how many (looking for %s); "+
			"restore the sentence or update this check", pattern)
	}
	if !strings.EqualFold(match[1], word) {
		return fmt.Errorf("the guide says %q, the repository holds %d (%q)", match[1], want, word)
	}
	return nil
}
