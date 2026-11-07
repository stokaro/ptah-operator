package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// literalSchemaFixtures names the fixture each publishSchema call in file reads
// when it spells its schema and revision out and passes no file of its own. A
// call that builds either name at run time reaches a fixture this cannot name.
func literalSchemaFixtures(file *ast.File) []string {
	var fixtures []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 5 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "publishSchema" {
			return true
		}
		schema, schemaOK := stringLiteral(call.Args[0])
		revision, revisionOK := stringLiteral(call.Args[1])
		named, namedOK := stringLiteral(call.Args[4])
		if schemaOK && revisionOK && namedOK && named == "" {
			fixtures = append(fixtures, schema+"-"+revision+".sql")
		}
		return true
	})
	return fixtures
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

// A missing fixture fails only when its row runs, which is on a cluster, and
// the failure ends the phase with every row behind it unmeasured.
func TestEveryNamedSchemaFixtureExists(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, source := range sources {
		// The parser reads a file whatever its build tag says.
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		for _, fixture := range literalSchemaFixtures(file) {
			examined++
			if _, err := os.Stat(filepath.Join("..", "..", "testdata", "e2e", fixture)); err != nil {
				t.Errorf("%s publishes a schema from %s: %v", source, fixture, err)
			}
		}
	}
	if examined == 0 {
		t.Fatal("no publishSchema call names a fixture, so the check read nothing")
	}
}

func TestLiteralSchemaFixturesReadsOnlyAFixtureTheCallNames(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "row.go", `package row
func row() {
	d.publishSchema("named", "v1", "postgres", d.registryReference("named"), "")
	d.publishSchema(slug, "v1", "postgres", reference, "")
	d.publishSchema("named", revision, "postgres", reference, "")
	d.publishSchema("own-file", "v1", "postgres", reference, "testdata/own.sql")
	d.publishSchema("own-file", "v1", "postgres", reference, path)
	d.publishOther("other-call", "v1", "postgres", reference, "")
	d.publishSchema("too-few", "v1", "postgres")
}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, want := literalSchemaFixtures(file), []string{"named-v1.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("literalSchemaFixtures = %v, want %v", got, want)
	}
}
