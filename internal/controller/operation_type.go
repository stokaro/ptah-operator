package controller

import (
	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// What each operation type is -- mutating or not, when it holds the database
// realm, whether it carries out a proof, which phase it publishes and what the
// runner is told -- is written down once, in internal/mutationlifecycle. These
// adapters read it for a claim, and a missing claim describes as the zero
// Operation, which answers no to everything.
//
// A site that asks one of those questions reads the answer here. A site that
// asks something else, and happens to name a type while it does -- a Plan's
// sealed payload, the fields an Apply claim binds, the proof a Plan settles
// -- keeps naming it: the answers overlap today, and the questions do not.

type (
	schemaOperationKind    = mutationlifecycle.Operation[operatorv1alpha1.ReconciliationPhase]
	migrationOperationKind = mutationlifecycle.Operation[operatorv1alpha1.MigrationPhase]
)

func schemaOperation(operation *operatorv1alpha1.ActiveOperationStatus) schemaOperationKind {
	if operation == nil {
		return schemaOperationKind{}
	}
	return mutationlifecycle.SchemaOperation(operation.Type)
}

func migrationOperation(operation *operatorv1alpha1.MigrationOperationStatus) migrationOperationKind {
	if operation == nil {
		return migrationOperationKind{}
	}
	return mutationlifecycle.MigrationOperation(operation.Type)
}

// schemaClaimHoldsLock reports whether the schema's active claim holds the
// database realm: an Apply or a Plan always, an Observe only while it carries
// out the proof a pending observation owes.
func schemaClaimHoldsLock(schema *operatorv1alpha1.PtahSchema) bool {
	return schemaOperation(schema.Status.ActiveOperation).HoldsLock(schema.Status.PendingObservation != nil)
}

// schemaClaimServesProof reports whether the schema's active claim is carrying
// out the proof its pending observation owes.
func schemaClaimServesProof(schema *operatorv1alpha1.PtahSchema) bool {
	return schema.Status.PendingObservation != nil && schemaOperation(schema.Status.ActiveOperation).ServesProof
}
