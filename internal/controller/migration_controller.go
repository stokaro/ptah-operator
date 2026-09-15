package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

const (
	migrationOperationFinalizer = "operator.ptah.run/migration-operation"
	defaultMigrationInterval    = 10 * time.Minute
)

// MigrationJobBuilder turns one already-persisted migration claim into a
// deterministic Job. It must never read Secret content.
type MigrationJobBuilder interface {
	NameForMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
	) (string, error)
	BuildMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
	) (*batchv1.Job, error)
	ExecutionBinding() (
		controllerImage string,
		controllerRevision string,
		controllerStateVersion int32,
		ptahVersion string,
		executorImage string,
		runnerImage string,
		runnerProtocolVersion int32,
	)
}

// MigrationReconciler carries one PtahMigration through Resolve -> Verify ->
// History and reports what the database's own revision table said.
//
// Nothing here mutates a database. Selecting and running the pending sequence
// is separate work with its own plan and approval, because the evidence this
// controller collects is what that decision is made from.
type MigrationReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Logs      PodLogReader
	Jobs      MigrationJobBuilder
	Locks     *targetlock.Locker
	// LockNamespace is one shared coordination namespace for every managed
	// resource, including resources that live in different namespaces: two
	// namespaces that address the same database must not run at the same time.
	LockNamespace    string
	Clock            func() time.Time
	Telemetry        telemetry.Observer
	AdmissionOptions podintent.Options
}

func (r *MigrationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(1).Info("migration reconciliation started")
	defer func() {
		if err != nil {
			logger.Error(err, "migration reconciliation failed")
		}
		if r.Telemetry == nil {
			return
		}
		if err != nil {
			r.Telemetry.ObserveReconciliation(telemetry.ReconciliationFailed)
			r.Telemetry.ObserveFailure(telemetry.FailureStageController, telemetry.FailureInfrastructure)
			return
		}
		r.Telemetry.ObserveReconciliation(telemetry.ReconciliationSucceeded)
	}()
	return r.reconcile(ctx, request)
}

func (r *MigrationReconciler) reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	migration := &operatorv1alpha1.PtahMigration{}
	if err := r.directReader().Get(ctx, request.NamespacedName, migration); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if migration.DeletionTimestamp != nil {
		return r.reconcileMigrationDeletion(ctx, migration)
	}
	if result, handled, err := r.reconcileMigrationExecutionBinding(ctx, migration); handled || err != nil {
		return result, err
	}
	if migration.Status.ActiveOperation != nil {
		return r.reconcileActiveMigration(ctx, migration)
	}
	if controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		if err := r.removeMigrationFinalizer(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		// The metadata patch advanced resourceVersion; reload before the status
		// write below so an optimistic conflict is seen here rather than later.
		if err := r.directReader().Get(ctx, request.NamespacedName, migration); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if !databaseEngineSupported(migration.Spec.Target.Engine) {
		return r.migrationBlocked(
			ctx, migration, operatorv1alpha1.ReasonUnsupportedEngine,
			fmt.Sprintf("Database engine %q is not supported by this operator", migration.Spec.Target.Engine),
		)
	}
	if migration.Spec.Suspend {
		before := migration.DeepCopy()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseSuspended
		migration.Status.ObservedGeneration = migration.Generation
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "New migration operations are suspended")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		return ctrl.Result{}, r.patchMigrationStatus(ctx, before, migration)
	}

	// After suspension and before any claim: a suspended resource runs nothing
	// and needs no verdict about the realm, and a resource already carrying an
	// operation returned above. A dispatched Apply is never abandoned for this.
	census, censusErr := takeRealmCensus(
		ctx, r.Client, migration.Spec.Target.Engine, migration.Spec.Target.CoordinationKey,
	)
	if censusErr != nil {
		return ctrl.Result{}, censusErr
	}
	if census.conflict() {
		return r.migrationRealmBlocked(ctx, migration, census)
	}

	now := r.now()
	if migration.Status.ObservedGeneration != migration.Generation {
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
	}
	switch migration.Status.Phase {
	case operatorv1alpha1.MigrationPhaseVerifying:
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationVerify)
	case operatorv1alpha1.MigrationPhaseReading, operatorv1alpha1.MigrationPhaseVerifyingHistory:
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationHistory)
	case operatorv1alpha1.MigrationPhaseAwaitingApproval:
		if due(migration.Status.NextReconciliationTime, now) {
			// Refresh the whole evidence chain before consulting even an exact
			// approval: a moved tag or a history somebody else advanced is what
			// makes an approval stale, and only a fresh reading shows it.
			return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
		}
		return r.reconcileMigrationApplyDecision(ctx, migration)
	case operatorv1alpha1.MigrationPhasePlanning:
		if due(migration.Status.NextReconciliationTime, now) {
			return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
		}
		return r.reconcileMigrationApplyDecision(ctx, migration)
	case operatorv1alpha1.MigrationPhaseInSync,
		operatorv1alpha1.MigrationPhaseBlocked,
		operatorv1alpha1.MigrationPhaseFailed:
		if !due(migration.Status.NextReconciliationTime, now) {
			return requeueAtDeadline(migration.Status.NextReconciliationTime, now), nil
		}
	}
	// A moved tag, a changed policy, or a history someone else advanced is only
	// observable by starting the read-only chain again from resolution.
	return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
}

// reconcileMigrationDeletion releases the resource once no Job of its claim can
// still be running. Every operation here is read-only, so a claim in flight is
// discarded rather than waited on; its Job keeps its own deadline and can no
// longer attribute a result to a claim that is gone.
func (r *MigrationReconciler) reconcileMigrationDeletion(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	if migration.Status.ActiveOperation != nil {
		before := migration.DeepCopy()
		migration.Status.ActiveOperation = nil
		if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.removeMigrationFinalizer(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileMigrationExecutionBinding publishes the component identity this
// resource's work is bound to, and retires a claim authorized under an older
// one. A rollout that changed the executor, the runner, or the manager itself
// must invalidate work in flight rather than let it finish under new bytes.
func (r *MigrationReconciler) reconcileMigrationExecutionBinding(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	configured, err := r.configuredMigrationBinding()
	if err != nil {
		result, failureErr := r.migrationOperationFailure(ctx, migration, err)
		return result, true, failureErr
	}
	current := migration.Status.ExecutionBinding
	if current != nil && executionBindingComponentsEqual(current, configured) {
		return ctrl.Result{}, false, nil
	}
	binding, err := newExecutionBinding(configured)
	if err != nil {
		result, failureErr := r.migrationOperationFailure(ctx, migration, err)
		return result, true, failureErr
	}
	if operation := migration.Status.ActiveOperation; operation != nil &&
		operation.Type == operatorv1alpha1.MigrationOperationApply &&
		(operation.DispatchStarted || operation.JobUID != "") {
		// A dispatched Apply is not retired by a rollout. Its Job may already
		// have changed the database, and dropping the claim would drop the only
		// record that it might have.
		//
		// The evidence is written first, under the binding that authorized the
		// run, and the binding moves on the next pass once no claim is in
		// flight. Two writes in that order, rather than one that would report a
		// run against components it never had.
		return r.applyUncertainUnderBindingChange(ctx, migration)
	}
	before := migration.DeepCopy()
	migration.Status.ExecutionBinding = binding
	if migration.Status.ActiveOperation != nil {
		r.event(migration, corev1.EventTypeWarning, "ExecutionBindingChanged",
			"Discarding the %s operation claim: an execution component changed", migration.Status.ActiveOperation.Type)
		migration.Status.ActiveOperation = nil
		migration.Status.Phase = operatorv1alpha1.MigrationPhasePending
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonExecutionBindingChanged, "The operation was retired because an execution component changed")
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// applyUncertainUnderBindingChange records that a dispatched Apply outlived the
// components that authorized it. It is the same answer as any other Apply the
// controller cannot read: the run's evidence is unknown, the resource is
// blocked, and nothing dispatches again until a person has looked.
func (r *MigrationReconciler) applyUncertainUnderBindingChange(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	operation := migration.Status.ActiveOperation
	var job *batchv1.Job
	if operation.JobUID != "" {
		candidate := &batchv1.Job{}
		key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
		if err := r.directReader().Get(ctx, key, candidate); err == nil && candidate.UID == operation.JobUID {
			job = candidate
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, fmt.Errorf("read the dispatched Apply Job: %w", err)
		}
	}
	result, err := r.finishUncertainMigrationApply(ctx, migration, job,
		errors.New("an execution component changed while the Apply was dispatched"))
	return result, true, err
}

func (r *MigrationReconciler) configuredMigrationBinding() (*operatorv1alpha1.ExecutionBindingStatus, error) {
	if r.Jobs == nil {
		return nil, errors.New("Job builder is not configured")
	}
	controllerImage, controllerRevision, controllerStateVersion,
		ptahVersion, executorImage, runnerImage, protocolVersion := r.Jobs.ExecutionBinding()
	return &operatorv1alpha1.ExecutionBindingStatus{
		ControllerImage:        controllerImage,
		ControllerRevision:     controllerRevision,
		ControllerStateVersion: controllerStateVersion,
		PtahVersion:            ptahVersion,
		ExecutorImage:          executorImage,
		RunnerImage:            runnerImage,
		RunnerProtocolVersion:  protocolVersion,
	}, nil
}

// claimMigration persists the decision before the Job exists. The claim carries
// the Job's deterministic name, which is what lets a controller that restarted
// mid-dispatch tell the Job it created from one it has not.
func (r *MigrationReconciler) claimMigration(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) (ctrl.Result, error) {
	if migration.Status.ActiveOperation != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	if migration.Status.ExecutionBinding == nil {
		return ctrl.Result{Requeue: true}, nil
	}
	inputFingerprint, err := r.migrationInputFingerprint(ctx, migration, operationType)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, err)
	}
	id, err := r.newMigrationOperationID(inputFingerprint)
	if err != nil {
		return ctrl.Result{}, err
	}
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 id,
		InputFingerprint:   inputFingerprint,
		StartedAt:          metav1.NewTime(r.now()),
		Attempt:            1,
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		if migration.Status.Artifact == nil {
			return r.migrationOperationFailure(ctx, migration, errors.New("the artifact has not been resolved to a digest"))
		}
		operation.Source = migrationSourceBinding(migration)
	}
	if operationType == operatorv1alpha1.MigrationOperationHistory {
		coordinationDigest, digestErr := fingerprint.DatabaseCoordinationDigest(
			string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
		)
		if digestErr != nil {
			return r.migrationOperationFailure(ctx, migration, fmt.Errorf("derive coordination digest: %w", digestErr))
		}
		operation.CoordinationDigest = coordinationDigest
		operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		}
	}
	if r.Jobs == nil {
		return r.migrationOperationFailure(ctx, migration, errors.New("Job builder is not configured"))
	}
	operation.JobName, err = r.Jobs.NameForMigration(migration, *operation)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("name %s Job: %w", operationType, err))
	}
	if err := r.ensureMigrationFinalizer(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	before := migration.DeepCopy()
	migration.Status.ActiveOperation = operation
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.NextReconciliationTime = nil
	migration.Status.Phase = migrationPhaseFor(operationType)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operationType))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	ctrl.LoggerFrom(ctx).Info("migration operation claimed", "operation", operationType, "phase", migration.Status.Phase)
	return ctrl.Result{Requeue: true}, nil
}

func (r *MigrationReconciler) reconcileActiveMigration(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	if operation.LeaseContinuityLost {
		return r.finishUncertainMigrationApply(ctx, migration, nil,
			errors.New("the database lock epoch changed under the dispatched run"))
	}
	applying := operation.Type == operatorv1alpha1.MigrationOperationApply
	if applying {
		acquired, requeue, lockErr := r.acquireMigrationApplyLock(ctx, migration)
		if lockErr != nil {
			return ctrl.Result{}, lockErr
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		operation = migration.Status.ActiveOperation
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	err := r.directReader().Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		if applying && (operation.DispatchStarted || operation.JobUID != "") {
			// A dispatched Apply is never recreated. Whether it ran is a question
			// for the database, not for a retry.
			return r.finishUncertainMigrationApply(ctx, migration, nil,
				errors.New("the dispatched Apply Job is missing and will not be recreated"))
		}
		return r.dispatchMigrationJob(ctx, migration, key)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read active migration Job: %w", err)
	}
	if migration.Spec.Suspend && !applying {
		return r.discardMigrationOperation(ctx, migration, errors.New("reconciliation was suspended while the operation ran"))
	}
	if operation.JobUID != "" && operation.JobUID != job.UID {
		if applying {
			return r.finishUncertainMigrationApply(ctx, migration, job, errors.New("the dispatched Apply Job was replaced"))
		}
		return r.retryMigrationOperation(ctx, migration, job, errors.New("the active Job was replaced"))
	}
	if !exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahMigration", migration.Name, migration.UID) {
		if applying {
			return r.finishUncertainMigrationApply(ctx, migration, job, errors.New("the dispatched Apply Job lost its owner"))
		}
		return r.retryMigrationOperation(ctx, migration, job, errors.New("the active Job is not owned by this migration"))
	}
	if operation.JobUID == "" {
		before := migration.DeepCopy()
		migration.Status.ActiveOperation.JobUID = job.UID
		if applying {
			migration.Status.ActiveOperation.DispatchStarted = true
		}
		if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
			return ctrl.Result{}, err
		}
		operation = migration.Status.ActiveOperation
	}
	if !jobTerminal(job) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	current, currentErr := r.migrationInputFingerprint(ctx, migration, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = errors.New("the operation inputs changed while the Job was running")
		}
		if applying {
			// An Apply Job that exists may already have changed the database,
			// whatever its formerly exact inputs now say.
			return r.finishUncertainMigrationApply(ctx, migration, job, currentErr)
		}
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return r.discardMigrationOperation(ctx, migration, currentErr)
	}
	evidence, err := r.migrationTerminalLogs(ctx, migration, job)
	if err != nil {
		if errors.Is(err, errTerminalPodPending) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if errors.Is(err, errTerminalPodMultiplicity) || errors.Is(err, errTerminalPodIntent) {
			if applying {
				return r.finishUncertainMigrationApply(ctx, migration, job, err)
			}
			return r.retryMigrationOperation(ctx, migration, job, err)
		}
		return ctrl.Result{}, err
	}
	result, parseErr := runner.ParseResultFor(evidence.Logs, migrationRunnerOperation(operation.Type), operation.ID)
	if applying {
		// The run's own evidence settles an Apply, whatever the Job's exit
		// status said: a run that stopped is exactly the run whose controller
		// has to be told what the database now holds.
		if parseErr != nil {
			return r.finishUncertainMigrationApply(ctx, migration, job,
				fmt.Errorf("read the Apply result: %w", parseErr))
		}
		if result.MigrationRun == nil || result.Uncertain {
			failure := errors.New("the Apply produced no readable account of what the database now holds")
			if result.Error != nil {
				failure = fmt.Errorf("%s: %s", result.Error.Code, bounded(result.Error.Message, 512))
			}
			if result.MigrationRun != nil {
				return r.consumeMigrationRun(ctx, migration, job, result)
			}
			return r.finishUncertainMigrationApply(ctx, migration, job, failure)
		}
		if result.CoordinationDigest != operation.CoordinationDigest ||
			operation.Target != nil && result.TargetIdentityDigest != migration.Status.History.TargetIdentityDigest {
			return r.finishUncertainMigrationApply(ctx, migration, job,
				errors.New("the Apply ran against a database other than the one it was planned for"))
		}
		return r.consumeMigrationRun(ctx, migration, job, result)
	}
	if parseErr != nil {
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("read %s result: %w", operation.Type, parseErr))
	}
	if !jobSucceeded(job) {
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("the %s Job failed", operation.Type))
	}
	if result.Error != nil {
		return r.retryMigrationOperation(ctx, migration, job,
			fmt.Errorf("%s: %s", result.Error.Code, bounded(result.Error.Message, 512)))
	}
	if result.Truncation != nil && result.Truncation.Stdout {
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("the %s result was truncated", operation.Type))
	}
	return r.consumeMigrationResult(ctx, migration, job, result)
}

// dispatchMigrationJob creates the Job the claim named, once. Every input the
// claim was decided from is re-read first: a Job is only worth creating while
// the decision behind it still holds.
func (r *MigrationReconciler) dispatchMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	key types.NamespacedName,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	if migration.Spec.Suspend {
		return r.discardMigrationOperation(ctx, migration, errors.New("reconciliation was suspended before dispatch"))
	}
	current, currentErr := r.migrationInputFingerprint(ctx, migration, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = errors.New("the operation inputs changed after the claim")
		}
		return r.discardMigrationOperation(ctx, migration, currentErr)
	}
	if operation.JobUID != "" {
		// A persisted UID proves this attempt already crossed its dispatch
		// boundary. Admission permits CREATE only while the claim has no UID,
		// so advance to a fresh attempt and a fresh deterministic name rather
		// than recreating the Job this claim already had.
		pods, podsErr := podsOwnedByJob(ctx, r.directReader(), migration.Namespace, operation.JobName, operation.JobUID)
		if podsErr != nil {
			return ctrl.Result{}, podsErr
		}
		for _, pod := range pods {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
			}
		}
		return r.retryMigrationOperation(ctx, migration, nil,
			fmt.Errorf("%s Job %q with persisted UID %q is missing", operation.Type, operation.JobName, operation.JobUID))
	}
	if r.Jobs == nil {
		return ctrl.Result{}, errors.New("Job builder is not configured")
	}
	job, err := r.Jobs.BuildMigration(migration, *operation)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("build %s Job: %w", operation.Type, err))
	}
	if job.Namespace != migration.Namespace || job.Name != operation.JobName {
		return ctrl.Result{}, errors.New("the Job builder returned an object outside the operation claim")
	}
	if operation.AdmissionSnapshot == nil {
		snapshot, snapshotErr := podintent.Resolve(ctx, r.directReader(), migration.Namespace, &job.Spec.Template, r.AdmissionOptions)
		if snapshotErr != nil {
			return r.migrationOperationFailure(ctx, migration, fmt.Errorf("resolve Pod admission snapshot: %w", snapshotErr))
		}
		before := migration.DeepCopy()
		migration.Status.ActiveOperation.AdmissionSnapshot = snapshot
		if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
			return ctrl.Result{}, err
		}
		// The snapshot is its own durable boundary: it is persisted before the
		// Job that carries its digest exists.
		return ctrl.Result{Requeue: true}, nil
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("validate persisted Pod admission snapshot: %w", err))
	}
	templateDigest, digestErr := podintent.DigestTemplate(&job.Spec.Template)
	if digestErr != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("digest rebuilt Job Pod template: %w", digestErr))
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return r.discardMigrationOperation(ctx, migration,
			errors.New("the rebuilt Job Pod template differs from the persisted admission snapshot"))
	}
	if operation.Type == operatorv1alpha1.MigrationOperationApply && !operation.DispatchStarted {
		if err := r.consumeMigrationApproval(ctx, migration, operation.ApprovalRef); err != nil {
			return ctrl.Result{}, err
		}
		before := migration.DeepCopy()
		migration.Status.ActiveOperation.DispatchStarted = true
		if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
			return ctrl.Result{}, err
		}
		operation = migration.Status.ActiveOperation
	}
	expected := job.DeepCopy()
	if err := r.Client.Create(ctx, job); err != nil {
		if operation.Type == operatorv1alpha1.MigrationOperationApply && !apierrors.IsAlreadyExists(err) {
			return r.finishUncertainMigrationApply(ctx, migration, nil,
				fmt.Errorf("the Apply Job create result is uncertain: %w", err))
		}
		if apierrors.IsAlreadyExists(err) {
			return r.retryMigrationOperation(ctx, migration, nil, errors.New("the claimed Job name was occupied during dispatch"))
		}
		return ctrl.Result{}, fmt.Errorf("create %s Job: %w", operation.Type, err)
	}
	if err := r.directReader().Get(ctx, key, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("read created %s Job: %w", operation.Type, err)
	}
	if err := validateMigrationJobIntent(job, expected, migration); err != nil {
		return r.retryMigrationOperation(ctx, migration, nil, fmt.Errorf("the created Job failed immutable intent validation: %w", err))
	}
	before := migration.DeepCopy()
	migration.Status.ActiveOperation.JobUID = job.UID
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeNormal, "OperationStarted", "%s Job %s started", operation.Type, job.Name)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *MigrationReconciler) consumeMigrationResult(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	result runner.Result,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	before := migration.DeepCopy()
	// The history document a plan is computed from, held until the status it
	// was read into has been persisted. Nothing else may set it.
	var pendingPlanReport *dataplane.MigrationStatusReport
	switch operation.Type {
	case operatorv1alpha1.MigrationOperationResolve:
		if !sha256DigestPattern.MatchString(result.ResolvedDigest) || result.ResolvedReference == "" {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the resolve result carries no immutable reference"))
		}
		if _, err := ocireference.Parse(result.ResolvedReference); err != nil {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the resolved reference is not a credential-free OCI reference"))
		}
		migration.Status.Artifact = &operatorv1alpha1.OCIArtifactAccessBinding{
			ResolvedReference: result.ResolvedReference,
			Digest:            result.ResolvedDigest,
			RegistryAuthFrom:  migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(),
		}
		migration.Spec.Artifact.Transport.DeepCopyInto(&migration.Status.Artifact.Transport)
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifying
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionUnknown,
			operatorv1alpha1.ReasonDigestPinned, "The artifact resolved to a digest and has not been verified yet")
	case operatorv1alpha1.MigrationOperationVerify:
		if result.ObservedArtifactType != dataplane.MigrationArtifactType {
			return r.retryMigrationOperation(ctx, migration, job,
				fmt.Errorf("the verified artifact is %q, not a migration artifact", bounded(result.ObservedArtifactType, 128)))
		}
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionTrue,
			operatorv1alpha1.ReasonPolicySatisfied, "The resolved artifact satisfied its verification policy")
	case operatorv1alpha1.MigrationOperationHistory:
		if result.MigrationHistory == nil {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the history result carries no status document"))
		}
		if !sha256DigestPattern.MatchString(result.TargetIdentityDigest) {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the history result carries no target identity"))
		}
		if err := r.recordMigrationHistory(migration, *result.MigrationHistory, result.TargetIdentityDigest); err != nil {
			return r.retryMigrationOperation(ctx, migration, job, err)
		}
		// The plan is published after this status is persisted, not here. A
		// plan names the evidence it was computed from, and the admission
		// guard re-derives it from the migration's own stored status: a plan
		// created from a status that exists only in this process references
		// evidence nobody else can see, and the guard refuses it -- which is
		// the guard being right.
		if migration.Status.Phase == operatorv1alpha1.MigrationPhasePlanning {
			pendingPlanReport = result.MigrationHistory
		} else {
			migration.Status.Plan = nil
		}
	default:
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("unsupported migration operation %q", operation.Type))
	}
	migration.Status.ActiveOperation = nil
	if migration.Status.Phase != operatorv1alpha1.MigrationPhaseVerifying &&
		migration.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
		next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
		migration.Status.NextReconciliationTime = &next
	}
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	if pendingPlanReport != nil {
		// A second write, deliberately. Between the two the resource is
		// Planning with no plan, and a controller that stopped there resolves
		// again on the next pass rather than acting on a plan nobody published.
		planned := migration.DeepCopy()
		if err := r.publishMigrationPlan(ctx, migration, *pendingPlanReport); err != nil {
			return r.migrationOperationFailure(ctx, migration, err)
		}
		if err := r.patchMigrationStatus(ctx, planned, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{Requeue: true}, nil
}

// recordMigrationHistory turns the database's own account into status. The
// classification is the database's, not this controller's: a dirty row and a
// modified applied migration are refusals Ptah reported, and neither is ever
// resolved by reconciling again.
func (r *MigrationReconciler) recordMigrationHistory(
	migration *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
	targetIdentityDigest string,
) error {
	historyFingerprint, err := migrationplan.HistoryFingerprint(report)
	if err != nil {
		return err
	}
	pending := report.Pending()
	modified := report.Modified()
	outOfOrder := report.OutOfOrder()
	artifactVersion := report.LastVersion()
	history := &operatorv1alpha1.MigrationHistoryStatus{
		ObservedAt:           metav1.NewTime(r.now()),
		ContractVersion:      int32(report.ContractVersion),
		CurrentVersion:       report.CurrentVersion,
		CheckpointVersion:    report.CheckpointVersion,
		Fingerprint:          historyFingerprint,
		TargetIdentityDigest: targetIdentityDigest,
		PendingCount:         int32(len(pending)),
		Dirty:                report.DirtyRevision != nil,
	}
	for _, record := range report.Migrations {
		if record.State == dataplane.MigrationStateApplied || record.State == dataplane.MigrationStateCheckpointCovered {
			history.AppliedCount++
		}
	}
	if len(modified) > 0 {
		history.ModifiedVersions = boundedVersions(modified, 64)
	}
	if len(outOfOrder) > 0 {
		history.OutOfOrderVersions = boundedVersions(outOfOrder, 64)
	}
	migration.Status.History = history

	switch {
	case history.Dirty:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryDirty,
			fmt.Sprintf("Revision %d is recorded dirty; a person has to decide what the interrupted run did", report.DirtyRevision.Version))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryDirty, "The revision table holds a dirty row")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryDirty, "Nothing runs while a revision is recorded dirty")
	case len(modified) > 0:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryModified,
			fmt.Sprintf("%d applied migrations no longer match their files", len(modified)))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryModified, "An applied migration was modified after it ran")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryModified, "Nothing runs while an applied migration no longer matches its file")
	case len(outOfOrder) > 0:
		// Ptah executes in linear order and refuses the whole run while a
		// pending migration sorts below the current version. Publishing a plan
		// for it would ask a person to approve a sequence the executor cannot
		// run, and the refusal would arrive as a failed Job instead of as the
		// answer it is.
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryOutOfOrder,
			fmt.Sprintf("%d migrations sort below applied version %d; linear execution refuses them", len(outOfOrder), report.CurrentVersion))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryOutOfOrder, "A migration arrived below the version the database has applied")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryOutOfOrder, "Nothing runs while a migration sorts below the applied version")
	// The database records work this artifact has never heard of. Nothing is
	// pending, because pending is a statement about the artifact's own
	// migrations, and a revision the artifact does not carry is in no state at
	// all -- which is exactly how this used to read as success. The operator
	// will not roll a database back to match an older artifact, and which of
	// the two is wrong is a question for whoever moved the tag.
	case report.CurrentVersion > artifactVersion:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryAhead,
			fmt.Sprintf("The database is at version %d and this artifact ends at version %d",
				report.CurrentVersion, artifactVersion))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryAhead, "The database records a migration this artifact does not carry")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryAhead, "Nothing runs while the database is ahead of the artifact")
	case len(pending) == 0:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseInSync
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "The history continues this artifact")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryMatched, "The database has every migration this artifact carries")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "Nothing is pending")
	default:
		migration.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "The history continues this artifact")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonMigrationsPending,
			fmt.Sprintf("%d migrations are pending", len(pending)))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonMigrationsPending,
			fmt.Sprintf("A plan for %d pending migrations is being published", len(pending)))
	}
	return nil
}

// publishMigrationPlan writes the immutable manifest of the pending sequence
// and records it. The plan's name is derived from its fingerprint, so
// publishing the same decision twice is the same object rather than a second
// copy of one decision.
func (r *MigrationReconciler) publishMigrationPlan(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
) error {
	binding := migration.Status.ExecutionBinding
	history := migration.Status.History
	if binding == nil || history == nil || migration.Status.Artifact == nil {
		return errors.New("a plan needs a resolved artifact, a read history, and an execution binding")
	}
	planned, err := migrationplan.Sequence(report)
	if err != nil {
		return fmt.Errorf("select the pending sequence: %w", err)
	}
	sequenceDigest, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		return err
	}
	policyBinding, err := policy.ConfigMapBinding(
		ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom,
	)
	if err != nil {
		return err
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest(
		string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
	)
	if err != nil {
		return fmt.Errorf("derive coordination digest: %w", err)
	}
	policyFingerprint, err := migrationPolicyFingerprint(migration)
	if err != nil {
		return err
	}
	planBinding := migrationplan.Binding{
		MigrationUID:             migration.UID,
		HistoryFingerprint:       history.Fingerprint,
		SequenceDigest:           sequenceDigest,
		ArtifactDigest:           migration.Status.Artifact.Digest,
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     history.TargetIdentityDigest,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyBinding.UID,
		VerificationPolicyDigest: policyBinding.Digest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
	}
	planFingerprint, err := planBinding.Fingerprint()
	if err != nil {
		return fmt.Errorf("derive the plan fingerprint: %w", err)
	}
	desired, err := migrationplan.Desired(migration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:          migrationplan.ContractVersion,
		MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
		Fingerprint:              planFingerprint,
		Migrations:               planned,
		HistoryFingerprint:       history.Fingerprint,
		CurrentVersion:           history.CurrentVersion,
		ArtifactDigest:           planBinding.ArtifactDigest,
		CoordinationDigest:       planBinding.CoordinationDigest,
		TargetIdentityDigest:     planBinding.TargetIdentityDigest,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyBinding.UID,
		VerificationPolicyDigest: policyBinding.Digest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          binding.ControllerImage,
		ControllerRevision:       binding.ControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              binding.RunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		CreatedAt:                metav1.NewTime(r.now()),
	})
	if err != nil {
		return err
	}
	published := desired.DeepCopy()
	if err := r.Client.Create(ctx, published); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("publish the migration plan: %w", err)
		}
		existing := &operatorv1alpha1.PtahMigrationPlan{}
		key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
		if err := r.directReader().Get(ctx, key, existing); err != nil {
			return fmt.Errorf("read the already published migration plan: %w", err)
		}
		if existing.Spec.Fingerprint != planFingerprint || existing.Spec.MigrationRef.UID != migration.UID {
			return errors.New("a different plan already holds this plan's deterministic name")
		}
		published = existing
	}
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: published.Name, UID: published.UID}
	r.applyMigrationPolicyPhase(migration, len(planned))
	return nil
}

// applyMigrationPolicyPhase reports what the policy says happens next. Nothing
// here executes: the phase and the conditions are what an operator reads, and
// what the approval an operator writes is judged against.
func (r *MigrationReconciler) applyMigrationPolicyPhase(migration *operatorv1alpha1.PtahMigration, planned int) {
	switch migration.Spec.Policy.Apply {
	case operatorv1alpha1.ApplyPolicyNever:
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
			operatorv1alpha1.ReasonNotRequired, "The apply policy is Never, so no approval is consulted")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyDisabled,
			fmt.Sprintf("%d migrations are pending and the apply policy is Never", planned))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyDisabled, "The apply policy is Never, so nothing runs from here")
	case operatorv1alpha1.ApplyPolicyAlways:
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
			operatorv1alpha1.ReasonNotRequired, "The apply policy is Always, so no approval is required")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyPending,
			fmt.Sprintf("%d migrations are planned", planned))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonApplyPending,
			fmt.Sprintf("An Apply of %d planned migrations is what happens next", planned))
	default:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseAwaitingApproval
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue,
			operatorv1alpha1.ReasonAwaitingApproval,
			fmt.Sprintf("%d planned migrations need an approval naming this plan", planned))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonAwaitingApproval, "The plan is waiting for the approval its policy requires")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonAwaitingApproval, "Nothing runs until a person writes the approval")
	}
}

// migrationPolicyFingerprint binds a plan to the terms it was planned under. A
// policy that changed after the plan was made invalidates it rather than
// executing under terms nobody approved.
func migrationPolicyFingerprint(migration *operatorv1alpha1.PtahMigration) (string, error) {
	return fingerprint.DigestCanonicalJSON(map[string]string{
		"apply":        string(migration.Spec.Policy.Apply),
		"lock_timeout": migration.Spec.Policy.LockTimeout.Duration.String(),
	})
}

// migrationInputFingerprint is what the operation was decided from. An input
// that changed while the Job ran is what makes its result stale rather than
// wrong.
func (r *MigrationReconciler) migrationInputFingerprint(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) (string, error) {
	if _, err := ocireference.Parse(migration.Spec.Artifact.OCIRef); err != nil {
		return "", errors.New("the artifact reference must be an OCI reference without credentials, whitespace, or query data")
	}
	inputs := map[string]string{
		"operation":       string(operationType),
		"generation":      strconv.FormatInt(migration.Generation, 10),
		"oci_ref":         migration.Spec.Artifact.OCIRef,
		"engine":          string(migration.Spec.Target.Engine),
		"target_secret":   migration.Spec.Target.URLFrom.Name,
		"target_key":      migration.Spec.Target.URLFrom.Key,
		"policy_object":   migration.Spec.Artifact.VerificationPolicyFrom.Name,
		"policy_key":      migration.Spec.Artifact.VerificationPolicyFrom.Key,
		"plain_http":      strconv.FormatBool(migration.Spec.Artifact.Transport.PlainHTTP),
		"execution_id":    migrationBindingEpoch(migration),
		"lock_timeout":    migration.Spec.Policy.LockTimeout.Duration.String(),
		"connect_timeout": migration.Spec.Execution.ConnectTimeout.Duration.String(),
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		if migration.Status.Artifact == nil {
			return "", errors.New("the artifact has not been resolved to a digest")
		}
		inputs["resolved_digest"] = migration.Status.Artifact.Digest
	}
	if operationType == operatorv1alpha1.MigrationOperationVerify {
		binding, err := policy.ConfigMapBinding(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom)
		if err != nil {
			return "", err
		}
		inputs["policy_uid"] = string(binding.UID)
		inputs["policy_digest"] = binding.Digest
	}
	return fingerprint.DigestCanonicalJSON(inputs)
}

// retryMigrationOperation advances to a fresh attempt with a fresh
// deterministic name. A Job that already exists is never reused: the claim it
// answered is the one that failed.
func (r *MigrationReconciler) retryMigrationOperation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
) (ctrl.Result, error) {
	if job != nil {
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
	}
	operation := migration.Status.ActiveOperation
	if operation == nil {
		return ctrl.Result{Requeue: true}, nil
	}
	before := migration.DeepCopy()
	next := operation.DeepCopy()
	next.Attempt = operation.Attempt + 1
	next.JobUID = ""
	next.AdmissionSnapshot = nil
	next.StartedAt = metav1.NewTime(r.now())
	name, err := r.Jobs.NameForMigration(migration, *next)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("name the retried %s Job: %w", operation.Type, err))
	}
	next.JobName = name
	migration.Status.ActiveOperation = next
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationFailed, bounded(failure.Error(), 512))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "OperationRetried", "%s attempt %d: %v", operation.Type, operation.Attempt, failure)
	return ctrl.Result{Requeue: true}, nil
}

// discardMigrationOperation drops a claim whose inputs no longer hold. Nothing
// durable was produced, so the next reconciliation starts the read-only chain
// again from resolution.
func (r *MigrationReconciler) discardMigrationOperation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.ActiveOperation = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhasePending
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonInputsChanged, bounded(failure.Error(), 512))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// migrationOperationFailure records a configuration or dispatch failure the
// controller cannot resolve by retrying immediately.
func (r *MigrationReconciler) migrationOperationFailure(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.ActiveOperation = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseFailed
	migration.Status.ObservedGeneration = migration.Generation
	next := metav1.NewTime(r.now().Add(migrationFailureRetry(migration)))
	migration.Status.NextReconciliationTime = &next
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationFailed, bounded(failure.Error(), 512))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationFailed, bounded(failure.Error(), 512))
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "OperationFailed", "%v", failure)
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FailureStageController, telemetry.FailureConfiguration)
	}
	return requeueAtDeadline(migration.Status.NextReconciliationTime, r.now()), nil
}

// migrationBlocked reports a state reconciliation cannot leave on its own.
// migrationRealmBlocked refuses a database more than one resource claims.
//
// What ends this is another resource's spec change, and that resource's events
// do not reach this one, so the verdict is re-taken on a bounded cadence rather
// than waited on: the shorter of this resource's interval and a minute.
func (r *MigrationReconciler) migrationRealmBlocked(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	census realmCensus,
) (ctrl.Result, error) {
	now := r.now()
	next := realmBlockDeadline(migration.Status.NextReconciliationTime, now, migration.Spec.Interval.Duration)
	before := migration.DeepCopy()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.NextReconciliationTime = &next
	message := census.message()
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonRealmConflict, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict, "No plan is approvable while the database realm is contested")
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return requeueAtDeadline(&next, r.now()), nil
}

func (r *MigrationReconciler) migrationBlocked(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	reason operatorv1alpha1.ConditionReason,
	message string,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.ObservedGeneration = migration.Generation
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, reason, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, reason, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, reason, message)
	return ctrl.Result{}, r.patchMigrationStatus(ctx, before, migration)
}

func (r *MigrationReconciler) migrationTerminalLogs(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (terminalEvidence, error) {
	operation := migration.Status.ActiveOperation
	if operation == nil {
		return terminalEvidence{}, errors.New("the active migration operation is missing")
	}
	evidence, selected, err := collectTerminalPodEvidence(
		ctx, r.directReader(), migration.Namespace, job, operation.AdmissionSnapshot,
	)
	if err != nil {
		return evidence, err
	}
	if selected == nil {
		if r.now().Sub(operation.StartedAt.Time) < terminalPodGrace {
			return evidence, errTerminalPodPending
		}
		return evidence, nil
	}
	if !evidence.Trusted {
		return evidence, nil
	}
	if r.Logs == nil {
		return evidence, errors.New("pod log reader is not configured")
	}
	logs, err := r.Logs.Read(ctx, migration.Namespace, selected.Name, executorContainerName)
	if err != nil {
		return evidence, err
	}
	evidence.Logs = logs
	return evidence, nil
}

// ensureMigrationFinalizer keeps the resource around while a claim is in
// flight, so a deletion cannot strand a Job nothing will account for.
func (r *MigrationReconciler) ensureMigrationFinalizer(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		return nil
	}
	before := migration.DeepCopy()
	controllerutil.AddFinalizer(migration, migrationOperationFinalizer)
	if err := r.Client.Patch(ctx, migration, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("add migration operation finalizer: %w", err)
	}
	return nil
}

// randomNonce makes one operation attempt distinct from every other attempt of
// the same operation, so a retry is a new claim rather than a second result for
// the old one.
func randomNonce() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("create operation nonce: %w", err)
	}
	return hex.EncodeToString(nonce), nil
}

func (r *MigrationReconciler) removeMigrationFinalizer(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if !controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		return nil
	}
	before := migration.DeepCopy()
	controllerutil.RemoveFinalizer(migration, migrationOperationFinalizer)
	if err := r.Client.Patch(ctx, migration, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("remove migration operation finalizer: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) markJobHarvested(ctx context.Context, job *batchv1.Job) error {
	if job == nil || job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	before := job.DeepCopy()
	job.Spec.TTLSecondsAfterFinished = ptr(jobCleanupTTLSeconds)
	if err := r.Client.Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("schedule completed Job cleanup: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) patchMigrationStatus(
	ctx context.Context,
	before, after *operatorv1alpha1.PtahMigration,
) error {
	if reflect.DeepEqual(before.Status, after.Status) {
		return nil
	}
	if err := r.Client.Status().Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch migration status: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) directReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *MigrationReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *MigrationReconciler) event(object client.Object, eventType, reason, message string, arguments ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, eventType, reason, message, arguments...)
}

// SetupWithManager registers the migration controller. Jobs are watched by
// owner so a terminal Job wakes its migration instead of waiting for the poll.
func (r *MigrationReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&operatorv1alpha1.PtahMigration{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		))).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func migrationSourceBinding(migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.OCIArtifactAccessBinding {
	source := &operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: migration.Status.Artifact.ResolvedReference,
		Digest:            migration.Status.Artifact.Digest,
		RegistryAuthFrom:  migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(),
	}
	migration.Spec.Artifact.Transport.DeepCopyInto(&source.Transport)
	return source
}

func migrationBindingEpoch(migration *operatorv1alpha1.PtahMigration) string {
	if migration.Status.ExecutionBinding == nil {
		return ""
	}
	return migration.Status.ExecutionBinding.Epoch
}

func migrationPhaseFor(operation operatorv1alpha1.MigrationOperationType) operatorv1alpha1.MigrationPhase {
	switch operation {
	case operatorv1alpha1.MigrationOperationResolve:
		return operatorv1alpha1.MigrationPhaseResolving
	case operatorv1alpha1.MigrationOperationVerify:
		return operatorv1alpha1.MigrationPhaseVerifying
	case operatorv1alpha1.MigrationOperationHistory:
		return operatorv1alpha1.MigrationPhaseReading
	default:
		return operatorv1alpha1.MigrationPhaseApplying
	}
}

func migrationRunnerOperation(operation operatorv1alpha1.MigrationOperationType) runner.Operation {
	switch operation {
	case operatorv1alpha1.MigrationOperationResolve:
		return runner.OperationResolve
	case operatorv1alpha1.MigrationOperationVerify:
		return runner.OperationVerify
	case operatorv1alpha1.MigrationOperationHistory:
		return runner.OperationMigrationHistory
	default:
		return runner.OperationMigrationApply
	}
}

func migrationInterval(migration *operatorv1alpha1.PtahMigration) time.Duration {
	if migration.Spec.Interval.Duration > 0 {
		return migration.Spec.Interval.Duration
	}
	return defaultMigrationInterval
}

func migrationFailureRetry(migration *operatorv1alpha1.PtahMigration) time.Duration {
	if migration.Spec.Execution.FailureRetryInterval.Duration > 0 {
		return migration.Spec.Execution.FailureRetryInterval.Duration
	}
	return defaultFailureRetry
}

func setMigrationCondition(
	migration *operatorv1alpha1.PtahMigration,
	conditionType string,
	status metav1.ConditionStatus,
	reason operatorv1alpha1.ConditionReason,
	message string,
) {
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: string(reason), Message: bounded(message, 1024),
		ObservedGeneration: migration.Generation, LastTransitionTime: metav1.Now(),
	})
}

func validateMigrationJobIntent(actual, expected *batchv1.Job, migration *operatorv1alpha1.PtahMigration) error {
	if actual == nil || expected == nil || migration == nil || actual.UID == "" {
		return errors.New("Job identity is incomplete")
	}
	if actual.Namespace != expected.Namespace || actual.Name != expected.Name ||
		!exactControllerOwner(actual.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahMigration", migration.Name, migration.UID) {
		return errors.New("Job ownership does not match the migration controller binding")
	}
	if !reflect.DeepEqual(actual.Labels, expected.Labels) || !reflect.DeepEqual(actual.Annotations, expected.Annotations) {
		return errors.New("Job operation metadata does not match the immutable claim")
	}
	actualCopy := actual.DeepCopy()
	expectedCopy := expected.DeepCopy()
	normalizeSupportedServiceAccountAlias(&actualCopy.Spec.Template.Spec)
	normalizeSupportedServiceAccountAlias(&expectedCopy.Spec.Template.Spec)
	if actualCopy.Spec.TTLSecondsAfterFinished != nil && *actualCopy.Spec.TTLSecondsAfterFinished == jobCleanupTTLSeconds {
		actualCopy.Spec.TTLSecondsAfterFinished = expectedCopy.Spec.TTLSecondsAfterFinished
	}
	if err := normalizeGeneratedJobSelector(actualCopy); err != nil {
		return err
	}
	if err := normalizeGeneratedJobSelector(expectedCopy); err != nil {
		return err
	}
	if !apiequality.Semantic.DeepEqualWithNilDifferentFromEmpty(actualCopy.Spec, expectedCopy.Spec) {
		return errors.New("Job workload spec does not match the immutable operation intent")
	}
	return nil
}

func boundedVersions(versions []int64, limit int) []int64 {
	if len(versions) > limit {
		versions = versions[:limit]
	}
	return append([]int64(nil), versions...)
}
