package mutationlifecycle

import (
	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// Every decision the two controllers make about a claim starts from the same
// few questions about its operation type: may it change the database, does it
// hold the database realm while it runs, is it the kind of work a pending proof
// is carried out by, which phase does a claim of it publish, and what is the
// runner told to do. Each controller used to answer them inline, as a
// comparison against one type name at every site that needed the answer --
// more than fifty of them -- so a type added to either family would have been
// found missing one site at a time. The answers are written down here, once
// per type.

// Lock says when a claim of an operation type holds the database realm Lease.
type Lock int

const (
	// LockNever: the operation never opens the database, or opens it only to
	// read something no other claimant's work can make wrong.
	LockNever Lock = iota
	// LockAlways: a claim of this type holds the realm from before its Job
	// exists until the claim is retired.
	LockAlways
	// LockWhileProving: a claim of this type holds the realm only while it
	// carries out a proof a pending observation owes, and then under the
	// proof's epoch rather than one of its own.
	LockWhileProving
)

// Operation is what the mutation lifecycle knows about one operation type.
//
// The zero value is what every type this binary does not define gets, and it
// answers no to every question. That is the right answer for a site that
// refuses on it and the wrong one for a site that acts on its negation: "not
// mutating" is not "read-only", because a claim of a type a newer operator
// wrote is neither as far as this binary can tell. ReadOnly exists for that.
type Operation[P ~string] struct {
	// Known says the type is one this binary defines.
	Known bool
	// Mutating says a claim of this type may change the database. It is the
	// whole of the asymmetry the lifecycle is built around: a read-only claim
	// that cannot be accounted for is run again, a mutating one never is.
	Mutating bool
	// Lock says when a claim of this type holds the database realm.
	Lock Lock
	// ServesProof says this is the kind of work a pending observation is
	// carried out by: a read of the database, or the plan that read requires.
	ServesProof bool
	// Phase is the phase a claim of this type publishes while it runs.
	Phase P
	// Runner is the operation the runner is told to perform.
	Runner runner.Operation
}

// ReadOnly reports whether a claim of this type is known not to change the
// database. It is false for an unknown type, which is not known either way.
func (o Operation[P]) ReadOnly() bool {
	return o.Known && !o.Mutating
}

// HoldsLock reports whether a claim of this type holds the database realm,
// given whether a proof is outstanding on the resource that carries it.
func (o Operation[P]) HoldsLock(proofOutstanding bool) bool {
	switch o.Lock {
	case LockAlways:
		return true
	case LockWhileProving:
		return proofOutstanding
	default:
		return false
	}
}

// SchemaOperation describes one PtahSchema operation type.
func SchemaOperation(operationType operatorv1alpha1.OperationType) Operation[operatorv1alpha1.ReconciliationPhase] {
	return schemaOperations[operationType]
}

// MigrationOperation describes one PtahMigration operation type.
func MigrationOperation(
	operationType operatorv1alpha1.MigrationOperationType,
) Operation[operatorv1alpha1.MigrationPhase] {
	return migrationOperations[operationType]
}

var schemaOperations = map[operatorv1alpha1.OperationType]Operation[operatorv1alpha1.ReconciliationPhase]{
	operatorv1alpha1.OperationResolve: {
		Known:  true,
		Phase:  operatorv1alpha1.PhaseResolving,
		Runner: runner.OperationResolve,
	},
	operatorv1alpha1.OperationVerify: {
		Known:  true,
		Phase:  operatorv1alpha1.PhaseVerifying,
		Runner: runner.OperationVerify,
	},
	// An ordinary Observe reads the database and takes nothing. The Observe
	// that proves an Apply runs under the Apply's own epoch, so no other
	// claimant can change the database between the Apply and the reading that
	// accounts for it.
	operatorv1alpha1.OperationObserve: {
		Known:       true,
		Lock:        LockWhileProving,
		ServesProof: true,
		Phase:       operatorv1alpha1.PhaseObserving,
		Runner:      runner.OperationObserve,
	},
	// A Plan holds the realm in its own right. It plans twice and requires the
	// two results to be identical, which says something only if nobody wrote
	// to the database in between.
	operatorv1alpha1.OperationPlan: {
		Known:       true,
		Lock:        LockAlways,
		ServesProof: true,
		Phase:       operatorv1alpha1.PhasePlanning,
		Runner:      runner.OperationPlan,
	},
	operatorv1alpha1.OperationApply: {
		Known:    true,
		Mutating: true,
		Lock:     LockAlways,
		Phase:    operatorv1alpha1.PhaseApplying,
		Runner:   runner.OperationApply,
	},
}

// A migration's History reading takes no Lease, the one after an Apply
// included: a migration hands the realm back once nothing its claim dispatched
// can still write, and settles the account afterwards. So no migration type
// serves a proof in the sense a schema's Observe does.
var migrationOperations = map[operatorv1alpha1.MigrationOperationType]Operation[operatorv1alpha1.MigrationPhase]{
	operatorv1alpha1.MigrationOperationResolve: {
		Known:  true,
		Phase:  operatorv1alpha1.MigrationPhaseResolving,
		Runner: runner.OperationResolve,
	},
	operatorv1alpha1.MigrationOperationVerify: {
		Known:  true,
		Phase:  operatorv1alpha1.MigrationPhaseVerifying,
		Runner: runner.OperationVerify,
	},
	operatorv1alpha1.MigrationOperationHistory: {
		Known:  true,
		Phase:  operatorv1alpha1.MigrationPhaseReading,
		Runner: runner.OperationMigrationHistory,
	},
	operatorv1alpha1.MigrationOperationApply: {
		Known:    true,
		Mutating: true,
		Lock:     LockAlways,
		Phase:    operatorv1alpha1.MigrationPhaseApplying,
		Runner:   runner.OperationMigrationApply,
	},
}
