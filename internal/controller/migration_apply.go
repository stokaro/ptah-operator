package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
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
	if !migrationplan.SupportedContractVersion(plan.Spec.ContractVersion) {
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
	statusArtifact, statusSource := sourcebinding.MigrationPlanSource(migration.Status.Artifact, migration.Status.ConfigMap, migration.Status.SourcePolicy)
	if !sourcebinding.SamePlanSource(plan.Spec.ArtifactDigest, plan.Spec.Source, statusArtifact, statusSource) {
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
	if plan.Spec.Source != nil {
		// A plan computed from a ConfigMap or Git binds no verification
		// policy; its source binding is compared with the resolved source,
		// and its source policy with the one the migration names now.
		return sourcepolicy.StillBinds(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.SourcePolicyFrom, plan.Spec.Source.Policy)
	}
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

// errMigrationApprovalWithdrawn is an approval the claim names that is gone, or
// was replaced under its name, when the claim comes to spend it.
var errMigrationApprovalWithdrawn = errors.New("the approval this Apply was claimed under was deleted or replaced before dispatch")

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
		return r.migrationBlocked(ctx, migration, operatorv1alpha1.ReasonApplyOutcomeUnknown, unresolvedRunHoldMessage(unresolved))
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
		PlanFingerprint:      plan.Spec.Fingerprint,
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		},
	}
	if approval != nil {
		operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: approval.Name, UID: approval.UID}
		operation.ApprovedBy = approval.Spec.Approver.DeepCopy()
		operation.ApprovedAt = approval.Spec.ApprovedAt.DeepCopy()
	}
	operation.Actor = workload.MigrationActor(migration, approval)
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
		operatorv1alpha1.ReasonApprovedPlan, fmt.Sprintf("Applying %d planned %s", len(plan.Spec.Migrations),
			byCount(len(plan.Spec.Migrations), "migration", "migrations")))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
		operatorv1alpha1.ReasonSatisfied, "The plan's approval requirements are satisfied")
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// waitForMigrationApplyLock is acquireMigrationApplyLock for a pass that can do
// nothing else until it holds the Lease, and it says so on the resource. A claim
// that lost the Lease before its dispatch has no Job and no Event, and the wait
// can outlast the Job it is waiting to create. A manager down for longer than the
// Lease lasts can lose it after the dispatch, with the Job still running.
func (r *MigrationReconciler) waitForMigrationApplyLock(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (bool, time.Duration, error) {
	acquired, contended, requeue, err := r.tryMigrationApplyLock(ctx, migration)
	if err != nil || !contended {
		return acquired, requeue, err
	}
	operation := migration.Status.ActiveOperation
	before := migration.DeepCopy()
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonWaitingForDatabaseLock, migrationLockWaitMessage(operation))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return false, 0, err
	}
	return false, requeue, nil
}

// migrationLockWaitMessage says what a claim is waiting for. It names the Lease,
// which names its holder to whoever may read the operator's namespace, and not
// the holder: the holder can be another team's resource in a shared realm. It also
// says when the claim may have started its Job, because a reader who took the
// wait for an Apply that has not started could act while its SQL runs.
func migrationLockWaitMessage(operation *operatorv1alpha1.MigrationOperationStatus) string {
	lease, err := targetlock.LeaseName(operation.CoordinationDigest)
	if err != nil {
		lease = "of this database's realm"
	}
	if migrationMayHaveDispatched(operation) {
		return fmt.Sprintf("%s is waiting for the database lock it lost, and Job %s may still be running: another operation on this database holds Lease %s. It asks again every %s",
			operation.Type, operation.JobName, lease, maxLockContentionPoll)
	}
	return fmt.Sprintf("%s is waiting for the database lock: another operation on this database holds Lease %s. It asks again every %s",
		operation.Type, lease, maxLockContentionPoll)
}

// acquireMigrationApplyLock serializes execution against every other operator
// operation that addresses the same database. The Lease is taken before the
// Job exists and its epoch is compared on every pass: a result produced across
// an epoch change is not this claim's result.
func (r *MigrationReconciler) acquireMigrationApplyLock(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (bool, time.Duration, error) {
	acquired, _, requeue, err := r.tryMigrationApplyLock(ctx, migration)
	return acquired, requeue, err
}

// tryMigrationApplyLock is one attempt at the Lease. contended is a Lease another
// operation holds, as apart from the write this pass makes to record an epoch,
// which also ends the pass without the Lease in hand.
func (r *MigrationReconciler) tryMigrationApplyLock(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (acquired, contended bool, requeue time.Duration, err error) {
	operation := migration.Status.ActiveOperation
	if !migrationOperation(operation).HoldsLock(false) {
		return true, false, 0, nil
	}
	if r.Locks == nil || strings.TrimSpace(r.LockNamespace) == "" {
		return false, false, 0, errors.New("the database lock coordinator is not configured")
	}
	if operation.CoordinationDigest == "" || operation.LeaseDurationSeconds < 1 {
		return false, false, 0, errors.New("the Apply claim carries no database lock binding")
	}
	result, err := r.Locks.Acquire(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace,
		CoordinationDigest:    operation.CoordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: migration.UID, OperationID: operation.ID},
		Duration:              time.Duration(operation.LeaseDurationSeconds) * time.Second,
		ExpectedEpoch:         operation.LeaseEpoch,
	})
	if err != nil {
		return false, false, 0, fmt.Errorf("acquire the database lock: %w", err)
	}
	if !result.Acquired {
		return false, true, lockContentionRequeue(result.Contention), nil
	}
	if result.Epoch == operation.LeaseEpoch && !result.ContinuityLost {
		return true, false, 0, nil
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
	// A wait ends here: taking the Lease from anybody else rotates its epoch, so
	// a claim that was waiting always reaches this write.
	if progressing := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing); progressing != nil &&
		progressing.Reason == string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation.Type))
	}
	migration.Status.ActiveOperation.LeaseContinuityLost = continuityLost
	if continuityLost {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonLeaseContinuityLost, "The database lock epoch changed under the running operation")
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return false, false, 0, err
	}
	return false, false, statusPatchRequeue, nil
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
	// log is what can be said of the run's Pod log.
	log     applyLogReading
	outcome operatorv1alpha1.MigrationRunOutcome
	applied []int64
	message string
	failure *operatorv1alpha1.MigrationRunFailureStatus
	// stoppedBeforeSelecting is a run that failed before Ptah selected any
	// migration, so it ran nothing and read no history. Only a frame's report
	// can say so; a termination summary cannot tell it from a first migration
	// that failed.
	stoppedBeforeSelecting bool
	// targetFailure is why the run could not open its target, when it stopped
	// before selecting and the frame named a kind.
	targetFailure error
	// leftDirtyRevision is a run whose report holds the revision row it left, on
	// which the History read that follows refuses. Only a frame's report can say
	// so: a termination summary carries no such fact.
	leftDirtyRevision bool
	// openedTarget is a run whose report shows it reached the database. A
	// termination summary cannot show that, so a run read from one leaves
	// DatabaseReachable to the history read that follows.
	openedTarget bool
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
		run := r.reportedMigrationRun(ctx, migration, result.MigrationRun, result.Error)
		reported.run = &run
		return reported
	}
	reported.failure = errors.New("the Apply produced no readable account of what the database now holds")
	if result.Error != nil {
		reported.failure = resultFailure(result.Error, migrationURLSecret(migration))
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

// migrationURLSecret is the Secret key the active migration operation read
// its target URL from.
func migrationURLSecret(migration *operatorv1alpha1.PtahMigration) corev1.SecretKeySelector {
	var bound *operatorv1alpha1.DatabaseTargetBinding
	if operation := migration.Status.ActiveOperation; operation != nil {
		bound = operation.Target
	}
	return operationURLSecret(bound, migration.Spec.Target.URLFrom)
}

// reportedMigrationRun is a run report in the terms status keeps. A missing
// report is an unknown outcome.
//
// A run that stopped before it selected anything ran nothing, and its message
// says so. When the runner named the reason a target could not be opened, the
// message is that reason's, which the frame carries as the result's code; a
// run that read the history is described by what it did, whatever the code.
func (r *MigrationReconciler) reportedMigrationRun(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	report *dataplane.MigrationRunReport,
	failure *runner.ResultError,
) migrationRunAccount {
	if report == nil {
		return migrationRunAccount{
			log:     applyLogUnread,
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			message: "The run produced no readable evidence",
		}
	}
	run := migrationRunAccount{
		log:     applyLogHoldsReport,
		outcome: jobclaim.MigrationRunOutcome(report.Outcome),
		applied: boundedVersions(report.Applied, 256),
		message: migrationRunMessage(*report),
		failure: migrationRunFailure(report.Failure),
		// The run reads the revision table after it stops, so this is the row the
		// next reading finds.
		leftDirtyRevision: dirtyRevision(*report) != nil,
	}
	if report.StoppedBeforeSelecting() {
		run.stoppedBeforeSelecting = true
		run.message = "The run stopped before it selected any migration, and ran none"
		if failure != nil {
			if kind, known := dataplane.ParseTargetFailureKind(failure.Code); known {
				run.targetFailure = &targetFailureError{kind: kind, url: migrationURLSecret(migration)}
				run.message = run.targetFailure.Error()
				run.log = applyLogSaysNothing
			}
		}
		return run
	}
	run.openedTarget = true
	if refusal, refused := r.sequenceRefusal(ctx, migration, migration.Status.ActiveOperation, *report); refused {
		run.message = refusal
	}
	return run
}

// recordMigrationRun writes a finished run into status and retires its claim. The
// verdict is the one Ptah read from the revision table, never the Job's exit status:
// a run that stopped is exactly the run whose controller has to be told what the
// database now holds. reportedTarget is the database the run's result named, which
// is the one an unresolved record has to name.
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
	switch outcome {
	case operatorv1alpha1.MigrationRunOutcomeApplied, operatorv1alpha1.MigrationRunOutcomeUpToDate:
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
	default:
		keepLog := jobclaim.MigrationRunKeepsLog(operation.ApprovalRef != nil, outcome, run.leftDirtyRevision)
		if err := retainFailedApplyJob(ctx, r.Client, job, keepLog, func(text string) {
			r.event(migration, corev1.EventTypeWarning, "FailedApplyLogShortened", "%s", text)
		}); err != nil {
			return ctrl.Result{}, err
		}
		message = withLogPointer(message, job, run.log)
	}
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
		Failure:         run.failure,
	}
	recordMigrationRunProvenance(migration.Status.LastRun, migration, operation, reportedTarget)
	migration.Status.Plan = nil
	setMigrationTargetFailure(migration, run.targetFailure)
	if run.openedTarget {
		setMigrationTargetOpened(migration, "The Apply opened the database")
	}
	switch {
	case jobclaim.MigrationRunUnresolved(outcome):
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
	case run.stoppedBeforeSelecting:
		// A run that stopped before it selected anything ran nothing and left
		// the history as the plan read it, so reading it back now would find
		// the same plan and, under the Always policy, start the same refusal
		// at once. Ptah refuses some runs for a reason the history read does
		// not share -- a lock or a timeout the server cannot carry, a lint
		// policy that does not load -- and the target that could not be
		// opened may answer the read and not the run. So the resource waits
		// the failure interval as a failed operation does, and then starts the
		// read-only chain again from resolution, which reads the history
		// before anything is planned or applied.
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseFailed
		next := metav1.NewTime(r.now().Add(migrationFailureRetry(migration)))
		migration.Status.NextReconciliationTime = &next
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonOperationFailed, bounded(message, 1024))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonOperationFailed, bounded(message, 1024))
	default:
		// Every other outcome is confirmed by reading the history back. What the
		// run claims and what the revision table holds are two statements, and
		// only the second one settles the resource.
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifyingHistory
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonVerifyingConvergence, "Reading the history back to confirm what the run did")
	}
	// The run was read from its one Pod after the Job reached a terminal
	// condition, so nothing it dispatched can still write.
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionAccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, migrationRunEventType(outcome), "MigrationRunFinished", "%s: %s", outcome,
		bounded(message, migrationRunEventBytes))
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
	unresolved.TargetIdentityDigest = migrationRunTarget(migration, reportedTarget)
	migration.Status.UnresolvedRun = unresolved
}

// migrationRunTarget is the database a hold on a run names.
//
// The run's own account wins. A result frame names the target the Pod's Secret
// held, and the runner refuses an Apply whose target is not its plan's before
// Ptah starts (target_binding_mismatch). So a frame that names another target
// than the plan's reports a refusal that changed nothing, and the hold names
// where that refusal looked, which is the database a person would go and read.
//
// With no frame, or one that named no target or something the status schema would
// refuse, the hold names the database the plan was computed against, which is the
// last history this resource read. By the same refusal it is the only database the
// run can have changed, so a clean reading of it says what the run did, and a
// reading of another one settles nothing. The run's own record does not take that
// value: it says what the run reported, and the run reported nothing.
func migrationRunTarget(migration *operatorv1alpha1.PtahMigration, reportedTarget string) string {
	if sha256Digest.MatchString(reportedTarget) {
		return reportedTarget
	}
	if history := migration.Status.History; history != nil {
		return history.TargetIdentityDigest
	}
	return ""
}

// recordMigrationRunProvenance writes what the claim knows about the run it
// dispatched into the run's record.
//
// The claim was written before the Job existed and it outlives the Job, the result,
// the approval and the plan, none of which is read here. Two values come from
// beside it. A ConfigMap or Git source takes its ConfigMap revision and source
// policy from status, which only a Resolve writes, and no Resolve runs while this
// claim stands. The database is the one the run's result named, and stays absent
// where it named none, since nothing then says where the run went.
//
// What the claim does not hold stays absent: a claim written by a release that
// copied less keeps its run's approver unknown, since an approval that holds the
// name today may be another decision.
func recordMigrationRunProvenance(
	run *operatorv1alpha1.MigrationRunStatus,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	reportedTarget string,
) {
	run.OperationID = operation.ID
	run.PlanRef = operation.PlanRef.DeepCopy()
	run.PlanFingerprint = operation.PlanFingerprint
	run.ArtifactDigest, run.Source = sourcebinding.MigrationPlanSource(
		operation.Source, migration.Status.ConfigMap, migration.Status.SourcePolicy)
	if sha256Digest.MatchString(reportedTarget) {
		run.TargetIdentityDigest = reportedTarget
	}
	run.ExecutionBindingID = operation.ExecutionBindingID
	run.Authorization = &operatorv1alpha1.MigrationRunAuthorizationStatus{
		Kind: operatorv1alpha1.MigrationRunAuthorizedByPolicy,
	}
	if operation.ApprovalRef != nil {
		run.Authorization = &operatorv1alpha1.MigrationRunAuthorizationStatus{
			Kind:        operatorv1alpha1.MigrationRunAuthorizedByApproval,
			ApprovalRef: operation.ApprovalRef.DeepCopy(),
			Approver:    operation.ApprovedBy.DeepCopy(),
			ApprovedAt:  operation.ApprovedAt.DeepCopy(),
		}
	}
}

// recordMigrationRunVerification records the first reading of a run's database
// after the run ended, in the status write that records the reading. A History
// claim is only ever taken with no Apply claim standing, so a reading harvested
// beside a finished run began after that run.
//
// A reading of another database says nothing about what the run did, so it leaves
// the run unverified, as it leaves an unresolved one unsettled. That covers a run
// that named no database, since a reading always names one: the run reported
// nothing, so a clean reading of the planned one is no statement about it. Its outcome is Unknown or Partial, which a wait stops on without
// a reading, and what settles its hold is not this record. A run with no operation ID
// was recorded by a release without this field and stays as it was.
func recordMigrationRunVerification(migration *operatorv1alpha1.PtahMigration) {
	run, history := migration.Status.LastRun, migration.Status.History
	if run == nil || run.FinishedAt == nil || run.OperationID == "" || run.Verification != nil || history == nil ||
		run.TargetIdentityDigest != history.TargetIdentityDigest {
		return
	}
	result := operatorv1alpha1.MigrationRunVerifiedPending
	switch migration.Status.Phase {
	case operatorv1alpha1.MigrationPhaseInSync:
		result = operatorv1alpha1.MigrationRunVerifiedInSync
	case operatorv1alpha1.MigrationPhaseBlocked:
		result = operatorv1alpha1.MigrationRunVerifiedBlocked
	}
	run.Verification = &operatorv1alpha1.MigrationRunVerificationStatus{
		ObservedAt:         history.ObservedAt,
		HistoryFingerprint: history.Fingerprint,
		CurrentVersion:     history.CurrentVersion,
		Result:             result,
	}
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
	if err := r.scheduleStoppedMigrationJobCleanup(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.retireMigrationClaim(ctx, migration.DeepCopy(), migration, mutationlifecycle.DispositionUnaccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// scheduleStoppedMigrationJobCleanup schedules the cleanup of the Job a retained
// claim names, once nothing that claim dispatched can still write, and before the
// write that drops the claim.
//
// That write is the last moment admission accepts the TTL. The controller-write
// webhook admits a migration Job's cleanup only once the Job has finished and only
// on the Job an active claim names by UID. A Job still running when its run went
// unknown was refused the TTL then, and the claim was kept because the Job could
// still write; once the claim is gone nothing can schedule it, and the Job stays
// until the resource is deleted, holding a place in the namespace's Job quota.
//
// A refusal does not hold the claim: a Job admission will not let the manager
// touch is not a reason to keep the database. Any other failure does, and the pass
// is retried, because the claim is what makes the next attempt possible. A read of
// the Job that fails holds it the same way.
//
// A claim that never recorded its Job's UID records it first (adoptMigrationJob),
// since admission admits the TTL only on the Job a claim names by UID. The pass that
// recorded the run unknown does that for a claim it keeps, so a claim reaches here
// without one where an earlier release kept it, or where the Job under the name is
// not this resource's. A write of the UID that fails holds the claim too.
//
// A deleting resource needs no TTL: the garbage collector takes the Job with its
// owner, and a TTL write that kept failing would hold the finalizer.
func (r *MigrationReconciler) scheduleStoppedMigrationJobCleanup(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if migration.DeletionTimestamp != nil {
		return nil
	}
	operation := migration.Status.ActiveOperation
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read the stopped Apply Job: %w", err)
	}
	if operation.JobUID == "" {
		adopted, err := r.adoptMigrationJob(ctx, migration, job)
		if err != nil {
			return fmt.Errorf("record the stopped Apply Job: %w", err)
		}
		if !adopted {
			// Another resource's Job holds the name, and admission would refuse
			// its TTL.
			return nil
		}
		operation = migration.Status.ActiveOperation
	}
	if job.UID != operation.JobUID {
		// The name holds a later Job, and admission would refuse its TTL.
		return nil
	}
	switch err := r.scheduleUnaccountedMigrationJobCleanup(ctx, migration, job); {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsForbidden(err), apierrors.IsInvalid(err):
		r.migrationJobCleanupNotScheduled(migration, operation, job, err)
		return nil
	default:
		return err
	}
}

// scheduleUnaccountedMigrationJobCleanup schedules the cleanup of the Job of an
// Apply nobody accounted for, by the rule every migration run keeps its Pod log by:
// what the run's own Pod reported, and whether a person approved it. No report of a
// dirty revision reaches here, so a run that reported Failed keeps the short TTL
// whatever it left behind. A Job that already carries a TTL keeps it.
func (r *MigrationReconciler) scheduleUnaccountedMigrationJobCleanup(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) error {
	if job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	operation := migration.Status.ActiveOperation
	approved := operation != nil && operation.ApprovalRef != nil
	keepLog := jobclaim.MigrationRunKeepsLog(approved, r.reportedMigrationOutcome(ctx, job, operation), false)
	return retainFailedApplyJob(ctx, r.Client, job, keepLog, func(text string) {
		r.event(migration, corev1.EventTypeWarning, "FailedApplyLogShortened", "%s", text)
	})
}

// migrationJobCleanupNotScheduled says that a Job will not be collected by a TTL.
// Nothing else collects it while its owner lives.
func (r *MigrationReconciler) migrationJobCleanupNotScheduled(
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
	err error,
) {
	r.event(migration, corev1.EventTypeWarning, "MigrationJobCleanupDeferred",
		"%s Job %q has no cleanup scheduled, so it stays until it is deleted by hand or with its owner: %s",
		operation.Type, job.Name, bounded(err.Error(), 512))
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
// reportedTarget is the database the run's result named, and is empty wherever
// no result frame was read -- which is most of the ways in. A run the runner
// refused because its Secret named another target arrives here through exactly
// one of them, with that target named. Naming the planned database instead
// would make the record say something the run's own result contradicts.
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
	job, unread, err := r.adoptUnrecordedApplyJob(ctx, migration, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	mayStillWrite := unread ||
		r.dispatchedApplyMayStillWrite(ctx, migration.Namespace, migration.Status.ActiveOperation, job)
	return r.retireUncertainMigrationApply(ctx, migration, job, failure, reportedTarget, mayStillWrite)
}

// adoptUnrecordedApplyJob records the Job an Apply claim dispatched before the run
// is recorded unknown, where the claim never recorded its UID, and returns the Job
// the rest of the pass settles: the one it recorded, or job as it was handed in. It
// reports unread where it could not read the name the claim reserved.
//
// The claim that never recorded a UID is one whose pass created the Job and
// stopped, or lost the create's answer, before it wrote the UID; a release that
// moved the executor then meets the claim before any pass has adopted the Job. With
// the UID recorded, the Job is settled by the rule a recorded one is. A finished
// Job takes its cleanup TTL in this pass and the run names its log. A running one
// keeps the claim, and the pass that retires it once the Job has stopped schedules
// the TTL (scheduleStoppedMigrationJobCleanup). Without it, admission admits no TTL
// on the Job, which stays until it is deleted by hand or with its owner.
//
// A Job this resource does not own exactly is not adopted (adoptMigrationJob). A
// read that fails adopts nothing and records the run all the same, and the caller
// keeps the claim and its Lease, as it does for any read that cannot say whether
// the Job may still write (dispatchedApplyMayStillWrite). The pass that retires the
// claim records the UID then. A write that fails fails the pass before anything
// else is recorded, so the retry finds the claim as it was.
func (r *MigrationReconciler) adoptUnrecordedApplyJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (settled *batchv1.Job, unread bool, err error) {
	operation := migration.Status.ActiveOperation
	if operation.JobUID != "" {
		return job, false, nil
	}
	standing := job
	if standing == nil {
		standing = &batchv1.Job{}
		key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
		switch readErr := r.directReader().Get(ctx, key, standing); {
		case apierrors.IsNotFound(readErr):
			return nil, false, nil
		case readErr != nil:
			ctrl.LoggerFrom(ctx).Info("could not read the dispatched Apply Job to record it",
				"job", operation.JobName, "error", readErr.Error())
			return nil, true, nil
		}
	}
	adopted, writeErr := r.adoptMigrationJob(ctx, migration, standing)
	if writeErr != nil {
		return nil, false, fmt.Errorf("record the dispatched Apply Job: %w", writeErr)
	}
	if adopted {
		return standing, false, nil
	}
	return job, false, nil
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
	if job != nil {
		// Scheduling the Job's cleanup is tidiness; recording a run nobody
		// accounted for is the point. A TTL this manager cannot set leaves a
		// finished Job in place until it is deleted with its owner. A record it
		// cannot write leaves a claim active over a Job a later pass can bind
		// and process -- the exact replay this path exists to refuse.
		//
		// The guard refuses the cleanup patch whenever the claim does not name
		// this Job by UID. finishUncertainMigrationApply has recorded the UID of
		// the Job a claim dispatched (adoptUnrecordedApplyJob), so that is a Job
		// this resource does not own exactly, a later Job under the name, or the
		// Job of a deleting resource, which the garbage collector takes with its
		// owner. So the failure is reported and the record is written.
		//
		// It refuses it on a Job that has not finished as well. Where the claim
		// names this Job and stays because the Job may still write, the pass
		// that retires the claim once nothing can write schedules the cleanup
		// (scheduleStoppedMigrationJobCleanup), so it is not asked for here. That
		// covers a finished Job whose Pod has not stopped too: the TTL then follows
		// what the stopped Pod reported, rather than a report it has not made yet.
		later := mayStillWrite && job.UID == operation.JobUID
		if !later {
			if err := r.scheduleUnaccountedMigrationJobCleanup(ctx, migration, job); err != nil {
				r.migrationJobCleanupNotScheduled(migration, operation, job, err)
			}
		}
		if pointer := failedApplyLogPointer(job, failedApplyLogReading(ctx, r.directReader(), job, failure)); pointer != "" {
			failure = fmt.Errorf("%w. %s", failure, pointer)
		}
	}
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
	recordMigrationRunProvenance(run, migration, operation, reportedTarget)
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
	setMigrationTargetFailure(migration, failure)
	next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
	migration.Status.NextReconciliationTime = &next
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

// reportedMigrationOutcome is the outcome this Job's Pod reported, read the way the
// webhook reads it. This path blocks the resource whatever the run reported, because
// something else may have stopped it: an edit of the resource, a changed component,
// another database. Only what the run reported earns the long TTL, so an edit cannot.
// A read that fails leaves the outcome unknown, and the webhook, which reads the Pods
// itself, answers next.
func (r *MigrationReconciler) reportedMigrationOutcome(
	ctx context.Context,
	job *batchv1.Job,
	operation *operatorv1alpha1.MigrationOperationStatus,
) operatorv1alpha1.MigrationRunOutcome {
	if operation == nil {
		return operatorv1alpha1.MigrationRunOutcomeUnknown
	}
	pods := &corev1.PodList{}
	if err := r.directReader().List(ctx, pods, client.InNamespace(job.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not read what the dispatched Apply reported", "job", job.Name, "error", err.Error())
		return operatorv1alpha1.MigrationRunOutcomeUnknown
	}
	return jobclaim.ReportedMigrationRun(pods.Items, job.UID, operation.ID)
}

// markMigrationApprovalsStale withdraws the decisions of this migration that name
// any plan but keep, so that a plan published later waits for a person even if it
// is the plan they approved. A plan's name comes from its fingerprint and an
// identical plan is the object already stored, so without this a plan that a
// reading replaced comes back with its UID and runs under the old approval, with
// nobody watching and after the person was told it authorized nothing.
//
// It is the one thing that makes a migration approval stale, and it answers where
// a read ended rather than where the plan went: a refusal and a reading that
// leaves no plan keep nothing, and a published plan keeps its own. A read that
// ends at the plan it began with keeps the decision, and so does a plan discarded
// between two reads, until the next one says what is current.
//
// Every approval of the migration, rather than those of the plan its status
// names, because a discarded plan leaves its status naming none. One already
// spent stays as the record of the run it authorized, and one being deleted
// authorizes nothing.
//
// The list is read from the API server. An approval admitted just before this
// pass may not have reached the cache, and it is the one this exists for.
func (r *MigrationReconciler) markMigrationApprovalsStale(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	keep *operatorv1alpha1.ImmutableObjectReference,
	reason operatorv1alpha1.ConditionReason,
	message string,
) error {
	return r.markMigrationApprovalsStaleWhere(ctx, migration, reason, message,
		func(approval *operatorv1alpha1.PtahMigrationApproval) bool {
			return keep == nil || approval.Spec.PlanRef.UID != keep.UID
		})
}

// markMigrationApprovalsStaleWhere is the writer, for the approvals of this
// migration that withdraw selects among those still able to authorize a run.
func (r *MigrationReconciler) markMigrationApprovalsStaleWhere(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	reason operatorv1alpha1.ConditionReason,
	message string,
	withdraw func(*operatorv1alpha1.PtahMigrationApproval) bool,
) error {
	approvals := &operatorv1alpha1.PtahMigrationApprovalList{}
	if err := r.directReader().List(ctx, approvals, client.InNamespace(migration.Namespace)); err != nil {
		return fmt.Errorf("list migration approvals to withdraw: %w", err)
	}
	for index := range approvals.Items {
		approval := &approvals.Items[index]
		if approval.Spec.MigrationRef.UID != migration.UID || approval.DeletionTimestamp != nil ||
			!withdraw(approval) ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
			continue
		}
		if err := r.markMigrationApprovalStale(ctx, approval, reason, message); err != nil {
			return err
		}
	}
	return nil
}

// markMigrationApprovalStale writes the mark on one approval, and answers nothing
// for one that is gone: a deleted approval authorizes nothing either.
func (r *MigrationReconciler) markMigrationApprovalStale(
	ctx context.Context,
	approval *operatorv1alpha1.PtahMigrationApproval,
	reason operatorv1alpha1.ConditionReason,
	message string,
) error {
	before := approval.DeepCopy()
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type:               operatorv1alpha1.ConditionApprovalStale,
		Status:             metav1.ConditionTrue,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: approval.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	approval.Status.ObservedGeneration = approval.Generation
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("mark migration approval %s stale: %w", approval.Name, err)
	}
	r.event(approval, corev1.EventTypeWarning, "ApprovalStale", "%s", message)
	if r.Telemetry != nil {
		r.Telemetry.ObserveApproval(telemetry.FamilyMigration, telemetry.ApprovalStale)
	}
	return nil
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
//
// That holds for the plan too. Another approval of the same plan would otherwise
// run it again once this run ended, if the reading after it ended at the same plan,
// as it does after a run that failed with nothing applied, and nobody would have
// seen the failure. So the plan's other approvals go stale here, after this one
// is spent, and a pass that finds it spent already finishes them.
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
		if apierrors.IsNotFound(err) {
			return errMigrationApprovalWithdrawn
		}
		return err
	}
	if approval.UID != reference.UID {
		return errMigrationApprovalWithdrawn
	}
	if !meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
		if err := r.recordMigrationApprovalConsumed(ctx, approval); err != nil {
			return err
		}
	}
	return r.markMigrationApprovalsStaleWhere(ctx, migration, operatorv1alpha1.ReasonSupersededApproval,
		fmt.Sprintf("Approval %s of the same plan was spent on a run, and an approval authorizes one run of a plan", approval.Name),
		func(other *operatorv1alpha1.PtahMigrationApproval) bool {
			return other.Spec.PlanRef.UID == approval.Spec.PlanRef.UID && other.UID != approval.UID
		})
}

// recordMigrationApprovalConsumed writes Consumed on the approval a claim spends.
func (r *MigrationReconciler) recordMigrationApprovalConsumed(
	ctx context.Context,
	approval *operatorv1alpha1.PtahMigrationApproval,
) error {
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
	// One increment per decision. consumeMigrationApproval calls this only for
	// an approval not yet marked consumed, so a pass that re-reads this one
	// after a lost answer counts nothing.
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

// byCount picks the words that agree with count in a sentence a person reads in
// status. Zero takes the plural.
func byCount(count int, one, other string) string {
	if count == 1 {
		return one
	}
	return other
}

// appliedBefore is the end of the sentence about a migration that failed.
func appliedBefore(count int) string {
	return fmt.Sprintf("%d %s recorded applied", count, byCount(count, "migration before it is", "migrations before it are"))
}

// migrationRunMessage says what happened without carrying database rows or the
// SQL a migration ran.
func migrationRunMessage(report dataplane.MigrationRunReport) string {
	switch report.Outcome {
	case dataplane.MigrationOutcomeUpToDate:
		return "The database already had every planned migration"
	case dataplane.MigrationOutcomeApplied:
		return fmt.Sprintf("%d %s recorded applied", len(report.Applied), byCount(len(report.Applied), "migration is", "migrations are"))
	case dataplane.MigrationOutcomeDryRun:
		return "The run was a dry run and changed nothing"
	case dataplane.MigrationOutcomeFailed:
		if failure := report.Failure; failure != nil && failure.Commit {
			// Every statement ran and the commit that ends them failed, so no
			// statement is to blame. The failure names the migration whose
			// commit it was: under --tx-mode all that is the last selected
			// one, and nothing before it is applied either, which the count
			// says.
			return fmt.Sprintf("%s failed at its commit%s and committed nothing; %s",
				migrationName(report, failure.Version), databaseAnswer(*failure), appliedBefore(len(report.Applied)))
		}
		version, found := stoppedMigration(report)
		if !found {
			return "A migration failed and committed nothing; " + appliedBefore(len(report.Applied))
		}
		where := ""
		if failure := failureIn(report, version); failure != nil {
			where = fmt.Sprintf(" at statement %d of %d%s", failure.Statement, failure.Statements, databaseAnswer(*failure))
		}
		return fmt.Sprintf("%s failed%s and committed nothing; %s",
			migrationName(report, version), where, appliedBefore(len(report.Applied)))
	case dataplane.MigrationOutcomePartial:
		if dirty := dirtyRevision(report); dirty != nil && dirty.Total > 0 {
			// The statements before the failed one are what the dirty row
			// counts, so only the database's answer is added.
			rest := " and not the rest"
			if failure := failureIn(report, dirty.Version); failure != nil && databaseAnswer(*failure) != "" {
				rest = ", then failed" + databaseAnswer(*failure)
			}
			return fmt.Sprintf("%s committed %d of its %d statements%s; retrying the file would run them twice",
				migrationName(report, dirty.Version), dirty.Applied, dirty.Total, rest)
		}
		return "A migration committed some of its statements and not the rest; retrying the file would run them twice"
	case dataplane.MigrationOutcomeUnknown:
		if dirty, failure := statementInFlight(report); failure != nil {
			return fmt.Sprintf("%s stopped at statement %d of %d%s, and whether it committed is not recorded",
				migrationName(report, dirty.Version), failure.Statement, failure.Statements, databaseAnswer(*failure))
		}
		return "The database's own account of the run could not be read"
	default:
		return "The database's own account of the run could not be read"
	}
}

// statementInFlight is the revision row and the failure of a run that stopped
// with a statement whose fate the database did not record. Since v0.23.0 Ptah
// reports such a run unknown: on MySQL a failed DDL statement such as CREATE
// UNIQUE INDEX may have committed on its own. The row it left counts the
// statements before the failed one as committed, and the failure names the
// next one in the same migration. The runner clears the row's text, Ptah's
// in-flight marker, so the shape is what identifies it. Anything else returns
// nil, and the message says the account could not be read.
func statementInFlight(report dataplane.MigrationRunReport) (*dataplane.MigrationDirty, *dataplane.MigrationRunFailure) {
	dirty := dirtyRevision(report)
	if dirty == nil {
		return nil, nil
	}
	failure := failureIn(report, dirty.Version)
	if failure == nil || failure.Commit || failure.Statement != dirty.Applied+1 || failure.Statements != dirty.Total {
		return nil, nil
	}
	return dirty, failure
}

// migrationDescriptionBytes bounds the description a run message quotes, so a
// message about a migration stays within 256 bytes and its version, counts
// and the database's answer are never the part an Event cuts.
const migrationDescriptionBytes = 64

// migrationRunEventBytes is how much of a run's message its
// MigrationRunFinished Event keeps: as much as the other failure Events, which
// holds a target failure's sentence whole for any Secret name and key under
// about a hundred bytes each. The run's own status keeps 1024.
const migrationRunEventBytes = 512

// stoppedMigration is the migration a failed run stopped in, from the run's
// own report: the revision row it left dirty, or, where it left none, the
// first version it selected and did not apply. The engine's message about why
// stays out (#69); the version and the description the artifact gave it carry
// no row, and they tell a reader which file to open (#678).
func stoppedMigration(report dataplane.MigrationRunReport) (int64, bool) {
	if dirty := dirtyRevision(report); dirty != nil {
		return dirty.Version, true
	}
	for _, version := range report.Planned {
		if !slices.Contains(report.Applied, version) {
			return version, true
		}
	}
	return 0, false
}

// migrationRunFailure is where the report says the run stopped, in the terms
// status keeps. The decoder already held the statement position, the SQLSTATE
// and the error number to the forms the API schema admits.
//
// A failed commit has no statement position, and the schema's statement
// counts from one, so it stays out of status: the run's message names the
// migration and the database's answer instead.
func migrationRunFailure(failure *dataplane.MigrationRunFailure) *operatorv1alpha1.MigrationRunFailureStatus {
	if failure == nil || failure.Commit {
		return nil
	}
	return &operatorv1alpha1.MigrationRunFailureStatus{
		Version:     failure.Version,
		Statement:   int32(failure.Statement),  // At most the statement count, which fits an int32.
		Statements:  int32(failure.Statements), // Bounded by maxMigrationStatements.
		SQLState:    failure.SQLState,
		ErrorNumber: int32(failure.ErrorNumber), // At most 65535.
	}
}

// failureIn is the run's failure when it is about the migration the message
// names, and nil otherwise, so a sentence never puts one migration's name and
// another's statement together.
func failureIn(report dataplane.MigrationRunReport, version int64) *dataplane.MigrationRunFailure {
	if report.Failure == nil || report.Failure.Version != version {
		return nil
	}
	return report.Failure
}

// databaseAnswer is the code the database gave for the failed statement, and
// nothing when it gave none, as after a lost connection. The runner admitted
// only a five-character SQLSTATE and a number, so neither can carry a row.
func databaseAnswer(failure dataplane.MigrationRunFailure) string {
	switch {
	case failure.SQLState != "" && failure.ErrorNumber > 0:
		return fmt.Sprintf(" with SQLSTATE %s (error %d)", failure.SQLState, failure.ErrorNumber)
	case failure.SQLState != "":
		return " with SQLSTATE " + failure.SQLState
	case failure.ErrorNumber > 0:
		return fmt.Sprintf(" with error %d", failure.ErrorNumber)
	default:
		return ""
	}
}

func dirtyRevision(report dataplane.MigrationRunReport) *dataplane.MigrationDirty {
	if report.Status == nil {
		return nil
	}
	return report.Status.DirtyRevision
}

// migrationName is "Migration 2 (Read Ledger)", or "Migration 2" for a
// version the report describes nowhere.
func migrationName(report dataplane.MigrationRunReport, version int64) string {
	name := "Migration " + strconv.FormatInt(version, 10)
	if report.Status == nil {
		return name
	}
	for _, record := range report.Status.Migrations {
		if record.Version != version {
			continue
		}
		description := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, strings.ToValidUTF8(record.Description, "�")))
		if description == "" {
			return name
		}
		if len(description) > migrationDescriptionBytes {
			cut := migrationDescriptionBytes
			for !utf8.RuneStart(description[cut]) {
				cut--
			}
			description = description[:cut] + "..."
		}
		return name + " (" + description + ")"
	}
	return name
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
