package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const guaranteesPage = "docs/site/src/content/docs/reference/guarantees.md"

var (
	backtickToken = regexp.MustCompile("`([A-Za-z][A-Za-z0-9.]*)`")
	goTestName    = regexp.MustCompile(`^Test[A-Za-z0-9]+$`)
	goIdentifier  = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	exportedName  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

// The guarantee map is only worth reading while the tests it cites are the
// tests that exist.
//
// A citation is the whole point of the page: it is what turns "the operator
// refuses this" into something a reader can go and run. A renamed or deleted
// test leaves a promise with no proof behind it, and nothing else in the tree
// would notice.
func TestTheGuaranteeMapCitesTestsThatExist(t *testing.T) {
	t.Parallel()
	page := readGuaranteesPage(t)
	sources := goSources(t)
	cited := 0
	for _, match := range backtickToken.FindAllStringSubmatch(page, -1) {
		token := match[1]
		if !goTestName.MatchString(token) {
			continue
		}
		cited++
		if !strings.Contains(sources.tests, "\nfunc "+token+"(") {
			t.Errorf("%s cites %s, which is not a test in this tree", guaranteesPage, token)
		}
	}
	if cited < 10 {
		t.Fatalf("%s cites %d tests; the map covers six guarantees for two families", guaranteesPage, cited)
	}
}

// And the enforcement points have to be the functions that enforce them. A
// page naming a function the code no longer has sends a reader looking for the
// guarantee somewhere it is not.
func TestTheGuaranteeMapNamesFunctionsThatExist(t *testing.T) {
	t.Parallel()
	page := readGuaranteesPage(t)
	sources := goSources(t)
	named := 0
	for _, match := range backtickToken.FindAllStringSubmatch(page, -1) {
		token := match[1]
		switch {
		case goTestName.MatchString(token):
			// Covered above.
		case strings.Contains(token, "."):
			// A field path on a resource, not a function.
		case strings.HasPrefix(token, "Ptah"):
			// A kind.
		case goIdentifier.MatchString(token) || exportedName.MatchString(token):
			if !declaresFunction(sources.all, token) && !declaresIdentifier(sources.all, token) {
				t.Errorf("%s names %s, which this tree declares neither as a function nor as a constant",
					guaranteesPage, token)
				continue
			}
			named++
		}
	}
	if named < 10 {
		t.Fatalf("%s names %d enforcement points; the map covers six guarantees", guaranteesPage, named)
	}
}

// Every guarantee is stated for both families, because the page exists to make
// the difference between them visible. A table that lost a column would read
// as a guarantee that holds everywhere.
func TestTheGuaranteeMapCoversBothFamilies(t *testing.T) {
	t.Parallel()
	page := readGuaranteesPage(t)
	headers := strings.Count(page, "| | `PtahSchema` | `PtahMigration` |")
	sections := strings.Count(page, "\n## ")
	if headers == 0 {
		t.Fatalf("%s states no guarantee for both families", guaranteesPage)
	}
	// The closing section is prose about the remaining asymmetries and carries
	// no table of its own.
	if headers != sections-1 {
		t.Errorf("%s has %d sections and %d two-family tables", guaranteesPage, sections, headers)
	}
}

type guaranteeSources struct {
	all   string
	tests string
}

// goSources concatenates the Go under internal/, separating the tests so a
// function and a test cannot satisfy each other's check.
func goSources(t *testing.T) guaranteeSources {
	t.Helper()
	var all, tests strings.Builder
	root := repositoryFile(t, "internal")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // A path this test walked under the repository.
		if readErr != nil {
			return readErr
		}
		if strings.HasSuffix(path, "_test.go") {
			tests.Write(content)
			tests.WriteString("\n")
			return nil
		}
		all.Write(content)
		all.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("read the Go sources: %v", err)
	}
	if all.Len() == 0 || tests.Len() == 0 {
		t.Fatal("no sources were read, so every check over them would pass over nothing")
	}
	return guaranteeSources{all: "\n" + all.String(), tests: "\n" + tests.String()}
}

// declaresFunction reports a plain or method function declaration.
func declaresFunction(sources, name string) bool {
	if strings.Contains(sources, "\nfunc "+name+"(") {
		return true
	}
	return regexp.MustCompile(`\nfunc \([^)]*\) ` + regexp.QuoteMeta(name) + `\(`).MatchString(sources)
}

// declaresIdentifier reports a constant the API declares under one of the
// prefixes the page quotes bare: a condition reason, and a phase.
func declaresIdentifier(sources, name string) bool {
	for _, prefix := range []string{"Reason", "Phase"} {
		if regexp.MustCompile(`\b` + prefix + regexp.QuoteMeta(name) + `\b`).MatchString(sources) {
			return true
		}
	}
	return false
}

func readGuaranteesPage(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, filepath.FromSlash(guaranteesPage))) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", guaranteesPage, err)
	}
	return string(content)
}

// The differences section says which asymmetries are deliberate and which are
// not. It must not also say which family keeps which record.
//
// It did, and the sentence was wrong for a day: the migration family got
// `status.pendingLockRelease` and the bullet still described it as the schema
// family's. Nothing noticed, because every function and every test the page
// cites still existed -- the check above measures citations, and this was
// prose.
//
// Field ownership is stated once, on the mutation-lifecycle page, where it is
// a table read against the API types. A second statement of it is a second
// thing to keep current, and this is the one that was not.
func TestTheDifferencesSectionDoesNotRestateFieldOwnership(t *testing.T) {
	t.Parallel()
	page := readGuaranteesPage(t)
	const heading = "## Where the families still differ"
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("%s has no %q section", guaranteesPage, heading)
	}
	section := page[start:]
	for _, field := range []string{
		"status.pendingObservation",
		"status.pendingLockRelease",
		"status.unresolvedRun",
	} {
		if strings.Contains(section, "`"+field+"`") {
			t.Errorf("the differences section names %s; ownership belongs to the durable-state table on the mutation-lifecycle page, which is checked against the API types",
				field)
		}
	}
}
