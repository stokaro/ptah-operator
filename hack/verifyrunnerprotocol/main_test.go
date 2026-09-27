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
	"path/filepath"
	"strings"
	"testing"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// TestEvaluateRefusesAProtocolTheSourceOutgrew walks the record against the
// version and digest a tree has. Each accepted row sits beside refused rows
// that differ from it in one thing.
func TestEvaluateRefusesAProtocolTheSourceOutgrew(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		record   record
		version  int
		digest   string
		wantPart string
	}{
		{
			name:    "the source is the one the record names",
			record:  record{ProtocolVersion: 5, SourceDigest: digestA},
			version: 5, digest: digestA,
		},
		{
			name:    "the source changed and the version did not",
			record:  record{ProtocolVersion: 5, SourceDigest: digestA},
			version: 5, digest: digestB,
			wantPart: "while runner.ProtocolVersion stayed 5",
		},
		{
			name: "the source changed under a declared reason",
			record: record{ProtocolVersion: 5, SourceDigest: digestA, Declared: []declaration{
				{ProtocolVersion: 5, SourceDigest: digestB, Reason: "comments only"},
			}},
			version: 5, digest: digestB,
		},
		{
			name: "the source changed again after a declared reason",
			record: record{ProtocolVersion: 5, SourceDigest: digestA, Declared: []declaration{
				{ProtocolVersion: 5, SourceDigest: digestB, Reason: "comments only"},
			}},
			version: 5, digest: digestC,
			wantPart: "while runner.ProtocolVersion stayed 5",
		},
		{
			name:    "the version moved and the record did not",
			record:  record{ProtocolVersion: 5, SourceDigest: digestA},
			version: 6, digest: digestB,
			wantPart: "is stale",
		},
		{
			name:    "the version moved and the source is the recorded one",
			record:  record{ProtocolVersion: 5, SourceDigest: digestA},
			version: 6, digest: digestA,
			wantPart: "is stale",
		},
		{
			name:    "the version went back",
			record:  record{ProtocolVersion: 6, SourceDigest: digestA},
			version: 5, digest: digestA,
			wantPart: "never goes back",
		},
		{
			name: "a declared reason for another version",
			record: record{ProtocolVersion: 6, SourceDigest: digestA, Declared: []declaration{
				{ProtocolVersion: 5, SourceDigest: digestB, Reason: "comments only"},
			}},
			version: 6, digest: digestB,
			wantPart: "excuses nothing under another version",
		},
		{
			name: "a stale declaration beside a matching record",
			record: record{ProtocolVersion: 6, SourceDigest: digestA, Declared: []declaration{
				{ProtocolVersion: 5, SourceDigest: digestB, Reason: "comments only"},
			}},
			version: 6, digest: digestA,
			wantPart: "excuses nothing under another version",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := evaluate(row.record, row.version, row.digest)
			if row.wantPart == "" {
				if err != nil {
					t.Fatalf("evaluate() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.wantPart) {
				t.Fatalf("evaluate() = %v, want a refusal naming %q", err, row.wantPart)
			}
		})
	}
}

// TestDecodeRecordRefusesAMalformedRecord keeps the record to one shape.
func TestDecodeRecordRefusesAMalformedRecord(t *testing.T) {
	t.Parallel()

	valid := `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA + `",` +
		`"declared":[{"protocolVersion":5,"sourceDigest":"` + digestB + `","reason":"comments only"}]}`
	if _, err := decodeRecord([]byte(valid)); err != nil {
		t.Fatalf("decodeRecord(valid) = %v", err)
	}
	for name, contents := range map[string]string{
		"another schema version": `{"schemaVersion":2,"protocolVersion":5,"sourceDigest":"` + digestA + `"}`,
		"an unknown key":         `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA + `","note":"x"}`,
		"no protocol version":    `{"schemaVersion":1,"sourceDigest":"` + digestA + `"}`,
		"a digest that is not":   `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"sha256:abc"}`,
		"a declaration without a reason": `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA +
			`","declared":[{"protocolVersion":5,"sourceDigest":"` + digestB + `","reason":" "}]}`,
		"a declaration of the recorded digest": `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA +
			`","declared":[{"protocolVersion":5,"sourceDigest":"` + digestA + `","reason":"x"}]}`,
		"a digest declared twice": `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA +
			`","declared":[{"protocolVersion":5,"sourceDigest":"` + digestB + `","reason":"x"},` +
			`{"protocolVersion":5,"sourceDigest":"` + digestB + `","reason":"y"}]}`,
		"two documents": `{"schemaVersion":1,"protocolVersion":5,"sourceDigest":"` + digestA + `"}{}`,
	} {
		if _, err := decodeRecord([]byte(contents)); err == nil {
			t.Errorf("decodeRecord(%s) accepted it", name)
		}
	}
}

// TestSourceDigestCoversWhatTheRunnerIsBuiltFrom changes one input at a time.
// A file of an in-module package and the go.sum line of a module the closure
// imports move the digest; a test file, a file of a package outside the
// closure, and the go.sum line of a module outside it do not.
func TestSourceDigestCoversWhatTheRunnerIsBuiltFrom(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(path, contents string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/runner/runner.go", "package runner\n")
	write("internal/runner/runner_test.go", "package runner\n")
	write("internal/runner/limits.json", "{}\n")
	write("internal/other/other.go", "package other\n")
	goSum := "example.com/lib v1.0.0 h1:one=\nexample.com/lib v1.0.0/go.mod h1:two=\nexample.com/unused v1.0.0 h1:three=\n"
	packages := []listedPackage{
		{ImportPath: "fmt", Standard: true},
		{
			ImportPath: "example.test/internal/runner", Dir: filepath.Join(root, "internal/runner"),
			GoFiles: []string{"runner.go"}, EmbedFiles: []string{"limits.json"},
			Module: &listedModule{Path: "example.test", Main: true},
		},
		{ImportPath: "example.com/lib", Module: &listedModule{Path: "example.com/lib", Version: "v1.0.0"}},
	}
	digest := func() string {
		t.Helper()
		value, err := sourceDigest(root, packages, []byte(goSum))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := digest()

	for name, change := range map[string]func(){
		"a test file":                   func() { write("internal/runner/runner_test.go", "package runner // edited\n") },
		"a package outside the closure": func() { write("internal/other/other.go", "package other // edited\n") },
		"a module outside the closure":  func() { goSum = strings.Replace(goSum, "h1:three=", "h1:edited=", 1) },
	} {
		change()
		if got := digest(); got != base {
			t.Fatalf("%s moved the digest", name)
		}
	}
	for name, change := range map[string]func(){
		"a file of the closure":      func() { write("internal/runner/runner.go", "package runner // edited\n") },
		"a file the closure embeds":  func() { write("internal/runner/limits.json", "{\"edited\":true}\n") },
		"a module the closure uses":  func() { goSum = strings.Replace(goSum, "h1:one=", "h1:edited=", 1) },
		"a module's go.mod checksum": func() { goSum = strings.Replace(goSum, "h1:two=", "h1:edited=", 1) },
	} {
		before := digest()
		change()
		if digest() == before {
			t.Fatalf("%s left the digest where it was", name)
		}
	}

	if _, err := sourceDigest(root, packages, []byte("example.com/unused v1.0.0 h1:three=\n")); err == nil {
		t.Fatal("a closure module with no go.sum line was digested")
	}
	replaced := append(packages[:2:2], listedPackage{ImportPath: "example.com/lib", Module: &listedModule{
		Path: "example.com/lib", Version: "v1.0.0", Replace: &listedModule{Path: "../lib"},
	}})
	if _, err := sourceDigest(root, replaced, []byte(goSum)); err == nil {
		t.Fatal("a module replaced by a directory was digested")
	}
}

// TestTheCommittedRecordMatchesTheRunner is the check make verify-source runs,
// against this tree.
func TestTheCommittedRecordMatchesTheRunner(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := run(context.Background(), filepath.Join("..", ".."), false, &stdout); err != nil {
		t.Fatal(err)
	}
}
