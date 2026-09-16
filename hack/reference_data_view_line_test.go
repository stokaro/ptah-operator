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
	"os"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/schemaview"
)

// The reference-data phase compares one line of `kubectl ptah schema` output
// against a literal, and a shell script cannot ask the renderer what it writes.
// So the renderer is asked here instead, offline, in the contour that runs on
// every pull request.
//
// Without this the script and the view agree until the column width moves, and
// then disagree ninety minutes into a lifecycle, on a cluster, about a space.
func TestReferenceDataPhasePinsTheLineTheViewWrites(t *testing.T) {
	t.Parallel()

	// The same drift the phase makes by hand: one managed row edited outside
	// the operator, nothing added and nothing removed.
	view := schemaview.View{
		Namespace: "namespace",
		Schema:    "schema",
		Observation: &schemaview.ObservationView{
			Drift:           true,
			HighestSeverity: "destructive",
			FindingCount:    1,
			Findings: []schemaview.FindingView{
				{Category: "data_rows_updated", Count: 1, Severity: "destructive"},
			},
			ReferenceData: &schemaview.ReferenceDataView{Inserts: 0, Updates: 1, Deletes: 0},
		},
	}
	var rendered bytes.Buffer
	if err := schemaview.Render(&rendered, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	written := ""
	for _, line := range strings.Split(rendered.String(), "\n") {
		if strings.HasPrefix(line, "Reference data:") {
			written = line
			break
		}
	}
	if written == "" {
		t.Fatalf("the view wrote no reference-data line:\n%s", rendered.String())
	}

	phase, err := os.ReadFile("e2e-reference-data.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(phase), written) {
		t.Fatalf("the reference-data phase does not pin the line the view writes.\n"+
			"view writes: %q\n"+
			"see assert_kubectl_ptah_schema_counts in hack/e2e-reference-data.sh", written)
	}
}
