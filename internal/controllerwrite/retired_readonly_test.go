package controllerwrite

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func retiredReadOnlyFixture() (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus) {
	retiredEpoch := "v1-" + strings.Repeat("1", 32)
	schema := &operatorv1alpha1.PtahSchema{}
	schema.Status.ExecutionBinding = &operatorv1alpha1.ExecutionBindingStatus{
		Epoch: "v1-" + strings.Repeat("9", 32),
	}
	operation := &operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationObserve,
		ExecutionBindingID: retiredEpoch,
		JobName:            "ptah-observe-app-0123456789abcdef",
		JobUID:             "retired-observe-job-uid",
	}
	schema.Status.PendingBindingRetirement = &operatorv1alpha1.BindingRetirementStatus{
		RetiredEpoch: retiredEpoch,
		Job: &operatorv1alpha1.RetiredJobStatus{
			Operation: operation.Type, Name: operation.JobName, UID: operation.JobUID,
		},
	}
	return schema, operation
}

// Cleaning up a Job that belongs to a retired execution binding goes down a
// path that asks none of the questions the current one asks, because a
// read-only run has nothing to be uncertain about. What keeps that true is
// this function, and every part of it could be removed with the package green.
//
// The retirement record is the permission: the Job has to be the one it names,
// by claim, name and UID, and the claim has to belong to the epoch it retired.
// Neither the phase nor a condition reason takes part, so no later refusal
// that rewrites one can grant or withdraw it.
//
// The operation type is the part with teeth. An Apply admitted here would have
// its Job collected through a path that never looks at whether the run
// accounted for itself -- and the Job is the only record that it ran.
//
// The distinct-epoch rule is the other half: with the epochs allowed to be
// equal, a Job of the binding still in force could be cleaned up as though it
// were retired, skipping the checks the current path makes.
func TestOnlyARetiredReadOnlyOperationPassesTheRetirementFence(t *testing.T) {
	t.Parallel()

	schema, operation := retiredReadOnlyFixture()
	if err := validateRetiredReadOnlyStatus(schema, operation); err != nil {
		t.Fatalf("a retired read-only status was refused, so nothing below proves anything: %v", err)
	}

	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus)
	}{
		{
			name: "no retirement is pending",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement = nil
			},
		},
		{
			name: "the retirement owes no Job",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.Job = nil
			},
		},
		{
			name: "the retirement names another claim",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.Job.Operation = operatorv1alpha1.OperationPlan
			},
		},
		{
			name: "the retirement names another Job",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.Job.Name += "-beside-it"
			},
		},
		{
			name: "the retirement has not adopted the Job's UID",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.Job.UID = ""
			},
		},
		{
			name: "the retirement names another Job identity",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.Job.UID = "a-job-that-ran-later"
			},
		},
		{
			// The one with teeth.
			name: "the operation is an Apply",
			change: func(schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.Type = operatorv1alpha1.OperationApply
				schema.Status.PendingBindingRetirement.Job.Operation = operatorv1alpha1.OperationApply
			},
		},
		{
			name: "the current epoch is malformed",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.ExecutionBinding.Epoch = "v1-not-a-binding-id"
			},
		},
		{
			name: "the retired epoch is malformed",
			change: func(schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.PendingBindingRetirement.RetiredEpoch = ""
				operation.ExecutionBindingID = ""
			},
		},
		{
			name: "the operation belongs to an epoch the record did not retire",
			change: func(_ *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.ExecutionBindingID = "v1-" + strings.Repeat("2", 32)
			},
		},
		{
			// Not retired at all: it is the binding in force.
			name: "the operation belongs to the binding still in force",
			change: func(schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
				schema.Status.PendingBindingRetirement.RetiredEpoch = schema.Status.ExecutionBinding.Epoch
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, operation := retiredReadOnlyFixture()
			row.change(schema, operation)
			if err := validateRetiredReadOnlyStatus(schema, operation); err == nil {
				t.Fatal("a status that does not prove a retired read-only operation was accepted")
			}
		})
	}

	// The phase and the condition reasons are what the fence used to read.
	// Whatever a later pass writes into them, the record alone decides.
	t.Run("a later refusal rewrote the phase and every reason", func(t *testing.T) {
		t.Parallel()

		schema, operation := retiredReadOnlyFixture()
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		for _, conditionType := range []string{
			operatorv1alpha1.ConditionPlanReady,
			operatorv1alpha1.ConditionApprovalRequired,
		} {
			schema.Status.Conditions = append(schema.Status.Conditions, metav1.Condition{
				Type: conditionType, Status: metav1.ConditionFalse, Reason: string(operatorv1alpha1.ReasonStale),
			})
		}
		if err := validateRetiredReadOnlyStatus(schema, operation); err != nil {
			t.Fatalf("a rewritten phase withdrew a permission the record grants: %v", err)
		}
	})
}
