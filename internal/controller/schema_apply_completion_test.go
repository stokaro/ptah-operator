package controller

import (
	"context"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// TestASucceedingApplyOwesAConvergenceProof is the schema family's Apply
// success path, which nothing in this package reached: instrumenting
// consumeResult showed it was never entered with an Apply operation at all.
// Every Apply in the suite ended uncertain, stale, retried or refused.
//
// What the path decides is worth pinning. A completed Apply Job is not a
// converged schema: the run reported success, and the operator records that as
// an obligation to observe the database rather than as the outcome. So the
// claim is retired, a pending observation is left owing the proof, and
// status.applied stays empty until something independent reads the database
// back.
func TestASucceedingApplyOwesAConvergenceProof(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationApply,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		MutationStarted:      true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	observations := &telemetryObservation{}
	reconciler.Telemetry = observations

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := safetyGetSchema(t, api, schema)
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("the completed Apply claim was not retired: %#v", actual.Status.ActiveOperation)
	}
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationApplySucceeded {
		t.Fatalf("pending observation = %#v, want a succeeded Apply owing its proof", pending)
	}
	if pending.ApplyOperationID != schema.Status.ActiveOperation.ID ||
		pending.ApplyJobUID != schema.Status.ActiveOperation.JobUID {
		t.Fatalf("the proof names another run: %#v", pending)
	}
	// Reporting success is not observing it. Attribution waits for the
	// read-only pass that reads the database back.
	if actual.Status.Applied != nil {
		t.Fatalf("a reported Apply was credited before anything observed it: %#v", actual.Status.Applied)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		t.Fatalf("phase = %q, want %q", actual.Status.Phase, operatorv1alpha1.PhaseVerifyingConvergence)
	}
	if condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApplying); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "JobCompleted" {
		t.Fatalf("Applying condition = %#v, want JobCompleted", condition)
	}
	wantApplies := []telemetry.ApplyOutcome{telemetry.ApplyCompleted}
	if !reflect.DeepEqual(observations.applies, wantApplies) {
		t.Fatalf("apply observations = %#v, want %#v", observations.applies, wantApplies)
	}
}
