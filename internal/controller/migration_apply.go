package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// migrationApplyLeaseGrace is how much of the Lease is held back from the Pod's
// own deadline, so the Pod cannot still be running when the Lease it was
// dispatched under expires.
const migrationApplyLeaseGrace = time.Minute

// reconcileMigrationApplyDecision decides what a published plan may do next.
//
// It is the only place that turns evidence into permission to run SQL, so
// everything it needs is re-read here rather than remembered: the plan, the
// history it was computed against, and the approval its policy requires.
func (r *MigrationReconciler) reconcileMigrationApplyDecision(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	plan, err := r.currentMigrationPlan(ctx, migration)
	if err != nil {
		// A plan that no longer describes this migration is not executed and not
		// repaired: the next reading of the history publishes the plan that does.
		return r.discardMigrationPlan(ctx, migration, err)
	}
	switch migration.Spec.Policy.Apply {
	case operatorv1alpha1.ApplyPolicyNever:
		return requeueAtDeadline(migration.Status.NextReconciliationTime, r.now()), nil
	case operatorv1alpha1.ApplyPolicyAlways:
		return r.claimMigrationApply(ctx, migration, plan, nil)
	}
	approval, err := r.findMigrationApproval(ctx, migration, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if approval == nil {
		return requeueAtDeadline(migration.Status.NextReconciliationTime, r.now()), nil
	}
	return r.claimMigrationApply(ctx, migration, plan, approval)
}

// currentMigrationPlan re-reads the published plan and refuses one the current
// evidence no longer supports.
func (r *MigrationReconciler) currentMigrationPlan(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (*operatorv1alpha1.PtahMigrationPlan, error) {
	if migration.Status.Plan == nil {
		return nil, errors.New("the migration has no published plan")
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: migration.Status.Plan.Name}
	if err := r.directReader().Get(ctx, key, plan); err != nil {
		return nil, fmt.Errorf("read the current migration plan: %w", err)
	}
	if plan.UID != migration.Status.Plan.UID || plan.DeletionTimestamp != nil {
		return nil, errors.New("the published plan was replaced or is being deleted")
	}
	if plan.Spec.ContractVersion != migrationplan.ContractVersion {
		return nil, errors.New("the published plan uses a contract version this manager does not publish")
	}
	binding := migration.Status.ExecutionBinding
	history := migration.Status.History
	if binding == nil || history == nil || migration.Status.Artifact == nil {
		return nil, errors.New("the migration lost the evidence its plan was computed from")
	}
	if plan.Spec.HistoryFingerprint != history.Fingerprint {
		return nil, errors.New("the database's history moved after the plan was published")
	}
	if plan.Spec.TargetIdentityDigest != history.TargetIdentityDigest {
		return nil, errors.New("the database identity changed after the plan was published")
	}
	if plan.Spec.ArtifactDigest != migration.Status.Artifact.Digest {
		return nil, errors.New("the resolved artifact changed after the plan was published")
	}
	if plan.Spec.ExecutionBindingID != binding.Epoch ||
		plan.Spec.ControllerImage != binding.ControllerImage ||
		plan.Spec.ControllerRevision != binding.ControllerRevision ||
		plan.Spec.ControllerStateVersion != binding.ControllerStateVersion ||
		plan.Spec.PtahVersion != binding.PtahVersion ||
		plan.Spec.ExecutorImage != binding.ExecutorImage ||
		plan.Spec.RunnerImage != binding.RunnerImage ||
		plan.Spec.RunnerProtocolVersion != binding.RunnerProtocolVersion {
		return nil, errors.New("an execution component changed after the plan was published")
	}
	policyFingerprint, err := migrationPolicyFingerprint(migration)
	if err != nil || policyFingerprint != plan.Spec.PolicyFingerprint {
		return nil, errors.New("the apply policy changed after the plan was published")
	}
	if err := r.verificationPolicyStillBinds(ctx, migration, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// verificationPolicyStillBinds re-reads the live verification policy and
// refuses a plan that was decided under another one.
//
// The plan records the policy object's UID and the digest of its content, and
// the admission that accepted the approval compared both -- once. Nothing
// repeated that comparison afterwards, so deleting the immutable ConfigMap and
// recreating it under the same name left the approval authorizing an Apply
// under terms that no longer exist. The UID is what catches exactly that: a
// replacement carries a new one even when the bytes are identical.
func (r *MigrationReconciler) verificationPolicyStillBinds(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) error {
	binding, err := policy.ConfigMapBinding(
		ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom,
	)
	if err != nil {
		return fmt.Errorf("read the verification policy the plan was decided under: %w", err)
	}
	if binding.UID != plan.Spec.VerificationPolicyUID || binding.Digest != plan.Spec.VerificationPolicyDigest {
		return errors.New("the verification policy was replaced after the plan was published")
	}
	return nil
}

// claimedApplyPolicyStillBinds is the same question at the dispatch boundary,
// asked about the plan the claim itself named.
//
// The claim's input fingerprint names the policy object -- its ConfigMap name
// and key -- and never its identity or its content, so a policy replaced
// between the decision and this instant is invisible to it. An undispatched
// claim is therefore re-checked here rather than trusted.
func (r *MigrationReconciler) claimedApplyPolicyStillBinds(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) error {
	if operation.PlanRef == nil {
		return errors.New("the Apply claim names no plan to check the verification policy against")
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.PlanRef.Name}
	if err := r.directReader().Get(ctx, key, plan); err != nil {
		return fmt.Errorf("read the plan the Apply claim named: %w", err)
	}
	if plan.UID != operation.PlanRef.UID {
		return errors.New("the plan the Apply claim named was replaced")
	}
	return r.verificationPolicyStillBinds(ctx, migration, plan)
}

// findMigrationApproval returns the one approval that authorizes this exact
// plan, and nil when none does. An approval that names another plan, another
// history, or another execution binding is not one of them.
func (r *MigrationReconciler) findMigrationApproval(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*operatorv1alpha1.PtahMigrationApproval, error) {
	approvals := &operatorv1alpha1.PtahMigrationApprovalList{}
	if err := r.directReader().List(ctx, approvals, client.InNamespace(migration.Namespace)); err != nil {
		return nil, fmt.Errorf("list migration approvals: %w", err)
	}
	var found *operatorv1alpha1.PtahMigrationApproval
	for index := range approvals.Items {
		approval := &approvals.Items[index]
		if approval.DeletionTimestamp != nil ||
			approval.Spec.MigrationRef.UID != migration.UID ||
			approval.Spec.PlanRef.UID != plan.UID ||
			approval.Spec.PlanFingerprint != plan.Spec.Fingerprint ||
			approval.Spec.HistoryFingerprint != plan.Spec.HistoryFingerprint ||
			approval.Spec.ExecutionBindingID != plan.Spec.ExecutionBindingID ||
			approval.Spec.TargetIdentityDigest != plan.Spec.TargetIdentityDigest ||
			approval.Spec.ArtifactDigest != plan.Spec.ArtifactDigest ||
			approval.Spec.PolicyFingerprint != plan.Spec.PolicyFingerprint ||
			strings.TrimSpace(approval.Spec.Approver.Username) == "" ||
			strings.TrimSpace(approval.Spec.MutationRequestUID) == "" ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
			continue
		}
		if found != nil {
			// Two approvals for one plan is not an error the controller resolves
			// by picking one: the oldest is the decision, and a later duplicate
			// changes nothing about it.
			if approval.CreationTimestamp.Time.Before(found.CreationTimestamp.Time) {
				found = approval
			}
			continue
		}
		found = approval
	}
	return found, nil
}

// claimMigrationApply writes the claim that authorizes one execution. Every
// bound the run is held to -- the Lease it serializes under, the instant after
// which it may not dispatch, and the instant after which it may not still be
// running -- is decided here and persisted before the Job exists.
//
// The run itself is `ptah migrations up`, which applies everything the artifact
// has and the database does not; there is no target-version flag to bound it to
// the planned sequence. The artifact is digest-pinned, so the migrations that
// could run are drawn from exactly the file set the plan enumerated, and a
// history that only advanced can have removed some of them -- that case applies
// a subset of the plan and the history read afterwards says which.
//
// A history that went backwards is the case this does not cover. Restore the
// database to an earlier version between the plan and the run and `migrations
// up` selects the versions that became pending again, which is a superset of
// what was approved. The claim's bindings travel to the runner and the runner
// refuses a child that names no approved sequence at all, but bounding the run
// to that sequence needs an executor that accepts one. Until then the plan's
// premise is checked against the history this controller last read, not
// against the history under the lock the mutation takes.
func (r *MigrationReconciler) claimMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
	approval *operatorv1alpha1.PtahMigrationApproval,
) (ctrl.Result, error) {
	if migration.Status.ActiveOperation != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	inputFingerprint, err := r.migrationInputFingerprint(ctx, migration, operatorv1alpha1.MigrationOperationApply)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, err)
	}
	id, err := r.newMigrationOperationID(inputFingerprint)
	if err != nil {
		return ctrl.Result{}, err
	}
	startedAt := r.now()
	leaseDuration := migrationLeaseDuration(migration)
	dispatchNotAfter := metav1.NewTime(startedAt.Add(leaseDuration - migrationApplyLeaseGrace))
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:                 operatorv1alpha1.MigrationOperationApply,
		ID:                   id,
		InputFingerprint:     inputFingerprint,
		StartedAt:            metav1.NewTime(startedAt),
		Attempt:              1,
		ExecutionBindingID:   migration.Status.ExecutionBinding.Epoch,
		Source:               migrationSourceBinding(migration),
		CoordinationDigest:   plan.Spec.CoordinationDigest,
		LeaseDurationSeconds: int32(leaseDuration / time.Second),
		LeaseEpoch:           "v1-" + strings.TrimPrefix(id, "sha256:")[:32],
		DispatchNotAfter:     &dispatchNotAfter,
		ExecutionNotAfter:    dispatchNotAfter.DeepCopy(),
		PlanRef:              &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		},
	}
	if approval != nil {
		operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: approval.Name, UID: approval.UID}
	}
	operation.JobName, err = r.Jobs.NameForMigration(migration, *operation)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("name the Apply Job: %w", err))
	}
	if err := r.ensureMigrationFinalizer(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	before := migration.DeepCopy()
	migration.Status.ActiveOperation = operation
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.NextReconciliationTime = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseApplying
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApprovedPlan, fmt.Sprintf("Applying %d planned migrations", len(plan.Spec.Migrations)))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
		operatorv1alpha1.ReasonSatisfied, "The plan's approval requirements are satisfied")
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// acquireMigrationApplyLock serializes execution against every other operator
// operation that addresses the same database. The Lease is taken before the
// Job exists and its epoch is compared on every pass: a result produced across
// an epoch change is not this claim's result.
func (r *MigrationReconciler) acquireMigrationApplyLock(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (bool, time.Duration, error) {
	operation := migration.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply {
		return true, 0, nil
	}
	if r.Locks == nil || strings.TrimSpace(r.LockNamespace) == "" {
		return false, 0, errors.New("the database lock coordinator is not configured")
	}
	if operation.CoordinationDigest == "" || operation.LeaseDurationSeconds < 1 {
		return false, 0, errors.New("the Apply claim carries no database lock binding")
	}
	result, err := r.Locks.Acquire(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace,
		CoordinationDigest:    operation.CoordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: migration.UID, OperationID: operation.ID},
		Duration:              time.Duration(operation.LeaseDurationSeconds) * time.Second,
		ExpectedEpoch:         operation.LeaseEpoch,
	})
	if err != nil {
		return false, 0, fmt.Errorf("acquire the database lock: %w", err)
	}
	if !result.Acquired {
		requeue := maxLockContentionPoll
		if result.Contention != nil && result.Contention.RequeueAfter > 0 {
			requeue = result.Contention.RequeueAfter
		}
		return false, requeue, nil
	}
	if result.Epoch == operation.LeaseEpoch && !result.ContinuityLost {
		return true, 0, nil
	}
	continuityLost := operation.LeaseEpoch == "" || result.ContinuityLost || result.Epoch != operation.LeaseEpoch
	if continuityLost && operation.LeaseEpoch != "" && !operation.DispatchStarted && operation.JobUID == "" {
		// The first acquisition necessarily assigns an epoch the claim could not
		// have known, and reports the loss for that reason. The claim persisted
		// its expected token before any dispatch, so adopting the assigned epoch
		// here cannot validate work that already ran.
		continuityLost = false
	}
	before := migration.DeepCopy()
	migration.Status.ActiveOperation.LeaseEpoch = result.Epoch
	migration.Status.ActiveOperation.LeaseContinuityLost = continuityLost
	if continuityLost {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonLeaseContinuityLost, "The database lock epoch changed under the running operation")
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return false, 0, err
	}
	return false, statusPatchRequeue, nil
}

// consumeMigrationRun records what the database accounted for. The verdict is
// the one Ptah read from the revision table, never the Job's exit status: a run
// that stopped is exactly the run whose controller has to be told what the
// database now holds.
func (r *MigrationReconciler) consumeMigrationRun(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	result runner.Result,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	report := result.MigrationRun
	outcome := operatorv1alpha1.MigrationRunOutcomeUnknown
	message := "The run produced no readable evidence"
	var applied []int64
	if report != nil {
		outcome = migrationRunOutcome(report.Outcome)
		applied = boundedVersions(report.Applied, 256)
		message = migrationRunMessage(*report)
	}
	finishedAt := metav1.NewTime(r.now())
	before := migration.DeepCopy()
	migration.Status.LastRun = &operatorv1alpha1.MigrationRunStatus{
		Outcome:         outcome,
		JobName:         job.Name,
		JobUID:          job.UID,
		StartedAt:       operation.StartedAt,
		FinishedAt:      &finishedAt,
		AppliedVersions: applied,
		Message:         bounded(message, 1024),
	}
	migration.Status.ActiveOperation = nil
	migration.Status.Plan = nil
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.MigrationRunOutcomeUnknown:
		// Neither may be retried. A partial run committed some of a migration's
		// statements and not the rest, and an unknown one cannot say whether it
		// did; running the same file again would run those statements twice.
		recordUnresolvedMigrationRun(migration, operation, migration.Status.LastRun, result.TargetIdentityDigest, r.now())
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, bounded(message, 1024))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, "The database has to be read by a person before anything else runs")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, "The run stopped and may not be retried")
		next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
		migration.Status.NextReconciliationTime = &next
	default:
		// Every other outcome is confirmed by reading the history back. What the
		// run claims and what the revision table holds are two statements, and
		// only the second one settles the resource.
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifyingHistory
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonVerifyingConvergence, "Reading the history back to confirm what the run did")
	}
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, migrationRunEventType(outcome), "MigrationRunFinished", "%s: %s", outcome, bounded(message, 256))
	r.releaseMigrationApplyLock(ctx, migration, operation)
	return ctrl.Result{Requeue: true}, nil
}

// recordUnresolvedMigrationRun latches the run whose effect on the database
// nobody established, and names what a person has to go and look at: the
// attempt, the Job it ran as, the plan it was carrying out, and the database it
// addressed.
//
// It is a record rather than a condition because a condition is about now. Any
// later refusal rewrites the reason this run left, and a refusal that was
// rewritten says nothing about whether the mutation was ever accounted for.
// Only a reading of that same database with nothing pending removes it.
func recordUnresolvedMigrationRun(
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	run *operatorv1alpha1.MigrationRunStatus,
	reportedTarget string,
	now time.Time,
) {
	if run == nil {
		return
	}
	unresolved := &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome:    run.Outcome,
		JobName:    run.JobName,
		JobUID:     run.JobUID,
		RecordedAt: metav1.NewTime(now),
	}
	if operation != nil {
		unresolved.OperationID = operation.ID
		unresolved.PlanRef = operation.PlanRef.DeepCopy()
	}
	// Which database to name is the whole point of the record, so the run's own
	// account of it wins. A result frame reports the target the executor opened,
	// and that is not always the one the plan was computed against: Secret
	// content can rotate between the history reading and the Apply. Recording
	// the planned database instead would let a clean reading of it settle a run
	// that never touched it, while a reading of the database that was touched
	// could not match what was stored.
	unresolved.TargetIdentityDigest = reportedTarget
	if unresolved.TargetIdentityDigest == "" {
		// No frame, or one that named no target: the best that is known is the
		// database the plan was computed against, which is the last history
		// this resource read.
		if history := migration.Status.History; history != nil {
			unresolved.TargetIdentityDigest = history.TargetIdentityDigest
		}
	}
	migration.Status.UnresolvedRun = unresolved
}

// releaseMigrationApplyLock hands the database back. A failure to release is
// not a failure of the run: the Lease expires on its own, and reporting the
// run's evidence matters more than the tidy release.
//
// The epoch the claim persisted travels with the request, because releasing
// without one is not a weaker release -- it is no release at all. Release
// refuses a request that names no epoch, so the Lease kept its holder until it
// expired, and every other claimant on that database waited out the full lease
// duration for a run that had already finished.
func (r *MigrationReconciler) releaseMigrationApplyLock(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) {
	if r.Locks == nil || operation == nil || operation.CoordinationDigest == "" {
		return
	}
	if err := r.Locks.Release(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace,
		CoordinationDigest:    operation.CoordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: migration.UID, OperationID: operation.ID},
		Duration:              time.Duration(operation.LeaseDurationSeconds) * time.Second,
		ExpectedEpoch:         operation.LeaseEpoch,
	}); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not release the database lock", "error", err.Error())
	}
}

// dispatchedApplyMayStillWrite reports whether the Apply this claim dispatched
// could still be executing SQL: its Job has not reached a terminal condition,
// or a Pod that Job owns has not stopped. The identity it asks that about comes
// from the claim, from the Job the caller already read, or -- when the claim
// never recorded a UID -- from the API server under the name the claim
// reserved.
//
// It decides whether the database may be handed back. An uncertain outcome
// says nothing about whether the executor is still running, and releasing the
// Lease under a live Pod is the one thing the Lease exists to prevent -- the
// next claimant acquires it and runs DDL beside that executor. So a Pod that
// has not stopped, and a read that could not say, both keep the Lease until it
// expires; migrationApplyLeaseGrace is what makes the expiry outlive the Pod.
func (r *MigrationReconciler) dispatchedApplyMayStillWrite(
	ctx context.Context,
	namespace string,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
) bool {
	jobName, jobUID := operation.JobName, operation.JobUID
	if jobUID == "" && job != nil {
		jobName, jobUID = job.Name, job.UID
	}
	if jobUID == "" && jobName == "" {
		// Nothing was named and nothing was created, so nothing can be writing.
		return false
	}
	if job == nil || job.UID != jobUID {
		// The caller had no snapshot of this claim's Job, or held a different
		// object under the same name. Either way whether that Job can still
		// start a Pod is unanswered, and the Pod list does not answer it: a Job
		// the scheduler has not reached owns no Pod yet and is still about to
		// run SQL. The continuity-loss path reaches here with a UID and no
		// snapshot, so reading the Job is what keeps the Lease held.
		//
		// A claim with no UID is not evidence that nothing was created either.
		// A create that fails with anything other than AlreadyExists leaves the
		// question open: a client timeout or a 5xx after the write persisted
		// leaves a Job running under the name this claim reserved, and nothing
		// collects it while the migration lives.
		job = nil
		if jobName != "" {
			dispatched := &batchv1.Job{}
			key := types.NamespacedName{Namespace: namespace, Name: jobName}
			switch err := r.directReader().Get(ctx, key, dispatched); {
			case apierrors.IsNotFound(err):
				// Nothing stands under the reserved name. With no UID recorded
				// that is as close as the controller comes to proof the create
				// never landed; with one, this claim's Job is gone and only the
				// Pods it owned can still be running, which the read below
				// settles.
				if jobUID == "" {
					return false
				}
			case err != nil:
				ctrl.LoggerFrom(ctx).Info("could not read the dispatched Apply Job", "error", err.Error())
				return true
			default:
				if jobUID == "" || dispatched.UID == jobUID {
					job, jobName, jobUID = dispatched, dispatched.Name, dispatched.UID
				}
				// Otherwise a different object holds the name, so this claim's
				// Job is gone and only its Pods matter.
			}
		}
	}
	if job != nil && job.UID == jobUID && !jobTerminal(job) {
		return true
	}
	pods, err := podsOwnedByJob(ctx, r.directReader(), namespace, jobName, jobUID)
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("could not read the dispatched Apply's Pods", "error", err.Error())
		return true
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return true
		}
	}
	return false
}

// finishUncertainMigrationApply is where an Apply goes when the controller
// cannot read what it did. The claim is retired and the resource is blocked:
// the database is the only thing that can settle it, and nothing dispatches
// again until a person has looked.
//
// reportedTarget is the database the run said it opened, and is empty wherever
// no result frame was read -- which is most of the ways in. A run that reached
// a database the plan was never computed against arrives here through exactly
// one of them, and that is the case where naming the planned database instead
// would be wrong: a clean reading of it would settle a run that never touched
// it, and a reading of the database that was touched could never match.
func (r *MigrationReconciler) finishUncertainMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
	reportedTarget string,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	before := migration.DeepCopy()
	finishedAt := metav1.NewTime(r.now())
	run := &operatorv1alpha1.MigrationRunStatus{
		Outcome:    operatorv1alpha1.MigrationRunOutcomeUnknown,
		StartedAt:  operation.StartedAt,
		FinishedAt: &finishedAt,
		Message:    bounded(failure.Error(), 1024),
	}
	if job != nil {
		run.JobName = job.Name
		run.JobUID = job.UID
	}
	migration.Status.LastRun = run
	recordUnresolvedMigrationRun(migration, operation, run, reportedTarget, r.now())
	migration.Status.ActiveOperation = nil
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, bounded(failure.Error(), 1024))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "What the dispatched run did is unknown until the database is read")
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "The dispatched run is over and may not be retried")
	next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
	migration.Status.NextReconciliationTime = &next
	if job != nil {
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "MigrationRunUncertain", "%v", failure)
	if !r.dispatchedApplyMayStillWrite(ctx, migration.Namespace, operation, job) {
		r.releaseMigrationApplyLock(ctx, migration, operation)
	}
	return ctrl.Result{}, nil
}

// discardMigrationPlan drops a plan the current evidence no longer supports.
// The next reading of the history publishes the plan that does.
func (r *MigrationReconciler) discardMigrationPlan(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhasePending
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPlanNoLongerCurrent, bounded(failure.Error(), 512))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPlanNoLongerCurrent, bounded(failure.Error(), 512))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// consumeMigrationApproval records that the decision was acted on. An approval
// authorizes one execution: a second Apply needs a second decision.
func (r *MigrationReconciler) consumeMigrationApproval(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	reference *operatorv1alpha1.ImmutableObjectReference,
) error {
	if reference == nil {
		return nil
	}
	approval := &operatorv1alpha1.PtahMigrationApproval{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: reference.Name}
	if err := r.directReader().Get(ctx, key, approval); err != nil {
		return client.IgnoreNotFound(err)
	}
	if approval.UID != reference.UID ||
		meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
		return nil
	}
	before := approval.DeepCopy()
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type:               operatorv1alpha1.ConditionApprovalConsumed,
		Status:             metav1.ConditionTrue,
		Reason:             string(operatorv1alpha1.ReasonDispatchCommitted),
		Message:            "The approved plan was dispatched",
		ObservedGeneration: approval.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	approval.Status.ObservedGeneration = approval.Generation
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record the consumed approval: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) newMigrationOperationID(inputFingerprint string) (string, error) {
	nonce, err := randomNonce()
	if err != nil {
		return "", err
	}
	return fingerprint.DigestCanonicalJSON(map[string]string{"input": inputFingerprint, "nonce": nonce})
}

func migrationRunOutcome(outcome string) operatorv1alpha1.MigrationRunOutcome {
	switch outcome {
	case dataplane.MigrationOutcomeUpToDate:
		return operatorv1alpha1.MigrationRunOutcomeUpToDate
	case dataplane.MigrationOutcomeApplied, dataplane.MigrationOutcomeDryRun:
		return operatorv1alpha1.MigrationRunOutcomeApplied
	case dataplane.MigrationOutcomeFailed:
		return operatorv1alpha1.MigrationRunOutcomeFailed
	case dataplane.MigrationOutcomePartial:
		return operatorv1alpha1.MigrationRunOutcomePartial
	default:
		return operatorv1alpha1.MigrationRunOutcomeUnknown
	}
}

// migrationRunMessage says what happened without carrying database rows or the
// SQL a migration ran.
func migrationRunMessage(report dataplane.MigrationRunReport) string {
	switch report.Outcome {
	case dataplane.MigrationOutcomeUpToDate:
		return "The database already had every planned migration"
	case dataplane.MigrationOutcomeApplied:
		return fmt.Sprintf("%d migrations are recorded applied", len(report.Applied))
	case dataplane.MigrationOutcomeDryRun:
		return "The run was a dry run and changed nothing"
	case dataplane.MigrationOutcomeFailed:
		return fmt.Sprintf("A migration failed and committed nothing; %d migrations before it are recorded applied", len(report.Applied))
	case dataplane.MigrationOutcomePartial:
		return "A migration committed some of its statements and not the rest; retrying the file would run them twice"
	default:
		return "The database's own account of the run could not be read"
	}
}

func migrationRunEventType(outcome operatorv1alpha1.MigrationRunOutcome) string {
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomeApplied, operatorv1alpha1.MigrationRunOutcomeUpToDate:
		return corev1.EventTypeNormal
	default:
		return corev1.EventTypeWarning
	}
}

func migrationLeaseDuration(migration *operatorv1alpha1.PtahMigration) time.Duration {
	deadline := time.Duration(migrationActiveDeadline(migration)) * time.Second
	return deadline + migrationApplyLeaseGrace
}

func migrationActiveDeadline(migration *operatorv1alpha1.PtahMigration) int64 {
	if migration.Spec.Execution.ActiveDeadlineSeconds > 0 {
		return migration.Spec.Execution.ActiveDeadlineSeconds
	}
	// The builder's own default, in the same unit the CRD uses.
	return 900
}
