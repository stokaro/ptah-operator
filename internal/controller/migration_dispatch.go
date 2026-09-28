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
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// migrationDispatch is the PtahMigration family's side of the dispatch
// sequence mutationlifecycle.Driver runs, for one pass.
type migrationDispatch struct {
	r         *MigrationReconciler
	migration *operatorv1alpha1.PtahMigration
}

var _ mutationlifecycle.Family = (*migrationDispatch)(nil)

func (d *migrationDispatch) operation() *operatorv1alpha1.MigrationOperationStatus {
	return d.migration.Status.ActiveOperation
}

func (d *migrationDispatch) Claim() mutationlifecycle.Claim {
	operation := d.operation()
	kind := migrationOperation(operation)
	return mutationlifecycle.Claim{
		Type:      string(operation.Type),
		Mutating:  kind.Mutating,
		HoldsLock: kind.HoldsLock(false),
		Dispatch: mutationlifecycle.DispatchState{
			DispatchStarted: operation.DispatchStarted,
			JobUID:          string(operation.JobUID),
		},
		Snapshot:          operation.AdmissionSnapshot,
		SnapshotRefreshed: operation.AdmissionSnapshotRefreshed,
	}
}

// Authorize re-reads every input the claim was decided from: a Job is only
// worth creating while the decision behind it still holds.
func (d *migrationDispatch) Authorize(ctx context.Context) mutationlifecycle.Outcome {
	r, migration, operation := d.r, d.migration, d.operation()
	// Suspension first, and before the retry deadline. A person who suspends a
	// resource is asking it to stop now, and a claim waiting out a retry has
	// dispatched nothing -- so making them wait out an interval of up to an
	// hour to have it retired would answer a different question than the one
	// they asked.
	if migration.Spec.Suspend {
		return mutationlifecycle.Stop(r.discardMigrationOperation(ctx, migration, telemetry.OperationStale,
			errors.New("reconciliation was suspended before dispatch")))
	}
	// The inputs are re-read before the deadline is applied, not after it. A
	// person correcting them -- a target Secret named wrong, an artifact
	// reference that does not resolve -- is the usual reason a read-only
	// operation failed at all, and the generation watch re-enters
	// reconciliation the moment they do. Weighing the deadline first would
	// hold that correction behind an interval of up to an hour, waiting out a
	// claim nothing is going to dispatch.
	//
	// A fingerprint that cannot be read is not a correction by itself, and it
	// waits like any other retry. Discarding on every pass that fails to read
	// an input would turn an unreadable Secret into a claim-and-discard loop
	// driven by whatever else the resource watches, which is the tight loop the
	// delay exists to stop.
	//
	// An edit is visible without reading the inputs at all, and that is what
	// separates the two: the API server bumps the generation, and the claim
	// recorded the generation it was made from. So a correction whose new
	// inputs cannot be read yet -- a reference that does not parse, a
	// verification policy nobody has created -- retires the claim now rather
	// than waiting out the interval it was meant to end.
	current, currentErr := r.migrationInputFingerprint(ctx, migration, operation.Type)
	inputsChanged := migration.Generation != migration.Status.ObservedGeneration ||
		(currentErr == nil && current != operation.InputFingerprint)
	// A retried attempt waits out the delay the resource asked for. The check
	// is here rather than only in the requeue that scheduled it, because a
	// restart and an early Job or watch event both re-enter reconciliation
	// immediately and would otherwise dispatch at once -- which is how a
	// failing operation becomes a tight loop against whatever it is failing on.
	if !inputsChanged && !due(operation.RetryNotBefore, r.now()) {
		return mutationlifecycle.Stop(requeueAtDeadline(operation.RetryNotBefore, r.now()), nil)
	}
	if inputsChanged || currentErr != nil {
		if currentErr == nil {
			currentErr = errors.New("the operation inputs changed after the claim")
		}
		return mutationlifecycle.Stop(r.discardMigrationOperation(ctx, migration, telemetry.OperationStale, currentErr))
	}
	// The plan an Apply names was decided under a verification policy, and a
	// policy replaced since is invisible to the input fingerprint, which names
	// the policy object and not its identity or content.
	if operation.Type == operatorv1alpha1.MigrationOperationApply {
		if err := r.claimedApplyPolicyStillBinds(ctx, migration, operation); err != nil {
			return mutationlifecycle.Stop(r.discardMigrationOperation(ctx, migration, telemetry.OperationStale, err))
		}
	}
	return mutationlifecycle.Proceed()
}

func (d *migrationDispatch) AcquireLease(ctx context.Context) mutationlifecycle.Outcome {
	acquired, requeue, err := d.r.acquireMigrationApplyLock(ctx, d.migration)
	if err != nil {
		return mutationlifecycle.Stop(ctrl.Result{}, err)
	}
	if !acquired {
		return mutationlifecycle.Stop(ctrl.Result{RequeueAfter: requeue}, nil)
	}
	return mutationlifecycle.Proceed()
}

// Stage has nothing to write: a migration plan carries no SQL, and the Apply
// reads its files from the verified artifact.
func (d *migrationDispatch) Stage(context.Context) mutationlifecycle.Outcome {
	return mutationlifecycle.Proceed()
}

func (d *migrationDispatch) Build(ctx context.Context) (*batchv1.Job, mutationlifecycle.Outcome) {
	r, migration, operation := d.r, d.migration, d.operation()
	if r.Jobs == nil {
		return nil, mutationlifecycle.Stop(ctrl.Result{}, errors.New("Job builder is not configured"))
	}
	if operation.JobUID != "" {
		// A persisted UID proves this attempt already crossed its dispatch
		// boundary. Admission permits CREATE only while the claim has no UID,
		// so advance to a fresh attempt and a fresh deterministic name rather
		// than recreating the Job this claim already had, once every Pod that
		// Job owned has stopped.
		stopped, err := podsStopped(ctx, r.directReader(), migration.Namespace, operation.JobName, operation.JobUID)
		if err != nil {
			return nil, mutationlifecycle.Stop(ctrl.Result{}, err)
		}
		if !stopped {
			return nil, mutationlifecycle.Stop(ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil)
		}
		return nil, mutationlifecycle.Stop(r.retryMigrationOperation(ctx, migration, nil,
			fmt.Errorf("%s Job %q with persisted UID %q is missing", operation.Type, operation.JobName, operation.JobUID)))
	}
	plan, err := r.migrationPlanForJob(ctx, migration, operation)
	if err != nil {
		return nil, mutationlifecycle.Stop(r.discardMigrationOperation(ctx, migration, telemetry.OperationStale, err))
	}
	job, err := r.Jobs.BuildMigration(migration, *operation, plan)
	if err != nil {
		return nil, mutationlifecycle.Stop(r.migrationOperationFailure(ctx, migration,
			fmt.Errorf("build %s Job: %w", operation.Type, err)))
	}
	if job.Namespace != migration.Namespace || job.Name != operation.JobName {
		return nil, mutationlifecycle.Stop(ctrl.Result{},
			errors.New("the Job builder returned an object outside the operation claim"))
	}
	return job, mutationlifecycle.Proceed()
}

func (d *migrationDispatch) Admission() mutationlifecycle.Admission {
	return podAdmission{reader: d.r.directReader(), namespace: d.migration.Namespace, options: d.r.AdmissionOptions}
}

func (d *migrationDispatch) SaveSnapshot(
	ctx context.Context,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) (ctrl.Result, error) {
	before := d.migration.DeepCopy()
	d.operation().AdmissionSnapshot = snapshot
	if err := d.r.patchMigrationStatus(ctx, before, d.migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// RefreshSnapshot drops a snapshot whose template moved. Nothing was
// dispatched and the claim's inputs still hold, so what differs is the manager
// that built the template: its recorded identity, and anything else the new
// release changed in the Job it builds. Neither binds the claim, so the claim
// stands and its snapshot is resolved again.
func (d *migrationDispatch) RefreshSnapshot(ctx context.Context) (ctrl.Result, error) {
	before := d.migration.DeepCopy()
	operation := d.operation()
	operation.AdmissionSnapshot = nil
	operation.AdmissionSnapshotRefreshed = true
	if err := d.r.patchMigrationStatus(ctx, before, d.migration); err != nil {
		return ctrl.Result{}, err
	}
	d.r.event(d.migration, corev1.EventTypeNormal, "AdmissionSnapshotRefreshed",
		"%s Job Pod template changed before dispatch; resolving its admission snapshot again", operation.Type)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (d *migrationDispatch) Refuse(
	ctx context.Context,
	_ mutationlifecycle.Refusal,
	failure error,
) (ctrl.Result, error) {
	return d.r.migrationOperationFailure(ctx, d.migration, failure)
}

func (d *migrationDispatch) Consume(ctx context.Context) mutationlifecycle.Outcome {
	if err := d.r.consumeMigrationApproval(ctx, d.migration, d.operation().ApprovalRef); err != nil {
		return mutationlifecycle.Stop(ctrl.Result{}, err)
	}
	return mutationlifecycle.Proceed()
}

func (d *migrationDispatch) Mark(ctx context.Context) error {
	before := d.migration.DeepCopy()
	d.operation().DispatchStarted = true
	return d.r.patchMigrationStatus(ctx, before, d.migration)
}

func (d *migrationDispatch) Create(ctx context.Context, job *batchv1.Job) error {
	return d.r.Client.Create(ctx, job)
}

func (d *migrationDispatch) Confirm(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	read := &batchv1.Job{}
	if err := d.r.directReader().Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, read); err != nil {
		return nil, err
	}
	return read, nil
}

func (d *migrationDispatch) Intent(actual, expected *batchv1.Job) error {
	return validateMigrationJobIntent(actual, expected, d.migration)
}

func (d *migrationDispatch) Record(ctx context.Context, job *batchv1.Job) (ctrl.Result, error) {
	before := d.migration.DeepCopy()
	operation := d.operation()
	operation.JobUID = job.UID
	if err := d.r.patchMigrationStatus(ctx, before, d.migration); err != nil {
		return ctrl.Result{}, err
	}
	d.r.event(d.migration, corev1.EventTypeNormal, "OperationStarted", "%s Job %s started", operation.Type, job.Name)
	if migrationOperation(operation).Mutating && d.r.Telemetry != nil {
		d.r.Telemetry.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyStarted)
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (d *migrationDispatch) Unaccounted(ctx context.Context, job *batchv1.Job, failure error) (ctrl.Result, error) {
	return d.r.finishUncertainMigrationApply(ctx, d.migration, job, failure, "")
}

func (d *migrationDispatch) Retry(ctx context.Context, failure error) (ctrl.Result, error) {
	return d.r.retryMigrationOperation(ctx, d.migration, nil, failure)
}
