// Package blockedsince decides what a stored PtahSchema or PtahMigration says
// about status.blockedSince (#1160): whether the pass that wrote it is still on
// its way, ended at Blocked for something a person has to change, or ended
// anywhere else.
//
// A Blocked resource does not stay in phase Blocked. The read it repeats at its
// interval claims Resolve, which writes the operation's phase, and the
// handoffs between that read's steps write theirs, before the pass ends at
// Blocked again. Phase alone therefore cannot date a block: a durable field
// has to, and it has to survive every write a pass makes on its way back.
//
// Both controllers apply Next on every status write. The state collector
// applies the same classification to what it reads, so a date the stored
// status no longer explains, such as one an older manager left on a resource
// it moved on, is not counted.
package blockedsince

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// End is what a status says about the pass that wrote it.
type End int

const (
	// Open is a write the pass makes on its way somewhere: a claim in flight,
	// including a read that failed and waits for its retry under the same
	// claim, a handoff between steps, or the Pending a spec edit leaves when
	// it discards a claim. It keeps whatever date the resource carries.
	Open End = iota
	// Blocked is a pass that ended at Blocked for something a person has to
	// change. It dates the block if nothing dates it yet.
	Blocked
	// Settled is a pass that ended anywhere else. It clears the date.
	Settled
)

// Counted reports whether a date on a status that classifies as end is the
// date of a block that still stands.
func (e End) Counted() bool {
	return e != Settled
}

// Next is the blockedSince a status write leaves. It reads the clock only to
// date a new block: the reconcilers' tests count clock reads, and a write
// that dates nothing has no reason to make one.
func Next(current *metav1.Time, end End, now func() time.Time) *metav1.Time {
	switch end {
	case Open:
		return current
	case Blocked:
		if current != nil {
			return current
		}
		since := metav1.NewTime(now())
		return &since
	default:
		return nil
	}
}

// Schema classifies a schema's status, read against its spec.
//
// Blocked is decided by the phase even with a claim in status: a proof whose
// target moved keeps its claim while it waits for a person. Every other phase
// with a claim is a pass still under way. A read that failed is phase Failed
// with its claim kept for the retry, and keeps the date like any claim.
//
// A spec edit keeps the date: a schema that was Blocked, was edited, and
// stops at Blocked again has needed a person since the first block, whatever
// the second one names.
func Schema(schema *operatorv1alpha1.PtahSchema) End {
	status := &schema.Status
	if status.Phase == operatorv1alpha1.PhaseBlocked {
		if schemaWaitsForNobody(schema) {
			return Settled
		}
		return Blocked
	}
	if status.ActiveOperation != nil {
		return Open
	}
	switch status.Phase {
	case operatorv1alpha1.PhasePending,
		operatorv1alpha1.PhaseResolving,
		operatorv1alpha1.PhaseVerifying,
		operatorv1alpha1.PhaseObserving,
		operatorv1alpha1.PhasePlanning,
		operatorv1alpha1.PhaseApplying,
		operatorv1alpha1.PhaseVerifyingConvergence:
		return Open
	default:
		// ReadyToApply, AwaitingApproval, InSync, Suspended, a Failed with no
		// read left to retry, and a phase this build does not know.
		return Settled
	}
}

// schemaWaitsForNobody reports a Blocked schema that is the apply policy at
// work. Under apply: Never a schema records plans on purpose: one it could
// apply reads ApplyDisabled, and one its destructive-change policy refuses
// would not have run either, so that block is not dated under Never. It reads
// PolicyBlocked where the Plan publishes it and DestructiveChangesDisabled
// where the approval gate meets it again. Under any other apply policy the
// same refusal waits for a person, and a read fault waits for one under every
// policy.
func schemaWaitsForNobody(schema *operatorv1alpha1.PtahSchema) bool {
	switch readyReason(schema.Status.Conditions, operatorv1alpha1.ConditionReady) {
	case operatorv1alpha1.ReasonApplyDisabled:
		return true
	case operatorv1alpha1.ReasonPolicyBlocked, operatorv1alpha1.ReasonDestructiveChangesDisabled:
		return schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever
	default:
		return false
	}
}

// Migration classifies a migration's status, as Schema does a schema's.
//
// Every Blocked a migration writes is a block: none of its reasons comes from
// the apply policy. Planning with no claim is written only by the history read
// that found nothing blocking, so it settles: from there a migration under
// apply: Always claims its Apply, one under OnApproval waits for an approval,
// and one under Never rests with Ready reason ApplyDisabled. A block the Apply
// ends in is dated from that block.
func Migration(migration *operatorv1alpha1.PtahMigration) End {
	status := &migration.Status
	if status.Phase == operatorv1alpha1.MigrationPhaseBlocked {
		return Blocked
	}
	if status.ActiveOperation != nil {
		return Open
	}
	switch status.Phase {
	case operatorv1alpha1.MigrationPhasePending,
		operatorv1alpha1.MigrationPhaseResolving,
		operatorv1alpha1.MigrationPhaseVerifying,
		operatorv1alpha1.MigrationPhaseReading,
		operatorv1alpha1.MigrationPhaseApplying,
		operatorv1alpha1.MigrationPhaseVerifyingHistory:
		return Open
	default:
		// Planning, AwaitingApproval, InSync, Suspended, a Failed with no read
		// left to retry, and a phase this build does not know.
		return Settled
	}
}

func readyReason(conditions []metav1.Condition, conditionType string) operatorv1alpha1.ConditionReason {
	ready := meta.FindStatusCondition(conditions, conditionType)
	if ready == nil {
		return ""
	}
	return operatorv1alpha1.ConditionReason(ready.Reason)
}
