package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const mutationLifecyclePage = "docs/site/src/content/docs/reference/mutation-lifecycle.md"

// The page puts the two families' enforcement points beside each other, which
// is only useful while the names are the ones the code uses. A renamed
// function turns the row into a claim about something that does not exist, and
// the reader who checks it finds nothing and concludes the page is stale --
// which it is, everywhere, once one row is.
//
// So every name in an enforcement cell is resolved: a bare name against the
// three controller files, a dotted name against the package it points at, and
// a `status.field` against the CRD the generator emitted. The page cannot name
// a symbol nothing defines.

// controllerFiles are where a bare name in an enforcement cell is looked for.
var controllerFiles = []string{
	"internal/controller/schema_controller.go",
	"internal/controller/migration_controller.go",
	"internal/controller/migration_apply.go",
	"internal/controller/result_arrival.go",
}

var (
	// enforcementRow matches a row whose first cell names a family.
	enforcementRow = regexp.MustCompile("(?m)^\\| `Ptah(?:Schema|Migration)` \\|(.*)\\|\\s*$")
	// codeSpan matches the names inside one cell.
	codeSpan = regexp.MustCompile("`([^`]+)`")
)

// mutationLifecycleSymbols returns every name the enforcement tables claim.
func mutationLifecycleSymbols(page string) []string {
	var symbols []string
	for _, row := range enforcementRow.FindAllStringSubmatch(page, -1) {
		for _, span := range codeSpan.FindAllStringSubmatch(row[1], -1) {
			symbols = append(symbols, span[1])
		}
	}
	return symbols
}

// symbolProblem resolves one name and returns why it could not be, or "".
func symbolProblem(t *testing.T, symbol string) string {
	t.Helper()
	switch {
	case strings.HasPrefix(symbol, "status."):
		field := strings.TrimPrefix(symbol, "status.")
		for _, path := range []string{
			"api/v1alpha1/ptahschema_types.go",
			"api/v1alpha1/ptahmigration_types.go",
		} {
			if strings.Contains(string(readRepositoryFile(t, path)), `json:"`+field+`,`) {
				return ""
			}
		}
		return fmt.Sprintf("no status field is serialized as %q", field)
	case strings.Contains(symbol, "."):
		pkg, name, _ := strings.Cut(symbol, ".")
		dir := filepath.Join(repositoryRoot(t), "internal", pkg)
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(matches) == 0 {
			return fmt.Sprintf("internal/%s is not a package", pkg)
		}
		for _, match := range matches {
			body, err := os.ReadFile(match) //nolint:gosec // A path this test globbed under the repository root.
			if err != nil {
				continue
			}
			if declaresFunc(string(body), name) {
				return ""
			}
		}
		return fmt.Sprintf("internal/%s declares no %s", pkg, name)
	default:
		for _, path := range controllerFiles {
			if declaresFunc(string(readRepositoryFile(t, path)), symbol) {
				return ""
			}
		}
		return "no controller file declares it"
	}
}

// declaresFunc reports whether the source declares a function or method of
// this name. A method's receiver sits between `func` and the name, so both
// shapes are accepted and nothing else is: a name that only appears in a call
// or a comment is not a definition.
func declaresFunc(source, name string) bool {
	return regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`).MatchString(source)
}

// Every enforcement point the page names exists.
func TestTheMutationLifecyclePageNamesRealEnforcementPoints(t *testing.T) {
	t.Parallel()
	page := string(readRepositoryFile(t, mutationLifecyclePage))
	symbols := mutationLifecycleSymbols(page)
	if len(symbols) < 20 {
		t.Fatalf("%s names %d enforcement points; the tables were written with more, so the rows or their shape moved",
			mutationLifecyclePage, len(symbols))
	}
	for _, symbol := range symbols {
		if problem := symbolProblem(t, symbol); problem != "" {
			t.Errorf("%s names %s as an enforcement point: %s", mutationLifecyclePage, symbol, problem)
		}
	}
}

// Both families answer every obligation. A table with one row is a contract
// only one implementation is held to, which is the drift the page exists to
// show rather than to hide.
func TestEveryObligationNamesBothFamilies(t *testing.T) {
	t.Parallel()
	page := string(readRepositoryFile(t, mutationLifecyclePage))
	tables := 0
	for _, block := range strings.Split(page, "\n## ") {
		schema := strings.Count(block, "| `PtahSchema` |")
		migration := strings.Count(block, "| `PtahMigration` |")
		if schema == 0 && migration == 0 {
			continue
		}
		tables++
		if schema != migration {
			heading, _, _ := strings.Cut(block, "\n")
			t.Errorf("%q has %d schema row(s) and %d migration row(s)", heading, schema, migration)
		}
	}
	if tables < 10 {
		t.Fatalf("%s carries %d enforcement tables; the obligations were written with more",
			mutationLifecyclePage, tables)
	}
}

// The two mistakes the check exists for, each fed to it as a page.
func TestTheMutationLifecycleCheckNoticesWhatItIsFor(t *testing.T) {
	t.Parallel()
	page := string(readRepositoryFile(t, mutationLifecyclePage))
	for _, mistake := range []struct {
		name    string
		mutate  func(string) string
		expects string
	}{
		{
			name:    "a renamed function",
			mutate:  func(s string) string { return strings.Replace(s, "`claimAt`", "`claimOperation`", 1) },
			expects: "no controller file declares it",
		},
		{
			name: "a package that has no such symbol",
			mutate: func(s string) string {
				return strings.Replace(s, "`acquireApplyLock`, `acquireActiveLock`, `targetlock.Acquire`",
					"`acquireApplyLock`, `acquireActiveLock`, `targetlock.Take`", 1)
			},
			expects: "declares no Take",
		},
		{
			name:    "a status field that was renamed",
			mutate:  func(s string) string { return strings.Replace(s, "`status.unresolvedRun`", "`status.unknownRun`", 1) },
			expects: "no status field is serialized as \"unknownRun\"",
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			broken := mistake.mutate(page)
			if broken == page {
				t.Fatalf("the mutation changed nothing, so it proves nothing about %s", mistake.name)
			}
			var problems []string
			for _, symbol := range mutationLifecycleSymbols(broken) {
				if problem := symbolProblem(t, symbol); problem != "" {
					problems = append(problems, symbol+": "+problem)
				}
			}
			if !strings.Contains(strings.Join(problems, "\n"), mistake.expects) {
				t.Errorf("a page with %s was accepted; problems were:\n%s",
					mistake.name, strings.Join(problems, "\n"))
			}
		})
	}
}
