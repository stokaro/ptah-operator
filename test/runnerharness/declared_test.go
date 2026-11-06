package runnerharness_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

// sourceCommands reads every Ptah command the two packages that start one write
// down: the runner's children, and the OCI fetch the builder gives a container.
func sourceCommands(t *testing.T) []string {
	t.Helper()
	var commands []string
	for _, directory := range []string{"internal/runner", "internal/workload"} {
		found, err := runnerharness.SourceCommands(filepath.Join(repositoryRoot, filepath.FromSlash(directory)))
		if err != nil {
			t.Fatal(err)
		}
		commands = append(commands, found...)
	}
	return commands
}

// A command added to the runner without a row here fails offline, whether or
// not any case reaches it.
func TestDeclaredCommandsAreTheOnesInTheSource(t *testing.T) {
	t.Parallel()
	source := sourceCommands(t)
	if len(source) < 6 {
		t.Fatalf("read the commands %q: that is not all of them", source)
	}
	if err := runnerharness.MatchSourceCommands(source, runnerharness.Declared.Commands); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredOperationsAreTheRunnerOperations(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join(repositoryRoot, "internal", "runner", "protocol.go"))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := runnerharness.SourceOperations(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) == 0 {
		t.Fatal("read no operation")
	}
	for _, operation := range operations {
		if !operation.Valid() {
			t.Errorf("%q is not an operation", operation)
		}
		if !slices.ContainsFunc(runnerharness.Declared.Operations, func(declared runnerharness.OperationCase) bool {
			return declared.Operation == operation
		}) {
			t.Errorf("the runner's operation %s has no row in Declared", operation)
		}
	}
	for _, declared := range runnerharness.Declared.Operations {
		if !slices.Contains(operations, declared.Operation) {
			t.Errorf("Declared names %s, which the runner does not declare", declared.Operation)
		}
	}
}

func TestSourceOperationsReadsAConstantAddedToTheRunner(t *testing.T) {
	t.Parallel()
	operations, err := runnerharness.SourceOperations([]byte(`package runner
const (
	OperationResolve Operation = "resolve"
	OperationLint Operation = "lint"
	NotAnOperation string = "other"
)
`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(operations, []runner.Operation{"resolve", "lint"}) {
		t.Fatalf("read %q", operations)
	}
}

// The reader has to find a new command in each form the sources write one, and
// has to leave alone what is not a command.
func TestSourceCommandsFindsACommandAddedToTheRunner(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, source string
		want         []string
	}{
		{"a field assigned", `spec.Args = []string{"schema", "lint", "--json"}`, []string{"schema lint"}},
		{"a field in a literal", `spec := CommandSpec{Path: path, Args: []string{"oci", "copy", reference}}`, []string{"oci copy"}},
		{"a computed verb", `spec.Args = []string{"migrations", verb, "--json"}`, []string{"migrations *"}},
		{"a computed family", `args := []string{family, "pull", reference}`, []string{"* pull"}},
		{"another list", `selectors := []string{"schema", "lint"}`, nil},
		{"flags alone", `spec.Args = []string{"--install-to", path}`, nil},
		{"one word", `spec.Args = []string{"version"}`, nil},
		{"appended flags", `spec.Args = append(spec.Args, "--dry-run")`, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			source := "package runner\nfunc f() {\n" + row.source + "\n_ = spec\n}\n"
			if err := os.WriteFile(filepath.Join(directory, "a.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := runnerharness.SourceCommands(directory)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("read %q, want %q", got, row.want)
			}
		})
	}
	t.Run("a test file is not the runner", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "a_test.go"), []byte("package runner\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runnerharness.SourceCommands(directory); err == nil {
			t.Fatal("reading a directory of no source succeeded")
		}
	})
}

func TestMatchSourceCommandsRefusesAnEitherSideAlone(t *testing.T) {
	t.Parallel()
	declared := []string{"schema pull", "migrations pull", "migrations up"}
	source := []string{"* pull", "migrations *"}
	if err := runnerharness.MatchSourceCommands(source, declared); err != nil {
		t.Fatalf("the matching set was refused: %v", err)
	}
	if err := runnerharness.MatchSourceCommands(append(slices.Clone(source), "schema lint"), declared); err == nil || !strings.Contains(err.Error(), "schema lint") {
		t.Fatalf("a command with no row: %v", err)
	}
	if err := runnerharness.MatchSourceCommands(source, append(slices.Clone(declared), "oci inspect")); err == nil || !strings.Contains(err.Error(), "oci inspect") {
		t.Fatalf("a row for no command: %v", err)
	}
}

// The count fails on a declared case nothing ran, on a case nobody declared, and
// on a run of nothing.
func TestCheckCountHoldsWhatRanToTheDeclaredSet(t *testing.T) {
	t.Parallel()
	resolveOCI := runnerharness.OperationCase{Operation: runner.OperationResolve, Source: runnerharness.SourceOCI}
	resolveGit := runnerharness.OperationCase{Operation: runner.OperationResolve, Source: runnerharness.SourceGit}
	operations := []runnerharness.OperationCase{resolveOCI}
	commands := []string{"oci resolve"}
	ran := func(mutate func(*runnerharness.Ran)) runnerharness.Ran {
		value := runnerharness.Ran{
			Rows: 1, Operations: map[runnerharness.OperationCase]int{resolveOCI: 1}, Commands: map[string]int{"oci resolve": 1},
		}
		if mutate != nil {
			mutate(&value)
		}
		return value
	}
	if err := runnerharness.CheckCount(operations, commands, ran(nil)); err != nil {
		t.Fatalf("the declared set was refused: %v", err)
	}
	for _, row := range []struct {
		name   string
		mutate func(*runnerharness.Ran)
		want   string
	}{
		{"no row", func(value *runnerharness.Ran) { value.Rows = 0 }, "no row ran"},
		{"an operation nothing ran", func(value *runnerharness.Ran) { delete(value.Operations, resolveOCI) }, "from a oci source"},
		{"an operation nobody declared", func(value *runnerharness.Ran) { value.Operations[resolveGit] = 1 }, "Declared does not name it"},
		{"a command nothing ran", func(value *runnerharness.Ran) { delete(value.Commands, "oci resolve") }, "no row ran `ptah oci resolve`"},
		{"a command nobody declared", func(value *runnerharness.Ran) { value.Commands["schema lint"] = 1 }, "`ptah schema lint`"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if err := runnerharness.CheckCount(operations, commands, ran(row.mutate)); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("CheckCount() = %v, want it to name %q", err, row.want)
			}
		})
	}
}

func TestCommandWordsNamesTheValidationApart(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		"schema apply --plan /p --auto-approve --json":           "schema apply",
		"schema apply --plan /p --auto-approve --json --dry-run": "schema apply --dry-run",
		"oci resolve oci://r/a:v1 --format json":                 "oci resolve",
		"version":                                                "version",
	} {
		if got := runnerharness.CommandWords(strings.Fields(args)); got != want {
			t.Errorf("CommandWords(%q) = %q, want %q", args, got, want)
		}
	}
}
