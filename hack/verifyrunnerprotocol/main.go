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

// Command verifyrunnerprotocol holds runner.ProtocolVersion to the runner's
// source.
//
// A plan and its approval bind the runner protocol version, not the runner
// image, so the version is the only thing that says the runner still enforces
// what it enforced when a person approved. The rule at the constant is that
// every change to the runner's side of the contract bumps it. This program
// makes forgetting that a refusal rather than a review finding: it digests the
// source the runner is built from and compares it with the record in
// support/runner-protocol.json. A digest that moved while the version did not
// is refused, unless the record declares why the change left the contract as
// it was.
//
// The digest covers the non-test Go files, and the files they embed, of every
// package in this module that ./cmd/ptah-runner imports, directly or not, for
// the platform the runner ships on; and the go.sum lines of every other module
// those packages import. It does not cover the Go toolchain.
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
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	recordPath    = "support/runner-protocol.json"
	runnerPackage = "./cmd/ptah-runner"
	schemaVersion = 1
	// reasonLimit bounds a declared reason: one paragraph, not a design
	// document.
	reasonLimit = 1024
)

// The platform the runner ships on. Build constraints select files by it, so
// the digest is taken for it wherever the check runs.
var runnerPlatform = []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// record is support/runner-protocol.json.
type record struct {
	SchemaVersion int `json:"schemaVersion"`
	// ProtocolVersion is runner.ProtocolVersion when the record was written.
	ProtocolVersion int `json:"protocolVersion"`
	// SourceDigest is the runner's source digest when the protocol version
	// was last recorded.
	SourceDigest string `json:"sourceDigest"`
	// Declared are the source changes accepted under ProtocolVersion without
	// a bump, each with the reason it left the contract as it was.
	Declared []declaration `json:"declared,omitempty"`
}

// declaration excuses one source digest under one protocol version.
type declaration struct {
	ProtocolVersion int    `json:"protocolVersion"`
	SourceDigest    string `json:"sourceDigest"`
	Reason          string `json:"reason"`
}

func main() {
	printDigest := flag.Bool("print", false, "print the protocol version and source digest this tree has, and check nothing")
	flag.Parse()
	if err := run(context.Background(), ".", *printDigest, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "verifyrunnerprotocol: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, root string, printDigest bool, stdout io.Writer) error {
	packages, err := listRunnerClosure(ctx, root)
	if err != nil {
		return err
	}
	goSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		return fmt.Errorf("read go.sum: %w", err)
	}
	digest, err := sourceDigest(root, packages, goSum)
	if err != nil {
		return err
	}
	if printDigest {
		current := record{SchemaVersion: schemaVersion, ProtocolVersion: runner.ProtocolVersion, SourceDigest: digest}
		encoded, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", encoded)
		return err
	}
	contents, err := os.ReadFile(filepath.Join(root, recordPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", recordPath, err)
	}
	committed, err := decodeRecord(contents)
	if err != nil {
		return err
	}
	if err := evaluate(committed, runner.ProtocolVersion, digest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "runner protocol %d matches its source (%s)\n", runner.ProtocolVersion, digest)
	return err
}

// decodeRecord reads the record strictly: a key this program does not know is
// a record written for another shape, and is refused rather than ignored.
func decodeRecord(contents []byte) (record, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var parsed record
	if err := decoder.Decode(&parsed); err != nil {
		return record{}, fmt.Errorf("decode %s: %w", recordPath, err)
	}
	if decoder.More() {
		return record{}, fmt.Errorf("%s holds more than one document", recordPath)
	}
	if parsed.SchemaVersion != schemaVersion {
		return record{}, fmt.Errorf("%s has schemaVersion %d; this program reads %d", recordPath, parsed.SchemaVersion, schemaVersion)
	}
	if parsed.ProtocolVersion < 1 || !digestPattern.MatchString(parsed.SourceDigest) {
		return record{}, fmt.Errorf("%s must name a positive protocol version and a sha256 source digest", recordPath)
	}
	seen := map[string]bool{}
	for index, declared := range parsed.Declared {
		reason := strings.TrimSpace(declared.Reason)
		if declared.ProtocolVersion < 1 || !digestPattern.MatchString(declared.SourceDigest) ||
			reason == "" || reason != declared.Reason || len(reason) > reasonLimit {
			return record{}, fmt.Errorf("%s declaration %d needs a positive protocol version, a sha256 source digest, "+
				"and a reason of at most %d bytes without surrounding space", recordPath, index, reasonLimit)
		}
		if seen[declared.SourceDigest] || declared.SourceDigest == parsed.SourceDigest {
			return record{}, fmt.Errorf("%s declares source digest %s more than once", recordPath, declared.SourceDigest)
		}
		seen[declared.SourceDigest] = true
	}
	return parsed, nil
}

// evaluate compares the record with the protocol version and the source
// digest this tree has.
func evaluate(committed record, protocolVersion int, digest string) error {
	// A declaration speaks for the version it names. Once the version moves,
	// the declarations made under the old one excuse nothing, and keeping
	// them would read as if they did.
	for _, declared := range committed.Declared {
		if declared.ProtocolVersion != committed.ProtocolVersion {
			return fmt.Errorf("%s declares source %s under runner protocol %d, but the record is for protocol %d; "+
				"a declaration excuses nothing under another version, so remove it",
				recordPath, declared.SourceDigest, declared.ProtocolVersion, committed.ProtocolVersion)
		}
	}
	switch {
	case protocolVersion < committed.ProtocolVersion:
		return fmt.Errorf("runner.ProtocolVersion is %d, below the %d %s records; a protocol version never goes back",
			protocolVersion, committed.ProtocolVersion, recordPath)
	case protocolVersion != committed.ProtocolVersion:
		return fmt.Errorf("%s is stale: runner.ProtocolVersion is %d and the record names %d; record protocol %d "+
			"with source digest %s (go run ./hack/verifyrunnerprotocol -print), and drop the declarations made under %d",
			recordPath, protocolVersion, committed.ProtocolVersion, protocolVersion, digest, committed.ProtocolVersion)
	case digest == committed.SourceDigest:
		return nil
	}
	for _, declared := range committed.Declared {
		if declared.SourceDigest == digest {
			return nil
		}
	}
	return fmt.Errorf("the runner's source changed (%s records %s, the tree has %s) while runner.ProtocolVersion "+
		"stayed %d. If what the runner accepts, enforces or returns changed, bump runner.ProtocolVersion and record the "+
		"new version; if it did not, declare this digest under protocol %d in %s with the reason",
		recordPath, committed.SourceDigest, digest, protocolVersion, protocolVersion, recordPath)
}

// listedPackage is what `go list -json` says about one package.
type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	GoFiles    []string
	CgoFiles   []string
	EmbedFiles []string
	Module     *listedModule
}

type listedModule struct {
	Path    string
	Version string
	Main    bool
	Replace *listedModule
}

// listRunnerClosure lists every package the runner is built from, for the
// platform it ships on.
func listRunnerClosure(ctx context.Context, root string) ([]listedPackage, error) {
	command := exec.CommandContext(ctx, "go", "list", "-deps", "-json", runnerPackage)
	command.Dir = root
	command.Env = append(os.Environ(), runnerPlatform...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps %s: %w: %s", runnerPackage, err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var packages []listedPackage
	for {
		var listed listedPackage
		err := decoder.Decode(&listed)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		packages = append(packages, listed)
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("go list named no package for %s", runnerPackage)
	}
	return packages, nil
}

// sourceDigest digests the files of the in-module packages and the go.sum
// lines of the external modules. Standard-library packages belong to the
// toolchain and are left out.
func sourceDigest(root string, packages []listedPackage, goSum []byte) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var entries []string
	modules := map[string]bool{}
	inModule := 0
	for _, listed := range packages {
		switch {
		case listed.Standard:
			continue
		case listed.Module == nil:
			return "", fmt.Errorf("package %s belongs to no module", listed.ImportPath)
		case listed.Module.Main:
			inModule++
			files := slices.Concat(listed.GoFiles, listed.CgoFiles, listed.EmbedFiles)
			for _, name := range files {
				path := filepath.Join(listed.Dir, name)
				relative, err := filepath.Rel(absoluteRoot, path)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return "", fmt.Errorf("file %s of %s lies outside the module", path, listed.ImportPath)
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					return "", fmt.Errorf("read %s: %w", relative, err)
				}
				sum := sha256.Sum256(contents)
				entries = append(entries, "file "+filepath.ToSlash(relative)+" "+hex.EncodeToString(sum[:]))
			}
		default:
			module := listed.Module
			if module.Replace != nil {
				module = module.Replace
			}
			if module.Version == "" {
				return "", fmt.Errorf("module %s is replaced by a directory, which go.sum does not cover", listed.Module.Path)
			}
			modules[module.Path+" "+module.Version] = true
		}
	}
	if inModule == 0 {
		return "", errors.New("the runner closure holds no package of this module")
	}
	lines := strings.Split(string(goSum), "\n")
	for module := range modules {
		found := 0
		for _, line := range lines {
			if strings.HasPrefix(line, module+" ") || strings.HasPrefix(line, module+"/go.mod ") {
				entries = append(entries, "sum "+line)
				found++
			}
		}
		if found == 0 {
			return "", fmt.Errorf("go.sum holds no line for %s", module)
		}
	}
	slices.Sort(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
