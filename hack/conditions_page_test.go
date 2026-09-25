package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Conditions are the operator's stable machine interface, and a consumer that
// waits for one has to know it exists. The site documented every reason
// exhaustively and never said which Conditions a kind publishes, so the only
// way to learn that a PtahMigrationApproval reports Consumed and nothing else
// was to read the controller.
//
// These hold the page to the controllers, per kind and both ways.

const conditionsPage = "docs/site/src/content/docs/troubleshoot/conditions.md"

var (
	conditionsKindHeading = regexp.MustCompile(`(?m)^## (Ptah[A-Za-z]+)$`)
	conditionsRow         = regexp.MustCompile("(?m)^\\| `([A-Za-z]+)` \\| (.*) \\|$")
)

// Every Condition a kind publishes has a row under that kind.
func TestEveryPublishedConditionIsOnThePage(t *testing.T) {
	t.Parallel()
	listed := conditionsByKindOnPage(t)
	for kind, conditions := range conditionsWrittenByKind(t) {
		for condition := range conditions {
			if !listed[kind][condition] {
				t.Errorf("a %s publishes %s and the page does not list it under that kind, so a consumer cannot learn to wait for it",
					kind, condition)
			}
		}
	}
}

// And every row is a Condition that kind publishes. A row under the wrong kind
// tells a consumer to wait for something that never arrives.
func TestThePageListsNoConditionAKindDoesNotPublish(t *testing.T) {
	t.Parallel()
	written := conditionsWrittenByKind(t)
	for kind, conditions := range conditionsByKindOnPage(t) {
		for condition := range conditions {
			if !written[kind][condition] {
				t.Errorf("the page lists %s under %s and nothing writes it there", condition, kind)
			}
		}
	}
}

// Each kind's table is read by looking a Condition up, and a row with no
// meaning answers nothing.
func TestTheConditionRowsReadInOrderAndSayWhatTheyMean(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, conditionsPage)
	sections := conditionsKindHeading.FindAllStringSubmatchIndex(page, -1)
	if len(sections) == 0 {
		t.Fatalf("%s names no kind, so this check reads the page and holds none of it", conditionsPage)
	}
	for index, section := range sections {
		end := len(page)
		if index+1 < len(sections) {
			end = sections[index+1][0]
		}
		kind := page[section[2]:section[3]]
		var order []string
		for _, row := range conditionsRow.FindAllStringSubmatch(page[section[1]:end], -1) {
			order = append(order, row[1])
			if strings.TrimSpace(row[2]) == "" {
				t.Errorf("%s under %s has a row and no meaning", row[1], kind)
			}
		}
		sorted := append([]string(nil), order...)
		sort.Slice(sorted, func(i, j int) bool { return strings.ToLower(sorted[i]) < strings.ToLower(sorted[j]) })
		for position := range order {
			if order[position] != sorted[position] {
				t.Errorf("%s lists %v...; alphabetically they are %v...", kind, order[position:], sorted[position:])
				break
			}
		}
	}
}

// conditionsByKindOnPage reads the page as kind -> Condition set.
func conditionsByKindOnPage(t *testing.T) map[string]map[string]bool {
	t.Helper()
	page := readDocumentationPage(t, conditionsPage)
	sections := conditionsKindHeading.FindAllStringSubmatchIndex(page, -1)
	if len(sections) == 0 {
		t.Fatalf("%s names no kind", conditionsPage)
	}
	listed := map[string]map[string]bool{}
	for index, section := range sections {
		end := len(page)
		if index+1 < len(sections) {
			end = sections[index+1][0]
		}
		kind := page[section[2]:section[3]]
		listed[kind] = map[string]bool{}
		for _, row := range conditionsRow.FindAllStringSubmatch(page[section[1]:end], -1) {
			listed[kind][row[1]] = true
		}
	}
	return listed
}

// conditionsWrittenByKind reads which Conditions the controllers write on which
// kind. A write this cannot attribute fails the check rather than shrinking the
// set, because a short set turns the page check into one that always passes.
func conditionsWrittenByKind(t *testing.T) map[string]map[string]bool {
	t.Helper()
	values := conditionConstants(t)
	written := map[string]map[string]bool{}
	var unresolved []string
	record := func(kind, constant string) {
		if written[kind] == nil {
			written[kind] = map[string]bool{}
		}
		written[kind][values[constant]] = true
	}
	fileSet := token.NewFileSet()
	root := repositoryFile(t, "internal")
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
				if !ok {
					return true
				}
				switch {
				case isNamedCall(call, "setCondition"):
					if constant, ok := conditionConstantArgument(call); ok {
						record("PtahSchema", constant)
					} else if function.Name.Name != "setCondition" {
						unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
					}
				case isNamedCall(call, "setMigrationCondition"):
					if constant, ok := conditionConstantArgument(call); ok {
						record("PtahMigration", constant)
					} else if function.Name.Name != "setMigrationCondition" {
						unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
					}
				case isMetaStatusCondition(call):
					constant, constantOK := conditionConstantArgument(call)
					kind, kindOK := conditionTargetKind(call, function)
					if !constantOK || !kindOK {
						if !isConditionForwardingHelper(function.Name.Name) {
							unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
						}
						return true
					}
					record(kind, constant)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Fatalf("these Condition writes cannot be attributed to a kind, so the set this compares the page with is incomplete: %v", unresolved)
	}
	if len(written) == 0 {
		t.Fatal("no Condition write was found, so this check would call the page complete")
	}
	return written
}

func isNamedCall(call *ast.CallExpr, name string) bool {
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

func isMetaStatusCondition(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "SetStatusCondition" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "meta"
}

// isConditionForwardingHelper names the two functions that write whatever
// Condition their caller handed them. Every caller is a site already read.
func isConditionForwardingHelper(name string) bool {
	return name == "setCondition" || name == "setMigrationCondition"
}

// conditionConstantArgument finds the Condition constant a call names, as an
// argument or as the Type field of a metav1.Condition literal.
func conditionConstantArgument(call *ast.CallExpr) (string, bool) {
	for _, argument := range call.Args {
		if name, ok := operatorConstant(argument); ok && strings.HasPrefix(name, "Condition") {
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
			if name, ok := operatorConstant(pair.Value); ok {
				return name, true
			}
		}
	}
	return "", false
}

// conditionTargetKind reads the kind whose conditions the call appends to, from
// the `&x.Status.Conditions` argument and where x comes from.
func conditionTargetKind(call *ast.CallExpr, function *ast.FuncDecl) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	unary, ok := call.Args[0].(*ast.UnaryExpr)
	if !ok {
		return "", false
	}
	conditions, ok := unary.X.(*ast.SelectorExpr)
	if !ok || conditions.Sel.Name != "Conditions" {
		return "", false
	}
	status, ok := conditions.X.(*ast.SelectorExpr)
	if !ok || status.Sel.Name != "Status" {
		return "", false
	}
	holder, ok := status.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return declaredAPIKind(function, holder.Name)
}
