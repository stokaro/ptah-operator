package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
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
		plan.Spec.ControllerStateVersion != binding.ControllerStateVersion ||
		plan.Spec.PtahVersion != binding.PtahVersion ||
		plan.Spec.ExecutorImage != binding.ExecutorImage ||
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
// plan, and nil when none does. An approval that names another migration,
// another plan, or this plan by a fingerprint it does not have is not one of
// them. The fingerprint binds the history, the sequence, the artifact, the
// policy and the execution binding the plan was computed under, so a plan
// computed under any other premise is another plan with another fingerprint,
// and the approval carries no copy of those to compare.
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
			strings.TrimSpace(approval.Spec.PlanFingerprint) == "" ||
			approval.Spec.PlanFingerprint != plan.Spec.Fingerprint ||
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
// The run itself is `ptah migrations up --expect-sequence`. The claim's plan
// travels to the runner, which hands Ptah the approved sequence, and Ptah
// compares it with what it selects under the migration lock the mutation takes.
// A history that moved after the approval, forwards or backwards, selects
// something else, and Ptah refuses before it changes anything: the run reads as
// Failed with nothing applied, and the history read that follows every failed
// run decides what comes next. What this controller checks here is the history
// it last read; the check that decides is the one under the lock.
func (r *MigrationReconciler) claimMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
	approval *operatorv1alpha1.PtahMigrationApproval,
) (ctrl.Result, error) {
	if migration.Status.ActiveOperation != nil {
		// Unreachable in the normal dispatch: reconcile's own ActiveOperation
		// check sends a resource carrying one to reconcileActiveMigration before
		// this function is ever called. Requeuing rather than erroring keeps
		// that true if it ever stops being true.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	// A run nobody accounted for refuses the next one here, where the claim is
	// taken, rather than only through the state the History reading leaves
	// behind.
	//
	// The record already blocks an Apply: a reading that refuses to settle it
	// puts the resource in Blocked and clears the plan, and a claim with no
	// plan cannot be built. But that is two cooperating sites holding one
	// invariant, and each of them is about something else -- one publishes a
	// phase, the other publishes a plan. Either could be given a branch that
	// leaves a plan standing, and the invariant would go with it silently. The
	// record is the authority on whether the database may be written again, so
	// it is read where that question is answered.
	if unresolved := migration.Status.UnresolvedRun; unresolved != nil {
		// Blocked rather than an error: this is where the resource belongs
		// while the record stands, and a pass that refuses without saying so
		// leaves a resource that reads as ready and never moves.
		return r.migrationBlocked(ctx, migration, operatorv1alpha1.ReasonApplyOutcomeUnknown,
			fmt.Sprintf("A %s run nobody accounted for is recorded against this database; "+
				"establish what it did before another Apply", unresolved.Outcome))
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
	dispatchNotAfter := metav1.NewTime(startedAt.Add(migrationApplyWindow(migration)))
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
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
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
	if !migrationOperation(operation).HoldsLock(false) {
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
	if continuityLost && operation.LeaseEpoch != "" && !migrationMayHaveDispatched(operation) {
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

// reportedMigrationApply is what a migration Apply's runner said about it, in
// the terms the controller decides by. It comes from the result frame, or, when
// the log held no frame, from the runner's termination summary; one decision,
// settleMigrationApply, reads either, so the summary cannot be taken where the
// frame would have been refused.
type reportedMigrationApply struct {
	// run is what the database accounted for, and nil when the runner decoded
	// no report.
	run *migrationRunAccount
	// failure is why there is no report, when there is none.
	failure              error
	uncertain            bool
	coordinationDigest   string
	targetIdentityDigest string
}

// migrationRunAccount is a run report in the terms status keeps.
type migrationRunAccount struct {
	outcome operatorv1alpha1.MigrationRunOutcome
	applied []int64
	message string
}

// frameMigrationApply is what a result frame says about a migration Apply.
func (r *MigrationReconciler) frameMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	result runner.Result,
) reportedMigrationApply {
	reported := reportedMigrationApply{
		uncertain:            result.Uncertain,
		coordinationDigest:   result.CoordinationDigest,
		targetIdentityDigest: result.TargetIdentityDigest,
	}
	if result.MigrationRun != nil {
		run := r.reportedMigrationRun(ctx, migration, result.MigrationRun)
		reported.run = &run
		return reported
	}
	reported.failure = errors.New("the Apply produced no readable account of what the database now holds")
	if result.Error != nil {
		reported.failure = fmt.Errorf("%s: %s", result.Error.Code, bounded(result.Error.Message, 512))
	}
	return reported
}

// settleMigrationApply decides a migration Apply from what its runner reported.
//
// A run with no report is one nobody accounted for. A run whose report the
// database accounted for is taken only from the database the claim named; a
// partial or unknown one is taken whatever it names, because it blocks the
// resource either way and is settled only by a reading of the database.
func (r *MigrationReconciler) settleMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	reported reportedMigrationApply,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	if reported.run == nil {
		return r.finishUncertainMigrationApply(ctx, migration, job, reported.failure, reported.targetIdentityDigest)
	}
	if !reported.uncertain && (reported.coordinationDigest != operation.CoordinationDigest ||
		operation.Target != nil && reported.targetIdentityDigest != migration.Status.History.TargetIdentityDigest) {
		return r.finishUncertainMigrationApply(ctx, migration, job,
			errors.New("the Apply ran against a database other than the one it was planned for"),
			reported.targetIdentityDigest)
	}
	return r.recordMigrationRun(ctx, migration, job, *reported.run, reported.targetIdentityDigest)
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
	return r.recordMigrationRun(ctx, migration, job,
		r.reportedMigrationRun(ctx, migration, result.MigrationRun), result.TargetIdentityDigest)
}

// reportedMigrationRun is a run report in the terms status keeps. A missing
// report is an unknown outcome.
func (r *MigrationReconciler) reportedMigrationRun(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	report *dataplane.MigrationRunReport,
) migrationRunAccount {
	if report == nil {
		return migrationRunAccount{
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			message: "The run produced no readable evidence",
		}
	}
	run := migrationRunAccount{
		outcome: migrationRunOutcome(report.Outcome),
		applied: boundedVersions(report.Applied, 256),
		message: migrationRunMessage(*report),
	}
	if refusal, refused := r.sequenceRefusal(ctx, migration, migration.Status.ActiveOperation, *report); refused {
		run.message = refusal
	}
	return run
}

// recordMigrationRun writes a finished run into status and retires its claim.
// reportedTarget is the database the run said it opened, which is the one an
// unresolved record has to name.
func (r *MigrationReconciler) recordMigrationRun(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	run migrationRunAccount,
	reportedTarget string,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	outcome := run.outcome
	message := run.message
	finishedAt := metav1.NewTime(r.now())
	before := migration.DeepCopy()
	migration.Status.LastRun = &operatorv1alpha1.MigrationRunStatus{
		Outcome:         outcome,
		JobName:         job.Name,
		JobUID:          job.UID,
		StartedAt:       operation.StartedAt,
		FinishedAt:      &finishedAt,
		AppliedVersions: run.applied,
		DispatchedBy:    dispatcherRecord(job),
		Message:         bounded(message, 1024),
	}
	migration.Status.Plan = nil
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.MigrationRunOutcomeUnknown:
		// Neither may be retried. A partial run committed some of a migration's
		// statements and not the rest, and an unknown one cannot say whether it
		// did; running the same file again would run those statements twice.
		recordUnresolvedMigrationRun(migration, operation, migration.Status.LastRun,
			reportedTarget, r.now())
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
	// The run was read from its one Pod after the Job reached a terminal
	// condition, so nothing it dispatched can still write.
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionAccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, migrationRunEventType(outcome), "MigrationRunFinished", "%s: %s", outcome, bounded(message, 256))
	r.observeMigrationRun(operation, outcome)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// sequenceRefusal names the selection Ptah refused to run.
//
// The runner hands Ptah the approved sequence, and Ptah refuses the run before
// it changes anything when what it selects under the migration lock is not that
// sequence. That reads as a failed run that applied nothing, which is also what
// a first migration failing looks like; what tells them apart is that only the
// refusal selected something other than the plan. The plan is read for its
// list, and when it cannot be read the run keeps the general message rather
// than a guess.
func (r *MigrationReconciler) sequenceRefusal(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	report dataplane.MigrationRunReport,
) (string, bool) {
	if report.Outcome != dataplane.MigrationOutcomeFailed || len(report.Applied) != 0 ||
		operation == nil || operation.PlanRef == nil {
		return "", false
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.PlanRef.Name}
	if err := r.directReader().Get(ctx, key, plan); err != nil || plan.UID != operation.PlanRef.UID {
		return "", false
	}
	approved := make([]int64, 0, len(plan.Spec.Migrations))
	for _, planned := range plan.Spec.Migrations {
		approved = append(approved, planned.Version)
	}
	if slices.Equal(approved, report.Planned) {
		return "", false
	}
	return fmt.Sprintf("Ptah selected %s under the migration lock and the plan approved %s, so it ran nothing: "+
		"the history moved after the approval", versionList(report.Planned), versionList(approved)), true
}

// versionList spells a list of versions for a status message, naming the
// first few and counting the rest so the message keeps its point within its
// bound.
func versionList(versions []int64) string {
	const shown = 8
	parts := make([]string, 0, shown+1)
	for index, version := range versions {
		if index == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(versions)-shown))
			break
		}
		parts = append(parts, strconv.FormatInt(version, 10))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// observeMigrationRun reports what the Apply did.
//
// Partial and Unknown are one signal here. The documented alert watches for an
// apply whose effect on the database nobody established, and a run that
// committed part of a migration and cannot say which part is that case as
// squarely as one that produced no evidence at all: both leave the record only
// a person can clear, and neither may be retried.
//
// Failed is not. A run that reported a failure said what it did, and the
// history read that follows confirms it; it is counted as an operation failure
// and not as an uncertain apply.
func (r *MigrationReconciler) observeMigrationRun(
	operation *operatorv1alpha1.MigrationOperationStatus,
	outcome operatorv1alpha1.MigrationRunOutcome,
) {
	if r.Telemetry == nil {
		return
	}
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.MigrationRunOutcomeUnknown:
		r.Telemetry.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyUncertain)
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageApply, telemetry.FailureUncertain)
		r.observeMigrationOperation(operation, telemetry.OperationUncertain)
	case operatorv1alpha1.MigrationRunOutcomeFailed:
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageApply, telemetry.FailureOperation)
		r.observeMigrationOperation(operation, telemetry.OperationSucceeded)
	default:
		r.Telemetry.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyCompleted)
		r.observeMigrationOperation(operation, telemetry.OperationSucceeded)
	}
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
//
// Both callers retire an Apply claim, and the one place that writes an Apply
// claim names its plan, so the attempt and the plan are always known here.
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
		Outcome:      run.Outcome,
		OperationID:  operation.ID,
		JobName:      run.JobName,
		JobUID:       run.JobUID,
		PlanRef:      *operation.PlanRef,
		DispatchedBy: run.DispatchedBy.DeepCopy(),
		RecordedAt:   metav1.NewTime(now),
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

// targetLockReleaseForMigrationOperation builds the complete credential-free
// release request out of the claim that took the Lease. Every field is
// required: a release that names no epoch is refused outright, and one that
// names no duration cannot reproduce the request that acquired it.
func targetLockReleaseForMigrationOperation(
	operation *operatorv1alpha1.MigrationOperationStatus,
) (*operatorv1alpha1.TargetLockReleaseStatus, error) {
	if operation == nil {
		return nil, fmt.Errorf("persist target lock release: %w", mutationlifecycle.ErrIncompleteBinding)
	}
	return mutationlifecycle.OwedRelease(mutationlifecycle.LockBinding{
		CoordinationDigest:   operation.CoordinationDigest,
		OperationID:          operation.ID,
		LeaseEpoch:           operation.LeaseEpoch,
		LeaseDurationSeconds: operation.LeaseDurationSeconds,
	})
}

// stageMigrationLockRelease records the release on the resource so the caller's
// own status patch carries it. The obligation and the claim it came from move
// in one write, which is what makes the gap between them survivable: a manager
// that stops after the write finds the record and hands the realm back, and
// one that stops before it finds the claim and re-derives the same verdict.
//
// It is never staged for a claim that took no Lease, and refuses to replace a
// release already owed -- two obligations on one resource would mean one of
// them was silently dropped.
func stageMigrationLockRelease(
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) error {
	if migration == nil {
		return fmt.Errorf("persist target lock release: migration is required")
	}
	if operation == nil || operation.LeaseEpoch == "" {
		return nil
	}
	release, err := targetLockReleaseForMigrationOperation(operation)
	if err != nil {
		return err
	}
	if migration.Status.PendingLockRelease != nil {
		return fmt.Errorf("persist target lock release: another release is already pending")
	}
	migration.Status.PendingLockRelease = release
	return nil
}

// completeMigrationPendingLockRelease performs the release the resource owes
// and clears the record only once it succeeded. A failed release leaves the
// record standing, so the next pass tries again rather than leaving the realm
// claimed until the Lease expires.
func (r *MigrationReconciler) completeMigrationPendingLockRelease(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	before := migration.DeepCopy()
	return mutationlifecycle.CompleteRelease(ctx, r.Locks, r.LockNamespace, migrationLockOwner{migration},
		func(ctx context.Context) error { return r.patchMigrationStatus(ctx, before, migration) })
}

// migrationRealmClaim describes a migration claim to the decision about what
// retiring it hands back. No migration claim carries out a proof: the History
// reading that settles a run takes no Lease.
func migrationRealmClaim(operation *operatorv1alpha1.MigrationOperationStatus) mutationlifecycle.RealmClaim {
	if operation == nil {
		return mutationlifecycle.RealmClaim{}
	}
	kind := migrationOperation(operation)
	return mutationlifecycle.RealmClaim{
		Mutating:    kind.Mutating,
		ServesProof: kind.ServesProof,
		Locked:      operation.LeaseEpoch != "",
	}
}

// retireMigrationClaim drops the active claim in one status write. The write
// carries everything the caller recorded on migration since before -- the run,
// the record of a run nobody accounted for, the phase and the conditions --
// and the database release the retirement owes, which
// mutationlifecycle.Retirement decides from the claim as it stood and why it is
// going. The release itself follows the write.
//
// The claim is the only stored thing that names the epoch its Lease was taken
// with, so the obligation moves into the write that drops the claim. A manager
// that stops after the write finds the record and hands the realm back on the
// next pass; one that stops before it finds the claim and reaches the same
// verdict again. Without that, every other claimant of the database waits out
// the whole lease duration -- sixteen minutes by default, and as much as a day
// where the claim asked for one -- for a claim that is already gone.
//
// A migration hands the realm back once nothing its claim dispatched can still
// write, so the caller asks mayStillWrite before the write, not after it: the
// obligation is recorded only where the release is safe, and a Lease left
// under a Pod that may still be running retains the claim and its renewal
// until the API server can confirm that workload stopped.
//
// Staging and releasing are best effort. The write carries the evidence of
// what a run did, which must not be lost to bookkeeping. Where the obligation
// cannot be staged the Lease expires as it would have, and a staged release
// that fails stays recorded for the top of the next pass.
func (r *MigrationReconciler) retireMigrationClaim(
	ctx context.Context,
	before, migration *operatorv1alpha1.PtahMigration,
	disposition mutationlifecycle.Disposition,
	mayStillWrite bool,
) error {
	operation := before.Status.ActiveOperation
	retirement := mutationlifecycle.Retirement{
		Claim:         migrationRealmClaim(operation),
		Disposition:   disposition,
		MayStillWrite: mayStillWrite,
	}
	if retirement.Releases() == mutationlifecycle.OwnerClaim {
		if err := stageMigrationLockRelease(migration, operation); err != nil {
			ctrl.LoggerFrom(ctx).Info("could not record the database release the claim owes", "error", err.Error())
		}
	}
	if disposition == mutationlifecycle.DispositionUnaccounted && mayStillWrite {
		// The record explains what is unknown; the claim still names the live
		// workload, its deadlines and its realm. Losing it would let deletion
		// collect the executor and would stop renewal under an isolated Pod.
		migration.Status.ActiveOperation = operation.DeepCopy()
	} else {
		migration.Status.ActiveOperation = nil
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return err
	}
	if migration.Status.PendingLockRelease == nil {
		return nil
	}
	if err := r.completeMigrationPendingLockRelease(ctx, migration); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not release the database lock", "error", err.Error())
		r.event(migration, corev1.EventTypeWarning, "TargetLockReleaseOwed",
			"the database lock was not released and will be retried: %s", bounded(err.Error(), 256))
	}
	return nil
}

// An unresolved run is never harvested again, even when its original executor
// eventually reports success. Only a fresh History reading or acknowledgment
// can settle that record. This path waits for the workload and releases just
// its claim, preserving the original run evidence throughout.
func (r *MigrationReconciler) reconcileUnaccountedMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	// The common gate treats failed Job/Pod reads as still writing. Renewal
	// must continue there too: an unreadable workload is not a stopped one.
	if r.dispatchedApplyMayStillWrite(ctx, migration.Namespace, migration.Status.ActiveOperation, nil) {
		if _, _, err := r.acquireMigrationApplyLock(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := r.retireMigrationClaim(ctx, migration.DeepCopy(), migration, mutationlifecycle.DispositionUnaccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
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
// has not stopped, and a read that could not say, both keep the original claim
// and its renewable Lease. migrationApplyLeaseGrace is the fallback horizon
// if no manager can continue the renewal.
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
// cannot read what it did. The resource is blocked; the claim remains while
// its workload may still write. A fresh database history or acknowledgment
// settles the record before another Apply can be authorized.
//
// reportedTarget is the database the run said it opened, and is empty wherever
// no result frame was read -- which is most of the ways in. A run that reached
// a database the plan was never computed against arrives here through exactly
// one of them, and that is the case where naming the planned database instead
// would be wrong: a clean reading of it would settle a run that never touched
// it, and a reading of the database that was touched could never match.
//
// Whether the database goes back in the same write depends on whether anything
// the claim dispatched can still write, and that is read here, before the
// write, so the write records the release only where it is safe.
func (r *MigrationReconciler) finishUncertainMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
	reportedTarget string,
) (ctrl.Result, error) {
	mayStillWrite := r.dispatchedApplyMayStillWrite(ctx, migration.Namespace, migration.Status.ActiveOperation, job)
	return r.retireUncertainMigrationApply(ctx, migration, job, failure, reportedTarget, mayStillWrite)
}

// retireUncertainMigrationApply is finishUncertainMigrationApply for a caller
// that already knows whether anything the claim dispatched can still write.
func (r *MigrationReconciler) retireUncertainMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
	reportedTarget string,
	mayStillWrite bool,
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
	switch {
	case job != nil && (operation.JobUID == "" || job.UID == operation.JobUID):
		run.JobName = job.Name
		run.JobUID = job.UID
		run.DispatchedBy = dispatcherRecord(job)
	case operation.JobUID != "":
		// Either no Job was handed in, or the one that was is not this claim's.
		// A Job that took the reserved name after this claim's was gone is a
		// later attempt, and naming it would point whoever has to account for
		// the run at an execution that did not perform it.
		//
		// The Job is gone -- collected, or removed by hand -- and the claim is
		// the only thing left that knows what ran. That is exactly when naming
		// it matters: status.lastRun exists so a person can see what happened
		// without the Job, and the commonest way into this branch is the Job
		// being missing. The UID is taken from the claim rather than the name
		// alone, because it is what separates this attempt from a later one
		// that reused the name.
		//
		// A recorded UID is also the proof a Job existed. A claim that started
		// a dispatch and never recorded one names nothing here, because the
		// name it reserved is not evidence that anything was created under it.
		run.JobName = operation.JobName
		run.JobUID = operation.JobUID
	}
	migration.Status.LastRun = run
	recordUnresolvedMigrationRun(migration, operation, run, reportedTarget, r.now())
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, bounded(failure.Error(), 1024))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "What the dispatched run did is unknown until the database is read")
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "The dispatched run's outcome is unknown and may not be retried")
	next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
	migration.Status.NextReconciliationTime = &next
	if job != nil {
		// Scheduling the Job's cleanup is tidiness; recording a run nobody
		// accounted for is the point. A TTL this manager cannot set leaves a
		// Job to live out its own deadline, which Kubernetes already bounds. A
		// record it cannot write leaves a claim active over a Job a later pass
		// can bind and process -- the exact replay this path exists to refuse.
		//
		// The guard refuses the cleanup patch whenever the claim does not name
		// this Job by UID, which is every Apply that failed before its create
		// was confirmed. So the failure is reported and the record is written.
		if err := r.markJobHarvested(ctx, job); err != nil {
			r.event(migration, corev1.EventTypeWarning, "MigrationJobCleanupDeferred",
				"%s Job %q keeps its own deadline because its cleanup could not be scheduled: %s",
				operation.Type, job.Name, bounded(err.Error(), 512))
		}
	}
	if err := r.retireMigrationClaim(
		ctx, before, migration, mutationlifecycle.DispositionUnaccounted, mayStillWrite,
	); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "MigrationRunUncertain", "%s",
		bounded(failure.Error(), 512))
	if r.Telemetry != nil {
		r.Telemetry.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyUncertain)
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageApply, telemetry.FailureUncertain)
	}
	r.observeMigrationOperation(operation, telemetry.OperationUncertain)
	// The reading that clears this record is the one this return schedules.
	//
	// Nothing else would. A status patch bumps no generation, the primary
	// watch filters on generation, annotations and labels, and the cache
	// resync re-delivers an unchanged object into the same filter. Where a Job
	// survives, marking it for cleanup above is an update to an owned object,
	// and that is what woke the resource -- so the shape that needed the
	// requeue most was the one shape that never got it: an Apply whose create
	// was never confirmed, or whose Job is already gone, leaves no owned
	// object behind to produce an event at all.
	if mayStillWrite {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return requeueAtDeadline(migration.Status.NextReconciliationTime, r.now()), nil
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
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
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
	// One increment per decision. The guard above returns early for an
	// approval already marked consumed, so a pass that re-reads this one after
	// a lost answer counts nothing.
	if r.Telemetry != nil {
		r.Telemetry.ObserveApproval(telemetry.FamilyMigration, telemetry.ApprovalAccepted)
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
	return migrationApplyWindow(migration) + workload.JobDeadlineGrace + migrationApplyLeaseGrace
}

// migrationApplyWindow is how long a migration Apply is authorized for. The Job
// outlives it by workload.JobDeadlineGrace and the Lease outlives the Job, for
// the reasons stated on those constants.
func migrationApplyWindow(migration *operatorv1alpha1.PtahMigration) time.Duration {
	return time.Duration(migrationActiveDeadline(migration)) * time.Second
}

func migrationActiveDeadline(migration *operatorv1alpha1.PtahMigration) int64 {
	if migration.Spec.Execution.ActiveDeadlineSeconds > 0 {
		return migration.Spec.Execution.ActiveDeadlineSeconds
	}
	// The builder's own default, in the same unit the CRD uses.
	return 900
}
