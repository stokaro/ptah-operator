package controller

import (
	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// Both families answer "may a Job exist for this claim" from the same two
// fields, and answered it inline at ten sites. These adapters are the whole of
// the difference: the decision is in internal/mutationlifecycle, so a claim
// that grows a third dispatch marker is added in one place rather than found
// missing at the tenth.

func schemaMayHaveDispatched(operation *operatorv1alpha1.ActiveOperationStatus) bool {
	if operation == nil {
		return false
	}
	return mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
		DispatchStarted: operation.DispatchStarted,
		JobUID:          string(operation.JobUID),
	})
}

func migrationMayHaveDispatched(operation *operatorv1alpha1.MigrationOperationStatus) bool {
	if operation == nil {
		return false
	}
	return mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
		DispatchStarted: operation.DispatchStarted,
		JobUID:          string(operation.JobUID),
	})
}
