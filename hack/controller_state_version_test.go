package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

// stampAssignment matches one `NAME := <number>` line of Makefile text.
var stampAssignment = regexp.MustCompile(`(?m)^([A-Z_]+) := ([0-9]+)$`)

// declaredStampVersion reads the single numeric assignment named by name out
// of Makefile text.
//
// A missing or repeated assignment is an error rather than a zero: a zero
// compares equal to nothing and would report a version this repository never
// stamped, which is the way a check like this passes while measuring an empty
// file.
func declaredStampVersion(makefile []byte, name string) (int64, error) {
	var found []string
	for _, match := range stampAssignment.FindAllStringSubmatch(string(makefile), -1) {
		if match[1] == name {
			found = append(found, match[2])
		}
	}
	switch len(found) {
	case 1:
	case 0:
		return 0, fmt.Errorf("the Makefile declares no %s := <number>", name)
	default:
		return 0, fmt.Errorf("the Makefile declares %s %d times", name, len(found))
	}
	version, err := strconv.ParseInt(found[0], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s is not a 32-bit number: %w", name, err)
	}
	if version < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %d", name, version)
	}
	return version, nil
}

// The controller-state version is written down twice on purpose. The manager
// compiles it, and the Makefile stamps it into the CRDs a release ships. The
// two live in different languages, so neither generator reads the other, and
// nothing outside this check makes them agree.
//
// Both directions of a disagreement are quiet, and one of them is unsafe. A
// Makefile left behind ships CRDs claiming a state contract older than the
// manager writes, so the release fence reads a successor's durable state as
// one it understands and admits a manager that cannot interpret it. A Makefile
// moved ahead retires a manager nothing replaced.
func TestTheStampedControllerStateVersionIsTheOneTheManagerCompiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(repositoryRoot(t), "Makefile")
	makefile, err := os.ReadFile(path) //nolint:gosec // A path this test built from the repository root.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	declared, err := declaredStampVersion(makefile, "CONTROLLER_STATE_VERSION")
	if err != nil {
		t.Fatalf("read the stamped controller-state version: %v", err)
	}
	if declared != int64(controllerstate.CurrentVersion) {
		t.Fatalf("the Makefile stamps controller-state version %d, and the manager compiles %d",
			declared, controllerstate.CurrentVersion)
	}
}

// Reading the check again catches a reasoning error and misses the one that
// matters, so the reader is handed the Makefiles it has to refuse. Each row is
// a mistake that would otherwise leave the comparison above green over
// something it never read.
func TestDeclaredStampVersionRefusesWhatWouldPassVacuously(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		makefile string
		want     int64
	}{
		{
			name:     "the assignment this repository writes",
			makefile: "CRD_SCHEMA_VERSION := 15\nCONTROLLER_STATE_VERSION := 7\nGO ?= go\n",
			want:     7,
		},
		{
			name:     "no assignment at all",
			makefile: "CRD_SCHEMA_VERSION := 15\n",
		},
		{
			name:     "a second assignment further down",
			makefile: "CONTROLLER_STATE_VERSION := 2\nGO ?= go\nCONTROLLER_STATE_VERSION := 3\n",
		},
		{
			name:     "a recipe line that mentions it",
			makefile: "manifests:\n\tsh hack/stamp.sh $(CONTROLLER_STATE_VERSION)\n",
		},
		{
			name:     "overridable rather than declared",
			makefile: "CONTROLLER_STATE_VERSION ?= 2\n",
		},
		{
			name:     "not a number",
			makefile: "CONTROLLER_STATE_VERSION := v2\n",
		},
		{
			name:     "zero, which every unread file would report",
			makefile: "CONTROLLER_STATE_VERSION := 0\n",
		},
		{
			name:     "wider than the contract the manager compares",
			makefile: "CONTROLLER_STATE_VERSION := 4294967296\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := declaredStampVersion([]byte(test.makefile), "CONTROLLER_STATE_VERSION")
			if test.want == 0 {
				if err == nil {
					t.Fatalf("the reader accepted %q and returned %d", test.makefile, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("the reader refused the assignment this repository writes: %v", err)
			}
			if got != test.want {
				t.Fatalf("read controller-state version %d, want %d", got, test.want)
			}
		})
	}
}

// controllerStateDocumentation is the page that tells an operator which
// manager can read which durable state.
const controllerStateDocumentation = "docs/site/src/content/docs/support/releases.md"

var documentedStateVersionRow = regexp.MustCompile(`(?m)^\| ([0-9]+) \| `)

// The page carries one row per controller-state version, and a row is the only
// place the difference between two versions is written down. Nothing else in
// the tree records it: the constant says which version is current and the
// annotation says which one a release stamps, and neither says what changed.
//
// So a bump that forgets the row leaves an operator reading a table that
// accounts for every version but the one their cluster refuses to accept.
func TestTheDocumentedStateVersionsReachTheOneTheManagerCompiles(t *testing.T) {
	t.Parallel()
	page := readRepositoryFile(t, controllerStateDocumentation)
	documented := make(map[int64]bool)
	for _, match := range documentedStateVersionRow.FindAllStringSubmatch(string(page), -1) {
		version, err := strconv.ParseInt(match[1], 10, 32)
		if err != nil {
			t.Fatalf("%s documents a row for %q, which is not a version: %v",
				controllerStateDocumentation, match[1], err)
		}
		documented[version] = true
	}
	for version := int64(1); version <= int64(controllerstate.CurrentVersion); version++ {
		if !documented[version] {
			t.Fatalf("%s documents no controller-state version %d, and the manager compiles %d",
				controllerStateDocumentation, version, controllerstate.CurrentVersion)
		}
		delete(documented, version)
	}
	for version := range documented {
		t.Fatalf("%s documents controller-state version %d, which no manager compiles",
			controllerStateDocumentation, version)
	}
}

// The page quotes what an operator sees when the fence refuses. A quotation is
// evidence only while the code still says it, and a refusal nothing emits any
// more is worse than no example: it sends the reader looking for a message
// their cluster will never print.
func TestTheDocumentedRefusalsAreTheOnesTheFenceEmits(t *testing.T) {
	t.Parallel()
	page := string(readRepositoryFile(t, controllerStateDocumentation))
	sources := controllerStateFenceSources(t)
	for _, refusal := range []string{
		"controller downgrade refused: ",
		"does not match compiled controller-state version ",
		"release activation controller-state rollback refused:",
	} {
		if !strings.Contains(page, refusal) {
			t.Fatalf("%s no longer quotes the refusal %q", controllerStateDocumentation, refusal)
		}
		if !strings.Contains(sources, refusal) {
			t.Fatalf("%s quotes the refusal %q, which internal/crdupgrade does not emit",
				controllerStateDocumentation, refusal)
		}
	}
}

// controllerStateFenceSources concatenates the non-test Go sources of the
// package that holds the fence.
func controllerStateFenceSources(t *testing.T) string {
	t.Helper()
	pattern := filepath.Join(repositoryRoot(t), "internal", "crdupgrade", "*.go")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no sources matched %s, so this check would pass over nothing", pattern)
	}
	var sources strings.Builder
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, readErr := os.ReadFile(path) //nolint:gosec // A path this test globbed inside the repository.
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		sources.Write(source)
	}
	return sources.String()
}

func readRepositoryFile(t *testing.T, relative string) []byte {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), relative)
	content, err := os.ReadFile(path) //nolint:gosec // A path this test built from the repository root.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}
