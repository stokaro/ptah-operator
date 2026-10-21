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
	want := make([]string, 0, len(sources))
	for _, file := range sources {
		want = append(want, file.path)
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("vendored sources = %q, want the files this program reads, %q", paths, want)
	}
}

func TestExtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		call    string
		want    []string
		failure string
	}{
		{
			name: "string literals, and a similarly named helper left alone",
			source: "add(&findings, \"tables_added\", len(a), Safe)\n" +
				"\tadd(&findings,\n\t\t\"rls_force_removed\", n, Destructive)\n" +
				"addFinding(&findings, \"data_rows_inserted\", n, Safe)\n",
			call: "add",
			want: []string{"tables_added", "rls_force_removed"},
		},
		{
			name:   "the other helper",
			source: "addFinding(&findings, \"data_rows_inserted\", n, Safe)\nadd(&findings, \"tables_added\", n, Safe)\n",
			call:   "addFinding",
			want:   []string{"data_rows_inserted"},
		},
		{
			name:    "a category that is not a literal",
			source:  "add(&findings, \"tables_added\", n, Safe)\nadd(&findings, categoryForcedRLS, n, Safe)\n",
			call:    "add",
			failure: "1 of 2 add(&findings, ...) calls",
		},
		{
			name:    "no call at all",
			source:  "func classify() []Finding { return nil }\n",
			call:    "add",
			failure: "no add(&findings, ...) call",
		},
		{
			name:    "a category that is not a machine name",
			source:  "add(&findings, \"Tables Added\", n, Safe)\n",
			call:    "add",
			failure: "is not a lowercase machine name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Extract([]byte(test.source), test.call)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("Extract() = %q, %v; want an error containing %q", got, err, test.failure)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Extract() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExtractConstant(t *testing.T) {
	t.Parallel()

	got, err := ExtractConstant([]byte("// FindingCategory is the category.\nconst FindingCategory = \"undecided\"\n"), "FindingCategory")
	if err != nil || !reflect.DeepEqual(got, []string{"undecided"}) {
		t.Fatalf("ExtractConstant() = %q, %v; want undecided", got, err)
	}
	for name, source := range map[string]string{
		"absent":          "const OtherCategory = \"undecided\"\n",
		"not a literal":   "const FindingCategory = prefix + \"undecided\"\n",
		"declared twice":  "const FindingCategory = \"undecided\"\nvar FindingCategory = \"other\"\n",
		"not a lowercase": "const FindingCategory = \"Undecided Objects\"\n",
	} {
		if got, err := ExtractConstant([]byte(source), "FindingCategory"); err == nil {
			t.Errorf("%s: ExtractConstant() = %q, want an error", name, got)
		}
	}
}

func TestExtractQualified(t *testing.T) {
	t.Parallel()

	source := "add(findings, \"feature_changes\"+suffix, 1, Destructive)\n" +
		"add(findings, \"feature_changes:\"+kind, 1, severity)\n" +
		"case schemaext.AccessWidens:\n\treturn \"feature_access_widened\"\n" +
		"strings.HasPrefix(finding.Category, \"feature_changes:\")\n" +
		"add(findings, \"tables_added\", 1, Safe)\n"
	got, err := ExtractQualified([]byte(source))
	if want := []string{"feature_access_widened", "feature_changes"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractQualified() = %q, %v; want %q", got, err, want)
	}
	if got, err := ExtractQualified([]byte("add(findings, \"tables_added\", 1, Safe)\n")); err == nil {
		t.Fatalf("ExtractQualified() of a file with no feature category = %q, want an error", got)
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
	writeFile(t, checkout, "internal/cli/drift/drift.go", "package drift\n")
	writeFile(t, checkout, "internal/datamigrate/datamigrate.go",
		"addFinding(&findings, \"data_rows_inserted\", n, safety.Safe)\n")
	writeFile(t, checkout, "internal/undecidednote/undecidednote.go", "const FindingCategory = \"undecided\"\n")
	writeFile(t, checkout, "migration/safety/features.go", "add(findings, \"feature_changes:\"+kind, 1, severity)\n")
	writeFile(t, checkout, "migration/safety/safety.go",
		"add(&findings, \"tables_added\", n, Safe)\nadd(&findings, \"rls_force_added\", n, Safe)\n")
	first := commitAll(t, checkout, "first")
	writeFile(t, checkout, "migration/safety/safety.go",
		"add(&findings, \"tables_added\", n, Safe)\nadd(&findings, \"rls_force_added\", n, Safe)\n"+
			"add(&findings, \"rls_force_removed\", n, Destructive)\n")
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
		!strings.Contains(err.Error(), "migration/safety/safety.go changed") {
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
