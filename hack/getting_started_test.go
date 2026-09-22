package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The pages a reader follows from an empty cluster, in the order they follow
// them.
var gettingStartedPages = []string{
	"docs/site/src/content/docs/start/install.md",
	"docs/site/src/content/docs/start/first-schema.md",
	"docs/site/src/content/docs/start/try-it.md",
}

var (
	repositoryPath = regexp.MustCompile(`(?:^|[^A-Za-z0-9./_-])((?:examples|charts|demo)/[A-Za-z0-9./_-]+)`)
	shellNamespace = regexp.MustCompile(`kubectl (?:-n|--namespace) ([a-z0-9-]+) `)
)

// A getting-started page that names a file the repository does not have sends
// a reader to a command that cannot run, and the reader has no way to tell
// whether they chose the wrong version or the page is wrong.
//
// This reads the paths out of the pages rather than listing them here, so a
// page that starts naming a fourth example is covered without anyone
// remembering to add it.
func TestTheGettingStartedPagesNameFilesThisVersionHas(t *testing.T) {
	t.Parallel()
	named := 0
	for _, page := range gettingStartedPages {
		content := readDocumentationPage(t, page)
		for _, match := range repositoryPath.FindAllStringSubmatch(content, -1) {
			path := strings.TrimSuffix(match[1], ".")
			// A hidden segment marks somewhere a command writes rather than
			// somewhere it reads: the lab directory a recording lands in does
			// not exist until the reader runs the command that makes it.
			if strings.Contains(path, "/.") {
				continue
			}
			// A directory is named as often as a file, and both have to exist.
			if _, err := os.Stat(repositoryFile(t, path)); err != nil {
				t.Errorf("%s names %s, which this version does not have", page, path)
				continue
			}
			named++
		}
	}
	if named == 0 {
		t.Fatal("no repository path was read, so this check would pass over nothing")
	}
}

// Every namespace a command writes into is one the reader was told to create.
//
// The first schema used to begin by creating a Secret in a namespace nothing
// had made, in a page whose whole job is to run from a fresh environment. The
// release namespace is exempt: the install creates it with --create-namespace.
func TestTheGettingStartedPagesCreateTheNamespacesTheyWriteInto(t *testing.T) {
	t.Parallel()
	created := map[string]bool{"ptah-system": true, "kube-system": true}
	var used []string
	for _, page := range gettingStartedPages {
		content := readDocumentationPage(t, page)
		for _, line := range strings.Split(content, "\n") {
			if match := regexp.MustCompile(`kubectl create namespace ([a-z0-9-]+)`).FindStringSubmatch(line); match != nil {
				created[match[1]] = true
			}
		}
	}
	for _, page := range gettingStartedPages {
		content := readDocumentationPage(t, page)
		for _, match := range shellNamespace.FindAllStringSubmatch(content, -1) {
			if !created[match[1]] {
				used = append(used, page+" writes into "+match[1])
			}
		}
	}
	sort.Strings(used)
	for _, problem := range distinctLines(used) {
		t.Errorf("%s, which no page creates", problem)
	}
}

// The first schema says what has and has not reached the database while a plan
// waits for a decision, and the distinction is the point of the page: the
// operator read the database to build the plan, and ran none of it.
//
// The page used to say nothing had run against the database at all, which is
// the reading a person would use to decide the operator had not connected.
func TestTheFirstSchemaPageSeparatesReadingFromApplying(t *testing.T) {
	t.Parallel()
	content := readDocumentationPage(t, "docs/site/src/content/docs/start/first-schema.md")
	if strings.Contains(content, "Nothing has run against the database yet") {
		t.Error("the first schema page claims the operator has not reached the database, and it observed it to plan")
	}
	for _, required := range []string{
		"already connected to the database",
		"not one statement it names has run",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("the first schema page no longer says %q", required)
		}
	}
}

func readDocumentationPage(t *testing.T, page string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, filepath.FromSlash(page))) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	return string(content)
}

// distinctLines drops repeats so one page naming a namespace twice is reported
// once.
func distinctLines(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// The shipped CRDs carry no labels, so no documented command may select them
// by one.
//
// This was a real defect in the install page: a readiness check that selected
// the CRDs by `app.kubernetes.io/name` printed an empty list, which reads
// exactly like an installation that created none. A reader following it would
// conclude the install failed and start over.
func TestNoDocumentedCommandSelectsTheCRDsByLabel(t *testing.T) {
	t.Parallel()
	labeled := 0
	for _, path := range shippedCRDPaths(t) {
		content, err := os.ReadFile(path) //nolint:gosec // A path this test globbed under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if regexp.MustCompile(`(?m)^  labels:`).Match(content) {
			labeled++
		}
	}
	if labeled > 0 {
		t.Skipf("%d shipped CRDs now carry labels; selecting them by one is legitimate", labeled)
	}
	selector := regexp.MustCompile(`kubectl get crd[^\n]*\s-l\s`)
	for _, page := range gettingStartedPages {
		if selector.MatchString(readDocumentationPage(t, page)) {
			t.Errorf("%s selects the CRDs by label, and the shipped CRDs carry none", page)
		}
	}
}

// shippedCRDPaths lists the CRD documents the chart installs.
func shippedCRDPaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(repositoryFile(t, filepath.Join("charts", "ptah-operator", "crds", "*.yaml")))
	if err != nil {
		t.Fatalf("list the shipped CRDs: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no shipped CRD was found, so this check would pass over nothing")
	}
	return paths
}
