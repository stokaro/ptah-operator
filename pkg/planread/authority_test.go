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

	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread"
)

// authorityFields names the fields of a struct an approval could be built from:
// the UIDs it binds, the plan object, and the stored bytes.
func authorityFields(of reflect.Type) []string {
	var found []string
	for index := range of.NumField() {
		field := of.Field(index)
		switch field.Type {
		case reflect.TypeFor[types.UID](), reflect.TypeFor[operatorv1alpha1.PtahSchemaPlan](), reflect.TypeFor[[]byte]():
			found = append(found, field.Name)
		}
	}
	return found
}

// The compiler holds a tolerant read away from approval only while the two types
// stay apart: internal/approve takes a Plan, and LoadForDisplay returns a
// DisplayPlan. A field added to DisplayPlan that an approval needs would bring
// back what the type took away, and the compiler would say nothing. Plan is the
// control: it carries each of these, so the filter is seen to find them.
func TestADisplayPlanCarriesNothingAnApprovalBinds(t *testing.T) {
	t.Parallel()
	if got, want := authorityFields(reflect.TypeFor[planread.Plan]()), []string{"SchemaUID", "PlanUID", "Manifest", "Document"}; !slices.Equal(got, want) {
		t.Fatalf("the filter finds %v in a Plan, want %v", got, want)
	}
	if got := authorityFields(reflect.TypeFor[planread.DisplayPlan]()); len(got) != 0 {
		t.Fatalf("a DisplayPlan carries %v, which an approval can be built from", got)
	}
}

func parseSource(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %q: %v", source, err)
	}
	return file
}

// displayDecoderUses names what reaches the tolerant path inside this package:
// for each of its identifiers, the top-level declarations that mention it.
func displayDecoderUses(files []*ast.File) map[string][]string {
	uses := map[string][]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			var body ast.Node = decl
			where := "package level"
			if function, ok := decl.(*ast.FuncDecl); ok {
				body, where = function.Body, function.Name.Name
			}
			ast.Inspect(body, func(node ast.Node) bool {
				if name, ok := node.(*ast.Ident); ok {
					switch name.Name {
					case "LoadForDisplay", "decodeForDisplay", "withoutUnreadFields":
						uses[name.Name] = append(uses[name.Name], where)
					}
				}
				return true
			})
		}
	}
	for _, callers := range uses {
		slices.Sort(callers)
	}
	return uses
}

// The type holds what leaves LoadForDisplay. It cannot hold a second read in this
// package that strips the fields and returns a Plan, so this does. The walk has
// two callers: the display read, and the strict read, which uses it only to name
// what it refuses.
func TestTheDisplayDecoderHasOneWayIn(t *testing.T) {
	t.Parallel()
	got := displayDecoderUses(parseDir(t, ".", false))
	want := map[string][]string{
		"decodeForDisplay":    {"LoadForDisplay"},
		"withoutUnreadFields": {"decodeForDisplay", "decodeStrictly"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the tolerant path is reached by %v, and only %v may reach it", got, want)
	}

	second := parseSource(t, `package planread
func LoadLenient() (Plan, error) {
	declared, _, _ := withoutUnreadFields(nil)
	return stored.read(dataplane.DecodePlan(declared, "")), nil
}`)
	if got := displayDecoderUses([]*ast.File{second}); !slices.Equal(got["withoutUnreadFields"], []string{"LoadLenient"}) {
		t.Fatalf("the filter misses a second tolerant read: %v", got)
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
			if file == root {
				return nil
			}
			name := entry.Name()
			// bin holds build output at the module's root, and a package
			// anywhere else may have that name.
			if strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules" ||
				(name == "bin" && filepath.Dir(file) == root) {
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

const moduleImportPath = "github.com/stokaro/ptah-operator"

// moduleImports is, for each directory, the module's packages its files import.
func moduleImports(sources map[string]*ast.File) map[string][]string {
	imports := map[string][]string{}
	for name, file := range sources {
		directory := path.Dir(name)
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err == nil && strings.HasPrefix(imported, moduleImportPath+"/") {
				imports[directory] = append(imports[directory], strings.TrimPrefix(imported, moduleImportPath+"/"))
			}
		}
	}
	return imports
}

// linkPath follows imports from a package to a target, and says which package
// imports it, or nothing when none on the way does.
func linkPath(imports map[string][]string, from, target string) string {
	seen := map[string]bool{from: true}
	for queue := []string{from}; len(queue) > 0; queue = queue[1:] {
		for _, imported := range imports[queue[0]] {
			if imported == target {
				return queue[0]
			}
			if !seen[imported] {
				seen[imported] = true
				queue = append(queue, imported)
			}
		}
	}
	return ""
}

func TestLinkPathFollowsImports(t *testing.T) {
	t.Parallel()
	imports := map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"a"}, "e": {"f"}}
	for _, test := range []struct{ from, target, want string }{
		{"a", "b", "a"},
		{"a", "d", "b"},
		{"c", "d", "b"},
		{"a", "f", ""},
		{"d", "a", ""},
	} {
		if got := linkPath(imports, test.from, test.target); got != test.want {
			t.Fatalf("linkPath(%s, %s) = %q, want %q", test.from, test.target, got, test.want)
		}
	}
}

// holdsAProgram says whether a directory holds a main package that imports
// anything, which a walk from it can start at.
func holdsAProgram(sources map[string]*ast.File, directory string) bool {
	for name, file := range sources {
		if path.Dir(name) == directory && file.Name.Name == "main" && len(file.Imports) > 0 {
			for _, decl := range file.Decls {
				if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == "main" {
					return true
				}
			}
		}
	}
	return false
}

// A DisplayPlan is not a Plan, but the type does not stop a manager reading one
// and deciding on its fields. This does: no program but the plugin links this
// package. Each root has to be a program, or a manager moved a directory down
// would leave an empty root that links nothing and passes, and the plugin is the
// control that the walk finds a link where there is one.
func TestTheActingProgramsDoNotLinkThisPackage(t *testing.T) {
	t.Parallel()
	const reader = "pkg/planread"
	sources := goSources(t)
	imports := moduleImports(sources)
	if linkPath(imports, "cmd/kubectl-ptah", reader) == "" {
		t.Fatal("the walk from the plugin does not reach this package, so it measures nothing")
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var programs []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "kubectl-ptah" {
			programs = append(programs, entry.Name())
		}
	}
	if want := []string{"manager", "ptah-cert-rotator", "ptah-crd-manager", "ptah-runner"}; !slices.Equal(programs, want) {
		t.Fatalf("the programs beside the plugin are %v, want %v; say here whether a new one may read plans", programs, want)
	}
	for _, program := range programs {
		root := "cmd/" + program
		if !holdsAProgram(sources, root) {
			t.Errorf("%s holds no main package with imports, so a walk from it proves nothing", root)
		}
		if via := linkPath(imports, root, reader); via != "" {
			t.Errorf("%s links %s, through %s", root, reader, via)
		}
	}
}
