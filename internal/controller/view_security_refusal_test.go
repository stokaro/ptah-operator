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
// schema's Plan, that frame stops the schema with Ready saying exactly those
// words, records no failure, and reads as a Plan that reached its database.
func TestACapturedMySQLViewSecurityRefusalReadsInStatus(t *testing.T) {
	t.Parallel()
	const want = "plan_refused: ptah refused to plan the change (view-security-option: differs_notes_view sql_security)"
	failure := capturedPlanFailure(t, "differs-view-invoker-unstated-mysql/plan")
	previous := previousReachability()["a target that refused"]
	for _, path := range planPaths(t) {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			_, actual, writes := harvestSchemaFailure(t, underItsVerifiedPolicy(path), failure, previous)
			assertPlanRefusedSays(t, actual, want)
			assertDatabaseReached(t, actual.Status.Conditions, "Database schema was observed")
			writes.assertReachedBeside(t, saysWhatTheReadCameTo(failure))
		})
	}
}

// Since Ptah v0.29.4 a MySQL plan refuses to replace a view that runs as its
// definer when the plan's read connected as another account
// (stokaro/ptah#6109). The runner harness captured the refusal from the release
// image as the documented login, which did not define the view, and the frame's
// message names the view, its definer and that login. Fed to a schema's Plan,
// the frame stops the schema with Ready saying those words.
func TestACapturedMySQLViewDefinerRefusalReadsInStatus(t *testing.T) {
	t.Parallel()
	const want = "plan_refused: ptah refused to plan the change (view-definer: differs_notes_view definer root@localhost account harness_view_other@%)"
	failure := capturedPlanFailure(t, "differs-view-definer-other-mysql/plan")
	previous := previousReachability()["a target that refused"]
	for _, path := range planPaths(t) {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			_, actual, _ := harvestSchemaFailure(t, underItsVerifiedPolicy(path), failure, previous)
			assertPlanRefusedSays(t, actual, want)
			assertDatabaseReached(t, actual.Status.Conditions, "Database schema was observed")
		})
	}
}

// assertPlanRefusedSays holds a schema whose Plan Ptah refused to Ready False
// with PlanRefused and want, and to no failure recorded.
func assertPlanRefusedSays(t *testing.T, actual *operatorv1alpha1.PtahSchema, want string) {
	t.Helper()
	ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(operatorv1alpha1.ReasonPlanRefused) ||
		ready.Message != want {
		t.Fatalf("Ready = %#v, want False PlanRefused with %q", ready, want)
	}
	if failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
		failed.Status == metav1.ConditionTrue {
		t.Fatalf("the refusal was recorded as a failure: %#v", failed)
	}
}
