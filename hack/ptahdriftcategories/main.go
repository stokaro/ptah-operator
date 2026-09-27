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
// holds the vendored file to Ptah: it reads the files that produce the
// categories at the commit support/ptah.json pins, and compares their digests
// and the categories they name with the file.
//
// A Ptah bump that leaves those files alone changes nothing here. One that
// edits them fails until the file is regenerated with -write, and the diff
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

// source is one Ptah file that decides which categories a drift report can
// carry. call names the helper whose first string argument is a category; a
// file with no call is held by its digest alone.
type source struct {
	path string
	call string
}

// sources are the files a category has to pass through. The drift command
// assembles the report's findings from the schema classifier and the
// managed-row summary, so a new producer edits one of the three.
var sources = []source{
	{path: "internal/cli/drift/drift.go"},
	{path: "internal/datamigrate/datamigrate.go", call: "addFinding"},
	{path: "migration/safety/safety.go", call: "add"},
}

var (
	categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	commitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Vendored is the file this program writes and compares.
type Vendored struct {
	Comment    string           `json:"comment"`
	Sources    []VendoredSource `json:"sources"`
	Categories []string         `json:"categories"`
}

// VendoredSource is one file's digest at the pinned commit.
type VendoredSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const vendoredComment = "The drift finding categories the Ptah build pinned in support/ptah.json can emit, and the digests of the files that produce them. " +
	"Written by `go run ./hack/ptahdriftcategories -ptah <checkout> -write`; CI compares it with the pinned source. " +
	"internal/dataplane's vocabulary and the PtahSchema status enum have to equal this list."

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
		_, err = fmt.Fprintf(stdout, "wrote %s: %d categories from Ptah %s\n", vendoredPath, len(computed.Categories), commit)
		return err
	}
	vendored, err := LoadVendored(root + "/" + vendoredPath)
	if err != nil {
		return err
	}
	if err := Compare(vendored, computed); err != nil {
		return fmt.Errorf("%s does not describe Ptah %s: %w; rerun with -write and bring the operator's vocabulary along", vendoredPath, commit, err)
	}
	_, err = fmt.Fprintf(stdout, "%s matches Ptah %s: %d categories\n", vendoredPath, commit, len(computed.Categories))
	return err
}

// Compute reads every source through read and returns what the vendored file
// should say.
func Compute(read func(path string) ([]byte, error)) (Vendored, error) {
	computed := Vendored{Comment: vendoredComment}
	for _, file := range sources {
		content, err := read(file.path)
		if err != nil {
			return Vendored{}, fmt.Errorf("read %s: %w", file.path, err)
		}
		digest := sha256.Sum256(content)
		computed.Sources = append(computed.Sources, VendoredSource{Path: file.path, SHA256: hex.EncodeToString(digest[:])})
		if file.call == "" {
			continue
		}
		categories, err := Extract(content, file.call)
		if err != nil {
			return Vendored{}, fmt.Errorf("%s: %w", file.path, err)
		}
		computed.Categories = append(computed.Categories, categories...)
	}
	slices.Sort(computed.Categories)
	computed.Categories = slices.Compact(computed.Categories)
	return computed, nil
}

// Extract returns the categories named by every call of the helper in a Go
// file. A call whose category is not a string literal is refused rather than
// skipped, and so is a file with no call at all: either means the source no
// longer says what this program reads, and a short list would pass.
func Extract(content []byte, call string) ([]string, error) {
	calls := regexp.MustCompile(`\b`+regexp.QuoteMeta(call)+`\(&findings,`).FindAllIndex(content, -1)
	literals := regexp.MustCompile(`\b`+regexp.QuoteMeta(call)+`\(&findings,\s*"([^"]*)"`).FindAllSubmatch(content, -1)
	if len(calls) == 0 {
		return nil, fmt.Errorf("no %s(&findings, ...) call names a category", call)
	}
	if len(literals) != len(calls) {
		return nil, fmt.Errorf("%d of %d %s(&findings, ...) calls name their category with a string literal", len(literals), len(calls), call)
	}
	categories := make([]string, 0, len(literals))
	for _, literal := range literals {
		category := string(literal[1])
		if !categoryPattern.MatchString(category) {
			return nil, fmt.Errorf("category %q is not a lowercase machine name", category)
		}
		categories = append(categories, category)
	}
	return categories, nil
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
