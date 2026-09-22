package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runbook says a deletion discards the unresolved run, and names the Event
// left behind so a reader can go and find it.
//
// A reason the operator stopped using is a field selector that matches
// nothing, and the reader running it concludes no run was ever discarded --
// which is the one conclusion that section exists to prevent.
func TestTheDiscardedRunEventIsOneTheControllerEmits(t *testing.T) {
	t.Parallel()
	const reason = "UnresolvedRunDiscarded"
	guide := readRepositoryText(t, filepath.Join(
		"docs", "site", "src", "content", "docs", "use", "operations.md"))
	if !strings.Contains(guide, reason) {
		t.Fatalf("the operations guide no longer names the %s Event", reason)
	}
	controller := readRepositoryText(t, filepath.Join(
		"internal", "controller", "migration_controller.go"))
	if !strings.Contains(controller, `"`+reason+`"`) {
		t.Fatalf("the operations guide names the %s Event, which the migration controller does not emit", reason)
	}
}

func readRepositoryText(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, path)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
