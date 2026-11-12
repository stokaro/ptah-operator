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
// with the Job still running.
func (r *SchemaReconciler) waitForOperationLock(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (bool, time.Duration, error) {
	acquired, contended, requeue, err := r.tryOperationLock(ctx, schema)
	if err != nil || !contended {
		return acquired, requeue, err
	}
	r.reportLockWait(ctx, schema)
	return false, requeue, nil
}

// reportLockWait writes the wait on Ready in place of the words a claim in
// flight carries, and in place of nothing else. A report about the claim's Pod
// stays: an Apply's ending reads it from Ready, and a Pod that cannot start
// says more than the wait does. A write that fails is not returned: an error
// requeue backs off, and this pass is what asks for the Lease again, so the
// report must not slow the claim down.
func (r *SchemaReconciler) reportLockWait(ctx context.Context, schema *operatorv1alpha1.PtahSchema) {
	operation := schema.Status.ActiveOperation
	ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady)
	if operation == nil || ready != nil &&
		ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) &&
		ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		return
	}
	before := schema.DeepCopy()
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonWaitingForDatabaseLock, schemaLockWaitMessage(operation))
	if err := r.patchStatus(ctx, before, schema); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not report the wait for the database lock", "error", err.Error())
	}
}

// endLockWait gives Ready back the words of a claim in flight once the claim
// holds the Lease again. It is written with the epoch the claim took.
func endLockWait(schema *operatorv1alpha1.PtahSchema) {
	operation := schema.Status.ActiveOperation
	ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady)
	if operation == nil || ready == nil || ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		return
	}
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation.Type))
}

func schemaLockWaitMessage(operation *operatorv1alpha1.ActiveOperationStatus) string {
	return lockWaitMessage(string(operation.Type), operation.JobName, operation.CoordinationDigest,
		schemaMayHaveDispatched(operation))
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
