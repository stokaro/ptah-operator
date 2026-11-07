package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

const managerMain = "../cmd/manager/main.go"

// retentionWiring reads where a manager's source builds the collector and hands
// the policy to admission, and how many of each sit under a condition.
//
// Retention has to run whatever a release sets, because a record outlives the
// setting that wrote it. The manager decides this in main, which no unit test
// reaches and envtest does not run, and a condition around either site is the
// shape that left records with no keeper while delivery was off.
func retentionWiring(t *testing.T, source []byte) (collectors, policies, conditional int) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stack []ast.Node
	underCondition := func() bool {
		for _, node := range stack {
			switch node.(type) {
			case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.ForStmt, *ast.RangeStmt:
				return true
			}
		}
		return false
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch site := node.(type) {
		case *ast.CallExpr:
			if selector, ok := site.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "New" {
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "resultcleanup" {
					collectors++
					if underCondition() {
						conditional++
					}
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := site.Key.(*ast.Ident); ok && key.Name == "ResultCleanup" {
				policies++
				if underCondition() {
					conditional++
				}
			}
		case *ast.AssignStmt:
			for _, target := range site.Lhs {
				if selector, ok := target.(*ast.SelectorExpr); ok && selector.Sel.Name == "ResultCleanup" {
					policies++
					if underCondition() {
						conditional++
					}
				}
			}
		}
		stack = append(stack, node)
		return true
	})
	return collectors, policies, conditional
}

func TestTheManagerRunsRetentionWhateverDeliverySays(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(managerMain)
	if err != nil {
		t.Fatal(err)
	}
	collectors, policies, conditional := retentionWiring(t, source)
	if collectors != 1 || policies != 1 {
		t.Fatalf("%s builds %d collectors and hands admission %d policies, want one of each", managerMain, collectors, policies)
	}
	if conditional != 0 {
		t.Fatalf("%s holds %d of them under a condition, so some installation runs without retention", managerMain, conditional)
	}
}

// The reading has to refuse the shape it exists for, and has to notice a source
// it found nothing in, which would otherwise pass as having no condition.
func TestRetentionWiringReadsItsSites(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name                            string
		source                          string
		collectors, policies, condition int
	}{
		{
			name: "both unconditional",
			source: `package main
func main() {
	c, _ := resultcleanup.New(a, b, d)
	_ = c
	v := &controllerwrite.Validator{ResultCleanup: &p}
	_ = v
}`,
			collectors: 1, policies: 1,
		},
		{
			name: "both under the delivery setting, as it was",
			source: `package main
func main() {
	if endpoint != "" {
		c, _ := resultcleanup.New(a, b, d)
		_ = c
	}
	v := &controllerwrite.Validator{}
	if results != nil {
		v.ResultCleanup = p
	}
}`,
			collectors: 1, policies: 1, condition: 2,
		},
		{
			name: "the policy under a condition in a literal",
			source: `package main
func main() {
	c, _ := resultcleanup.New(a, b, d)
	_ = c
	if enabled {
		v := &controllerwrite.Validator{ResultCleanup: p}
		_ = v
	}
}`,
			collectors: 1, policies: 1, condition: 1,
		},
		{
			name:   "nothing to read",
			source: "package main\nfunc main() {}\n",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			collectors, policies, conditional := retentionWiring(t, []byte(row.source))
			if collectors != row.collectors || policies != row.policies || conditional != row.condition {
				t.Fatalf("read %d collectors, %d policies, %d conditional; want %d, %d, %d",
					collectors, policies, conditional, row.collectors, row.policies, row.condition)
			}
		})
	}
}
