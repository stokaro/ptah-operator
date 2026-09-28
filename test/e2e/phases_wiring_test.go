package e2e

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// The binary the driver builds runs a phase by the test name package phases
// declares, and go test reports a name that matches nothing as a pass. The
// harness refuses that run on its own, but only after a cluster was stood up
// for it; this refuses it in the unit contour. Every declared phase has to be
// a test function in a file only the e2e tag builds, and every test function in
// such a file has to be a declared phase, because one that is not would never
// be run by anything.
func TestEveryPhaseIsATestOnlyTheE2ETagBuilds(t *testing.T) {
	t.Parallel()
	tagged, untagged := testFunctions(t)
	if len(tagged) == 0 {
		t.Fatal("no test function is built only with the e2e tag, so the check below checked nothing")
	}
	declared := map[string]bool{}
	for _, phase := range phases.All() {
		declared[phase.Test] = true
		if !tagged[phase.Test] {
			t.Errorf("phase %s is %s, which no file built only with the e2e tag defines", phase.Name, phase.Test)
		}
		if untagged[phase.Test] {
			t.Errorf("phase %s is %s, which a file without the e2e tag defines, so plain go test would run it", phase.Name, phase.Test)
		}
	}
	for name := range tagged {
		if !declared[name] {
			t.Errorf("%s is built only with the e2e tag and no phase declares it, so nothing runs it", name)
		}
	}
}

// testFunctions sorts the package's top-level TestXxx functions by whether
// their file is built only with the e2e tag. TestMain is the harness's entry
// and not a phase.
func testFunctions(t *testing.T) (tagged, untagged map[string]bool) {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("read the package's test files: %v", err)
	}
	tagged, untagged = map[string]bool{}, map[string]bool{}
	fileSet := token.NewFileSet()
	for _, file := range files {
		source, err := os.ReadFile(file) //nolint:gosec // A file of this package.
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fileSet, file, source, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		onlyWithE2E := requiresE2ETag(t, parsed)
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") ||
				function.Name.Name == "TestMain" {
				continue
			}
			if onlyWithE2E {
				tagged[function.Name.Name] = true
			} else {
				untagged[function.Name.Name] = true
			}
		}
	}
	return tagged, untagged
}

// requiresE2ETag reports whether the file's build constraint holds with the
// e2e tag and fails without it.
func requiresE2ETag(t *testing.T, file *ast.File) bool {
	t.Helper()
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expression, err := constraint.Parse(comment.Text)
			if err != nil {
				t.Fatalf("%s: %v", comment.Text, err)
			}
			with := expression.Eval(func(tag string) bool { return tag == "e2e" })
			without := expression.Eval(func(string) bool { return false })
			return with && !without
		}
	}
	return false
}

func TestTestFunctionsSeeBothKindsOfFile(t *testing.T) {
	t.Parallel()
	tagged, untagged := testFunctions(t)
	// This file is untagged and the certificate phase is not: a reader that
	// put everything on one side would pass the check above for the wrong
	// reason.
	if !untagged["TestTestFunctionsSeeBothKindsOfFile"] || !tagged["TestCertRotation"] {
		t.Fatalf("tagged %v, untagged %d functions", slices.Sorted(maps.Keys(tagged)), len(untagged))
	}
}
