package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// capturedPlanFailure is the error the runner framed for one Plan of the
// runner harness, as the transcript under internal/dataplane/testdata keeps it.
func capturedPlanFailure(t *testing.T, row string) *runner.ResultError {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "runner-harness", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript []struct {
		Name   string `json:"name"`
		Result struct {
			Operation string              `json:"operation"`
			Error     *runner.ResultError `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatal(err)
	}
	for _, each := range transcript {
		if each.Name != row {
			continue
		}
		if each.Result.Operation != string(runner.OperationPlan) || each.Result.Error == nil {
			t.Fatalf("%s framed %s with error %+v, want a failed Plan", row, each.Result.Operation, each.Result.Error)
		}
		return each.Result.Error
	}
	t.Fatalf("the transcript has no row %s", row)
	return nil
}

// Since Ptah v0.29.3 a MySQL plan refuses a view the database holds as SQL
// SECURITY INVOKER and the declaration leaves without a mode
// (stokaro/ptah#6105). The runner harness captured the refusal from the
// release image and the frame the runner wrote for it, whose message names the
// refusal's code and the view and option the document lists. Fed to a
// schema's Plan, that frame reads as a failed reconciliation in exactly those
// words, and as a Plan that reached its database.
func TestACapturedMySQLViewSecurityRefusalReadsInStatus(t *testing.T) {
	t.Parallel()
	const want = "plan_refused: ptah refused to plan the change (view-security-option: differs_notes_view sql_security)"
	failure := capturedPlanFailure(t, "differs-view-invoker-unstated-mysql/plan")
	previous := previousReachability()["a target that refused"]
	for _, path := range planPaths(t) {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			_, actual, writes := harvestSchemaFailure(t, path, failure, previous)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue ||
				failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) || failed.Message != want {
				t.Fatalf("ReconciliationFailed = %#v, want True OperationFailed with %q", failed, want)
			}
			assertDatabaseReached(t, actual.Status.Conditions, "Database schema was observed")
			writes.assertReachedBeside(t, func(conditions []metav1.Condition) bool {
				return conditionMatches(conditions, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, operatorv1alpha1.ReasonOperationFailed)
			})
		})
	}
}
