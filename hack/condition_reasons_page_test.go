package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Conditions are the operator's stable machine interface, and this page is
// where a reader looks up the reason a resource is reporting. It states that
// the Go API exports every value it lists as a typed ConditionReason.
//
// That is a claim about the tree, and it had drifted: ProtectedTable -- the
// fence that leaves a schema Blocked with no approval that overrides it -- was
// exported, set on three Conditions, and absent from the page. A reader who
// reached the page for exactly the state it names found nothing.

const conditionReasonsPage = "docs/site/src/content/docs/troubleshoot/condition-reasons.md"

var conditionReasonRow = regexp.MustCompile("(?m)^\\| `([A-Za-z]+)` \\| (.*) \\|$")

// Every reason the API exports has a row.
func TestEveryConditionReasonIsOnThePage(t *testing.T) {
	t.Parallel()
	listed := conditionReasonRows(t)
	for _, reason := range exportedConditionReasons(t) {
		if _, ok := listed[reason]; !ok {
			t.Errorf("the API exports %s and the page does not list it, so a resource reporting it sends its reader to a page that does not have it", reason)
		}
	}
}

// And every row is a reason the API exports. A row for a value nothing sets
// is worse than a missing one: it reads as a state a reader should expect.
func TestThePageListsNoReasonTheAPIDropped(t *testing.T) {
	t.Parallel()
	exported := map[string]bool{}
	for _, reason := range exportedConditionReasons(t) {
		exported[reason] = true
	}
	for reason := range conditionReasonRows(t) {
		if !exported[reason] {
			t.Errorf("the page lists %s and the API exports no such reason", reason)
		}
	}
}

// The table is read by looking a reason up, which is alphabetical order or
// nothing. A row appended at the end is a row a reader scrolls past.
func TestTheConditionReasonsReadInOrder(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, conditionReasonsPage)
	var order []string
	for _, match := range conditionReasonRow.FindAllStringSubmatch(page, -1) {
		order = append(order, match[1])
	}
	if len(order) == 0 {
		t.Fatalf("%s has no reason rows, so this check reads the page and holds none of it", conditionReasonsPage)
	}
	// Case-insensitively, which is how a reader scans a column: InputsChanged
	// belongs after InSync to a byte comparison and before it to a person.
	sorted := append([]string(nil), order...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i]) < strings.ToLower(sorted[j])
	})
	for index := range order {
		if order[index] != sorted[index] {
			t.Fatalf("the reasons are listed %v...; alphabetically they are %v...",
				order[max(0, index-1):min(len(order), index+2)],
				sorted[max(0, index-1):min(len(sorted), index+2)])
		}
	}
}

// A row with no meaning is a row that answers nothing.
func TestEveryConditionReasonRowSaysWhatItMeans(t *testing.T) {
	t.Parallel()
	rows := conditionReasonRows(t)
	if len(rows) == 0 {
		t.Fatalf("%s has no reason rows, so this check reads the page and holds none of it", conditionReasonsPage)
	}
	for reason, meaning := range rows {
		if strings.TrimSpace(meaning) == "" {
			t.Errorf("%s has a row and no stable meaning", reason)
		}
	}
}

// conditionReasonRows reads the table as reason -> stable meaning.
func conditionReasonRows(t *testing.T) map[string]string {
	t.Helper()
	rows := map[string]string{}
	for _, match := range conditionReasonRow.FindAllStringSubmatch(readDocumentationPage(t, conditionReasonsPage), -1) {
		rows[match[1]] = match[2]
	}
	return rows
}

// exportedConditionReasons reads the constants out of the API package rather
// than out of a list here, so a reason added without a row fails by name.
func exportedConditionReasons(t *testing.T) []string {
	t.Helper()
	directory := repositoryFile(t, filepath.Join("api", "v1alpha1"))
	packages, err := parser.ParseDir(token.NewFileSet(), directory, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", directory, err)
	}
	var reasons []string
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				general, ok := decl.(*ast.GenDecl)
				if !ok || general.Tok != token.CONST {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || !isConditionReason(value.Type) {
						continue
					}
					for _, expression := range value.Values {
						literal, ok := expression.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							continue
						}
						text, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", literal.Value, err)
						}
						reasons = append(reasons, text)
					}
				}
			}
		}
	}
	if len(reasons) == 0 {
		t.Fatal("the API package declares no ConditionReason, so this check compares the page with nothing")
	}
	sort.Strings(reasons)
	return reasons
}

func isConditionReason(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "ConditionReason"
}
