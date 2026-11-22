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

// Ptah v0.35.0 compares the ADMIN, INHERIT and SET options of a role
// membership a schema declares (stokaro/ptah#6424) and reports one that
// differs under a new category. The document is what the pinned Ptah printed
// for `schema drift` of a YAML schema, which the runner passes to Ptah as it
// is; internal/runner/testdata/role-membership-option says where it is from. A
// vocabulary without the category refuses the report, which would fail
// Observe for every such membership.
func TestDecodeDriftReadsPtahV0350RoleMembershipCategory(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "runner", "testdata", "role-membership-option", "schema-drift-postgres-17.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeDrift(data, 1)
	if err != nil {
		t.Fatalf("DecodeDrift() refused the captured report: %v", err)
	}
	want := []dataplane.DriftFinding{{Category: "role_memberships_modified", Count: 1, Severity: "warning"}}
	if !report.Drift || report.HighestSeverity != "warning" || !slices.Equal(report.Findings, want) {
		t.Fatalf("the captured report reads drift=%v highest=%q findings=%#v, want drift at warning with %#v",
			report.Drift, report.HighestSeverity, report.Findings, want)
	}
	if !dataplane.IsKnownDriftFindingCategory("role_memberships_modified") {
		t.Fatal(`IsKnownDriftFindingCategory("role_memberships_modified") = false for a category the pinned Ptah emits`)
	}
}
