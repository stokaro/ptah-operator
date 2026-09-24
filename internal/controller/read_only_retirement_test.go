package controller

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// An execution-binding change retires the claim it finds and hands back the
// database that claim took. Only an Apply takes one, and the condition says so
// by naming Apply rather than by negating isReadOnlyOperation.
//
// The difference is what this measures. isReadOnlyOperation answers false for
// anything it does not recognize, so `!isReadOnlyOperation` means "an Apply, or
// a type this binary has never heard of". A stored object written by a newer
// operator supplies exactly that, and the CRD upgrade path is where it arrives.
// Retiring one would stage a release under an epoch belonging to work this
// binary cannot reason about.
func TestOnlyAnApplyOwesTheDatabaseBackWhenTheBindingChanges(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.OperationType
		owes      bool
	}{
		{name: "Apply", operation: operatorv1alpha1.OperationApply, owes: true},
		{name: "Plan", operation: operatorv1alpha1.OperationPlan, owes: false},
		{name: "Observe", operation: operatorv1alpha1.OperationObserve, owes: false},
		{
			// Not a value this API defines. isReadOnlyOperation reports it
			// exactly as it reports an Apply, so a condition written as
			// !isReadOnlyOperation retires it.
			name: "a type this binary does not know", operation: "Rehearse", owes: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyLockedOperationSchema(operatorv1alpha1.OperationApply)
			schema.Status.ActiveOperation.Type = row.operation
			plan := schema.Status.Plan.DeepCopy()

			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			writes := &[]retiredClaim{}
			reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
			reconciler.Locks = targetlock.New(api, api, nil)

			stored := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.executionBindingChanged(
				context.Background(), stored, plan,
				errors.New("the controller image changed under the claim")); err != nil {
				t.Fatal(err)
			}

			owed := false
			for _, write := range *writes {
				if write.owed {
					owed = true
				}
			}
			if owed != row.owes {
				t.Fatalf("a %q claim recorded owed=%t across the binding change, want %t; writes: %#v",
					row.operation, owed, row.owes, *writes)
			}
		})
	}
}
