package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A printed column is what `kubectl get` shows without being asked, so a column
// reading a Condition nothing sets is empty in every row of every cluster,
// forever. It reads as a resource that has not reached that state yet.
//
// PtahMigrationPlan printed CURRENT from `.status.conditions[?(@.type==
// 'Current')].status`, and nothing has ever written a Condition on a migration
// plan. This is the check that found it.

var (
	conditionColumnMarker = regexp.MustCompile(
		`\+kubebuilder:printcolumn:name="([^"]+)",[^\n]*JSONPath=` + "`" + `\.status\.conditions\[\?\(@\.type=='([A-Za-z]+)'\)\]`)
	conditionConstant = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Condition[A-Za-z]+)\s*=\s*"([A-Za-z]+)"`)
)

// forwardedCondition marks a write whose type the enclosing function was
// handed rather than named. Only the two controller helpers may do that.
const forwardedCondition = "(forwarded)"

// isConditionHelper reports whether a function's whole job is to forward a
// Condition its caller named.
func isConditionHelper(name string) bool {
	return name == "setCondition" || name == "setMigrationCondition"
}

// printedConditionColumn is one column and the Condition type it reads.
type printedConditionColumn struct {
	file   string
	column string
	reads  string
}

func TestEveryPrintedConditionColumnHasSomethingThatSetsIt(t *testing.T) {
	t.Parallel()
	printed := printedConditionColumns(t)
	if len(printed) == 0 {
		t.Fatal("no printed column reads a Condition, so this check reads the API and holds none of it")
	}
	written := writtenConditionTypes(t)
	if len(written) == 0 {
		t.Fatal("no Condition write was found, so this check would call every printed column empty")
	}
	for _, column := range printed {
		if !written[column.reads] {
			t.Errorf("%s prints %q from Condition %s and nothing sets that Condition, so the column is empty in every row",
				column.file, column.column, column.reads)
		}
	}
}

// And the measurement stays honest only while every write names its Condition
// where this can read it. A write that computes the type makes the set above
// incomplete, and an incomplete set turns this check into one that passes.
func TestEveryConditionWriteNamesItsTypeAsAConstant(t *testing.T) {
	t.Parallel()
	if unresolved := unresolvedConditionWrites(t); len(unresolved) > 0 {
		t.Fatalf("these Condition writes do not name a declared Condition constant, so nothing can tell which types are ever set: %v", unresolved)
	}
}

// printedConditionColumns reads the markers the CRD is generated from, so the
// column and the Condition come from one place rather than from the YAML.
func printedConditionColumns(t *testing.T) []printedConditionColumn {
	t.Helper()
	var columns []printedConditionColumn
	directory := repositoryFile(t, filepath.Join("api", "v1alpha1"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_types.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // A path under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range conditionColumnMarker.FindAllStringSubmatch(string(content), -1) {
			columns = append(columns, printedConditionColumn{file: entry.Name(), column: match[1], reads: match[2]})
		}
	}
	return columns
}

// conditionConstants maps every declared Condition constant to its value.
func conditionConstants(t *testing.T) map[string]string {
	t.Helper()
	values := map[string]string{}
	directory := repositoryFile(t, filepath.Join("api", "v1alpha1"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_types.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // A path under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range conditionConstant.FindAllStringSubmatch(string(content), -1) {
			values[match[1]] = match[2]
		}
	}
	if len(values) == 0 {
		t.Fatal("the API package declares no Condition constant")
	}
	return values
}

// writtenConditionTypes is the set of Condition types production code sets.
func writtenConditionTypes(t *testing.T) map[string]bool {
	t.Helper()
	written, _ := conditionWrites(t)
	return written
}

// unresolvedConditionWrites lists the write sites whose Condition this cannot
// resolve to a declared constant.
func unresolvedConditionWrites(t *testing.T) []string {
	t.Helper()
	_, unresolved := conditionWrites(t)
	sort.Strings(unresolved)
	return unresolved
}

func conditionWrites(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	values := conditionConstants(t)
	written := map[string]bool{}
	var unresolved []string
	root := repositoryFile(t, "internal")
	fileSet := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isConditionWrite(call) {
					return true
				}
				name, found := conditionArgument(call)
				switch {
				case found && values[name] != "":
					written[values[name]] = true
				case name == forwardedCondition && isConditionHelper(function.Name.Name):
					// The helper writes whatever its caller named, and every
					// caller is a write site this already read.
				case found:
					unresolved = append(unresolved, fileSet.Position(call.Pos()).String()+" names "+name)
				default:
					unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return written, unresolved
}

// isConditionWrite reports whether the call sets a Condition: the two
// controller helpers, or apimachinery's setter directly.
func isConditionWrite(call *ast.CallExpr) bool {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name == "setCondition" || function.Name == "setMigrationCondition"
	case *ast.SelectorExpr:
		receiver, ok := function.X.(*ast.Ident)
		return ok && receiver.Name == "meta" && function.Sel.Name == "SetStatusCondition"
	}
	return false
}

// conditionArgument finds the Condition constant the call names, either as an
// argument or as the Type field of a metav1.Condition literal.
func conditionArgument(call *ast.CallExpr) (string, bool) {
	for _, argument := range call.Args {
		if name, ok := conditionSelector(argument); ok {
			return name, true
		}
		literal, ok := argument.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, element := range literal.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := pair.Key.(*ast.Ident)
			if !ok || key.Name != "Type" {
				continue
			}
			if name, ok := conditionSelector(pair.Value); ok {
				return name, true
			}
			if _, ok := pair.Value.(*ast.Ident); ok {
				// A value the enclosing function was handed.
				return forwardedCondition, false
			}
			if basic, ok := pair.Value.(*ast.BasicLit); ok && basic.Kind == token.STRING {
				// A type written as a string rather than as the constant.
				if text, err := strconv.Unquote(basic.Value); err == nil {
					return "literal:" + text, false
				}
			}
			return "", false
		}
	}
	return "", false
}

func conditionSelector(expression ast.Expr) (string, bool) {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, "Condition") {
		return "", false
	}
	return selector.Sel.Name, true
}
