package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A record of a mutation nobody could account for is the one piece of status
// that decides whether a database may be written again. Both families keep one
// -- `pendingObservation` for a schema, `unresolvedRun` for a migration -- and
// the property that makes each trustworthy is not its shape: it is that one
// function writes it and one place clears it.
//
// That property is invisible in a diff. A second writer somewhere reasonable
// reads correctly, passes every test about the state it writes, and quietly
// makes "what set this" unanswerable. A second clear is worse: it is a way out
// of the latch that nobody documented, and the record exists precisely because
// the way out has to be one a person chose.
//
// So the writers are declared here and the controllers are parsed against them.

// uncertaintyRecords maps each family's record to the functions allowed to
// assign it. A writer sets the record; a clear removes it.
var uncertaintyRecords = []struct {
	field   string
	writers []string
	clears  []string
}{
	{
		field:   "PendingObservation",
		writers: []string{"consumeResult", "finishUncertainApplyWithEvidenceAndBinding"},
		clears:  []string{"consumeResult", "reconcileDeletion", "verificationPolicyChanged"},
	},
	{
		field:   "UnresolvedRun",
		writers: []string{"recordUnresolvedMigrationRun"},
		clears:  []string{"recordMigrationHistory"},
	},
}

// assignmentsTo returns the functions that assign `.Status.<field>`, split by
// whether the assignment sets the record or removes it.
func assignmentsTo(t *testing.T, field string) (writers, clears []string) {
	t.Helper()
	// Every non-test file in the package, so a writer added to a new file is
	// caught rather than depending on a list that has to be kept current.
	fileSet := token.NewFileSet()
	for _, path := range controllerSources(t) {
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for index, target := range assignment.Lhs {
					if !assignsStatusField(target, field) {
						continue
					}
					if index < len(assignment.Rhs) && isNil(assignment.Rhs[index]) {
						clears = append(clears, function.Name.Name)
						continue
					}
					writers = append(writers, function.Name.Name)
				}
				return true
			})
		}
	}
	return unique(writers), unique(clears)
}

// assignsStatusField reports whether the expression is `x.Status.<field>`.
func assignsStatusField(target ast.Expr, field string) bool {
	outer, ok := target.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != field {
		return false
	}
	inner, ok := outer.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Status"
}

func isNil(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "nil"
}

// One function writes each record, and the ways out are the declared ones.
func TestOnlyDeclaredFunctionsTouchAnUncertaintyRecord(t *testing.T) {
	t.Parallel()
	for _, record := range uncertaintyRecords {
		t.Run(record.field, func(t *testing.T) {
			t.Parallel()
			writers, clears := assignmentsTo(t, record.field)
			if len(writers) == 0 {
				t.Fatalf("nothing writes status.%s, so this check would pass over nothing", record.field)
			}
			compareFunctions(t, "writes", record.field, record.writers, writers)
			compareFunctions(t, "clears", record.field, record.clears, clears)
		})
	}
}

// compareFunctions holds the declared set to what the source does, both ways.
// A function that stopped assigning the record matters as much as one that
// started: the table is the documentation, and a stale entry is a reader
// looking in the wrong place.
func compareFunctions(t *testing.T, verb, field string, declared, found []string) {
	t.Helper()
	declaredSet := map[string]bool{}
	for _, name := range unique(declared) {
		declaredSet[name] = true
	}
	foundSet := map[string]bool{}
	for _, name := range found {
		foundSet[name] = true
	}
	for _, name := range found {
		if !declaredSet[name] {
			t.Errorf("%s %s status.%s and is not declared as one of the functions that may", name, verb, field)
		}
	}
	for name := range declaredSet {
		if !foundSet[name] {
			t.Errorf("%s is declared to %s status.%s and no longer does", name, verb, field)
		}
	}
}
