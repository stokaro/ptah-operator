package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// A schema proves an Apply by reading the target that Apply was planned for, and
// the login is part of that target. When the URL Secret names another one, every
// attempt of the proof reads somewhere the Apply may never have touched, so no
// attempt can say what the Apply did, and no number of them ends the proof.
//
// So the resource stops and says so: phase Blocked, reason TargetIdentityChanged,
// with the proof and its Lease kept. It goes on reading at the retry interval,
// because the way out that loses nothing is putting the first target back, and a
// Secret raises no event here.
//
// The other way out is a person's statement. Crediting the Apply from a reading
// of another target would attribute it to a database it may never have reached,
// and giving the proof up unasked would let a store's mistake reach the next
// plan with nobody looking. So the proof ends only when
// operatorv1alpha1.SettledApplyAnnotation names the Apply it is for, and the
// operator has itself just seen the target move: a value that names another
// attempt, or a proof that is failing for another reason, settles nothing.
//
// Settling touches no database. It waits for every Pod the Apply could still be
// running, as deletion does, because a person's statement does not stop one. Then
// it gives up the proof, its plan and its Lease in one status write, and the
// resource starts again from Resolve under whatever the Secret names now. That
// next plan binds the new target, and an approval of the old plan does not carry
// over, so nothing runs on the binding the Apply was approved under. No Apply is
// recorded in status.applied, since nothing proved one.

// proofReadAnotherTarget reports a proof's read that names a target other than
// the one its Apply was planned for. The runner works the digest out from the
// URL before the child starts, so it is there whether the child could log in or
// not, and a result with none says nothing about where it read.
func proofReadAnotherTarget(pending *operatorv1alpha1.PendingObservationStatus, digest string) bool {
	return pending != nil && digest != "" && digest != pending.Plan.TargetIdentityDigest
}

// blockProofOnMovedTarget records what stops the proof and what ends it. It is a
// retry with the failure named, so the claim, its pace and the Lease it serves
// stay what they are for any failed read.
func (r *SchemaReconciler) blockProofOnMovedTarget(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (ctrl.Result, error) {
	failure := fmt.Errorf("the URL Secret names another target than the plan this Apply ran from, so no read can say what the Apply did; "+
		"put the first login or address back and the proof finishes, or give the proof up and read the database as it is now: "+
		"kubectl annotate ptahschema %s -n %s %s=%s",
		schema.Name, schema.Namespace, operatorv1alpha1.SettledApplyAnnotation, schema.Status.PendingObservation.ApplyOperationID)
	return r.retryOperationAs(ctx, schema, job, operatorv1alpha1.ReasonTargetIdentityChanged, failure, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonTargetIdentityChanged,
			"The proof of the last Apply needs a person: ReconciliationFailed says what to do")
	})
}

// schemaProofBlocked reports a resource stopped by blockProofOnMovedTarget.
func schemaProofBlocked(schema *operatorv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == operatorv1alpha1.PhaseBlocked && schema.Status.PendingObservation != nil
}

// proofSettlementAsked reports that a person named the Apply whose proof the
// operator has just seen the target move under.
func proofSettlementAsked(schema *operatorv1alpha1.PtahSchema) bool {
	pending := schema.Status.PendingObservation
	if pending == nil || pending.ApplyOperationID == "" ||
		schema.Annotations[operatorv1alpha1.SettledApplyAnnotation] != pending.ApplyOperationID {
		return false
	}
	failed := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	return failed != nil && failed.Status == metav1.ConditionTrue && failed.Reason == string(operatorv1alpha1.ReasonTargetIdentityChanged)
}

// proofClaimBetweenAttempts reports that the claim serving the proof has no Job:
// none recorded, and none under the name it reserved, which a pass that lost its
// status write would leave.
func (r *SchemaReconciler) proofClaimBetweenAttempts(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, error) {
	operation := schema.Status.ActiveOperation
	if operation.JobUID != "" {
		return false, nil
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}, job)
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("read the proof's Job: %w", err)
	}
	return true, nil
}

// settleProofOnMovedTarget ends the proof in the name of the person who asked.
// The caller holds the Lease and has seen every Apply Pod stop.
func (r *SchemaReconciler) settleProofOnMovedTarget(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	pending := schema.Status.PendingObservation
	if operation == nil {
		// With a claim beside it the retirement below stages this.
		if err := stagePendingLockRelease(schema, pending); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.PendingObservation = nil
	schema.Status.Plan = nil
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.NextReconciliationTime = nil
	const settled = "A person settled the proof of the last Apply; no read established what that Apply did"
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofSettled, settled)
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationCanceled)
	r.event(schema, corev1.EventTypeNormal, "ProofSettled",
		"The proof of Apply %s was given up as %s asked; the database is read again under the target the URL Secret names, and no Apply is recorded",
		pending.ApplyOperationID, operatorv1alpha1.SettledApplyAnnotation)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}
