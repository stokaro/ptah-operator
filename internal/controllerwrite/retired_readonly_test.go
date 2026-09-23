package controllerwrite

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func retiredReadOnlyFixture() (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus) {
	schema := &operatorv1alpha1.PtahSchema{}
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.ExecutionBinding = &operatorv1alpha1.ExecutionBindingStatus{
		Epoch: "v1-" + strings.Repeat("9", 32),
	}
	schema.Status.Conditions = []metav1.Condition{
		{
			Type: operatorv1alpha1.ConditionPlanReady, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonExecutionBindingChanged),
		},
		{
			Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonExecutionBindingChanged),
		},
	}
	operation := &operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationObserve,
		ExecutionBindingID: "v1-" + strings.Repeat("1", 32),
	}
	return schema, operation
}

// Cleaning up a Job that belongs to a retired execution binding goes down a
// path that asks none of the questions the current one asks, because a
// read-only run has nothing to be uncertain about. What keeps that true is
// this function, and every part of it could be removed with the package green.
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
			name: "the schema has not entered the retirement fence",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.Phase = operatorv1alpha1.PhaseObserving
			},
		},
		{
			name: "a retirement condition is missing",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.Conditions = schema.Status.Conditions[:1]
			},
		},
		{
			name: "a retirement condition is not false",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.Conditions[1].Status = metav1.ConditionTrue
			},
		},
		{
			// False for some other reason is not proof of retirement.
			name: "a retirement condition gives another reason",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.Conditions[0].Reason = string(operatorv1alpha1.ReasonStale)
			},
		},
		{
			// The one with teeth.
			name: "the operation is an Apply",
			change: func(_ *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.Type = operatorv1alpha1.OperationApply
			},
		},
		{
			name: "the current epoch is malformed",
			change: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.ActiveOperationStatus) {
				schema.Status.ExecutionBinding.Epoch = "v1-not-a-binding-id"
			},
		},
		{
			name: "the operation's epoch is malformed",
			change: func(_ *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.ExecutionBindingID = ""
			},
		},
		{
			// Not retired at all: it is the binding in force.
			name: "the operation belongs to the binding still in force",
			change: func(schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) {
				operation.ExecutionBindingID = schema.Status.ExecutionBinding.Epoch
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
}
