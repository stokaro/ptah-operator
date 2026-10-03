package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

type frozenFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type qualificationFreeze struct {
	SourceFiles      []frozenFile `json:"sourceFiles"`
	FunctionalMatrix frozenFile   `json:"functionalMatrix"`
}

var frozenInputs = []string{
	"support/qualification/0.2.0.md",
	"support/qualification/0.2.0-coverage.md",
	"support/qualification/0.2.0-monitoring.yaml",
	"support/acceptance/0.2.0.json",
	"support/kubernetes.json",
	"support/e2e-suites.json",
	"support/ptah.json",
	"docs/site/src/content/docs/support/databases.md",
}

// Keep the declared input inventory independent of the freeze: deleting a row
// must not turn an unchecked requirement into a passing hash verification.
func verifyQualificationFreeze(files fs.FS, freeze qualificationFreeze) error {
	if len(freeze.SourceFiles) != len(frozenInputs) {
		return fmt.Errorf("freeze has %d source files, require %d", len(freeze.SourceFiles), len(frozenInputs))
	}
	seen := map[string]bool{}
	for _, entry := range freeze.SourceFiles {
		if !slices.Contains(frozenInputs, entry.Path) || seen[entry.Path] {
			return fmt.Errorf("unexpected or repeated frozen input %q", entry.Path)
		}
		seen[entry.Path] = true
	}
	if freeze.FunctionalMatrix.Path != "support/qualification/0.2.0-functional-matrix.md" {
		return fmt.Errorf("freeze must include the functional matrix")
	}
	for _, entry := range append(slices.Clone(freeze.SourceFiles), freeze.FunctionalMatrix) {
		content, err := fs.ReadFile(files, entry.Path)
		if err != nil {
			return err
		}
		if actual := fmt.Sprintf("%x", sha256.Sum256(content)); actual != entry.SHA256 {
			return fmt.Errorf("%s differs from its qualification freeze: got %s, want %s; preserve the prior freeze and record a new revision before qualification", entry.Path, actual, entry.SHA256)
		}
	}
	return nil
}

func TestQualificationFreezeMatchesCurrentInputs(t *testing.T) {
	files := os.DirFS(repositoryRoot)
	raw, err := fs.ReadFile(files, "support/qualification/0.2.0-freeze.json")
	if err != nil {
		t.Fatal(err)
	}
	var freeze qualificationFreeze
	if err := json.Unmarshal(raw, &freeze); err != nil {
		t.Fatal(err)
	}
	if err := verifyQualificationFreeze(files, freeze); err != nil {
		t.Fatal(err)
	}
	table, err := buildCoverage(repositoryRoot, "edge")
	if err != nil {
		t.Fatal(err)
	}
	matrix, err := fs.ReadFile(files, freeze.FunctionalMatrix.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(matrix) != table.markdown() {
		t.Fatal("frozen functional matrix differs from current coverage; regenerate it and revise the freeze before qualification")
	}
}

func TestQualificationFreezeRejectsChangedOrMissingEvidence(t *testing.T) {
	fixture := func() (fstest.MapFS, qualificationFreeze) {
		files := fstest.MapFS{}
		freeze := qualificationFreeze{}
		for _, path := range append(slices.Clone(frozenInputs), "support/qualification/0.2.0-functional-matrix.md") {
			files[path] = &fstest.MapFile{Data: []byte(path)}
			entry := frozenFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(path)))}
			if strings.HasSuffix(path, "-matrix.md") {
				freeze.FunctionalMatrix = entry
			} else {
				freeze.SourceFiles = append(freeze.SourceFiles, entry)
			}
		}
		return files, freeze
	}
	files, freeze := fixture()
	if err := verifyQualificationFreeze(files, freeze); err != nil {
		t.Fatal(err)
	}
	for _, path := range append(slices.Clone(frozenInputs), freeze.FunctionalMatrix.Path) {
		t.Run(path, func(t *testing.T) {
			files, freeze := fixture()
			files[path].Data = []byte("changed requirement")
			if err := verifyQualificationFreeze(files, freeze); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("changed input accepted or misidentified: %v", err)
			}
			delete(files, path)
			if err := verifyQualificationFreeze(files, freeze); err == nil {
				t.Fatal("missing input accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*qualificationFreeze){
		"empty inventory": func(f *qualificationFreeze) { f.SourceFiles = nil },
		"omitted row":     func(f *qualificationFreeze) { f.SourceFiles = f.SourceFiles[1:] },
		"duplicate row":   func(f *qualificationFreeze) { f.SourceFiles[0] = f.SourceFiles[1] },
		"unexpected row":  func(f *qualificationFreeze) { f.SourceFiles[0].Path = "other.md" },
		"missing matrix":  func(f *qualificationFreeze) { f.FunctionalMatrix = frozenFile{} },
	} {
		t.Run(name, func(t *testing.T) {
			files, freeze := fixture()
			mutate(&freeze)
			if err := verifyQualificationFreeze(files, freeze); err == nil {
				t.Fatal("incomplete freeze accepted")
			}
		})
	}
}
