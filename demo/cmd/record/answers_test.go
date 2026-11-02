package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A step that answers a prompt the command never prints waits for it until
// the prompt deadline, and learns about a reworded question an hour into a
// recording. This holds every prompt a scenario answers to the question
// kubectl ptah approve asks, read from the plugin's source, before any lab
// exists. The question is read rather than imported because the plugin is a
// main package.
func TestEveryAnsweredPromptIsInTheQuestionApproveAsks(t *testing.T) {
	t.Parallel()
	question := approveQuestion(t)
	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, one := range loaded {
		for index, current := range one.Steps {
			if len(current.Answers) == 0 {
				continue
			}
			where := fmt.Sprintf("%s step %d", one.ID, index+1)
			if !strings.HasPrefix(strings.TrimSpace(current.Run), "kubectl ptah approve ") {
				t.Errorf("%s answers a command other than kubectl ptah approve, whose prompt this test cannot read", where)
				continue
			}
			for _, typed := range current.Answers {
				examined++
				if problem := promptProblem(question, typed.Prompt); problem != "" {
					t.Errorf("%s waits for %q: %s", where, typed.Prompt, problem)
				}
			}
		}
	}
	if examined == 0 {
		t.Fatal("no scenario answers a prompt, so this test held nothing")
	}

	// The comparison has to refuse a prompt approve does not print.
	for _, wrong := range []string{"Type y to approve: ", "Type yes to print it: ", ""} {
		if promptProblem(question, wrong) == "" {
			t.Errorf("the comparison accepted %q, which approve does not print", wrong)
		}
	}
}

// promptProblem says why approve, asking question, never prints prompt.
func promptProblem(question, prompt string) string {
	switch {
	case prompt == "":
		return "an empty prompt is always found"
	case strings.Contains(prompt, "%"):
		return "it holds a formatting verb, and approve prints the value instead"
	case !strings.Contains(question, prompt):
		return fmt.Sprintf("approve asks %q", question)
	}
	return ""
}

// approveQuestion is the question kubectl ptah approve asks before it creates
// an approval: the string the source declares as question, before a dry run
// replaces it.
func approveQuestion(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "cmd", "kubectl-ptah", "approve.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || name.Name != "question" {
			return true
		}
		literal, ok := assign.Rhs[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Fatalf("%s declares question as something other than a string literal", path)
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, value)
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%s declares question %d times; this test reads the one declaration", path, len(found))
	}
	return found[0]
}
