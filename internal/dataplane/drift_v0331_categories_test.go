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
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Ptah v0.33.1 compares a MySQL table's declared default character set and
// collation (stokaro/ptah#6291) and reports what it finds under two new
// categories. The documents are what the pinned Ptah printed for `schema
// drift` in the runner harness rows differs-table-collation-pinned-mysql and
// differs-table-charset-mysql. A vocabulary without the categories refuses
// both reports, which would fail Observe for every such table.
func TestDecodeDriftReadsPtahV0331CollationAndCharsetCategories(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		file     string
		findings []dataplane.DriftFinding
		highest  string
	}{
		{
			file: "drift-table-collations-changed-mysql.json",
			findings: []dataplane.DriftFinding{
				{Category: "table_collations_changed", Count: 1, Severity: "warning"},
			},
			highest: "warning",
		},
		{
			file: "drift-column-charsets-converted-mysql.json",
			findings: []dataplane.DriftFinding{
				{Category: "column_charsets_converted", Count: 1, Severity: "destructive"},
				{Category: "columns_modified", Count: 1, Severity: "warning"},
				{Category: "table_collations_changed", Count: 1, Severity: "warning"},
			},
			highest: "destructive",
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("testdata", "differing", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeDrift(data, 1)
			if err != nil {
				t.Fatalf("DecodeDrift() refused the captured report: %v", err)
			}
			if !report.Drift || report.HighestSeverity != tc.highest {
				t.Fatalf("the captured report reads drift=%v highest=%q, want drift at %q", report.Drift, report.HighestSeverity, tc.highest)
			}
			if !slices.Equal(report.Findings, tc.findings) {
				t.Fatalf("the captured report's findings are %#v, want %#v", report.Findings, tc.findings)
			}
			for _, finding := range report.Findings {
				if !dataplane.IsKnownDriftFindingCategory(finding.Category) {
					t.Fatalf("IsKnownDriftFindingCategory(%q) = false for a category the pinned Ptah emits", finding.Category)
				}
			}
		})
	}
}
