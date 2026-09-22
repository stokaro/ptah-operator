package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// eventCall matches one Event the controllers emit, with its format string.
var eventCall = regexp.MustCompile(`r\.event\([^,]+,\s*[^,]+,\s*"[^"]*",\s*"([^"]*)"`)

// Text that reaches an Event goes through bounded() first, and the format
// strings are what say so.
//
// Every value these controllers interpolate with %v is an error. The schema
// family already passed each through bounded(), which truncates and replaces
// invalid UTF-8; the migration family passed three of them raw, so the same
// failure reached status sanitized and reached the Event stream as whatever
// the error happened to hold.
//
// A format with no %v cannot carry one by accident, which is what this reads.
func TestNoControllerEventInterpolatesARawValue(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, path := range controllerSources(t) {
		content, err := os.ReadFile(path) //nolint:gosec // A path this test globbed under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := filepath.Base(path)
		for _, match := range eventCall.FindAllStringSubmatch(string(content), -1) {
			checked++
			if strings.Contains(match[1], "%v") {
				t.Errorf("%s emits an Event formatted %q; interpolate bounded(err.Error(), n) with %%s instead",
					name, match[1])
			}
		}
	}
	if checked < 20 {
		t.Fatalf("read %d Event calls; the controllers emit more than that", checked)
	}
}

// controllerSources lists the controller package's own Go, tests excluded.
func controllerSources(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(repositoryFile(t, filepath.Join("internal", "controller", "*.go")))
	if err != nil {
		t.Fatalf("list the controller sources: %v", err)
	}
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			kept = append(kept, path)
		}
	}
	if len(kept) == 0 {
		t.Fatal("no controller source was read, so this check would pass over nothing")
	}
	sort.Strings(kept)
	return kept
}
