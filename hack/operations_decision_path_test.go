package main

import (
	"regexp"
	"strings"
	"testing"
)

// Second and third level alike: the routing table sends a reader to whichever
// one answers, and the site builds an anchor for both.
var operationsSectionHeading = regexp.MustCompile(`(?m)^#{2,3} (.+?)(?: \{#.+\})?$`)

// The operations guide answers an on-call reader, and it is long enough that
// the answer has to be findable before the reading.
//
// Its first section used to be installation and upgrades, four hundred lines
// of it, with recovery and refusals below. Somebody looking at a blocked
// resource at three in the morning met a Helm invocation.
func TestTheOperationsGuideRoutesBeforeItExplains(t *testing.T) {
	t.Parallel()
	guide := readOperationsGuide(t)
	const start = "## Start here"
	index := strings.Index(string(guide), start)
	if index < 0 {
		t.Fatalf("%s has no %q section", operationsGuide, start)
	}
	if lines := strings.Count(string(guide)[:index], "\n"); lines > 10 {
		t.Errorf("the routing table starts %d lines in; it is the first thing or it is not routing", lines)
	}
	for _, later := range []string{"## Installation and upgrades", "## Observability"} {
		if position := strings.Index(string(guide), later); position >= 0 && position < index {
			t.Errorf("%q comes before the routing table", later)
		}
	}
}

// Every in-page target the routing table names is a section this guide has.
//
// The site's link check catches a target that resolves to nothing once the
// page is built; this catches it in the source, where whoever moved the
// heading is looking.
func TestTheOperationsRoutingTableNamesSectionsThatExist(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	anchors := map[string]bool{}
	for _, match := range operationsSectionHeading.FindAllStringSubmatch(guide, -1) {
		anchors[slugify(match[1])] = true
	}
	for _, match := range regexp.MustCompile(`\]\(#([a-z0-9-]+)\)`).FindAllStringSubmatch(routingTable(t, guide), -1) {
		if !anchors[match[1]] {
			t.Errorf("the routing table sends a reader to #%s, which this guide has no section for", match[1])
		}
	}
}

// routingTable returns the rows between the routing heading and the section
// after it.
func routingTable(t *testing.T, guide string) string {
	t.Helper()
	start := strings.Index(guide, "## Start here")
	if start < 0 {
		t.Fatalf("%s has no routing table", operationsGuide)
	}
	rest := guide[start+len("## Start here"):]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		t.Fatalf("the routing table is not followed by a section")
	}
	table := rest[:end]
	if !strings.Contains(table, "| --- | --- |") {
		t.Fatalf("the routing section carries no table")
	}
	return table
}

// slugify renders a heading the way the site builds its anchor.
func slugify(heading string) string {
	var out strings.Builder
	for _, character := range strings.ToLower(heading) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			out.WriteRune(character)
		case character == ' ' || character == '-':
			out.WriteRune('-')
		}
	}
	return out.String()
}
