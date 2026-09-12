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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures name commits of their own. Reusing the catalogue's real pin
// would read as a test of production data, and a fixture that happens to match
// it would keep passing after the catalogue moved on.
const (
	testCommit      = "abcdef0123456789abcdef0123456789abcdef01"
	testOtherCommit = "1111111111111111111111111111111111111111"
)

func testToday(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.DateOnly, "2026-09-12")
	if err != nil {
		t.Fatalf("parse the fixed validation date: %v", err)
	}
	return parsed
}

// validCatalog is the shape every case below starts from, so a failure names
// the one field the case changed.
func validCatalog() catalog {
	return catalog{
		SchemaVersion: schemaVersion,
		LastVerified:  "2026-09-01",
		Axis:          "operator version to the Ptah build it executes",
		Releases: []release{
			{
				Operator:      edgeVersion,
				Stage:         "development",
				Documentation: documentation{Published: true, Source: "master"},
				Declared:      declared{Range: nil, Statement: "no range is claimed"},
				Verified: []verified{
					{
						PtahCommit:   testCommit,
						PtahDescribe: "v0.3.0-201-gabcdef012",
						Evidence:     "kubernetes-e2e",
						Scope:        "the full lifecycle",
					},
				},
			},
		},
		Evidence: map[string]evidence{
			"kubernetes-e2e": {What: "the lifecycle suite", Pin: "read from this file"},
		},
	}
}

func TestValidateAcceptsTheCatalogueShape(t *testing.T) {
	t.Parallel()

	if err := validate(validCatalog(), testToday(t)); err != nil {
		t.Fatalf("the valid catalogue was refused: %v", err)
	}
}

// TestValidateRefusesTheShapesThatBlurAClaim drives one rule per row. Every
// case starts from the accepted catalogue above, so a row that stops failing is
// a rule that stopped being in effect rather than a fixture that drifted.
func TestValidateRefusesTheShapesThatBlurAClaim(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*catalog)
		wantErr string
	}{
		{
			name:    "a validation date in the future",
			mutate:  func(c *catalog) { c.LastVerified = "2099-01-01" },
			wantErr: "is in the future",
		},
		{
			name:    "a validation date that is not a date",
			mutate:  func(c *catalog) { c.LastVerified = "last Tuesday" },
			wantErr: "is not a YYYY-MM-DD date",
		},
		{
			name:    "no stated axis",
			mutate:  func(c *catalog) { c.Axis = "  " },
			wantErr: "axis is empty",
		},
		{
			name:    "no operator version at all",
			mutate:  func(c *catalog) { c.Releases = nil },
			wantErr: "lists no operator version",
		},
		{
			name: "one operator version listed twice",
			mutate: func(c *catalog) {
				c.Releases = append(c.Releases, c.Releases[0])
			},
			wantErr: "appears twice",
		},
		{
			name: "no development row",
			mutate: func(c *catalog) {
				c.Releases[0].Operator = "v0.1.0"
				c.Releases[0].Stage = "released"
			},
			wantErr: "names edge 0 times",
		},
		{
			name:    "a version that is neither edge nor a tag",
			mutate:  func(c *catalog) { c.Releases[0].Operator = "nightly" },
			wantErr: "is neither edge nor vMAJOR.MINOR.PATCH",
		},
		{
			name:    "documentation published with no source",
			mutate:  func(c *catalog) { c.Releases[0].Documentation.Source = "" },
			wantErr: "names no source revision",
		},
		{
			name: "documentation not published and a source anyway",
			mutate: func(c *catalog) {
				c.Releases[0].Documentation.Published = false
			},
			wantErr: "does not publish it",
		},
		{
			name: "no declared range and no reason",
			mutate: func(c *catalog) {
				c.Releases[0].Declared.Statement = ""
			},
			wantErr: "does not say why",
		},
		{
			name: "an empty declared range instead of an absent one",
			mutate: func(c *catalog) {
				empty := "   "
				c.Releases[0].Declared.Range = &empty
			},
			wantErr: "declares an empty range",
		},
		{
			name: "nothing verified and nothing said about it",
			mutate: func(c *catalog) {
				c.Releases[0].Verified = nil
			},
			wantErr: "does not say which check is missing",
		},
		{
			name: "verified builds and an unverified reason at once",
			mutate: func(c *catalog) {
				c.Releases[0].UnverifiedReason = "nothing has run"
			},
			wantErr: "one of the two is stale",
		},
		{
			name: "one commit verified twice",
			mutate: func(c *catalog) {
				c.Releases[0].Verified = append(c.Releases[0].Verified, c.Releases[0].Verified[0])
			},
			wantErr: "twice",
		},
		{
			name: "a commit that is not a commit",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].PtahCommit = "abcdef0"
			},
			wantErr: "not an exact 40-character lowercase Git commit",
		},
		{
			name: "a verified release that is not a release",
			mutate: func(c *catalog) {
				name := "0.3"
				c.Releases[0].Verified[0].PtahRelease = &name
			},
			wantErr: "not vMAJOR.MINOR.PATCH",
		},
		{
			name: "an identity that abbreviates a different commit",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].PtahDescribe = "v0.3.0-201-g1111111"
			},
			wantErr: "names a different commit",
		},
		{
			name: "an identity that is neither a describe nor an abbreviation",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].PtahDescribe = "the one we tested"
			},
			wantErr: "neither a describe of it nor an abbreviation of it",
		},
		{
			name: "an identity longer than the chart binds",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].PtahDescribe = strings.Repeat("v", ptahVersionLimit+1)
			},
			wantErr: "the chart binds at most",
		},
		{
			name: "a measurement that does not say what ran",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].Scope = ""
			},
			wantErr: "does not say what ran",
		},
		{
			name: "evidence the catalogue does not describe",
			mutate: func(c *catalog) {
				c.Releases[0].Verified[0].Evidence = "somebody tried it"
			},
			wantErr: "which the catalogue does not describe",
		},
		{
			name: "evidence that backs nothing",
			mutate: func(c *catalog) {
				c.Evidence["retired-suite"] = evidence{What: "a suite", Pin: "a pin"}
			},
			wantErr: "backs no verified row",
		},
		{
			name: "evidence that does not say what runs",
			mutate: func(c *catalog) {
				c.Evidence["kubernetes-e2e"] = evidence{Pin: "a pin"}
			},
			wantErr: "does not say what runs",
		},
		{
			name: "an empty limitation",
			mutate: func(c *catalog) {
				c.Releases[0].Limitations = []string{" "}
			},
			wantErr: "limitation 0 is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			loaded := validCatalog()
			test.mutate(&loaded)

			err := validate(loaded, testToday(t))
			if err == nil {
				t.Fatalf("the catalogue was accepted; wanted a refusal naming %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("refused for the wrong reason:\n wanted: %s\n got:    %v", test.wantErr, err)
			}
		})
	}
}

// TestValidateAcceptsAnHonestAbsence is the control for the row above it: a
// version nobody has measured is a legitimate entry, and what the rule demands
// is that the missing check be named rather than that a measurement exist.
func TestValidateAcceptsAnHonestAbsence(t *testing.T) {
	t.Parallel()

	loaded := validCatalog()
	loaded.Releases[0].Verified = nil
	loaded.Releases[0].UnverifiedReason = "the lifecycle suite has not run against any Ptah build for this version"
	// The evidence goes with the measurement it backed. A description left
	// behind is the stale-check shape the rule beside this one refuses, and
	// keeping it here would make this case pass for the wrong reason.
	loaded.Evidence = nil

	if err := validate(loaded, testToday(t)); err != nil {
		t.Fatalf("an absence that says what is missing was refused: %v", err)
	}
}

// TestValidateAcceptsAnAbbreviatedIdentity keeps the describe rule from
// demanding a shape a shallow checkout cannot produce: CI checks the pinned
// commit out at depth one, where `git describe --tags --always` answers with
// the abbreviation and no tag name.
func TestValidateAcceptsAnAbbreviatedIdentity(t *testing.T) {
	t.Parallel()

	loaded := validCatalog()
	loaded.Releases[0].Verified[0].PtahDescribe = "abcdef0"

	if err := validate(loaded, testToday(t)); err != nil {
		t.Fatalf("an abbreviation of the verified commit was refused: %v", err)
	}
}

func TestCheckSinglePinRefusesASecondCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repeated := filepath.Join(dir, "repeats.sh")
	if err := os.WriteFile(repeated, []byte("E2E_PTAH_REVISION="+testCommit+"\n"), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	err := checkSinglePin(validCatalog(), []string{repeated})
	if err == nil {
		t.Fatal("a file repeating the pin was accepted")
	}
	if !strings.Contains(err.Error(), "a second time") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestCheckSinglePinAcceptsAnotherCommit is the control. Without it the rule
// above would pass over a file holding any forty hex characters at all, and the
// check would be about hex rather than about this pin.
func TestCheckSinglePinAcceptsAnotherCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	unrelated := filepath.Join(dir, "unrelated.sh")
	if err := os.WriteFile(unrelated, []byte("CONTROLLER_REVISION="+testOtherCommit+"\n"), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	if err := checkSinglePin(validCatalog(), []string{unrelated}); err != nil {
		t.Fatalf("an unrelated commit was refused: %v", err)
	}
}

// TestLoadRefusesAnUnknownField keeps a field nobody reads from looking like a
// field somebody honored. A catalogue carrying `supported: true` beside the
// declared and verified halves would publish a claim this program never saw.
func TestLoadRefusesAnUnknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ptah.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"supported":true}`), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	_, err := load(path)
	if err == nil {
		t.Fatal("a catalogue carrying an unknown field was accepted")
	}
	if !strings.Contains(err.Error(), "supported") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestEdgeCommitIsTheDeclaration pins the value the pipeline reads. The
// lifecycle job checks this commit out and builds the executor from it, so a
// change here changes what the published matrix is a claim about.
func TestEdgeCommitIsTheDeclaration(t *testing.T) {
	t.Parallel()

	commit, err := edgeCommit(validCatalog())
	if err != nil {
		t.Fatalf("the development row has no commit: %v", err)
	}
	if commit != testCommit {
		t.Fatalf("read %s, wanted %s", commit, testCommit)
	}
}
