package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// retryPastRefusedSchemaJob moves a read-only claim to its next attempt past the
// Job reconcileActive refused, once the claim owes that Job nothing more
// (letGoOfReadOnlySchemaJob).
func (r *SchemaReconciler) retryPastRefusedSchemaJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	failure error,
) (ctrl.Result, error) {
	waiting := fmt.Sprintf("Waiting for Job %s, which this release cannot confirm, to stop before the %s operation runs again",
		job.Name, schema.Status.ActiveOperation.Type)
	if result, held, err := r.letGoOfReadOnlySchemaJob(ctx, schema, job, waiting); held || err != nil {
		return result, err
	}
	return r.retryOperation(ctx, schema, nil, failure)
}

// letGoOfReadOnlySchemaJob settles what a read-only claim owes the Job under its
// reserved name before the claim lets go of it, by the rule both families keep
// (releaseReadOnlyJob). It reports held, with the pass's result, while the claim
// has to stay over the Job. The caller lets go once it is not held: it retries
// past a Job it refused, or discards the claim of a suspended resource. A claim an
// execution-binding change retires is kept and settled by cleanupRetiredReadOnlyJob
// instead, under the retired epoch. Every caller holds a read-only claim, and
// decides that before it asks.
//
// A claim of a suspended resource is not held, whichever pass asks: suspension
// is the stop button. A claim that serves post-apply proof is held as any other
// is (schemaClaimMayWait), and the rule is kept here rather than by each caller.
//
// The Job is held to what the claim fixes without a rebuild under the execution
// binding status records (validateJobEnvelope), the match the controller-write
// webhook holds a schema Job's cleanup TTL to. A claim that never recorded the
// Job's UID records it first. A Warning names a Job admission would refuse a TTL
// (schemaJobCleanupNotScheduled). A Plan's claim keeps renewing the realm Lease
// while it waits, as it does while it supervises a Job, and Ready says what the
// claim waits for in the words of waiting.
func (r *SchemaReconciler) letGoOfReadOnlySchemaJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	waiting string,
) (result ctrl.Result, held bool, err error) {
	mayWait := schemaClaimMayWait(schema)
	release := func() (readOnlyJobRelease, error) {
		verdict, _ := mutationlifecycle.VerdictFor(schemaJobClaim(schema, job))
		return releaseReadOnlyJob(job, verdict, validateJobEnvelope(job, schema, schema.Status.ActiveOperation), r.now(),
			mayWait)
	}
	step, reason := release()
	if step == recordReadOnlyJob {
		before := schema.DeepCopy()
		schema.Status.ActiveOperation.JobUID = job.UID
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("record the %s Job before its cleanup: %w",
				schema.Status.ActiveOperation.Type, err)
		}
		step, reason = release()
	}
	operation := schema.Status.ActiveOperation
	switch step {
	case waitForReadOnlyJob:
		result, err := r.waitForReadOnlySchemaJob(ctx, schema, waiting)
		return result, true, err
	case collectReadOnlyJob:
		return ctrl.Result{}, false, settleJobCleanupAnswer(r.markJobHarvested(ctx, job), func(refusal error) {
			r.schemaJobCleanupNotScheduled(schema, operation, job, refusal)
		})
	case leaveReadOnlyJobUncollected:
		r.schemaJobCleanupNotScheduled(schema, operation, job, reason)
	}
	return ctrl.Result{}, false, nil
}

// schemaClaimMayWait reports whether the read-only claim may wait for the Job
// under its name to stop before it lets go, in every wait a PtahSchema claim
// keeps (stopWaitingForJob). Suspension is the stop button, so a claim of a
// suspended resource does not wait, unless it serves post-apply proof. Proof
// outranks suspension: suspension does not discard its claim, the proof keeps
// renewing its Lease, and a proof read whose Job the claim did not refuse is
// supervised to its end. Waiting for a refused one holds nothing more, and
// letting go would leave that Job with no cleanup, running beside the proof read
// that starts on resume.
//
// It asks what reconcileActive's suspension site asks, whether a proof is owed,
// rather than whether this claim serves it (schemaClaimServesProof): a claim
// suspension does not discard is never let go here either.
func schemaClaimMayWait(schema *operatorv1alpha1.PtahSchema) bool {
	return !schema.Spec.Suspend || schema.Status.PendingObservation != nil
}

// waitForReadOnlySchemaJob keeps the claim over the running Job it owes a
// cleanup, renews the realm Lease a Plan's claim holds, and says so in Ready with
// waiting. The words name the Job and nothing that changes from one pass to the
// next, so a pass that finds them already written writes nothing (patchStatus).
func (r *SchemaReconciler) waitForReadOnlySchemaJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	waiting string,
) (ctrl.Result, error) {
	holdsLock := schemaClaimHoldsLock(schema)
	if holdsLock {
		acquired, requeue, err := r.acquireOperationLock(ctx, schema)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}
	before := schema.DeepCopy()
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationInProgress, waiting)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: activeJobPollInterval(holdsLock)}, nil
}

// schemaJobCleanupNotScheduled says that a Job will not be collected by a TTL.
// Nothing else collects it while its owner lives.
func (r *SchemaReconciler) schemaJobCleanupNotScheduled(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
	err error,
) {
	r.event(schema, corev1.EventTypeWarning, "SchemaJobCleanupDeferred",
		"%s Job %q has no cleanup scheduled, so it stays until it is deleted by hand or with its owner: %s",
		operation.Type, job.Name, bounded(err.Error(), 512))
}

// warnReadJobOverdue records the ReadJobOverdue Warning for the running Job of
// the active read-only claim, which the claim stops waiting for at the Job's stop
// bound, and then does what then says. It names the Job's first Pod that has not
// stopped. A failed list names no Pod and is logged: the claim does not wait for
// a Pod it cannot name either.
func (r *SchemaReconciler) warnReadJobOverdue(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	then string,
) {
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, job.Name, job.UID)
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("could not list the Pods of a read Job past its stop bound", "job", job.Name, "error", err.Error())
	}
	running, _ := podsStillStopping(pods)
	r.event(schema, corev1.EventTypeWarning, "ReadJobOverdue", "%s", readJobOverdueText(
		readJobOverdueLead(string(schema.Status.ActiveOperation.Type), job.Name, jobStopBound(job), then), running))
}

// retryPastOverdueSchemaJob moves a read-only claim to its next attempt past the
// running Job it supervised, once the Job is past its stop bound
// (stopWaitingForJob). Since Kubernetes 1.31 a Job past its deadline carries only
// FailureTarget until every Pod has terminated, and a Pod on a node that is gone
// may never terminate, so the claim would supervise the Job forever. The Job has
// not finished, so admission refuses its cleanup TTL, and it stays until it is
// deleted by hand or with its owner; the Warning says so.
func (r *SchemaReconciler) retryPastOverdueSchemaJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	stopBy := jobStopBound(job)
	r.warnReadJobOverdue(ctx, schema, job, fmt.Sprintf(
		"The %s runs again under a new Job, and Job %s stays until it is deleted by hand or with its owner.",
		operation.Type, job.Name))
	return r.retryOperation(ctx, schema, nil, fmt.Errorf("%s Job %q did not stop by %s, its own deadline plus the grace its Pods are given",
		operation.Type, job.Name, stopBy.UTC().Format(time.RFC3339)))
}

// schemaJobClaim describes job, standing under the name the active claim
// reserved, to the decision about what a pass does with it
// (mutationlifecycle.VerdictFor).
func schemaJobClaim(schema *operatorv1alpha1.PtahSchema, job *batchv1.Job) mutationlifecycle.JobClaim {
	operation := schema.Status.ActiveOperation
	return mutationlifecycle.JobClaim{
		Mutating:        schemaOperation(operation).Mutating,
		DispatchStarted: operation.DispatchStarted,
		RecordedJobUID:  string(operation.JobUID),
		Found:           true,
		FoundJobUID:     string(job.UID),
		OwnedExactly: exactControllerOwner(job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(), "PtahSchema", schema.Name, schema.UID),
	}
}
