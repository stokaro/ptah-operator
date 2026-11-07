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
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The operator's closed vocabulary is the list the pinned Ptah can emit, no
// more and no less. A category Ptah emits and the vocabulary lacks fails
// Observe for every change in it; one the vocabulary keeps after Ptah dropped
// it is an enum value nothing can produce.
func TestVendoredCategoriesAreTheOperatorVocabulary(t *testing.T) {
	t.Parallel()

	vendored, err := LoadVendored(filepath.Join("..", "..", vendoredPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(vendored.Categories) == 0 {
		t.Fatal("the vendored file lists no category")
	}
	if !slices.IsSorted(vendored.Categories) || len(slices.Compact(slices.Clone(vendored.Categories))) != len(vendored.Categories) {
		t.Fatalf("vendored categories are not sorted and unique: %q", vendored.Categories)
	}
	if got := dataplane.DriftFindingCategories(); !reflect.DeepEqual(got, vendored.Categories) {
		t.Fatalf("operator vocabulary = %q\npinned Ptah emits    %q", got, vendored.Categories)
	}
	if len(vendored.Qualified) == 0 {
		t.Fatal("the vendored file lists no qualified category")
	}
	if got := dataplane.DriftFindingQualifiedCategories(); !reflect.DeepEqual(got, vendored.Qualified) {
		t.Fatalf("operator qualified vocabulary = %q\npinned Ptah qualifies     %q", got, vendored.Qualified)
	}
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	paths := make([]string, 0, len(vendored.Sources))
	for _, file := range vendored.Sources {
		if !digest.MatchString(file.SHA256) {
			t.Errorf("%s has digest %q", file.Path, file.SHA256)
		}
		paths = append(paths, file.Path)
	}
	if want := []string{vocabularyPath}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("vendored sources = %q, want the file this program reads, %q", paths, want)
	}
}

// vocabulary is a docs/drift-findings.json with the rows given.
func vocabulary(rows ...string) string {
	return `{"comment": "test", "categories": [` + strings.Join(rows, ",") + `]}`
}

// A fixed category is one Ptah writes bare only; one it writes with a kind
// is qualified, whether or not it also writes it bare.
func TestComputeSplitsFixedFromQualified(t *testing.T) {
	t.Parallel()
	content := vocabulary(
		`{"category": "tables_added", "severities": ["safe"], "bare": true, "qualified": false}`,
		`{"category": "feature_access_widened", "severities": ["destructive"], "bare": false, "qualified": true}`,
		`{"category": "feature_changes", "severities": ["safe", "warning", "destructive"], "bare": true, "qualified": true}`,
		`{"category": "columns_removed", "severities": ["destructive"], "bare": true, "qualified": false}`,
	)

	computed, err := Compute(func(path string) ([]byte, error) {
		if path != vocabularyPath {
			t.Fatalf("read %s, want %s", path, vocabularyPath)
		}
		return []byte(content), nil
	})

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"columns_removed", "tables_added"}; !reflect.DeepEqual(computed.Categories, want) {
		t.Fatalf("categories = %q, want %q", computed.Categories, want)
	}
	if want := []string{"feature_access_widened", "feature_changes"}; !reflect.DeepEqual(computed.Qualified, want) {
		t.Fatalf("qualified = %q, want %q", computed.Qualified, want)
	}
	if len(computed.Sources) != 1 || computed.Sources[0].Path != vocabularyPath || len(computed.Sources[0].SHA256) != 64 {
		t.Fatalf("sources = %+v, want the vocabulary file and its digest", computed.Sources)
	}
}

// A vocabulary this program could read wrong is refused, never read short.
func TestParseVocabularyRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, content, says string
	}{
		{name: "no category", content: vocabulary(), says: "lists no category"},
		{name: "an unknown field", content: vocabulary(`{"category": "a", "severities": ["safe"], "bare": true, "plural": true}`),
			says: `unknown field "plural"`},
		{name: "a category that is no machine name", content: vocabulary(`{"category": "Tables Added", "severities": ["safe"], "bare": true}`),
			says: `"Tables Added" is not a lowercase machine name`},
		{name: "a category twice", content: vocabulary(
			`{"category": "tables_added", "severities": ["safe"], "bare": true}`,
			`{"category": "tables_added", "severities": ["warning"], "bare": true}`), says: "tables_added is listed twice"},
		{name: "no form", content: vocabulary(`{"category": "tables_added", "severities": ["safe"]}`),
			says: "tables_added is written neither bare nor qualified"},
		{name: "no severity", content: vocabulary(`{"category": "tables_added", "severities": [], "bare": true}`),
			says: "tables_added carries no severity"},
		{name: "a severity a report does not use", content: vocabulary(`{"category": "tables_added", "severities": ["critical"], "bare": true}`),
			says: `tables_added carries severity "critical"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseVocabulary([]byte(test.content))
			if err == nil || !strings.Contains(err.Error(), test.says) {
				t.Fatalf("ParseVocabulary() = %v, want an error saying %q", err, test.says)
			}
		})
	}
}

func TestCompareNamesEveryDifference(t *testing.T) {
	t.Parallel()

	base := Vendored{
		Sources: []VendoredSource{
			{Path: "internal/cli/drift/drift.go", SHA256: strings.Repeat("a", 64)},
			{Path: "migration/safety/safety.go", SHA256: strings.Repeat("b", 64)},
		},
		Categories: []string{"rls_force_added", "tables_added"},
		Qualified:  []string{"feature_changes"},
	}
	if err := Compare(base, base); err != nil {
		t.Fatalf("Compare(identical) = %v", err)
	}
	computed := Vendored{
		Sources: []VendoredSource{
			{Path: "internal/cli/drift/drift.go", SHA256: strings.Repeat("a", 64)},
			{Path: "migration/safety/safety.go", SHA256: strings.Repeat("c", 64)},
			{Path: "internal/datamigrate/datamigrate.go", SHA256: strings.Repeat("d", 64)},
		},
		Categories: []string{"rls_force_removed", "tables_added"},
		Qualified:  []string{"feature_access_widened"},
	}
	err := Compare(base, computed)
	if err == nil {
		t.Fatal("Compare() accepted a vendored file that differs from its source")
	}
	for _, want := range []string{
		"migration/safety/safety.go changed",
		"internal/datamigrate/datamigrate.go is not recorded",
		"Ptah emits rls_force_removed",
		"Ptah no longer emits rls_force_added",
		"Ptah emits feature_access_widened with a kind",
		"Ptah no longer emits feature_changes with a kind",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compare() error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "drift.go") {
		t.Errorf("Compare() error %q names a file that did not change", err)
	}
}

// The command end to end, against a Ptah history built here: it writes the
// file from the pinned commit, accepts that file, and refuses it once the pin
// moves to a commit whose classifier gained a category, naming the category.
func TestRunFollowsThePinnedCommit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}

	checkout := t.TempDir()
	gitIn(t, checkout, "init", "-q")
	writeFile(t, checkout, vocabularyPath, vocabulary(
		`{"category": "data_rows_inserted", "severities": ["safe"], "bare": true}`,
		`{"category": "feature_changes", "severities": ["warning"], "bare": true, "qualified": true}`,
		`{"category": "rls_force_added", "severities": ["safe"], "bare": true}`,
		`{"category": "tables_added", "severities": ["safe"], "bare": true}`,
		`{"category": "undecided", "severities": ["warning"], "bare": true}`,
	))
	first := commitAll(t, checkout, "first")
	writeFile(t, checkout, vocabularyPath, vocabulary(
		`{"category": "data_rows_inserted", "severities": ["safe"], "bare": true}`,
		`{"category": "feature_changes", "severities": ["warning"], "bare": true, "qualified": true}`,
		`{"category": "rls_force_added", "severities": ["safe"], "bare": true}`,
		`{"category": "rls_force_removed", "severities": ["destructive"], "bare": true}`,
		`{"category": "tables_added", "severities": ["safe"], "bare": true}`,
		`{"category": "undecided", "severities": ["warning"], "bare": true}`,
	))
	second := commitAll(t, checkout, "second")

	root := t.TempDir()
	pin(t, root, first)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-ptah", checkout, "-write"}, root, &out); err != nil {
		t.Fatalf("run(-write) = %v", err)
	}
	written, err := LoadVendored(filepath.Join(root, vendoredPath))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"data_rows_inserted", "rls_force_added", "tables_added", "undecided"}; !reflect.DeepEqual(written.Categories, want) {
		t.Fatalf("written categories = %q, want %q", written.Categories, want)
	}
	if want := []string{"feature_changes"}; !reflect.DeepEqual(written.Qualified, want) {
		t.Fatalf("written qualified categories = %q, want %q", written.Qualified, want)
	}
	if err := run(context.Background(), []string{"-ptah", checkout}, root, &out); err != nil {
		t.Fatalf("run() against the commit it was written from = %v", err)
	}

	pin(t, root, second)
	err = run(context.Background(), []string{"-ptah", checkout}, root, &out)
	if err == nil || !strings.Contains(err.Error(), "Ptah emits rls_force_removed") ||
		!strings.Contains(err.Error(), vocabularyPath+" changed") {
		t.Fatalf("run() after the pin moved = %v, want the new category and the changed file named", err)
	}
	if err := run(context.Background(), []string{"-ptah", filepath.Join(root, "absent")}, root, &out); err == nil {
		t.Fatal("run() accepted a checkout that does not exist")
	}
}

func pin(t *testing.T, root, commit string) {
	t.Helper()
	writeFile(t, root, catalogPath,
		`{"releases":[{"operator":"edge","verified":[{"ptahCommit":"`+commit+`"}]}]}`)
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, checkout, message string) string {
	t.Helper()
	gitIn(t, checkout, "add", "-A")
	gitIn(t, checkout, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
	return strings.TrimSpace(gitIn(t, checkout, "rev-parse", "HEAD"))
}

func gitIn(t *testing.T, checkout string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", checkout}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
