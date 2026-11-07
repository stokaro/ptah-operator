package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// A schema proves an Apply by reading the target that Apply was planned for.
// The target's identity covers the login, the host, the port, the database and
// the URL parameters that are not a credential or a connection timeout
// (internal/runner/identity.go). When the Secret the proof reads holds a URL of
// another identity, every attempt reads somewhere the Apply may never have
// touched, so no attempt can say what the Apply did, and no number of them ends
// the proof.
//
// So the resource stops and says so: phase Blocked, reason TargetIdentityChanged,
// with the proof and its Lease kept. It goes on reading at the retry interval,
// because the way out that loses nothing is putting the first URL back, and a
// Secret raises no event here.
//
// The other way out is a person's statement. Crediting the Apply from a reading
// of another target would attribute it to a database it may never have reached,
// and giving the proof up unasked would let a store's mistake reach the next
// plan with nobody looking. So the proof ends only on a statement about this
// Apply and this target: SettledApplyAnnotation holds the Apply's operation ID
// with the digest of the target. That value is no secret. The release namespace
// is trusted, and anyone who knows the URL can work the digest out. What it takes
// is a person naming the target, and the Apply's ID alone names none.
//
// The statement holds until the proof ends, and it is taken at a read rather than
// when it is written. The read has to name the target the value names, and the
// last stop has to have printed that value, so the stop that says what settling
// does always comes first. A store that leaves the target and comes back stops
// the resource there again, and the next read takes the statement with no new
// write. That is what the statement said, and removing the annotation takes it
// back. A suspended resource starts no reads, so it takes the statement from a
// read already under way or after it resumes. Every Pod the Apply could still be
// running has stopped by then, because a claim serving the proof reaches a
// harvest only past that check. A statement does not stop a Pod.
//
// Settling gives up the proof, its plan and its Lease in one status write, and
// the resource starts again from Resolve under whatever the Secret holds. The
// next plan binds that target, so nothing runs on the binding the Apply was
// approved under. That plan needs an approval of its own, unless apply is Always
// and it destroys nothing and changes no privilege (planRequiresApproval), and
// the message says so before anyone runs the command. No Apply is recorded in
// status.applied, since nothing proved one.

// proofReadAnotherTarget reports a proof's read that names a target other than
// the one its Apply was planned for. The runner works the digest out from the
// URL before the child starts, so it is there whether the child could log in or
// not, and a result with none says nothing about where it read.
func proofReadAnotherTarget(pending *operatorv1alpha1.PendingObservationStatus, digest string) bool {
	return pending != nil && digest != "" && digest != pending.Plan.TargetIdentityDigest
}

// settledApplyValue is what SettledApplyAnnotation has to hold to settle the
// proof of one Apply on one target.
func settledApplyValue(applyOperationID, targetDigest string) string {
	return applyOperationID + "/" + targetDigest
}

// movedTargetEvent is what the stop's Event says. The command stays on the
// condition: an Event keeps 512 bytes, the command runs past them at ordinary
// names, and kubectl accepts a value cut short with nothing to say it was wrong.
const movedTargetEvent = "The URL Secret now holds another target than the plan this Apply ran from, so the proof cannot be read: " +
	"the ReconciliationFailed condition says how to end it"

// movedTargetFailure is the sentence the stop reports, and proofSettlementAsked
// reads the value back from it. It keeps below the 1024 bytes a condition holds at
// the longest name and namespace, with the command last so that a person can run
// it as it stands. It says when the annotation holds another value, since nothing
// refuses one when it is written, and does not repeat that value, which can be as
// long as an annotation can.
func movedTargetFailure(schema *operatorv1alpha1.PtahSchema, targetDigest string) error {
	value := settledApplyValue(schema.Status.PendingObservation.ApplyOperationID, targetDigest)
	another := ""
	if held, written := schema.Annotations[operatorv1alpha1.SettledApplyAnnotation]; written && held != value {
		another = "the settled-apply annotation holds another value, which settles nothing on this target; "
	}
	return fmt.Errorf("the URL Secret now holds another target than the plan this Apply ran from: another login, host, port, "+
		"database or URL parameter, so no read can say what the Apply did; %sput the first URL back and the proof finishes, "+
		"or give the proof up, which reads the database under the new URL and, under apply: Always, applies the next plan "+
		"with no approval unless it is destructive or changes privileges: kubectl annotate ptahschema %s -n %s %s=%s --overwrite",
		another, schema.Name, schema.Namespace, operatorv1alpha1.SettledApplyAnnotation, value)
}

// blockProofOnMovedTarget records what stops the proof and what ends it. It is a
// retry with the failure named, so the claim, its pace and the Lease it serves
// stay what they are for any failed read.
func (r *SchemaReconciler) blockProofOnMovedTarget(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	targetDigest string,
) (ctrl.Result, error) {
	failure := movedTargetFailure(schema, targetDigest)
	return r.retryOperationReporting(ctx, schema, job, operatorv1alpha1.ReasonTargetIdentityChanged, failure, movedTargetEvent, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonTargetIdentityChanged,
			"The proof of the last Apply needs a person: ReconciliationFailed says what to do")
	})
}

// schemaProofBlocked reports a resource stopped by blockProofOnMovedTarget.
func schemaProofBlocked(schema *operatorv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == operatorv1alpha1.PhaseBlocked && schema.Status.PendingObservation != nil
}

// proofSettlementAsked reports that the annotation holds the value for the target
// a read names, and that the last stop printed that value. The condition is the
// only record of where the resource last stopped: a stop on another target prints
// another value, and a read that finished or failed otherwise leaves no value, so
// a statement about a target the resource has not just stopped on waits for that
// stop.
func proofSettlementAsked(schema *operatorv1alpha1.PtahSchema, targetDigest string) bool {
	pending := schema.Status.PendingObservation
	if pending == nil {
		return false
	}
	value := settledApplyValue(pending.ApplyOperationID, targetDigest)
	if schema.Annotations[operatorv1alpha1.SettledApplyAnnotation] != value {
		return false
	}
	failed := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	return failed != nil && failed.Status == metav1.ConditionTrue && failed.Reason == string(operatorv1alpha1.ReasonTargetIdentityChanged) &&
		strings.Contains(failed.Message, operatorv1alpha1.SettledApplyAnnotation+"="+value+" ")
}

// settleProofOnMovedTarget ends the proof in the name of the person who asked. It
// runs at the harvest of the read that named the target, so the caller holds the
// Lease and has seen every Apply Pod stop. The Event follows the status write and
// not the release of the Lease: a release that fails is retried from its record,
// and the settlement has happened either way.
func (r *SchemaReconciler) settleProofOnMovedTarget(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	pending := schema.Status.PendingObservation
	if err := stagePendingLockRelease(schema, pending); err != nil {
		return ctrl.Result{}, err
	}
	schema.Status.ActiveOperation = nil
	schema.Status.PendingObservation = nil
	schema.Status.Plan = nil
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.NextReconciliationTime = nil
	const settled = "A person settled the proof of the last Apply; no read established what that Apply did"
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationCanceled)
	r.event(schema, corev1.EventTypeNormal, "ProofSettled",
		"The proof of Apply %s was given up as %s asked, and no Apply is recorded. The database is read again under the URL the "+
			"Secret holds, and under apply: Always the next plan applies with no approval unless it is destructive or changes privileges",
		pending.ApplyOperationID, operatorv1alpha1.SettledApplyAnnotation)
	if err := r.completePendingLockRelease(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}
