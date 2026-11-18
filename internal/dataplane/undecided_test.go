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

package dataplane_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// undecidedDriftDocument is what the pinned Ptah printed for `schema drift`
// under a URL that pins search_path in a PostgreSQL 17 database with a second
// schema, for a declaration that names a publication: no drift, failed, and the
// publication undecided. The runner harness row differs-publication-scoped
// stores it.
func undecidedDriftDocument(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "differing", "drift-undecided-publication-postgres.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// editedDocument decodes document, applies edit and encodes it again, keeping
// numbers as they were written.
func editedDocument(t *testing.T, document []byte, edit func(map[string]any)) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Ptah fails a drift check it could not complete. A report that found no
// difference and could not decide some declared objects says failed without
// drift, and its only findings are the undecided ones; DecodeDrift reads it.
// Every other report that fails without drift is refused as before: one with no
// finding, one with another finding beside the undecided one, and one whose
// undecided finding carries a severity outside the vocabulary.
func TestDecodeDriftReadsAFailureOfUndecidedObjectsAlone(t *testing.T) {
	t.Parallel()

	captured := undecidedDriftDocument(t)
	report, err := dataplane.DecodeDrift(captured, 1)
	if err != nil {
		t.Fatalf("DecodeDrift() refused the captured report: %v", err)
	}
	if report.Drift || !report.Failed || !dataplane.UndecidedOnly(report.Findings) {
		t.Fatalf("the captured report reads drift=%v failed=%v findings=%#v, want undecided objects alone",
			report.Drift, report.Failed, report.Findings)
	}
	// At the threshold destructive Ptah passes the same check, with exit 0.
	passed := editedDocument(t, captured, func(document map[string]any) {
		document["failed"] = false
		document["failure_threshold"] = "destructive"
	})
	if _, err := dataplane.DecodeDrift(passed, 0); err != nil {
		t.Fatalf("DecodeDrift() refused the report at the threshold destructive: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{
			name: "no finding",
			edit: func(document map[string]any) { delete(document, "findings") },
		},
		{
			name: "another finding beside the undecided one",
			edit: func(document map[string]any) {
				document["findings"] = []any{
					map[string]any{"category": "columns_added", "count": 1, "severity": "warning"},
					map[string]any{"category": "undecided", "count": 1, "severity": "warning"},
				}
			},
		},
		{
			name: "an undecided finding of an unknown severity",
			edit: func(document map[string]any) {
				document["findings"] = []any{map[string]any{"category": "undecided", "count": 1, "severity": "critical"}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodeDrift(editedDocument(t, captured, test.edit), 1); err == nil {
				t.Fatal("DecodeDrift() accepted the report")
			}
		})
	}
}

// A plan report lists what its comparison could not decide. Each entry has to
// say what it is, since the runner names it.
func TestDecodeSchemaPlanReadsWhatItCouldNotDecide(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "differing", "plan-undecided-publication-postgres.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeSchemaPlan(data)
	if err != nil {
		t.Fatalf("DecodeSchemaPlan() refused the captured report: %v", err)
	}
	if report.Outcome != dataplane.SchemaPlanOutcomeNoChanges || report.Undecided.Count() != 1 {
		t.Fatalf("the captured report reads %s with %d undecided objects, want no-changes with 1", report.Outcome, report.Undecided.Count())
	}
	feature := report.Undecided.Features[0]
	if feature.Label != "PostgreSQL publication" || feature.Subject.Name.Source != "differs_notes_pub" || feature.Reason == "" {
		t.Fatalf("the undecided object reads %#v", feature)
	}

	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{
			name: "a feature object without its reason",
			edit: func(document map[string]any) {
				document["undecided"] = map[string]any{"features": []any{map[string]any{"kind": "ptah.run/pgpublication/publication"}}}
			},
		},
		{
			name: "a common object of no kind",
			edit: func(document map[string]any) {
				document["undecided"] = map[string]any{"common": []any{map[string]any{"name": "v"}}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := dataplane.DecodeSchemaPlan(editedDocument(t, data, test.edit)); err == nil {
				t.Fatal("DecodeSchemaPlan() accepted the report")
			}
		})
	}
}
