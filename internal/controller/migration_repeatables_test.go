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

package controller

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Since Ptah v0.30.0 a history lists the repeatable migrations of the
// directory and of the database's record. This operator approves and runs a
// versioned sequence, and Ptah refuses a run whose approval names no
// repeatable the directory holds, so a plan published here could never run.
// The history read stops before a plan instead, names the files, and reads
// again at the interval. The control is the same reading without them, which
// publishes the plan: a fixture that never got that far would pass the row.
func TestAHistoryThatListsRepeatableMigrationsPublishesNoPlan(t *testing.T) {
	t.Parallel()

	many := make([]dataplane.MigrationRepeatable, 0, 11)
	for index := range 11 {
		many = append(many, dataplane.MigrationRepeatable{
			Identity: fmt.Sprintf("view_%02d", index), Path: fmt.Sprintf("R__view_%02d.sql", index), State: "pending",
		})
	}
	for _, test := range []struct {
		name        string
		repeatables []dataplane.MigrationRepeatable
		says        []string
	}{
		{
			name:        "a file the artifact holds",
			repeatables: []dataplane.MigrationRepeatable{{Identity: "account_names", Path: "R__account_names.sql", State: "pending"}},
			says:        []string{"lists 1 repeatable migration,", ": R__account_names.sql."},
		},
		{
			// A row the database's record keeps after its file went.
			name:        "a row the database records and no file carries",
			repeatables: []dataplane.MigrationRepeatable{{Identity: "old_grants", State: "orphaned"}},
			says:        []string{"old_grants (recorded by the database, in no file of the artifact)", "ptah migrations repair --repeatable <identity>", "with --force while its file"},
		},
		{
			name:        "more files than a message names",
			repeatables: many,
			says:        []string{"lists 11 repeatable migrations,", "R__view_07.sql, and 3 more."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			migration, _ := awaitingApprovalFixture(t)
			report := pendingMigrationHistory()
			report.Repeatables = test.repeatables
			actual, _ := readMigrationHistory(t, migration, report)
			assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonRepeatableMigrationsUnsupported)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || actual.Status.Plan != nil {
				t.Fatalf("phase %s with plan %#v, want Blocked and no plan", actual.Status.Phase, actual.Status.Plan)
			}
			blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			for _, want := range test.says {
				if !strings.Contains(blocked.Message, want) {
					t.Errorf("Blocked says %q, want it to carry %q", blocked.Message, want)
				}
			}
			if meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired) {
				t.Fatalf("a history nothing may run against asks for an approval: %#v", actual.Status.Conditions)
			}
			// The history it read is kept: it is what the database holds.
			if actual.Status.History == nil || actual.Status.History.PendingCount != 1 {
				t.Fatalf("History = %#v, want the reading recorded", actual.Status.History)
			}
			if actual.Status.NextReconciliationTime == nil {
				t.Fatal("the refusal named no next reading")
			}
		})
	}

	migration, _ := awaitingApprovalFixture(t)
	planned, _ := readMigrationHistory(t, migration, pendingMigrationHistory())
	if planned.Status.Phase != operatorv1alpha1.MigrationPhasePlanning && planned.Status.Plan == nil {
		t.Fatalf("the control published no plan, so the row measured nothing: phase %s", planned.Status.Phase)
	}
	if blocked := meta.FindStatusCondition(planned.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked); blocked != nil &&
		blocked.Status == metav1.ConditionTrue {
		t.Fatalf("the control is blocked: %#v", blocked)
	}
}
