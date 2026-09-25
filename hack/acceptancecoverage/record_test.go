package main

import (
	"fmt"
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
