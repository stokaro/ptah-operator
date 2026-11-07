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

// Command ptahdriftcategories keeps support/ptah-drift-categories.json equal to
// the drift finding categories the pinned Ptah build can emit.
//
// The operator refuses a drift report that names a category outside its
// closed vocabulary, so a Ptah that grows a category fails Observe for every
// change in it until the vocabulary follows. The vendored file is what the
// vocabulary is held to by a unit test that always runs. This program is what
// holds the vendored file to Ptah: it reads docs/drift-findings.json, the
// vocabulary Ptah generates from its own classifier and holds to it with a
// test, at the commit support/ptah.json pins, and compares its digest and the
// categories it lists with the file.
//
// A Ptah bump that leaves the vocabulary alone changes nothing here. One that
// changes it fails until the file is regenerated with -write, and the diff
// then shows which categories moved. CI runs the comparison where it already
// has the pinned source, in the job that builds the executor from it.
//
// Usage:
//
//	go run ./hack/ptahdriftcategories -ptah <Ptah checkout> [-write]
//
// The checkout has to contain the pinned commit; its working tree is not read.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

const (
	catalogPath  = "support/ptah.json"
	vendoredPath = "support/ptah-drift-categories.json"
)

// vocabularyPath is where Ptah publishes every category a drift report can
// carry, generated from its classifier.
const vocabularyPath = "docs/drift-findings.json"

var (
	categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	commitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Vendored is the file this program writes and compares.
type Vendored struct {
	Comment    string           `json:"comment"`
	Sources    []VendoredSource `json:"sources"`
	Categories []string         `json:"categories"`
	// Qualified are the categories Ptah writes either bare or followed by
	// ":<kind>", where the kind names the feature owner's object type.
	Qualified []string `json:"qualified"`
}

// VendoredSource is one file's digest at the pinned commit.
type VendoredSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const vendoredComment = "The drift finding categories the Ptah build pinned in support/ptah.json can emit, and the digests of the files that produce them. " +
	"A qualified category is written bare or followed by \":<kind>\". " +
	"Written by `go run ./hack/ptahdriftcategories -ptah <checkout> -write`; CI compares it with the pinned source. " +
	"internal/dataplane's vocabulary has to equal these lists, and the PtahSchema status pattern has to admit them."

func main() {
	if err := run(context.Background(), os.Args[1:], ".", os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ptahdriftcategories:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, root string, stdout io.Writer) error {
	flags := flag.NewFlagSet("ptahdriftcategories", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkout := flags.String("ptah", "", "a Ptah Git checkout that contains the pinned commit")
	write := flags.Bool("write", false, "rewrite the vendored file instead of comparing with it")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *checkout == "" {
		return errors.New("-ptah must name a Ptah Git checkout")
	}
	commit, err := pinnedCommit(root + "/" + catalogPath)
	if err != nil {
		return err
	}
	read := func(path string) ([]byte, error) { return gitShow(ctx, *checkout, commit, path) }
	computed, err := Compute(read)
	if err != nil {
		return fmt.Errorf("read the pinned Ptah %s: %w", commit, err)
	}
	if *write {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(computed); err != nil {
			return err
		}
		if err := os.WriteFile(root+"/"+vendoredPath, encoded.Bytes(), 0o644); err != nil { // #nosec G306 -- a tracked repository file.
			return err
		}
		_, err = fmt.Fprintf(stdout, "wrote %s: %d categories and %d qualified from Ptah %s\n",
			vendoredPath, len(computed.Categories), len(computed.Qualified), commit)
		return err
	}
	vendored, err := LoadVendored(root + "/" + vendoredPath)
	if err != nil {
		return err
	}
	if err := Compare(vendored, computed); err != nil {
		return fmt.Errorf("%s does not describe Ptah %s: %w; rerun with -write and bring the operator's vocabulary along", vendoredPath, commit, err)
	}
	_, err = fmt.Fprintf(stdout, "%s matches Ptah %s: %d categories and %d qualified\n",
		vendoredPath, commit, len(computed.Categories), len(computed.Qualified))
	return err
}

// Compute reads Ptah's vocabulary through read and returns what the vendored
// file should say. A category written only bare is a fixed one; one Ptah
// writes followed by ":<kind>" is qualified, and the operator admits it bare
// as well.
func Compute(read func(path string) ([]byte, error)) (Vendored, error) {
	content, err := read(vocabularyPath)
	if err != nil {
		return Vendored{}, fmt.Errorf("read %s: %w", vocabularyPath, err)
	}
	vocabulary, err := ParseVocabulary(content)
	if err != nil {
		return Vendored{}, fmt.Errorf("%s: %w", vocabularyPath, err)
	}
	digest := sha256.Sum256(content)
	computed := Vendored{
		Comment: vendoredComment,
		Sources: []VendoredSource{{Path: vocabularyPath, SHA256: hex.EncodeToString(digest[:])}},
	}
	for _, entry := range vocabulary.Categories {
		if entry.Qualified {
			computed.Qualified = append(computed.Qualified, entry.Category)
			continue
		}
		computed.Categories = append(computed.Categories, entry.Category)
	}
	slices.Sort(computed.Categories)
	slices.Sort(computed.Qualified)
	return computed, nil
}

// Vocabulary is docs/drift-findings.json as Ptah writes it.
type Vocabulary struct {
	Comment    string          `json:"comment"`
	Categories []VocabularyRow `json:"categories"`
}

// VocabularyRow is one category, the severities a finding of it can carry,
// and the forms it is written in: bare, followed by ":<kind>", or both.
type VocabularyRow struct {
	Category   string   `json:"category"`
	Severities []string `json:"severities"`
	Bare       bool     `json:"bare"`
	Qualified  bool     `json:"qualified"`
}

var severities = []string{"safe", "warning", "destructive"}

// ParseVocabulary reads the vocabulary strictly and refuses one this program
// could read wrong: an unknown field, no category, a category that is not a
// lowercase machine name or comes twice, one with no form or no severity, or
// a severity outside the three a report uses.
func ParseVocabulary(content []byte) (Vocabulary, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var vocabulary Vocabulary
	if err := decoder.Decode(&vocabulary); err != nil {
		return Vocabulary{}, fmt.Errorf("decode: %w", err)
	}
	if len(vocabulary.Categories) == 0 {
		return Vocabulary{}, errors.New("lists no category")
	}
	seen := make(map[string]bool, len(vocabulary.Categories))
	for _, row := range vocabulary.Categories {
		switch {
		case !categoryPattern.MatchString(row.Category):
			return Vocabulary{}, fmt.Errorf("category %q is not a lowercase machine name", row.Category)
		case seen[row.Category]:
			return Vocabulary{}, fmt.Errorf("category %s is listed twice", row.Category)
		case !row.Bare && !row.Qualified:
			return Vocabulary{}, fmt.Errorf("category %s is written neither bare nor qualified", row.Category)
		case len(row.Severities) == 0:
			return Vocabulary{}, fmt.Errorf("category %s carries no severity", row.Category)
		}
		for _, severity := range row.Severities {
			if !slices.Contains(severities, severity) {
				return Vocabulary{}, fmt.Errorf("category %s carries severity %q", row.Category, severity)
			}
		}
		seen[row.Category] = true
	}
	return vocabulary, nil
}

// Compare names every difference between the vendored file and the source.
func Compare(vendored, computed Vendored) error {
	var problems []string
	want := make(map[string]string, len(computed.Sources))
	for _, file := range computed.Sources {
		want[file.Path] = file.SHA256
	}
	have := make(map[string]string, len(vendored.Sources))
	for _, file := range vendored.Sources {
		have[file.Path] = file.SHA256
	}
	for _, file := range computed.Sources {
		switch digest, found := have[file.Path]; {
		case !found:
			problems = append(problems, file.Path+" is not recorded")
		case digest != file.SHA256:
			problems = append(problems, file.Path+" changed")
		}
	}
	for _, file := range vendored.Sources {
		if _, found := want[file.Path]; !found {
			problems = append(problems, file.Path+" is recorded and not read")
		}
	}
	for _, category := range computed.Categories {
		if !slices.Contains(vendored.Categories, category) {
			problems = append(problems, "Ptah emits "+category)
		}
	}
	for _, category := range vendored.Categories {
		if !slices.Contains(computed.Categories, category) {
			problems = append(problems, "Ptah no longer emits "+category)
		}
	}
	for _, category := range computed.Qualified {
		if !slices.Contains(vendored.Qualified, category) {
			problems = append(problems, "Ptah emits "+category+" with a kind")
		}
	}
	for _, category := range vendored.Qualified {
		if !slices.Contains(computed.Qualified, category) {
			problems = append(problems, "Ptah no longer emits "+category+" with a kind")
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// LoadVendored reads the vendored file strictly.
func LoadVendored(path string) (Vendored, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- a repository file this program names.
	if err != nil {
		return Vendored{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var vendored Vendored
	if err := decoder.Decode(&vendored); err != nil {
		return Vendored{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return vendored, nil
}

// pinnedCommit is the commit the lifecycle suite builds its executor from:
// the first verified commit of the edge row, which is what hack/e2e-kind.sh
// and the CI support job read.
func pinnedCommit(path string) (string, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- a repository file this program names.
	if err != nil {
		return "", err
	}
	var catalog struct {
		Releases []struct {
			Operator string `json:"operator"`
			Verified []struct {
				PtahCommit string `json:"ptahCommit"`
			} `json:"verified"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	for _, release := range catalog.Releases {
		if release.Operator == "edge" && len(release.Verified) > 0 {
			commit := release.Verified[0].PtahCommit
			if !commitPattern.MatchString(commit) {
				return "", fmt.Errorf("%s pins %q, which is not an exact commit", path, commit)
			}
			return commit, nil
		}
	}
	return "", fmt.Errorf("%s verifies no Ptah commit for edge", path)
}

func gitShow(ctx context.Context, checkout, commit, path string) ([]byte, error) {
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, "git", "-C", checkout, "show", commit+":"+path) // #nosec G204 -- fixed program, arguments are not a shell.
	command.Stderr = &stderr
	content, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w: %s", commit, path, err, strings.TrimSpace(stderr.String()))
	}
	return content, nil
}
