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

// A runbook is a task that changes something, which is why it owes a stopping
// condition and a way back. A section that only answers a question -- which
// plans are pinned, how a record clears on its own -- is reference, and saying
// "where to stop" about a read would be filler. Each group below names which
// of its sections is which, and a section in neither list is the finding.

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

// runbookEntry names a task section. anchor is the section's own anchor;
// prefix is what its five element anchors start with, which is shorter than
// the anchor wherever the section name is long.
type runbookEntry struct{ anchor, prefix string }

// runbookGroup is a second-level heading and the decision about each section
// under it. page is where it lives: the shape belongs to a runbook wherever it
// is written, not to one file.
type runbookGroup struct {
	page      string
	heading   string
	runbooks  []runbookEntry
	reference []string
}

const migrationsGuide = "docs/site/src/content/docs/use/migrations.md"

// runbookGroups is the one place the guide's tasks are written down.
var runbookGroups = []runbookGroup{
	{
		page:    operationsGuide,
		heading: "Installation and upgrades",
		runbooks: []runbookEntry{
			{"install", "install"},
			{"upgrade", "upgrade"},
			{"retry-upgrade", "retry"},
			{"repair-runtime", "repair"},
			{"uninstall", "uninstall"},
			{"offline-singleton-migration", "offline"},
		},
		reference: []string{
			"One database, one manager",
			"Kubernetes admission configuration",
		},
	},
	{
		page:     operationsGuide,
		heading:  "A migration run nobody accounted for",
		runbooks: []runbookEntry{{"clear-unresolved-run", "clear"}},
		reference: []string{
			"How it clears",
			"Deleting the resource discards it",
		},
	},
	{
		page:     operationsGuide,
		heading:  "Pruning stored plans",
		runbooks: []runbookEntry{{"prune-plans", "prune"}},
		reference: []string{
			"Which plans are pinned",
			"Export a plan before deleting it",
		},
	},
	{
		page:      migrationsGuide,
		heading:   "Adopting an existing database",
		runbooks:  []runbookEntry{{"adopt", "adopt"}},
		reference: nil,
	},
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
func runbookProblems(pages map[string]string) []string {
	var problems []string
	for _, declared := range runbookGroups {
		markdown, known := pages[declared.page]
		if !known {
			continue
		}
		var found *docSection
		for _, section := range parseSections(markdown) {
			if section.title == declared.heading {
				found = section
			}
		}
		if found == nil {
			problems = append(problems, fmt.Sprintf("%s has no %q section", declared.page, declared.heading))
			continue
		}
		problems = append(problems, groupProblems(declared, found)...)
	}
	return problems
}

// groupProblems holds one second-level heading to the decision its entry made
// about every section under it.
func groupProblems(declaredGroup runbookGroup, group *docSection) []string {
	var problems []string
	byAnchor := map[string]*docSection{}
	byTitle := map[string]*docSection{}
	for _, section := range group.children {
		byAnchor[section.anchor] = section
		byTitle[section.title] = section
	}

	seen := map[*docSection]bool{}
	for _, runbook := range declaredGroup.runbooks {
		section, ok := byAnchor[runbook.anchor]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"no section under %q declares the anchor {#%s}, so the runbook is gone or renamed",
				declaredGroup.heading, runbook.anchor))
			continue
		}
		seen[section] = true
		problems = append(problems, elementProblems(section, runbook.prefix)...)
	}
	for _, title := range declaredGroup.reference {
		section, ok := byTitle[title]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%q is declared as reference under %q and is not there", title, declaredGroup.heading))
			continue
		}
		seen[section] = true
	}
	for _, section := range group.children {
		if !seen[section] {
			problems = append(problems, fmt.Sprintf(
				"%q is under %q and is neither a declared runbook nor declared reference",
				section.title, declaredGroup.heading))
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

// runbookPages reads every page a declared group lives on.
func runbookPages(t *testing.T) map[string]string {
	t.Helper()
	pages := map[string]string{}
	for _, group := range runbookGroups {
		if _, read := pages[group.page]; read {
			continue
		}
		pages[group.page] = string(readRepositoryFile(t, group.page))
	}
	if len(pages) < 2 {
		t.Fatalf("the groups name %d page(s); runbooks live on more than one", len(pages))
	}
	return pages
}

// Every runbook answers the five questions, in order, under stable anchors.
func TestEveryRunbookAnswersTheFiveQuestions(t *testing.T) {
	t.Parallel()
	for _, problem := range runbookProblems(runbookPages(t)) {
		t.Errorf("%s", problem)
	}
}

// An element anchor is a URL somebody sends to somebody else, so two headings
// answering to one anchor is a link that lands on whichever the build picked.
func TestNoTwoRunbookElementsClaimOneAnchor(t *testing.T) {
	t.Parallel()
	pages := runbookPages(t)
	owner := map[string]string{}
	anchorPage := map[string]string{}
	declared := 0
	for _, group := range runbookGroups {
		for _, runbook := range group.runbooks {
			for _, element := range runbookElements {
				anchor := runbook.prefix + "-" + element.suffix
				declared++
				page := group.page
				if previous, taken := owner[anchor]; taken {
					t.Errorf("{#%s} is claimed by both %s and %s", anchor, previous, runbook.anchor)
					continue
				}
				owner[anchor] = runbook.anchor
				anchorPage[anchor] = page
			}
		}
	}
	if declared == 0 {
		t.Fatal("no runbook is declared, so every check over them would pass over nothing")
	}
	for anchor, page := range anchorPage {
		if count := strings.Count(pages[page], "{#"+anchor+"}"); count != 1 {
			t.Errorf("%s declares {#%s} %d times", page, anchor, count)
		}
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
			pages := runbookPages(t)
			pages[operationsGuide] = broken
			problems := strings.Join(runbookProblems(pages), "\n")
			if !strings.Contains(problems, mistake.expects) {
				t.Errorf("a page with %s was accepted; problems were:\n%s", mistake.name, problems)
			}
		})
	}
}

// runbookAccess is what each runbook needs to be allowed to do, keyed by the
// runbook's anchor.
//
// "Do I have the rights to do this" is the first question somebody acting on a
// runbook asks, and answering it two paragraphs in -- or not at all -- costs
// the reader the walk to find out. It is also the element the shape was
// missing: prerequisites, commands, evidence, stopping conditions and a
// recovery path were all there, and none of them says who you have to be.
var runbookAccess = map[string]string{
	"install":                     "`cluster-admin`",
	"upgrade":                     "`cluster-admin`",
	"retry-upgrade":               "`cluster-admin`",
	"repair-runtime":              "`cluster-admin`",
	"uninstall":                   "`cluster-admin`",
	"offline-singleton-migration": "maintenance window",
	"clear-unresolved-run":        "`status` subresource",
	"prune-plans":                 "delete access",
	"adopt":                       "write access to the target database",
}

// Every runbook says what access it needs, before it says anything else.
func TestEveryRunbookNamesTheAccessItNeeds(t *testing.T) {
	t.Parallel()
	pages := runbookPages(t)
	checked := 0
	for _, group := range runbookGroups {
		var found *docSection
		for _, section := range parseSections(pages[group.page]) {
			if section.title == group.heading {
				found = section
			}
		}
		if found == nil {
			t.Errorf("%s has no %q section", group.page, group.heading)
			continue
		}
		for _, runbook := range group.runbooks {
			required, declared := runbookAccess[runbook.anchor]
			if !declared {
				t.Errorf("runbook %s declares no required access in this table", runbook.anchor)
				continue
			}
			before := elementBody(found, runbook.anchor, runbook.prefix+"-before")
			if before == "" {
				t.Errorf("runbook %s has no prerequisites to read", runbook.anchor)
				continue
			}
			checked++
			if !strings.Contains(collapsed(before), required) {
				t.Errorf("runbook %s does not say it needs %s before anything else", runbook.anchor, required)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no runbook was read, so this check would pass over nothing")
	}
}

// elementBody returns the lines under one runbook element, joined.
func elementBody(group *docSection, runbookAnchor, elementAnchor string) string {
	for _, runbook := range group.children {
		if runbook.anchor != runbookAnchor {
			continue
		}
		for _, element := range runbook.children {
			if element.anchor == elementAnchor {
				return strings.Join(element.body, "\n")
			}
		}
	}
	return ""
}

// collapsed folds the wrapping so a phrase split across two lines still reads
// as the phrase. Checking prose without this passes for the wrong reason.
func collapsed(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
