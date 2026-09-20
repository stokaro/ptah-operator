// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A recipe that builds in a temporary directory has to clean it up, and the
// cleanup runs last. A shell reports the status of the last command, so a
// recipe that ends in `rm -rf` reports the status of the removal and reports
// success whatever happened before it.
//
// `docs-reference` ended that way, so a generator that refused the tree still
// left make exiting 0: the pages were not written, nothing said so, and the
// next check to look at them was the one in CI. Its sibling
// `docs-reference-check` had the right shape all along, which is what makes
// this a slip rather than a decision.
//
// The rule is narrow on purpose. It applies only to a recipe that creates a
// temporary directory, because that is the shape where cleanup has to come
// after the work and cannot be left to the shell.
const recipeMakefilePath = "Makefile"

func TestEveryMakeRecipeWithATempDirectoryReportsItsOwnStatus(t *testing.T) {
	t.Parallel()

	document, err := os.ReadFile(repositoryFile(t, recipeMakefilePath))
	if err != nil {
		t.Fatalf("read %s: %v", recipeMakefilePath, err)
	}

	recipes := makeRecipes(string(document))
	if len(recipes) == 0 {
		t.Fatalf("%s holds no recipes, which cannot be right", recipeMakefilePath)
	}

	checked := 0
	for _, recipe := range recipes {
		if !strings.Contains(recipe.body, "mktemp -d") {
			continue
		}
		checked++
		if err := recipeReportsItsOwnStatus(recipe.body); err != nil {
			t.Errorf("%s: %v", recipe.target, err)
		}
	}
	if checked == 0 {
		t.Fatal("no recipe builds in a temporary directory, so this check measured nothing")
	}
}

// recipeReportsItsOwnStatus is the rule, as a function, so the mutations below
// can assert a refusal rather than taking the test down with them.
func recipeReportsItsOwnStatus(body string) error {
	commands := recipeCommands(body)
	if len(commands) == 0 {
		return fmt.Errorf("the recipe is empty")
	}
	last := commands[len(commands)-1]
	if last != "exit $$status" {
		return fmt.Errorf("ends with %q; a recipe that cleans up last reports the cleanup's status, "+
			"so capture the work's status first and end with `exit $$status`", last)
	}
	capture := -1
	for index, command := range commands {
		if command != "status=$$?" {
			continue
		}
		if capture != -1 {
			return fmt.Errorf("captures a status twice; only the one taken after the work says anything")
		}
		capture = index
	}
	if capture == -1 {
		return fmt.Errorf("ends with %q but never captures a status to exit with", last)
	}
	// Everything between the capture and the exit has to be cleanup. A recipe
	// that captures early -- mktemp's own status, say -- then does the work and
	// exits that stale value reports success whatever the work did, which is
	// the failure this check exists to refuse and the one it used to accept.
	for _, command := range commands[capture+1 : len(commands)-1] {
		if strings.HasPrefix(command, "rm ") {
			continue
		}
		return fmt.Errorf("runs %q after capturing the status, so what it exits with is not that command's; "+
			"capture immediately after the work and clean up between", command)
	}
	return nil
}

type makeRecipe struct {
	target string
	body   string
}

// makeRecipes splits a Makefile into its recipes. A recipe is the tab-indented
// block under a target line; everything else -- variables, comments, .PHONY --
// is not one.
func makeRecipes(document string) []makeRecipe {
	var recipes []makeRecipe
	var target string
	var body strings.Builder

	flush := func() {
		if target != "" && body.Len() > 0 {
			recipes = append(recipes, makeRecipe{target: target, body: body.String()})
		}
		body.Reset()
	}

	for _, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(line, "\t") {
			if target != "" {
				body.WriteString(strings.TrimPrefix(line, "\t"))
				body.WriteString("\n")
			}
			continue
		}
		flush()
		target = ""
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ".") {
			continue
		}
		if name, _, found := strings.Cut(line, ":"); found && !strings.HasPrefix(line, " ") {
			if !strings.Contains(name, "=") {
				target = strings.TrimSpace(name)
			}
		}
	}
	flush()
	return recipes
}

// recipeCommands returns the recipe's commands in order, joining the lines a
// trailing backslash continues and dropping the `@` that silences echo.
func recipeCommands(body string) []string {
	joined := strings.ReplaceAll(body, "\\\n", " ")
	var commands []string
	for _, line := range strings.Split(joined, "\n") {
		for _, command := range strings.Split(line, ";") {
			command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "@"))
			command = strings.Join(strings.Fields(command), " ")
			if command != "" {
				commands = append(commands, command)
			}
		}
	}
	return commands
}

// A gate that has never refused anything has not been measured. The first
// mutation is the shape `docs-reference` actually had; the second is the
// half-fix that exits on a status nothing ever captured.
func TestTheMakeRecipeStatusCheckRefusesARecipeThatSwallowsIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		wants bool
	}{
		{
			name: "cleanup runs last, so make reports the cleanup",
			body: "@tmp=$$(mktemp -d); \\\n" +
				"$(GO) run ./hack/crdreference -crds $$tmp -write; \\\n" +
				"rm -rf $$tmp\n",
			wants: true,
		},
		{
			name: "exits on a status nothing captured",
			body: "@tmp=$$(mktemp -d); \\\n" +
				"$(GO) run ./hack/crdreference -crds $$tmp -write; \\\n" +
				"rm -rf $$tmp; exit $$status\n",
			wants: true,
		},
		{
			// Review found this one: the capture is taken before the work, so
			// the recipe exits mktemp's status and reports success whatever
			// the generator did.
			name: "captures a status taken before the work",
			body: "@tmp=$$(mktemp -d); \\\n" +
				"status=$$?; \\\n" +
				"$(GO) run ./hack/crdreference -crds $$tmp -write; \\\n" +
				"rm -rf $$tmp; exit $$status\n",
			wants: true,
		},
		{
			// And this one: a literal exit passes whatever was captured.
			name: "exits a literal rather than the captured status",
			body: "@tmp=$$(mktemp -d); \\\n" +
				"$(GO) run ./hack/crdreference -crds $$tmp -write; \\\n" +
				"status=$$?; rm -rf $$tmp; exit 0\n",
			wants: true,
		},
		{
			name: "captures the status and exits with it",
			body: "@tmp=$$(mktemp -d); \\\n" +
				"$(GO) run ./hack/crdreference -crds $$tmp -write; \\\n" +
				"status=$$?; rm -rf $$tmp; exit $$status\n",
			wants: false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := recipeReportsItsOwnStatus(test.body)
			if test.wants && err == nil {
				t.Fatal("the check accepted a recipe that swallows its status")
			}
			if !test.wants && err != nil {
				t.Fatalf("the check refused a sound recipe: %v", err)
			}
		})
	}
}
