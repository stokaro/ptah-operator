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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The architecture page is prose, and prose has no compiler. It went eight
// months describing kinds that were never watched and a migration controller
// that had already shipped, because nothing in the repository could tell.
//
// This is the narrow part a check can hold: the page has to name every phase,
// every operation type and every kind the API serves, and may not name one
// that does not exist. It says nothing about whether the sentences around
// those names are still true -- that is what review is for -- but a renamed
// phase, a new phase and an invented one all stop being silent.
const architecturePagePath = "docs/site/src/content/docs/reference/architecture.md"

// The status vocabularies the page is held to, each with the section that has
// to name it. Scoping matters: the two families share most of their spelling,
// so a page-wide search lets the schema lifecycle answer for the migration one
// and hides exactly the omission this check exists to catch. The first draft
// of this page had no `Failed` in its migration diagram, and an unscoped
// search passed it.
var architectureVocabularies = []struct {
	TypeName string
	Section  string
}{
	{TypeName: "ReconciliationPhase", Section: "## The schema lifecycle"},
	{TypeName: "OperationType", Section: "## The schema lifecycle"},
	{TypeName: "MigrationPhase", Section: "## The migration lifecycle"},
	{TypeName: "MigrationOperationType", Section: "## The migration lifecycle"},
}

// vocabularyTypeNames is the list the API reader is given.
func vocabularyTypeNames() []string {
	names := make([]string, 0, len(architectureVocabularies))
	for _, vocabulary := range architectureVocabularies {
		names = append(names, vocabulary.TypeName)
	}
	return names
}

// section returns the text under one `##` heading, which is the scope a
// vocabulary is judged in.
func section(t *testing.T, page, heading string) string {
	t.Helper()
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("%s has no %q section", architecturePagePath, heading)
	}
	rest := page[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestTheArchitecturePageNamesEveryPhaseAndOperation(t *testing.T) {
	t.Parallel()

	page := readArchitecturePage(t)
	values, err := apiVocabularyValues(repositoryFile(t, "api/v1alpha1"), vocabularyTypeNames())
	if err != nil {
		t.Fatalf("read the API vocabularies: %v", err)
	}

	var missing []string
	for _, vocabulary := range architectureVocabularies {
		if len(values[vocabulary.TypeName]) == 0 {
			t.Fatalf("no %s constants were found; the check is reading the wrong package",
				vocabulary.TypeName)
		}
		scope := section(t, page, vocabulary.Section)
		for _, name := range values[vocabulary.TypeName] {
			if !namesValue(scope, name) {
				missing = append(missing, fmt.Sprintf("%s %q in %q",
					vocabulary.TypeName, name, vocabulary.Section))
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%s does not name %d value(s) the API serves: %s",
			architecturePagePath, len(missing), strings.Join(missing, ", "))
	}
}

func TestTheArchitecturePageNamesEveryServedKind(t *testing.T) {
	t.Parallel()

	page := readArchitecturePage(t)
	kinds, err := apiRootKinds(repositoryFile(t, "api/v1alpha1"))
	if err != nil {
		t.Fatalf("read the API kinds: %v", err)
	}
	if len(kinds) == 0 {
		t.Fatal("no root kinds were found; the check is reading the wrong package")
	}

	var missing []string
	for _, kind := range kinds {
		// A List type is API machinery rather than something a reader holds.
		if strings.HasSuffix(kind, "List") {
			continue
		}
		if !strings.Contains(page, kind) {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s does not name %d served kind(s): %s",
			architecturePagePath, len(missing), strings.Join(missing, ", "))
	}
}

// A page may not name a phase or operation the API does not serve. Without
// this half, deleting a phase from the code leaves the page describing it and
// the first check still passes.
func TestTheArchitecturePageNamesNoValueTheAPIDropped(t *testing.T) {
	t.Parallel()

	page := readArchitecturePage(t)
	values, err := apiVocabularyValues(repositoryFile(t, "api/v1alpha1"), vocabularyTypeNames())
	if err != nil {
		t.Fatalf("read the API vocabularies: %v", err)
	}
	served := map[string]struct{}{}
	for _, names := range values {
		for _, name := range names {
			served[name] = struct{}{}
		}
	}

	// Only the words the page presents as status values are judged, which is
	// what a state-diagram line and a backticked name have in common: they
	// stand alone. Ordinary prose that happens to contain "Pending" is not a
	// claim about the API.
	var invented []string
	for _, candidate := range architectureStateWords(page) {
		if _, ok := served[candidate]; ok {
			continue
		}
		invented = append(invented, candidate)
	}
	sort.Strings(invented)
	if len(invented) > 0 {
		t.Fatalf("%s names %d value(s) the API does not serve: %s",
			architecturePagePath, len(invented), strings.Join(invented, ", "))
	}
}

func readArchitecturePage(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile(repositoryFile(t, architecturePagePath))
	if err != nil {
		t.Fatalf("read the architecture page: %v", err)
	}
	return string(page)
}

// namesValue accepts the two ways the page introduces a status value: inside
// backticks, and as a bare word in a Mermaid state diagram.
func namesValue(page, name string) bool {
	if strings.Contains(page, "`"+name+"`") {
		return true
	}
	for _, word := range architectureStateWords(page) {
		if word == name {
			return true
		}
	}
	return false
}

// architectureStateWords collects the capitalized bare words that appear as
// states in the page's Mermaid state diagrams. Those lines are where the page
// makes its claims about the API's vocabulary without backticks.
func architectureStateWords(page string) []string {
	var words []string
	inDiagram := false
	stateDiagram := false
	for _, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```mermaid"):
			inDiagram = true
			stateDiagram = false
			continue
		case strings.HasPrefix(trimmed, "```"):
			inDiagram = false
			stateDiagram = false
			continue
		}
		if inDiagram && strings.HasPrefix(trimmed, "stateDiagram") {
			stateDiagram = true
			continue
		}
		// Only a state diagram declares status values. A flowchart names
		// components and a sequence diagram names participants, and reading
		// either as a phase is how a check invents findings.
		if !stateDiagram || !strings.Contains(trimmed, "-->") {
			continue
		}
		transition := trimmed
		if index := strings.Index(transition, ":"); index >= 0 {
			transition = transition[:index]
		}
		for _, side := range strings.Split(transition, "-->") {
			word := strings.TrimSpace(side)
			if word == "" || word == "[*]" || !isCapitalizedWord(word) {
				continue
			}
			words = append(words, word)
		}
	}
	return words
}

func isCapitalizedWord(word string) bool {
	if word[0] < 'A' || word[0] > 'Z' {
		return false
	}
	for _, character := range word {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
			return false
		}
	}
	return true
}

// apiVocabularyValues reads the string values of the constants declared with
// each named type, so the check follows a rename rather than a spelling kept
// in step by hand.
func apiVocabularyValues(directory string, vocabularies []string) (map[string][]string, error) {
	wanted := map[string]struct{}{}
	for _, vocabulary := range vocabularies {
		wanted[vocabulary] = struct{}{}
	}
	values := map[string][]string{}
	err := forEachAPIFile(directory, func(file *ast.File) {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type == nil {
					continue
				}
				typeName, ok := value.Type.(*ast.Ident)
				if !ok {
					continue
				}
				if _, ok := wanted[typeName.Name]; !ok {
					continue
				}
				for _, expression := range value.Values {
					literal, ok := expression.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(literal.Value)
					if err != nil || unquoted == "" {
						continue
					}
					values[typeName.Name] = append(values[typeName.Name], unquoted)
				}
			}
		}
	})
	return values, err
}

// apiRootKinds reads the types marked as served objects, which is the same
// marker the CRD generator reads.
func apiRootKinds(directory string) ([]string, error) {
	var kinds []string
	err := forEachAPIFile(directory, func(file *ast.File) {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE || general.Doc == nil {
				continue
			}
			root := false
			for _, comment := range general.Doc.List {
				if strings.Contains(comment.Text, "+kubebuilder:object:root=true") {
					root = true
				}
			}
			if !root {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				kinds = append(kinds, typeSpec.Name.Name)
			}
		}
	})
	sort.Strings(kinds)
	return kinds, err
}

func forEachAPIFile(directory string, visit func(*ast.File)) error {
	entries, err := filepath.Glob(filepath.Join(directory, "*_types.go"))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s holds no API types", directory)
	}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		parsed, err := parser.ParseFile(fileSet, entry, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("%s: %w", entry, err)
		}
		visit(parsed)
	}
	return nil
}
