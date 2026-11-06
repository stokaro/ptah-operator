package runnerharness

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// Source says where an operation read its artifact.
type Source string

const (
	SourceOCI Source = "oci"
	SourceGit Source = "git"
	// SourceNone is an operation that reads no artifact: a schema Apply reads
	// its plan's bytes.
	SourceNone Source = "none"
)

// OperationCase is one operation from one source.
type OperationCase struct {
	Operation runner.Operation
	Source    Source
}

// Declared is the set a complete run is held to. A row that runs anything else
// fails the run, and so does anything here that no row ran. A runner operation
// or a Ptah command added without a row here fails TestDeclaredCommandsAreTheOnesInTheSource,
// and a row reaching a new command fails the run: adding it is one line here and
// the row that runs it.
var Declared = struct {
	Operations []OperationCase
	Commands   []string
}{
	Operations: []OperationCase{
		{runner.OperationResolve, SourceOCI},
		{runner.OperationResolve, SourceGit},
		{runner.OperationVerify, SourceOCI},
		{runner.OperationObserve, SourceOCI},
		{runner.OperationObserve, SourceGit},
		{runner.OperationPlan, SourceOCI},
		{runner.OperationPlan, SourceGit},
		{runner.OperationApply, SourceNone},
		{runner.OperationMigrationHistory, SourceOCI},
		{runner.OperationMigrationHistory, SourceGit},
		{runner.OperationMigrationApply, SourceOCI},
		{runner.OperationMigrationApply, SourceGit},
	},
	Commands: []string{
		"migrations pull",
		"migrations status",
		"migrations up",
		"oci inspect",
		"oci resolve",
		"oci verify",
		"schema apply",
		"schema apply --dry-run",
		"schema drift",
		"schema plan",
		"schema pull",
		"schema validate",
	},
}

// CommandWords names a Ptah command by its first two arguments. The plan
// validation is a command of its own: it reads the same report at another
// outcome, and a release can change one and not the other.
func CommandWords(args []string) string {
	if len(args) < 2 {
		return strings.Join(args, " ")
	}
	words := args[0] + " " + args[1]
	if slices.Contains(args, "--dry-run") {
		words += " --dry-run"
	}
	return words
}

// Ran is what a complete run counted.
type Ran struct {
	Rows       int
	Operations map[OperationCase]int
	Commands   map[string]int
}

// CheckCount holds what ran to the declared set, and fails on a declared case
// nothing ran, on a case nobody declared, and on a run of no rows.
func CheckCount(operations []OperationCase, commands []string, ran Ran) error {
	var problems []error
	if ran.Rows == 0 {
		problems = append(problems, errors.New("no row ran"))
	}
	for _, operation := range operations {
		if ran.Operations[operation] == 0 {
			problems = append(problems, fmt.Errorf("no row ran the %s operation from a %s source", operation.Operation, operation.Source))
		}
	}
	for operation := range ran.Operations {
		if !slices.Contains(operations, operation) {
			problems = append(problems, fmt.Errorf("a row ran the %s operation from a %s source, and Declared does not name it", operation.Operation, operation.Source))
		}
	}
	for _, command := range commands {
		if ran.Commands[command] == 0 {
			problems = append(problems, fmt.Errorf("no row ran `ptah %s`", command))
		}
	}
	for command := range ran.Commands {
		if !slices.Contains(commands, command) {
			problems = append(problems, fmt.Errorf("a row ran `ptah %s`, and Declared does not name it: add it, with the row that holds its document", command))
		}
	}
	return errors.Join(problems...)
}

// SourceCommands reads the Ptah commands a package's sources write down: every
// []string literal assigned to Args or args whose first element is not a flag.
// An element that is not a string literal reads as "*", which stands for
// whichever verb or family the code computes.
func SourceCommands(directory string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return nil, err
	}
	var found []string
	read := 0
	for _, file := range matches {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file) // #nosec G304 -- a package of this repository
		if err != nil {
			return nil, err
		}
		commands, err := commandsInSource(file, source)
		if err != nil {
			return nil, err
		}
		found = append(found, commands...)
		read++
	}
	if read == 0 {
		return nil, fmt.Errorf("%s holds no source to read", directory)
	}
	slices.Sort(found)
	return slices.Compact(found), nil
}

func commandsInSource(name string, source []byte) ([]string, error) {
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, name, source, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []string
	note := func(target string, value ast.Expr) {
		if target != "Args" && target != "args" {
			return
		}
		if words, ok := commandLiteral(value); ok {
			found = append(found, words)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			for index, left := range typed.Lhs {
				if index >= len(typed.Rhs) {
					break
				}
				switch target := left.(type) {
				case *ast.SelectorExpr:
					note(target.Sel.Name, typed.Rhs[index])
				case *ast.Ident:
					note(target.Name, typed.Rhs[index])
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := typed.Key.(*ast.Ident); ok {
				note(key.Name, typed.Value)
			}
		}
		return true
	})
	return found, nil
}

// commandLiteral reads the first two elements of a []string literal.
func commandLiteral(value ast.Expr) (string, bool) {
	literal, ok := value.(*ast.CompositeLit)
	if !ok || len(literal.Elts) < 2 {
		return "", false
	}
	if array, ok := literal.Type.(*ast.ArrayType); !ok || array.Len != nil {
		return "", false
	} else if element, ok := array.Elt.(*ast.Ident); !ok || element.Name != "string" {
		return "", false
	}
	first, second := commandWord(literal.Elts[0]), commandWord(literal.Elts[1])
	if strings.HasPrefix(first, "-") {
		return "", false
	}
	return first + " " + second, true
}

func commandWord(value ast.Expr) string {
	if word, ok := value.(*ast.BasicLit); ok && word.Kind == token.STRING {
		if unquoted, err := strconv.Unquote(word.Value); err == nil {
			return unquoted
		}
	}
	return "*"
}

// MatchSourceCommands compares what the sources write down with the declared
// commands. "*" in a source command matches any word there.
func MatchSourceCommands(source, declared []string) error {
	var problems []error
	matches := func(pattern, command string) bool {
		patternWords := strings.Fields(pattern)
		commandWords := strings.Fields(command)
		if len(commandWords) < 2 || len(patternWords) != 2 {
			return false
		}
		return (patternWords[0] == "*" || patternWords[0] == commandWords[0]) &&
			(patternWords[1] == "*" || patternWords[1] == commandWords[1])
	}
	for _, pattern := range source {
		if !slices.ContainsFunc(declared, func(command string) bool { return matches(pattern, command) }) {
			problems = append(problems, fmt.Errorf("the source starts `ptah %s`, and Declared does not name it: add it, with the row that holds its document", pattern))
		}
	}
	for _, command := range declared {
		if !slices.ContainsFunc(source, func(pattern string) bool { return matches(pattern, command) }) {
			problems = append(problems, fmt.Errorf("Declared names `ptah %s`, and no source starts it", command))
		}
	}
	return errors.Join(problems...)
}

// SourceOperations reads the Operation constants a runner source declares.
func SourceOperations(source []byte) ([]runner.Operation, error) {
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "protocol.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []runner.Operation
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if kind, ok := spec.Type.(*ast.Ident); !ok || kind.Name != "Operation" {
			return true
		}
		for _, value := range spec.Values {
			if literal, ok := value.(*ast.BasicLit); ok {
				if unquoted, err := strconv.Unquote(literal.Value); err == nil {
					found = append(found, runner.Operation(unquoted))
				}
			}
		}
		return true
	})
	return found, nil
}
