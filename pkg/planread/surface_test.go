package planread_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Another module can call an exported function whose signature names an
// internal type, but it cannot name that type, so it cannot hold the result in
// a variable of its own or build an argument. The exported surface of these
// packages names no internal package, and the tests in this directory import
// none, so everything they do a program outside this module can do.
func TestPublicSurfaceNamesNoInternalPackage(t *testing.T) {
	t.Parallel()
	// The declarations each directory must be seen to check: a walker that
	// skipped them would pass by reading nothing.
	required := map[string][]string{
		".":            {"Load", "Plan", "Statement", "SQL"},
		"planreadtest": {"NewLab", "Lab", "Cases", "Case", "Want", "NewClient"},
	}
	for dir, names := range required {
		checked, guarded := map[string]bool{}, 0
		for _, file := range parseDir(t, dir, false) {
			internal := internalImports(file)
			guarded += len(internal)
			for _, decl := range file.Decls {
				for name, expr := range exportedTypeExpressions(decl) {
					checked[name] = true
					ast.Inspect(expr, func(node ast.Node) bool {
						selector, ok := node.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						if name, ok := selector.X.(*ast.Ident); ok && internal[name.Name] {
							t.Errorf("%s: exported declaration names %s.%s from an internal package",
								dir, name.Name, selector.Sel.Name)
						}
						return true
					})
				}
			}
		}
		// The check is about something only if the package does import
		// internal code.
		if guarded == 0 {
			t.Fatalf("%s imports no internal package, so the check measured nothing", dir)
		}
		for _, name := range names {
			if !checked[name] {
				t.Fatalf("%s: the exported declaration %s was not checked", dir, name)
			}
		}
	}

	tests := parseDir(t, ".", true)
	if len(tests) == 0 {
		t.Fatal("no test files read")
	}
	for _, file := range tests {
		for name := range internalImports(file) {
			t.Errorf("a test in this directory imports the internal package %s", name)
		}
	}
}

func parseDir(t *testing.T, dir string, tests bool) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") != tests {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// internalImports maps the local name of each internal import to true.
func internalImports(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || !strings.Contains(importPath, "/internal/") {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = true
	}
	return names
}

// exportedTypeExpressions returns, by name, the type expressions a caller of
// the declaration sees: an exported function's signature, an exported type's
// definition with its exported fields, an exported value's declared type.
func exportedTypeExpressions(decl ast.Decl) map[string]ast.Expr {
	exprs := map[string]ast.Expr{}
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		if decl.Name.IsExported() && (decl.Recv == nil || exportedReceiver(decl.Recv)) {
			exprs[decl.Name.Name] = decl.Type
		}
	case *ast.GenDecl:
		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				if spec.Name.IsExported() {
					exprs[spec.Name.Name] = visibleType(spec.Type)
				}
			case *ast.ValueSpec:
				if spec.Type != nil && anyExported(spec.Names) {
					exprs[spec.Names[0].Name] = spec.Type
				}
			}
		}
	}
	return exprs
}

func exportedReceiver(receiver *ast.FieldList) bool {
	expr := receiver.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	name, ok := expr.(*ast.Ident)
	return ok && name.IsExported()
}

// visibleType drops a struct's unexported fields, which no caller sees.
func visibleType(expr ast.Expr) ast.Expr {
	structType, ok := expr.(*ast.StructType)
	if !ok {
		return expr
	}
	visible := &ast.FieldList{}
	for _, field := range structType.Fields.List {
		if len(field.Names) == 0 || anyExported(field.Names) {
			visible.List = append(visible.List, field)
		}
	}
	return &ast.StructType{Fields: visible}
}

func anyExported(names []*ast.Ident) bool {
	for _, name := range names {
		if name.IsExported() {
			return true
		}
	}
	return false
}
