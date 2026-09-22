package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The operations guide holds both the tasks an operator performs and the
// contracts those tasks rest on, and the two were written as one wall of
// prose. A reader who has an upgrade to run reads about per-endpoint
// admission proofs on the way to the command, and a reader whose upgrade
// stopped half way through reads the same page from the top to find out
// whether anything may still be undone.
//
// The fix is a shape rather than a length: a task section answers five
// questions, in the order the person acting on it needs them, and everything
// else about the release lives on the reference page. This measures the shape,
// because prose reorganized once drifts back the moment a paragraph is added
// to whichever section it fits into most easily.

// runbookGroup is the heading whose task sections this holds to the shape.
const runbookGroup = "Installation and upgrades"

// runbookElements are the five questions a runbook answers, in order. The
// anchor suffix matters as much as the heading: a reader who sends somebody
// "the upgrade one that says where to stop" sends a URL, and a slug derived
// from wording breaks the moment the wording is edited.
var runbookElements = []struct{ suffix, heading string }{
	{"before", "Before you start"},
	{"run", "Run it"},
	{"evidence", "What proves it worked"},
	{"stop", "Where to stop"},
	{"recovery", "If it fails"},
}

// declaredRunbooks is the one place the runbooks are written down. anchor is
// the section's own anchor; prefix is what its five element anchors start
// with, which is shorter than the anchor wherever the section name is long.
var declaredRunbooks = []struct{ anchor, prefix string }{
	{"install", "install"},
	{"upgrade", "upgrade"},
	{"retry-upgrade", "retry"},
	{"repair-runtime", "repair"},
	{"uninstall", "uninstall"},
	{"offline-singleton-migration", "offline"},
}

// declaredReference is every other section under the same heading: material a
// reader consults rather than acts on. A section that is in neither list is
// the finding -- somebody added prose and did not decide which it is.
var declaredReference = []string{
	"One database, one manager",
	"Kubernetes admission configuration",
}

var markdownHeading = regexp.MustCompile(`^(#{2,4}) (.+?)(?: \{#([a-z0-9-]+)\})?$`)

// docSection is one heading and the lines under it, up to the next heading of
// the same level or shallower.
type docSection struct {
	level    int
	title    string
	anchor   string
	body     []string
	children []*docSection
}

// parseSections reads the headings of a Markdown document, ignoring anything
// inside a fenced block: a fence can contain a line that starts with hashes,
// and this page's fences hold shell comments.
func parseSections(markdown string) []*docSection {
	var top []*docSection
	var current, sub *docSection
	fenced := false
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced {
			if match := markdownHeading.FindStringSubmatch(line); match != nil {
				section := &docSection{level: len(match[1]), title: match[2], anchor: match[3]}
				switch section.level {
				case 2:
					top = append(top, section)
					current, sub = section, nil
					continue
				case 3:
					if current != nil {
						current.children = append(current.children, section)
					}
					sub = section
					continue
				default:
					if sub != nil {
						sub.children = append(sub.children, section)
					}
					continue
				}
			}
		}
		switch {
		case sub != nil && len(sub.children) > 0:
			last := sub.children[len(sub.children)-1]
			last.body = append(last.body, line)
		case sub != nil:
			sub.body = append(sub.body, line)
		case current != nil:
			current.body = append(current.body, line)
		}
	}
	return top
}

// runbookProblems is the whole check, over content rather than a path, so the
// tests below can prove it fires by feeding it a page with one thing wrong.
func runbookProblems(markdown string) []string {
	var problems []string
	var group *docSection
	for _, section := range parseSections(markdown) {
		if section.title == runbookGroup {
			group = section
		}
	}
	if group == nil {
		return []string{fmt.Sprintf("the guide has no %q section", runbookGroup)}
	}

	byAnchor := map[string]*docSection{}
	byTitle := map[string]*docSection{}
	for _, section := range group.children {
		byAnchor[section.anchor] = section
		byTitle[section.title] = section
	}

	declared := map[*docSection]bool{}
	for _, runbook := range declaredRunbooks {
		section, ok := byAnchor[runbook.anchor]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"no section under %q declares the anchor {#%s}, so the runbook is gone or renamed",
				runbookGroup, runbook.anchor))
			continue
		}
		declared[section] = true
		problems = append(problems, elementProblems(section, runbook.prefix)...)
	}
	for _, title := range declaredReference {
		section, ok := byTitle[title]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%q is declared as reference under %q and is not there", title, runbookGroup))
			continue
		}
		declared[section] = true
	}
	for _, section := range group.children {
		if !declared[section] {
			problems = append(problems, fmt.Sprintf(
				"%q is under %q and is neither a declared runbook nor declared reference",
				section.title, runbookGroup))
		}
	}
	return problems
}

// elementProblems holds one runbook to the five questions, their order, their
// anchors, and the one thing a reader cannot do without: a "Run it" that
// carries something to run.
func elementProblems(runbook *docSection, prefix string) []string {
	var problems []string
	position := map[string]int{}
	for index, element := range runbook.children {
		position[element.title] = index
	}
	previous := -1
	for _, element := range runbookElements {
		index, ok := position[element.heading]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"runbook %q does not answer %q", runbook.title, element.heading))
			continue
		}
		found := runbook.children[index]
		anchor := prefix + "-" + element.suffix
		if found.anchor != anchor {
			problems = append(problems, fmt.Sprintf(
				"runbook %q answers %q under {#%s}, not {#%s}",
				runbook.title, element.heading, found.anchor, anchor))
		}
		if index < previous {
			problems = append(problems, fmt.Sprintf(
				"runbook %q answers %q out of order", runbook.title, element.heading))
		}
		previous = index
		if element.suffix == "run" && !carriesSomethingToRun(found.body) {
			problems = append(problems, fmt.Sprintf(
				"runbook %q has nothing to run under %q", runbook.title, element.heading))
		}
	}
	return problems
}

// carriesSomethingToRun accepts a fenced block or a numbered sequence. Not
// every task is one command: the offline migration is an ordered procedure
// whose steps are performed against resources the page cannot name.
func carriesSomethingToRun(body []string) bool {
	for _, line := range body {
		if strings.HasPrefix(line, "```sh") {
			return true
		}
		if regexp.MustCompile(`^\d+\. `).MatchString(line) {
			return true
		}
	}
	return false
}

// Every runbook answers the five questions, in order, under stable anchors.
func TestEveryRunbookAnswersTheFiveQuestions(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	for _, problem := range runbookProblems(guide) {
		t.Errorf("%s: %s", operationsGuide, problem)
	}
}

// A page that answers nothing would pass every check above by having no
// runbook at all, so the count is asserted against the table that declares it.
func TestTheGuideCarriesEveryDeclaredRunbook(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	var group *docSection
	for _, section := range parseSections(guide) {
		if section.title == runbookGroup {
			group = section
		}
	}
	if group == nil {
		t.Fatalf("%s has no %q section", operationsGuide, runbookGroup)
	}
	if len(group.children) < len(declaredRunbooks) {
		t.Fatalf("%s carries %d sections under %q; %d runbooks are declared",
			operationsGuide, len(group.children), runbookGroup, len(declaredRunbooks))
	}
}

// The three mistakes this exists to catch, each fed to the check as a page.
func TestTheRunbookShapeNoticesWhatItIsFor(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	for _, mistake := range []struct {
		name    string
		mutate  func(string) string
		expects string
	}{
		{
			name:    "an element dropped",
			mutate:  func(s string) string { return strings.Replace(s, "#### Where to stop {#upgrade-stop}", "", 1) },
			expects: "does not answer \"Where to stop\"",
		},
		{
			name: "a runbook with nothing to run",
			mutate: func(s string) string {
				return strings.Replace(s, "```sh\nhelm upgrade <release> <chart-at-the-installed-version> --values <values>\n```", "", 1)
			},
			expects: "has nothing to run",
		},
		{
			name: "a section that decided nothing",
			mutate: func(s string) string {
				return strings.Replace(s, "### One database, one manager",
					"### Notes on upgrading\n\nSomething.\n\n### One database, one manager", 1)
			},
			expects: "neither a declared runbook nor declared reference",
		},
		{
			name: "an anchor that moved",
			mutate: func(s string) string {
				return strings.Replace(s, "{#uninstall-recovery}", "{#uninstall-what-to-do}", 1)
			},
			expects: "not {#uninstall-recovery}",
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			broken := mistake.mutate(guide)
			if broken == guide {
				t.Fatalf("the mutation changed nothing, so it proves nothing about %s", mistake.name)
			}
			problems := strings.Join(runbookProblems(broken), "\n")
			if !strings.Contains(problems, mistake.expects) {
				t.Errorf("a page with %s was accepted; problems were:\n%s", mistake.name, problems)
			}
		})
	}
}
