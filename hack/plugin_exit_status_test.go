package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	pluginSource   = "cmd/kubectl-ptah/main.go"
	readAPlanPage  = "docs/site/src/content/docs/use/read-a-plan.md"
	exitStatusLead = "Exit status:"
)

var (
	exitConstant   = regexp.MustCompile(`(?m)^\t(exit[A-Za-z]+) = (\d+)$`)
	documentedExit = regexp.MustCompile("`(\\d+)`")
)

// The plugin's exit statuses are a contract a script depends on, written down
// in two places: the constants say "which scripts may rely on", and the page
// tells a reader what each one means.
//
// Renumbering one without the other leaves a script that reads success as a
// missing plan, or the reverse, and nothing else in the tree compares them.
func TestTheDocumentedExitStatusesAreTheOnesThePluginReturns(t *testing.T) {
	t.Parallel()
	source := readSourceFile(t, pluginSource)

	compiled := map[string]int{}
	for _, match := range exitConstant.FindAllStringSubmatch(source, -1) {
		value, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("%s declares %s = %q, which is not a status", pluginSource, match[1], match[2])
		}
		compiled[match[1]] = value
	}
	if len(compiled) < 4 {
		t.Fatalf("read %d exit statuses from %s; the plugin declares more than that", len(compiled), pluginSource)
	}

	// A status the plugin declares and never returns is one the page promises
	// and nobody sees.
	names := make([]string, 0, len(compiled))
	for name := range compiled {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.Contains(source, "return "+name) {
			t.Errorf("%s declares %s and never returns it", pluginSource, name)
		}
	}

	documented := documentedExitStatuses(t)
	for _, name := range names {
		if !documented[compiled[name]] {
			t.Errorf("%s returns %s = %d, which %s does not document",
				pluginSource, name, compiled[name], readAPlanPage)
		}
		delete(documented, compiled[name])
	}
	for value := range documented {
		t.Errorf("%s documents exit status %d, which the plugin does not return", readAPlanPage, value)
	}
}

// documentedExitStatuses reads the numbers from the page's exit-status
// sentence, which runs to the end of its paragraph.
func documentedExitStatuses(t *testing.T) map[int]bool {
	t.Helper()
	page := readSourceFile(t, readAPlanPage)
	start := strings.Index(page, exitStatusLead)
	if start < 0 {
		t.Fatalf("%s no longer states the exit statuses", readAPlanPage)
	}
	rest := page[start:]
	end := strings.Index(rest, "\n\n")
	if end < 0 {
		t.Fatalf("the exit-status sentence does not end")
	}
	found := map[int]bool{}
	for _, match := range documentedExit.FindAllStringSubmatch(rest[:end], -1) {
		value, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		found[value] = true
	}
	if len(found) == 0 {
		t.Fatalf("%s states no exit status, so this check would pass over nothing", readAPlanPage)
	}
	return found
}

func readSourceFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, filepath.FromSlash(path))) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
