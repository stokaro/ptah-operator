package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// schemaDispatch is the PtahSchema family's side of the dispatch sequence
// mutationlifecycle.Driver runs. It lives for one pass: the plan an Apply is
// authorized by is read in Authorize and used by every later step of the same
// pass, and read again on the next.
type schemaDispatch struct {
	r      *SchemaReconciler
	schema *operatorv1alpha1.PtahSchema

	plan    *operatorv1alpha1.PtahSchemaPlan
	content []byte
}

var _ mutationlifecycle.Family = (*schemaDispatch)(nil)

func (d *schemaDispatch) operation() *operatorv1alpha1.ActiveOperationStatus {
	return d.schema.Status.ActiveOperation
}

func (d *schemaDispatch) Claim() mutationlifecycle.Claim {
	operation := d.operation()
	return mutationlifecycle.Claim{
		Type:      string(operation.Type),
		Mutating:  schemaOperation(operation).Mutating,
		HoldsLock: schemaClaimHoldsLock(d.schema),
		Dispatch: mutationlifecycle.DispatchState{
			DispatchStarted: operation.DispatchStarted,
			JobUID:          string(operation.JobUID),
		},
		Snapshot:          operation.AdmissionSnapshot,
		SnapshotRefreshed: operation.AdmissionSnapshotRefreshed,
	}
}

// Authorize re-reads what the claim was decided from. For an Apply that is the
// plan object and its bytes, the execution binding, the verification policy
// and the approval, all read before the realm is taken, so an authorization
// that moved retires the claim without waiting for another holder of the
// Lease.
func (d *schemaDispatch) Authorize(ctx context.Context) mutationlifecycle.Outcome {
	r, schema, operation := d.r, d.schema, d.operation()
	if schema.Spec.Suspend {
		return mutationlifecycle.Stop(r.suspendActiveOperation(ctx, schema))
	}
	current, err := r.operationInputFingerprint(schema, operation.Type)
	if err != nil || current != operation.InputFingerprint {
		if err == nil {
			err = errors.New("operation inputs changed after the claim")
		}
		if schemaOperation(operation).Mutating {
			return mutationlifecycle.Stop(r.applyBecameStale(ctx, schema, err))
		}
		return mutationlifecycle.Stop(r.discardStaleOperation(ctx, schema, err))
	}
	if operation.Type == operatorv1alpha1.OperationVerify {
		binding, bindingErr := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
		if bindingErr != nil || binding.UID != operation.VerificationPolicyUID || binding.Digest != operation.VerificationPolicyDigest {
			if bindingErr == nil {
				bindingErr = errors.New("verification policy object changed after the operation claim")
			}
			return mutationlifecycle.Stop(r.verificationPolicyChanged(ctx, schema, bindingErr))
		}
	}
	if !schemaOperation(operation).Mutating {
		return mutationlifecycle.Proceed()
	}
	plan, err := r.currentPlan(ctx, schema)
	if err != nil {
		return mutationlifecycle.Stop(r.applyBecameStale(ctx, schema, err))
	}
	if err := r.ensureCurrentExecutionBinding(schema, plan); err != nil {
		return mutationlifecycle.Stop(r.executionBindingChanged(ctx, schema, err))
	}
	content, err := r.Plans.Load(ctx, plan)
	if err != nil {
		return mutationlifecycle.Stop(r.applyBecameStale(ctx, schema, fmt.Errorf("verify plan storage: %w", err)))
	}
	// The plan may have been published by another build of this manager, and
	// the approval and the apply policy were decided on that build's reading
	// of the bytes. This build reads them again before it dispatches; if it
	// reads them differently, the plan is retired and planned again under this
	// reading, which the plan fingerprint binds, so it cannot be the same plan.
	if err := planReadingMatches(schema, plan, content); err != nil {
		return mutationlifecycle.Stop(r.applyBecameStale(ctx, schema, err))
	}
	binding, err := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil || binding.UID != plan.Spec.VerificationPolicyUID || binding.Digest != plan.Spec.VerificationPolicyDigest {
		if err == nil {
			err = errors.New("verification policy object changed")
		}
		return mutationlifecycle.Stop(r.verificationPolicyChanged(ctx, schema, err))
	}
	if planRequiresApproval(schema, plan) {
		valid, err := r.ensureCurrentApproval(ctx, schema, plan, false)
		if err != nil {
			return mutationlifecycle.Stop(ctrl.Result{}, err)
		}
		if !valid {
			return mutationlifecycle.Stop(r.approvalBecameInvalid(ctx, schema))
		}
	}
	d.plan, d.content = plan, content
	return mutationlifecycle.Proceed()
}

func (d *schemaDispatch) AcquireLease(ctx context.Context) mutationlifecycle.Outcome {
	acquired, requeue, err := d.r.acquireOperationLock(ctx, d.schema)
	if err != nil {
		return mutationlifecycle.Stop(ctrl.Result{}, err)
	}
	if !acquired {
		return mutationlifecycle.Stop(ctrl.Result{RequeueAfter: requeue}, nil)
	}
	return mutationlifecycle.Proceed()
}

// Stage projects an Apply's verified plan bytes into the ConfigMaps its Pod
// mounts: the Pod holds no Kubernetes credential and cannot read the chunk
// kind. They are written on every pass that can still create the Job, and the
// controller-write webhook admits them only before the dispatch marker.
func (d *schemaDispatch) Stage(ctx context.Context) mutationlifecycle.Outcome {
	if !schemaOperation(d.operation()).Mutating {
		return mutationlifecycle.Proceed()
	}
	if d.plan == nil {
		return mutationlifecycle.Stop(ctrl.Result{}, errors.New("an Apply reached the plan projection unauthorized"))
	}
	if err := d.r.Plans.Project(ctx, d.plan, d.content); err != nil {
		if errors.Is(err, planstore.ErrProjectionConflict) {
			return mutationlifecycle.Stop(d.r.applyBecameStale(ctx, d.schema, fmt.Errorf("project plan for Apply: %w", err)))
		}
		return mutationlifecycle.Stop(ctrl.Result{}, fmt.Errorf("project plan for Apply: %w", err))
	}
	return mutationlifecycle.Proceed()
}

func (d *schemaDispatch) Build(ctx context.Context) (*batchv1.Job, mutationlifecycle.Outcome) {
	r, schema, operation := d.r, d.schema, d.operation()
	if r.Jobs == nil {
		return nil, mutationlifecycle.Stop(ctrl.Result{}, errors.New("Job builder is not configured"))
	}
	if operation.JobUID != "" {
		if !isReadOnlyOperation(operation) {
			return nil, mutationlifecycle.Stop(ctrl.Result{},
				fmt.Errorf("missing %s Job has an unsupported persisted UID boundary", operation.Type))
		}
		return nil, mutationlifecycle.Stop(r.retryLostReadOnlyJob(ctx, schema))
	}
	job, err := r.Jobs.Build(schema, *operation, d.plan)
	if err != nil {
		if schemaOperation(operation).Mutating {
			return nil, mutationlifecycle.Stop(r.applyBecameStale(ctx, schema, fmt.Errorf("build Apply Job: %w", err)))
		}
		return nil, mutationlifecycle.Stop(r.operationFailure(ctx, schema, fmt.Errorf("build %s Job: %w", operation.Type, err)))
	}
	if job.Namespace != schema.Namespace || job.Name != operation.JobName {
		return nil, mutationlifecycle.Stop(ctrl.Result{}, errors.New("Job builder returned an object outside the operation claim"))
	}
	return job, mutationlifecycle.Proceed()
}

func (d *schemaDispatch) Admission() mutationlifecycle.Admission {
	return podAdmission{reader: d.r.directReader(), namespace: d.schema.Namespace, options: d.r.AdmissionOptions}
}

func (d *schemaDispatch) SaveSnapshot(
	ctx context.Context,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) (ctrl.Result, error) {
	before := d.schema.DeepCopy()
	d.operation().AdmissionSnapshot = snapshot
	if err := d.r.patchStatus(ctx, before, d.schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (d *schemaDispatch) RefreshSnapshot(ctx context.Context) (ctrl.Result, error) {
	return d.r.refreshAdmissionSnapshot(ctx, d.schema)
}

// Refuse ends a claim whose Job cannot be held to its admission envelope. A
// read-only claim whose rebuilt template cannot be matched is discarded and
// read again; everything else is a failure the resource reports and retries.
func (d *schemaDispatch) Refuse(
	ctx context.Context,
	refusal mutationlifecycle.Refusal,
	failure error,
) (ctrl.Result, error) {
	switch refusal {
	case mutationlifecycle.RefusalTemplateUnreadable, mutationlifecycle.RefusalTemplateMoved:
		if !schemaOperation(d.operation()).Mutating {
			return d.r.discardStaleOperation(ctx, d.schema, failure)
		}
	}
	return d.r.operationFailure(ctx, d.schema, failure)
}

// Consume spends the approval, holding it once more to the plan it was
// recorded for.
func (d *schemaDispatch) Consume(ctx context.Context) mutationlifecycle.Outcome {
	if d.plan == nil || !planRequiresApproval(d.schema, d.plan) {
		return mutationlifecycle.Proceed()
	}
	valid, err := d.r.ensureCurrentApproval(ctx, d.schema, d.plan, true)
	if err != nil {
		return mutationlifecycle.Stop(ctrl.Result{}, err)
	}
	if !valid {
		return mutationlifecycle.Stop(d.r.approvalBecameInvalid(ctx, d.schema))
	}
	return mutationlifecycle.Proceed()
}

// Mark records the dispatch boundary. A Plan records beside it the digest of
// the key its Job is sealed to: the Job just built is sealed to this process's
// current public key, and harvest compares its own key against this digest
// rather than opening a payload it may not hold the private key for. A
// boundary a previous process crossed without creating keeps its marker and
// takes this process's digest, since this process builds the Job created
// next.
func (d *schemaDispatch) Mark(ctx context.Context) error {
	before := d.schema.DeepCopy()
	operation := d.operation()
	operation.DispatchStarted = true
	if operation.Type == operatorv1alpha1.OperationPlan {
		operation.PlanSealPublicKeyDigest = planSealPublicKeyDigest(d.r.SealKey.PublicKey())
	}
	return d.r.patchStatus(ctx, before, d.schema)
}

func (d *schemaDispatch) Create(ctx context.Context, job *batchv1.Job) error {
	return d.r.Client.Create(ctx, job)
}

func (d *schemaDispatch) Confirm(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	read := &batchv1.Job{}
	if err := d.r.directReader().Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, read); err != nil {
		return nil, err
	}
	return read, nil
}

func (d *schemaDispatch) Intent(actual, expected *batchv1.Job) error {
	return validateJobIntent(actual, expected, d.schema)
}

func (d *schemaDispatch) Record(ctx context.Context, job *batchv1.Job) (ctrl.Result, error) {
	before := d.schema.DeepCopy()
	operation := d.operation()
	operation.JobUID = job.UID
	if err := d.r.patchStatus(ctx, before, d.schema); err != nil {
		return ctrl.Result{}, err
	}
	d.r.event(d.schema, corev1.EventTypeNormal, "OperationStarted", "%s Job %s started", operation.Type, job.Name)
	if schemaOperation(operation).Mutating && d.r.Telemetry != nil {
		d.r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStarted)
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// Unaccounted retires the Apply as an outcome nobody established. The Job in
// hand is not passed on: the claim has recorded no UID for it, so the
// controller-write guard would refuse its cleanup TTL, and the pending
// observation finds its Pods by the name the claim reserved.
func (d *schemaDispatch) Unaccounted(ctx context.Context, _ *batchv1.Job, failure error) (ctrl.Result, error) {
	return d.r.finishUncertainApply(ctx, d.schema, nil, failure)
}

func (d *schemaDispatch) Retry(ctx context.Context, failure error) (ctrl.Result, error) {
	return d.r.retryOperation(ctx, d.schema, nil, failure)
}

// retryLostReadOnlyJob moves a read-only claim whose recorded Job is gone to a
// fresh attempt, once every Pod that Job owned has stopped. Job deletion does
// not prove an already-created Pod has stopped, so the exact owner UID is
// polled until every old attempt is terminal or gone. The claim already
// crossed its dispatch boundary, and Job admission permits CREATE only while
// the claim records no UID, so the attempt advances to a fresh name before
// anything is created again.
func (r *SchemaReconciler) retryLostReadOnlyJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	stopped, err := podsStopped(ctx, r.directReader(), schema.Namespace, operation.JobName, operation.JobUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
	}
	return r.retryOperation(ctx, schema, nil,
		fmt.Errorf("%s Job %q with persisted UID %q is missing", operation.Type, operation.JobName, operation.JobUID))
}
