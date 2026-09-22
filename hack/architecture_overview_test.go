package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// A platform owner has to reach the deployment shape before the package
// inventory.
//
// The page opens for somebody changing the code, and its twenty-row package
// table used to be the third thing on it. A reader deciding how to run the
// operator met implementation names before the answer to how many releases a
// cluster takes, which is the first thing they have to know.
func TestTheArchitectureOverviewReachesAPlatformOwnerFirst(t *testing.T) {
	t.Parallel()
	page := readArchitecturePage(t)
	const overview = "## What a deployment looks like"
	scope := strings.Index(page, overview)
	if scope < 0 {
		t.Fatalf("the architecture page carries no %q section", overview)
	}
	for _, later := range []string{
		"## Where the code lives",
		"## The shape of the system",
		"## Where the rest of it is",
	} {
		index := strings.Index(page, later)
		if index < 0 {
			t.Errorf("the architecture page no longer has %q", later)
			continue
		}
		if index < scope {
			t.Errorf("%q comes before the deployment shape a platform owner reads first", later)
		}
	}
	// And it has to be near the top rather than merely before those. A reader
	// who has to scroll past a page of prose has the same problem.
	if lines := strings.Count(page[:scope], "\n"); lines > 40 {
		t.Errorf("the deployment shape starts %d lines in; it is the overview, not a section", lines)
	}
}

// The overview says exactly one installation per cluster is supported, and
// what makes that true is the admission configuration's name being a literal.
//
// A name templated from the release would let two releases stand side by side,
// each with its own admission singleton, and the sentence would be wrong
// without anybody editing it.
func TestTheAdmissionSingletonNameIsFixed(t *testing.T) {
	t.Parallel()
	helpers := readChartHelpers(t)
	const definition = `{{- define "ptah-operator.approvalWebhookConfigurationName" -}}`
	start := strings.Index(helpers, definition)
	if start < 0 {
		t.Fatalf("the chart declares no admission configuration name helper")
	}
	rest := helpers[start+len(definition):]
	end := strings.Index(rest, "{{- end -}}")
	if end < 0 {
		t.Fatal("the admission configuration name helper does not end")
	}
	body := rest[:end]
	if strings.Contains(body, ".Release") || strings.Contains(body, ".Values") {
		t.Fatalf("the admission configuration name is derived from the release or the values (%q),"+
			" so more than one installation can stand in a cluster", strings.TrimSpace(body))
	}
	if !strings.Contains(body, `"`+crdupgrade.AdmissionConfigurationName+`"`) {
		t.Fatalf("the chart names the admission configuration %q and the manager expects %q",
			strings.TrimSpace(body), crdupgrade.AdmissionConfigurationName)
	}
}

func readChartHelpers(t *testing.T) string {
	t.Helper()
	path := filepath.Join("charts", "ptah-operator", "templates", "_helpers.tpl")
	content, err := os.ReadFile(repositoryFile(t, path)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// The overview keeps its URL and names every page the group was split into.
//
// That URL is what four READMEs and every external link point at, so a reader
// who lands on it has to be able to reach the contract they came for. A page
// added to the group and not listed here is one only the sidebar knows about.
func TestTheOverviewNamesEveryPageInItsGroup(t *testing.T) {
	t.Parallel()
	overview := readArchitecturePageFile(t, architecturePagePaths[0])
	for _, path := range architecturePagePaths[1:] {
		slug := strings.TrimSuffix(filepath.Base(path), ".md")
		if !strings.Contains(overview, "(../"+slug+"/)") {
			t.Errorf("the overview does not link to ../%s/, which is in its group", slug)
		}
	}
	// And the page that already had a home of its own.
	if !strings.Contains(overview, "(../guarantees/)") {
		t.Error("the overview does not link to ../guarantees/")
	}
}

func readArchitecturePageFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, path)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
