package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// waitForOperationLock is acquireOperationLock for a pass that can do nothing
// else until it holds the Lease, and it says so in Ready. A claim that lost the
// Lease before its dispatch has no Job and no Event, and Ready said only that
// the operation was in progress, which reads the same as a Job at work. A
// manager down for longer than the Lease lasts can lose it after the dispatch,
// with the Job still running. jobStopped is a pass that read the claim's Job
// finished, so the wait does not say it may still run.
func (r *SchemaReconciler) waitForOperationLock(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	jobStopped bool,
) (bool, time.Duration, error) {
	acquired, contended, requeue, err := r.tryOperationLock(ctx, schema)
	if err != nil || !contended {
		return acquired, requeue, err
	}
	r.reportLockWait(ctx, schema, jobStopped)
	return false, requeue, nil
}

// reportLockWait writes the wait on Ready in place of what a claim in flight
// says there (lockWaitReplacesReady), and in place of nothing else. A report
// about the claim's Pod stays: an Apply's ending reads it from Ready, and a Pod
// that cannot start says more than the wait does. A write that fails is not
// returned: an error requeue backs off, and this pass is what asks for the
// Lease again, so the report must not slow the claim down.
func (r *SchemaReconciler) reportLockWait(ctx context.Context, schema *operatorv1alpha1.PtahSchema, jobStopped bool) {
	if schema.Status.ActiveOperation == nil || !lockWaitReplacesReady(schema) {
		return
	}
	before := schema.DeepCopy()
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonWaitingForDatabaseLock, schemaLockWaitMessage(schema, jobStopped))
	if err := r.patchStatus(ctx, before, schema); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not report the wait for the database lock", "error", err.Error())
	}
}

// lockWaitReplacesReady reports whether Ready says only what a claim in flight
// says: nothing yet, that the operation is in progress, that it waits for the
// lock, or that an earlier attempt failed (retriedFailure). The last is what a
// read tried again carries until it ends, and ReconciliationFailed keeps the
// failure while the wait stands in its place on Ready.
func lockWaitReplacesReady(schema *operatorv1alpha1.PtahSchema) bool {
	ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil {
		return true
	}
	switch ready.Reason {
	case string(operatorv1alpha1.ReasonOperationInProgress), string(operatorv1alpha1.ReasonWaitingForDatabaseLock):
		return true
	}
	failed := retriedFailure(schema)
	return failed != nil && ready.Reason == failed.Reason && ready.Message == reconciliationIncomplete
}

// retriedFailure is the ReconciliationFailed condition of a claim tried again
// after a failure, or nil. retryOperationReporting moves the claim to its next
// attempt with the phase Failed and records the failure through setFailure,
// which writes its reason on Ready with the words reconciliationIncomplete. A
// claim made afresh writes another phase, so a failure that an earlier claim
// left on ReconciliationFailed is not taken for this claim's.
func retriedFailure(schema *operatorv1alpha1.PtahSchema) *metav1.Condition {
	failed := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if schema.Status.Phase != operatorv1alpha1.PhaseFailed || failed == nil || failed.Status != metav1.ConditionTrue {
		return nil
	}
	return failed
}

// endLockWait gives Ready back what the claim said before it waited once it
// holds the Lease again: a claim tried again after a failure goes on under that
// failure, as one that never waited does, and any other claim says it is in
// progress. It is written with the epoch the claim took.
func endLockWait(schema *operatorv1alpha1.PtahSchema) {
	operation := schema.Status.ActiveOperation
	ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady)
	if operation == nil || ready == nil || ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		return
	}
	if failed := retriedFailure(schema); failed != nil {
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
			operatorv1alpha1.ConditionReason(failed.Reason), reconciliationIncomplete)
		return
	}
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation.Type))
}

// schemaLockWaitMessage is lockWaitMessage for the schema's claim. jobStopped
// is a pass that read the claim's Job finished.
//
// A read that carries out an Apply's proof, the Observe or the Plan it
// requires, waits for the Lease the Apply took, and asks for it before it looks
// at the Apply's Pods: a Job of the Apply that was active with no Pod when the
// Apply ended can still start one. Until the read has dispatched a Job of its
// own, the Job that may still run is the Apply's, which the pending
// observation names.
func schemaLockWaitMessage(schema *operatorv1alpha1.PtahSchema, jobStopped bool) string {
	operation := schema.Status.ActiveOperation
	jobName, mayRun := operation.JobName, schemaMayHaveDispatched(operation) && !jobStopped
	if pending := schema.Status.PendingObservation; !schemaMayHaveDispatched(operation) &&
		schemaClaimServesProof(schema) && pending.ApplyJobName != "" {
		jobName, mayRun = pending.ApplyJobName, true
	}
	return lockWaitMessage(string(operation.Type), jobName, operation.CoordinationDigest, mayRun)
}

// lockWaitMessage says what a claim is waiting for, in the same words on either
// family. It names the Lease, which names its holder to whoever may read the
// operator's namespace, and not the holder: the holder can be another team's
// resource in a shared realm. It also says when the claim may have started its
// Job, because a reader who took the wait for an operation that has not started
// could act while its SQL runs.
func lockWaitMessage(operationType, jobName, coordinationDigest string, mayHaveDispatched bool) string {
	lease, err := targetlock.LeaseName(coordinationDigest)
	if err != nil {
		lease = "of this database's realm"
	}
	if mayHaveDispatched {
		return fmt.Sprintf("%s is waiting for the database lock it lost, and Job %s may still be running: another operation on this database holds Lease %s. It asks again every %s",
			operationType, jobName, lease, maxLockContentionPoll)
	}
	return fmt.Sprintf("%s is waiting for the database lock: another operation on this database holds Lease %s. It asks again every %s",
		operationType, lease, maxLockContentionPoll)
}
