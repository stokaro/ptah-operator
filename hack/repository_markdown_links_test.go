package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The site's own link check reads the built site and stops there. A Markdown
// file at the repository root -- the first page a reader opens on GitHub, and
// the one the issue chooser and the security policy point at -- is outside it,
// so a page renamed on the site leaves those links resolving to a 404 with
// nothing objecting.

// repositoryMarkdown lists the files this holds. They are the ones GitHub
// renders for a visitor rather than for a reader of the site.
var repositoryMarkdown = []string{
	"README.md",
	"README.de.md",
	"README.fr.md",
	"README.ja.md",
	"CONTRIBUTING.md",
	"SECURITY.md",
	"CODE_OF_CONDUCT.md",
	"AGENTS.md",
}

var (
	siteLink     = regexp.MustCompile(`https://operator\.ptah\.run(/[^)\s"'>]*)?`)
	relativeLink = regexp.MustCompile(`\]\(([^)#:]+\.md)(?:#[^)]*)?\)`)
)

// routesWithoutAPage are the addresses the site serves from something other
// than a content file.
var routesWithoutAPage = map[string]bool{
	"":       true, // the site root
	"demo":   true, // the recorded runs, built from demo/recordings
	"edge":   true, // the version root
	"stable": true,
}

// Every operator.ptah.run address a root Markdown file names is a page the
// site has.
func TestEveryDocumentationLinkFromTheRepositoryRootHasAPage(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, file := range repositoryMarkdown {
		content := readDocumentationPage(t, file)
		for _, match := range siteLink.FindAllStringSubmatch(content, -1) {
			route := strings.Trim(match[1], "/")
			route = strings.TrimPrefix(route, "edge/")
			route = strings.TrimPrefix(route, "stable/")
			if index := strings.IndexAny(route, "#?"); index >= 0 {
				route = route[:index]
			}
			route = strings.Trim(route, "/")
			if routesWithoutAPage[route] {
				continue
			}
			checked++
			if !documentationPageExists(t, route) {
				t.Errorf("%s links to /%s/ and the site has no page there", file, route)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no root Markdown file links to a documentation page, so this check read them all and held none")
	}
}

// And every repository-relative Markdown link resolves to a file in the tree.
func TestEveryRepositoryLinkFromTheRootResolves(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, file := range repositoryMarkdown {
		content := readDocumentationPage(t, file)
		for _, match := range relativeLink.FindAllStringSubmatch(content, -1) {
			target := match[1]
			if strings.HasPrefix(target, "http") {
				continue
			}
			checked++
			path := repositoryFile(t, filepath.FromSlash(filepath.Join(filepath.Dir(file), target)))
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s links to %s, which is not in the tree", file, target)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no root Markdown file links to a file in the tree, so this check read them all and held none")
	}
}

// documentationPageExists reports whether the site has a content file for the
// route, either as a page or as a directory index.
func documentationPageExists(t *testing.T, route string) bool {
	t.Helper()
	base := repositoryFile(t, filepath.Join("docs", "site", "src", "content", "docs"))
	for _, candidate := range []string{
		filepath.Join(base, filepath.FromSlash(route)+".md"),
		filepath.Join(base, filepath.FromSlash(route), "index.md"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}
