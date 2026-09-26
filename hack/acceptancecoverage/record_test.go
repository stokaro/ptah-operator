package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The record exists so that "Not assessed" means somebody looked, rather than
// that the page was never written. These hold it to that.

// Every requirement of #242 has a row, in order and without a gap.
func TestTheRecordCarriesEveryRequirement(t *testing.T) {
	t.Parallel()
	if len(requirements) != 12 {
		t.Fatalf("the record carries %d requirements; #242 states PA-01 through PA-12", len(requirements))
	}
	for index, entry := range requirements {
		want := fmt.Sprintf("PA-%02d", index+1)
		if entry.id != want {
			t.Errorf("requirement %d is %s; the record reads in order and %s is missing", index, entry.id, want)
		}
		if strings.TrimSpace(entry.title) == "" {
			t.Errorf("%s has no title, so the row names nothing", entry.id)
		}
		if strings.TrimSpace(entry.needs) == "" {
			t.Errorf("%s does not say what a disposition would need, which is the column that makes it actionable", entry.id)
		}
	}
}

// The record awards nothing. A generated pass is the failure this whole issue
// is about: a closed issue, a merged fix and a green pull-request run are none
// of them evidence about a candidate, so nothing the repository can compute is
// allowed to read as one.
func TestTheRecordAwardsNoPass(t *testing.T) {
	t.Parallel()
	table := buildRecordForTest(t)
	read := 0
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "| **PA-") {
			continue
		}
		read++
		if !strings.Contains(line, "| Not assessed |") {
			t.Errorf("a requirement row carries a disposition the repository awarded itself: %s", line)
		}
	}
	if read != len(requirements) {
		t.Fatalf("this check recognized %d requirement rows of %d, so a row whose shape it does not know could award itself anything", read, len(requirements))
	}
}

// A candidate field is either read from the tree or says what supplies it.
// An empty cell with no reason reads as a field that does not matter, and an
// unfilled target leaves acceptance incomplete.
func TestEveryCandidateBlankSaysWhatFillsIt(t *testing.T) {
	t.Parallel()
	coverage, err := buildCoverage("../..", "edge")
	if err != nil {
		t.Fatalf("build the coverage: %v", err)
	}
	fields := candidateFields("../..", coverage, nil)
	if len(fields) < 10 {
		t.Fatalf("the candidate block carries %d fields; it was written with more", len(fields))
	}
	derived := 0
	for _, field := range fields {
		if strings.TrimSpace(field.name) == "" {
			t.Error("a candidate field has no name")
		}
		if field.value != "" {
			derived++
			continue
		}
		if strings.TrimSpace(field.supplied) == "" {
			t.Errorf("%s is blank and says nothing about what fills it", field.name)
		}
	}
	if derived == 0 {
		t.Fatal("no candidate field was read from the tree, so the record derives nothing")
	}
}

func buildRecordForTest(t *testing.T) string {
	t.Helper()
	coverage, err := buildCoverage("../..", "edge")
	if err != nil {
		t.Fatalf("build the coverage: %v", err)
	}
	return coverage.recordMarkdown("../..", nil)
}

// A proof the record names is one a reader can open. A shell function that
// was renamed or a script that was split leaves the record pointing at
// nothing while it goes on reading as coverage, so every backquoted name has
// to resolve: a path to a file or directory in the tree, a function to a
// definition in a script under hack.
func TestEveryProofTheRecordNamesExists(t *testing.T) {
	t.Parallel()
	scripts, err := filepath.Glob("../../hack/*.sh")
	if err != nil || len(scripts) == 0 {
		t.Fatalf("read the scripts under hack: %v", err)
	}
	var defined strings.Builder
	for _, script := range scripts {
		body, err := os.ReadFile(script) //nolint:gosec // A path the glob above produced.
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		defined.Write(body)
		defined.WriteByte('\n')
	}
	quoted := regexp.MustCompile("`([^`]+)`")
	function := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)
	resolved := 0
	for _, entry := range requirements {
		for _, match := range quoted.FindAllStringSubmatch(entry.repository, -1) {
			name := match[1]
			switch {
			case strings.Contains(name, "/"):
				if _, err := os.Stat(filepath.Join("../..", name)); err != nil {
					t.Errorf("%s names %s, which the tree does not have", entry.id, name)
					continue
				}
			case function.MatchString(name):
				definition := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\(\) \{`)
				if !definition.MatchString(defined.String()) {
					t.Errorf("%s names %s, which no script under hack defines", entry.id, name)
					continue
				}
			default:
				t.Errorf("%s quotes %q, which is neither a path nor a shell function, so nothing here can check it", entry.id, name)
				continue
			}
			resolved++
		}
	}
	// Counted against the names this was written over, so a change that
	// stopped quoting them could not pass by checking nothing.
	if resolved < 35 {
		t.Fatalf("resolved %d named proofs; the record was written naming more", resolved)
	}
}

// The gaps are rendered where a reader of the record finds them, one line per
// requirement that has any.
func TestTheRecordListsWhatNothingExercises(t *testing.T) {
	t.Parallel()
	record := buildRecordForTest(t)
	if !strings.Contains(record, "### Not yet exercised") {
		t.Fatal("the record no longer lists what nothing in the tree exercises")
	}
	listed := 0
	for _, entry := range requirements {
		if entry.untested == "" {
			continue
		}
		listed++
		if !strings.Contains(record, "- **"+entry.id+"**: "+entry.untested+".") {
			t.Errorf("the record does not list what %s leaves unexercised", entry.id)
		}
	}
	if listed == 0 {
		t.Fatal("no requirement lists a gap, so this check read nothing")
	}
}
