package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A rollout that moves the executor image or the Ptah version while an Apply's
// Job runs does not settle that Apply as outcome unknown at once. The rotation
// waits: the Apply is supervised and consumed as every Apply is, under the
// binding status and its claim still record, and the binding moves on the
// first pass that finds no claim in flight. Its run is recorded first, under
// the binding that authorized it, and the binding moves second, the order the
// Unknown ending keeps too.
//
// Nothing dispatches while the rotation waits. The claim holds the resource,
// and a new claim is made only once it is gone, on a pass that rotates the
// binding before it claims anything. A claim the Apply's own pass makes, such
// as a retry of an Apply that never dispatched, is retired by that rotation
// before its Job exists, and this manager's dispatch and the controller-write
// webhook both refuse a Job under a binding they are not configured with.

// RotationWaitWords is what an Apply's condition adds while a rotation waits
// for it: a migration's Progressing and a schema's Applying. The acceptance
// rows read it to tell the replacement manager's wait from the dispatching
// manager's own words.
const RotationWaitWords = "; an execution component changed, and the execution binding moves once the Apply's result is read"

// rotationMayWaitFor reports whether a rotation from the binding status
// records to the one this manager is configured with may wait for a claim made
// under the recorded one, before anything is read of the claim's Job.
//
// It may wait only for a claim this manager can read as it was recorded: a
// claim of the binding status records, under the controller-state version and
// the runner protocol this manager is configured with. A rollout that moved
// either changes what the claim means or what the runner's frame says, and a
// dispatched Apply is recorded outcome unknown as before. What is left to have
// moved is the executor image and the Ptah version.
func rotationMayWaitFor(recorded, configured *operatorv1alpha1.ExecutionBindingStatus, claimEpoch string) bool {
	return recorded != nil && configured != nil &&
		validExecutionBindingID(recorded.Epoch) && claimEpoch == recorded.Epoch &&
		recorded.ControllerStateVersion == configured.ControllerStateVersion &&
		recorded.RunnerProtocolVersion == configured.RunnerProtocolVersion
}

// applyHoldsRotation reports whether the Job of a dispatched Apply a rotation
// may wait for (rotationMayWaitFor) holds the binding back, so that the Apply
// is read under the binding its claim was made under before the binding moves.
//
// The wait is bounded by the term the claim took the database Lease for, its
// startedAt plus its leaseDurationSeconds. Both were written with the claim,
// and the term covers the claim's execution deadline and the grace its Pod is
// given to stop, so a Job still running at its end has outlived every deadline
// the claim set. A Job that has stopped holds the rotation whenever it is read:
// its result is there to be read, and reading it is the ordinary Apply path.
func applyHoldsRotation(job *batchv1.Job, termEnd, now time.Time) bool {
	return job != nil && (jobTerminal(job) || now.Before(termEnd))
}

// dispatchedMigrationApplyHoldsRotation reads the Job of the dispatched Apply a
// rotation meets and reports whether the rotation waits for it
// (applyHoldsRotation). The Job is read from the API server, as every pass that
// supervises it reads it: a cached read could hold a rotation for a Job that is
// gone. It is read only where the rotation may wait at all, so a rollout that
// cannot wait settles the Apply as it always has. A Job under the reserved name
// that another UID or owner holds is not the claim's and holds nothing.
// Whether one that passes is the Job the claim built is asked by the pass that
// supervises it, under the binding the claim was made under
// (holdMigrationJobToItsClaim).
func (r *MigrationReconciler) dispatchedMigrationApplyHoldsRotation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	configured *operatorv1alpha1.ExecutionBindingStatus,
) (bool, error) {
	operation := migration.Status.ActiveOperation
	if !rotationMayWaitFor(migration.Status.ExecutionBinding, configured, operation.ExecutionBindingID) ||
		operation.LeaseDurationSeconds < 1 || operation.JobName == "" {
		return false, nil
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the dispatched Apply Job: %w", err)
	}
	if (operation.JobUID != "" && job.UID != operation.JobUID) ||
		!exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahMigration",
			migration.Name, migration.UID) {
		return false, nil
	}
	return applyHoldsRotation(job, migrationApplyLeaseTermEnd(operation), r.now()), nil
}

// waitForMigrationApplyUnderRotation lets the pass go on to supervise the
// Apply a rotation waits for. Progressing says why the binding has not moved,
// once, after the words an Apply in flight carries: the claim's ApprovedPlan,
// or OperationInProgress once a wait for the Lease ended or a Pod admission
// refusal cleared. A Pod admission refusal or a wait for the Lease keeps its
// own words.
func (r *MigrationReconciler) waitForMigrationApplyUnderRotation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	progressing := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		(progressing.Reason != string(operatorv1alpha1.ReasonApprovedPlan) &&
			progressing.Reason != string(operatorv1alpha1.ReasonOperationInProgress)) ||
		strings.HasSuffix(progressing.Message, RotationWaitWords) {
		return ctrl.Result{}, false, nil
	}
	before := migration.DeepCopy()
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ConditionReason(progressing.Reason), progressing.Message+RotationWaitWords)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
}

// migrationBindingRotating reports whether this manager is configured with
// components other than the ones status records. The supervising pass reaches
// a claim under such a binding only while a rotation waits for its Apply.
func (r *MigrationReconciler) migrationBindingRotating(migration *operatorv1alpha1.PtahMigration) (bool, error) {
	configured, err := r.configuredMigrationBinding()
	if err != nil {
		return false, err
	}
	return !executionBindingComponentsEqual(migration.Status.ExecutionBinding, configured), nil
}

// dispatchedApplyHoldsRotation is dispatchedMigrationApplyHoldsRotation for a
// schema's Apply, whose Job the supervising pass holds to its claim in
// reconcileActive.
func (r *SchemaReconciler) dispatchedApplyHoldsRotation(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	configured *operatorv1alpha1.ExecutionBindingStatus,
) (bool, error) {
	operation := schema.Status.ActiveOperation
	if !rotationMayWaitFor(schema.Status.ExecutionBinding, configured, operation.ExecutionBindingID) ||
		operation.LeaseDurationSeconds < 1 || operation.JobName == "" {
		return false, nil
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the dispatched Apply Job: %w", err)
	}
	if (operation.JobUID != "" && job.UID != operation.JobUID) ||
		!exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema",
			schema.Name, schema.UID) {
		return false, nil
	}
	termEnd := operation.StartedAt.Add(time.Duration(operation.LeaseDurationSeconds) * time.Second)
	return applyHoldsRotation(job, termEnd, r.now()), nil
}

// waitForApplyUnderRotation is waitForMigrationApplyUnderRotation for a
// schema, whose Applying condition says why the binding has not moved.
func (r *SchemaReconciler) waitForApplyUnderRotation(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, bool, error) {
	applying := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApplying)
	if applying == nil || applying.Status != metav1.ConditionTrue ||
		applying.Reason != string(operatorv1alpha1.ReasonApprovedPlan) ||
		strings.HasSuffix(applying.Message, RotationWaitWords) {
		return ctrl.Result{}, false, nil
	}
	before := schema.DeepCopy()
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionTrue, operatorv1alpha1.ReasonApprovedPlan,
		applying.Message+RotationWaitWords)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
}

// schemaBindingRotating is migrationBindingRotating for a schema.
func (r *SchemaReconciler) schemaBindingRotating(schema *operatorv1alpha1.PtahSchema) (bool, error) {
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return false, err
	}
	return !executionBindingComponentsEqual(schema.Status.ExecutionBinding, configured), nil
}
