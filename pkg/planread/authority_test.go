package planread_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const planreadImportPath = "github.com/stokaro/ptah-operator/pkg/planread"

// displayReadUses names the top-level declarations of one source file that reach
// the display read, by call or by function value. A dot import hides every use,
// so it counts as one.
func displayReadUses(file *ast.File) []string {
	alias := ""
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != planreadImportPath {
			continue
		}
		alias = path.Base(imported)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
	}
	switch alias {
	case "", "_":
		return nil
	case ".":
		return []string{"a dot import"}
	}
	var uses []string
	for _, decl := range file.Decls {
		where := "package level"
		if function, ok := decl.(*ast.FuncDecl); ok {
			where = function.Name.Name
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "LoadForDisplay" {
				return true
			}
			if name, ok := selector.X.(*ast.Ident); ok && name.Name == alias {
				uses = append(uses, where)
			}
			return true
		})
	}
	return uses
}

func parseSource(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %q: %v", source, err)
	}
	return file
}

// The filter below decides what may read tolerantly, so it has to be seen to
// refuse each way of reaching the read, and to pass the strict one.
func TestDisplayReadUsesFindsEveryWayToReachIt(t *testing.T) {
	t.Parallel()
	const anImport = `import "` + planreadImportPath + `"` + "\n"
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{"a call", anImport + `func decide() { planread.LoadForDisplay(nil, nil, "", "", "") }`, []string{"decide"}},
		{"a call under another name", `import reader "` + planreadImportPath + `"` + "\n" + `func decide() { reader.LoadForDisplay(nil, nil, "", "", "") }`, []string{"decide"}},
		{"a function value", anImport + `func decide() { read := planread.LoadForDisplay; _ = read }`, []string{"decide"}},
		{"a value at package level", anImport + `var read = planread.LoadForDisplay`, []string{"package level"}},
		{"a dot import", `import . "` + planreadImportPath + `"` + "\n" + `func decide() { LoadForDisplay(nil, nil, "", "", "") }`, []string{"a dot import"}},
		{"the strict read", anImport + `func decide() { planread.Load(nil, nil, "", "", "") }`, nil},
		{"another package of that name", `import planread "example.com/other"` + "\n" + `func decide() { planread.LoadForDisplay() }`, nil},
		{"no import of it", `func decide() { planread.LoadForDisplay() }`, nil},
		{"a blank import", `import _ "` + planreadImportPath + `"`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := displayReadUses(parseSource(t, "package x\n"+test.source)); !slices.Equal(got, test.want) {
				t.Fatalf("displayReadUses() = %v, want %v", got, test.want)
			}
		})
	}
}

// goSources are the module's non-test Go files, by their path from its root.
func goSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	sources, err := moduleSources()
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

var moduleSources = sync.OnceValues(func() (map[string]*ast.File, error) {
	root := filepath.Join("..", "..")
	sources := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if file != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(relative)] = parsed
		return nil
	})
	return sources, err
})

// The display read decides nothing, so nothing that decides may call it. An
// approval is the decision this read could reach: approve shows a person the
// plan, and the person's yes is bound to its fingerprint. The one use in this
// module is the command that prints a plan and exits.
//
// Test files are left out because they make no decision. A use anywhere else
// fails here, whichever package it is in, and whoever adds one has to say here
// why it decides nothing.
func TestOnlyThePlanCommandReadsForDisplay(t *testing.T) {
	t.Parallel()
	sources := goSources(t)
	for _, required := range []string{"cmd/kubectl-ptah/main.go", "cmd/kubectl-ptah/approve.go", "internal/approve/approve.go", "internal/controller/schema_controller.go", "internal/runner/runner.go"} {
		if sources[required] == nil {
			t.Fatalf("the walk did not read %s, so it measured nothing about it", required)
		}
	}
	found := map[string][]string{}
	for name, file := range sources {
		if uses := displayReadUses(file); len(uses) > 0 {
			found[name] = uses
		}
	}
	want := map[string][]string{"cmd/kubectl-ptah/main.go": {"plan"}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("the display read is used by %v, and only %v may use it", found, want)
	}
}

// The file that holds the one use cannot reach an approval. It does not import
// the package that builds one, and the function makes no call that writes.
func TestThePlanCommandCannotApprove(t *testing.T) {
	t.Parallel()
	file := goSources(t)["cmd/kubectl-ptah/main.go"]
	for _, spec := range file.Imports {
		if imported, _ := strconv.Unquote(spec.Path.Value); strings.HasSuffix(imported, "/internal/approve") {
			t.Fatalf("the file that reads for display imports %s", imported)
		}
	}
	checked := false
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Name.Name != "plan" {
			continue
		}
		checked = true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				if callee.Name == "approveCommand" {
					t.Error("the plan command calls approveCommand")
				}
			case *ast.SelectorExpr:
				if slices.Contains([]string{"Create", "Update", "Patch", "Delete", "DeleteAllOf", "Status", "SubResource"}, callee.Sel.Name) {
					t.Errorf("the plan command calls %s", callee.Sel.Name)
				}
			}
			return true
		})
	}
	if !checked {
		t.Fatal("cmd/kubectl-ptah/main.go has no function plan, so this measured nothing")
	}
}

// Inside this package the decoder that tolerates a field has one way in, and
// the exported read has no other caller here that could wrap it.
func TestTheDisplayDecoderHasOneWayIn(t *testing.T) {
	t.Parallel()
	callers := map[string][]string{}
	for _, file := range parseDir(t, ".", false) {
		for _, decl := range file.Decls {
			function, isFunction := decl.(*ast.FuncDecl)
			var body ast.Node = decl
			where := "package level"
			if isFunction {
				body, where = function.Body, function.Name.Name
			}
			ast.Inspect(body, func(node ast.Node) bool {
				if name, ok := node.(*ast.Ident); ok && (name.Name == "decodeForDisplay" || name.Name == "LoadForDisplay") {
					callers[name.Name] = append(callers[name.Name], where)
				}
				return true
			})
		}
	}
	want := map[string][]string{"decodeForDisplay": {"LoadForDisplay"}}
	if !reflect.DeepEqual(callers, want) {
		t.Fatalf("the display read is reached by %v, and only %v may reach it", callers, want)
	}
}

// The package this read lives in is not linked into the programs that act on a
// plan. Every path from the manager, the runner, the certificate rotator and the
// CRD manager is followed through this module's packages by their imports, so a
// new import that brings it in fails here.
func TestTheActingProgramsDoNotLinkThisPackage(t *testing.T) {
	t.Parallel()
	const module = "github.com/stokaro/ptah-operator"
	imports := map[string][]string{}
	for name, file := range goSources(t) {
		directory := path.Dir(name)
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err == nil && strings.HasPrefix(imported, module+"/") {
				imports[directory] = append(imports[directory], strings.TrimPrefix(imported, module+"/"))
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "kubectl-ptah" {
			continue
		}
		checked++
		root := "cmd/" + entry.Name()
		seen := map[string]bool{root: true}
		for queue := []string{root}; len(queue) > 0; queue = queue[1:] {
			for _, imported := range imports[queue[0]] {
				if imported == "pkg/planread" {
					t.Errorf("%s links pkg/planread, through %s", root, queue[0])
				}
				if !seen[imported] {
					seen[imported] = true
					queue = append(queue, imported)
				}
			}
		}
	}
	const declaredPrograms = 4
	if checked != declaredPrograms {
		t.Fatalf("followed %d programs, and the module ships %d beside the plugin", checked, declaredPrograms)
	}
}
