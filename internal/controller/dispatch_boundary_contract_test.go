package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The dispatch boundary decides whether a missing Job is created or is an
// outcome nobody can account for. Both families have to answer it identically,
// because the cost of the two answers is not symmetric: one runs a migration a
// second time, the other asks a person to go and look.
//
// The row that matters is a claim that says it dispatched and carries no UID.
// That is what a create whose response never arrived leaves behind, and it is
// the only row a reasonable-looking mistake gets wrong -- requiring both
// fields, rather than either, passes every other row here.
func TestBothFamiliesAnswerTheDispatchBoundaryTheSameWay(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name            string
		dispatchStarted bool
		jobUID          types.UID
		dispatched      bool
	}{
		{"a claim that has not reached the boundary", false, "", false},
		{"a create whose response never arrived", true, "", true},
		{"a Job adopted under a claim that lost its marker", false, "job-uid", true},
		{"a claim that dispatched and confirmed", true, "job-uid", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := &operatorv1alpha1.ActiveOperationStatus{
				Type:            operatorv1alpha1.OperationApply,
				DispatchStarted: row.dispatchStarted,
				JobUID:          row.jobUID,
			}
			if got := schemaMayHaveDispatched(schema); got != row.dispatched {
				t.Errorf("PtahSchema answered %t, want %t", got, row.dispatched)
			}

			migration := &operatorv1alpha1.MigrationOperationStatus{
				Type:            operatorv1alpha1.MigrationOperationApply,
				DispatchStarted: row.dispatchStarted,
				JobUID:          row.jobUID,
			}
			if got := migrationMayHaveDispatched(migration); got != row.dispatched {
				t.Errorf("PtahMigration answered %t, want %t", got, row.dispatched)
			}
		})
	}
}

// No claim is not a claim that dispatched. Both families reach these helpers
// from branches where the claim has already been cleared.
func TestNoClaimHasNotDispatched(t *testing.T) {
	t.Parallel()

	if schemaMayHaveDispatched(nil) {
		t.Error("PtahSchema read a claim that is not there as having dispatched")
	}
	if migrationMayHaveDispatched(nil) {
		t.Error("PtahMigration read a claim that is not there as having dispatched")
	}
}
