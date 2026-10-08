// Package controller reconciles credential-isolated desired schema state.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/schemaselector"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	activeOperationFinalizer = "operator.ptah.run/active-operation"
	executorContainerName    = "ptah"
	approvalSchemaIndex      = "spec.schemaRef.name"
	schemaPolicyIndex        = "spec.desired.verificationPolicyFrom.name"
	defaultInterval          = 10 * time.Minute
	defaultFailureRetry      = 30 * time.Second
	terminalPodGrace         = 10 * time.Second
	jobCleanupTTLSeconds     = jobclaim.CleanupTTLSeconds
	maxLockContentionPoll    = 5 * time.Second
	// statusPatchRequeue ends a pass whose last act was a status write the
	// next pass has to start from, and asks for that pass at once. Neither
	// primary watch passes a status-only update, so the write wakes nothing by
	// itself, and the next pass reads the object through the API reader rather
	// than a cache that may not have seen the write yet.
	statusPatchRequeue = time.Millisecond
	// dueRequeue is what requeueAtDeadline returns for a deadline that has
	// already passed, or was never set: the next pass should run at once, the
	// same intent as statusPatchRequeue but for a different reason, so it gets
	// its own name rather than borrowing that comment.
	//
	// It has to be RequeueAfter and not Result{Requeue: true}. In the pinned
	// controller-runtime (pkg/internal/controller/controller.go), the
	// RequeueAfter branch calls Queue.Forget before re-adding the item, and the
	// Requeue branch does not; Requeue adds it through the rate limiter with no
	// Forget in between. A resource sitting at a due-now deadline calls this on
	// every pass, so the per-item exponential backoff that limiter applies
	// would grow on every one of those passes instead of resetting, turning
	// "check again at once" into a wait that gets longer each time nothing else
	// changed.
	dueRequeue = time.Millisecond
	// applyTerminationGrace is recorded on every Apply claim. The builder
	// gives the Apply Pod this grace and tells the runner the same number, and
	// the controller dates the end of the Apply's execution horizon by it.
	applyTerminationGrace = runner.DefaultTerminationGracePeriod
)

var (
	controllerImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	sha256DigestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// JobBuilder turns one already-persisted operation claim into a deterministic
// Job. It must never read Secret content.
type JobBuilder interface {
	NameFor(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error)
	Build(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus, plan *operatorv1alpha1.PtahSchemaPlan) (*batchv1.Job, error)
	NameForMigration(migration *operatorv1alpha1.PtahMigration, operation operatorv1alpha1.MigrationOperationStatus) (string, error)
	BuildMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
		plan *operatorv1alpha1.PtahMigrationPlan,
	) (*batchv1.Job, error)
	// ExecutionBinding is what this manager executes with: the components a
	// plan and an approval bind, and whose change starts a new epoch.
	ExecutionBinding() (
		controllerStateVersion int32,
		ptahVersion string,
		executorImage string,
		runnerProtocolVersion int32,
	)
	// ManagerIdentity is the manager's own release and the runner image built
	// beside it. What the manager publishes records it; nothing binds it.
	ManagerIdentity() (controllerImage, controllerRevision, runnerImage string)
}

// SchemaReconciler implements the Resolve -> Verify -> Observe -> Plan ->
// Approval -> Apply -> Observe convergence state machine.
type SchemaReconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	Logs              PodLogReader
	Results           OperationResults
	ResultCredentials ResultCredentialIssuer
	enrollmentHints   *resultEnrollmentHints
	// ResultReadTimeout bounds the pod/log read of one terminal operation.
	// Zero means defaultResultReadTimeout, which is what the manager runs.
	ResultReadTimeout time.Duration
	// resultLogs is when this process first failed to read each result log
	// it is still waiting on; resultLogLossWindow is measured on it. It is made
	// on first use, by resultLogFailuresOf.
	resultLogs *resultLogFailures
	Jobs       JobBuilder
	// SealKey is this manager process's key pair. The Plan Job builder is
	// given its public half; this reconciler opens the Plan payload sealed
	// to it. The private half is never persisted, so a process that
	// generated a different key pair before a restart cannot open a Plan
	// result it dispatched before that restart, and re-plans instead.
	SealKey planseal.KeyPair
	Plans   planstore.Store
	Locks   *targetlock.Locker
	// LockNamespace is one shared coordination namespace for every managed
	// PtahSchema, including schemas that live in different namespaces.
	LockNamespace string
	Clock         func() time.Time
	Telemetry     telemetry.Observer
	// AdmissionOptions must match cluster-wide built-in Pod admission settings.
	// The exact values are copied into each durable operation snapshot.
	AdmissionOptions podintent.Options

	// dispatch takes a claim to its one permitted create. Its zero value runs
	// the order mutationlifecycle writes down; tests swap two of its steps to
	// show which boundary each of them holds.
	dispatch mutationlifecycle.Driver
}

func (r *SchemaReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(1).Info("reconciliation started")
	defer func() {
		if err != nil {
			logger.Error(err, "reconciliation failed")
		} else {
			logger.V(1).Info(
				"reconciliation completed",
				"requeueAfter", result.RequeueAfter,
			)
		}
		if r.Telemetry == nil {
			return
		}
		if err != nil {
			r.Telemetry.ObserveReconciliation(telemetry.FamilySchema, telemetry.ReconciliationFailed)
			r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.FailureStageController, telemetry.FailureInfrastructure)
			return
		}
		r.Telemetry.ObserveReconciliation(telemetry.FamilySchema, telemetry.ReconciliationSucceeded)
	}()
	return r.reconcile(ctx, request)
}

func (r *SchemaReconciler) reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	schema := &operatorv1alpha1.PtahSchema{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, request.NamespacedName, schema); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := r.rejectUnsupportedStoredControllerState(schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		return r.reconcilePendingLockRelease(ctx, schema)
	}
	if schema.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, schema)
	}
	if result, handled, err := r.reconcileExecutionBinding(ctx, schema); handled || err != nil {
		return result, err
	}
	if schema.Status.ActiveOperation != nil {
		return r.reconcileActive(ctx, schema)
	}
	if controllerutil.ContainsFinalizer(schema, activeOperationFinalizer) && schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
		// The metadata patch advances resourceVersion. Reload and repeat the
		// downgrade fence before a status transition in this pass. An in-flight
		// concurrent status write conflicts with the optimistic finalizer patch;
		// a write after it is caught by this direct reload.
		if err := reader.Get(ctx, request.NamespacedName, schema); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if err := r.rejectUnsupportedStoredControllerState(schema); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := r.now()
	// Post-apply proof is safety work, not ordinary desired-state progress. It
	// survives display-phase changes and takes priority over suspension and a
	// newer generation until no mutating Pod can still run.
	if schema.Status.PendingObservation != nil {
		return r.reconcilePendingObservation(ctx, schema)
	}
	if result, handled, engineErr := r.reconcileEngineSupport(ctx, schema); handled || engineErr != nil {
		return result, engineErr
	}
	if validationErr := schemaselector.Validate(schema.Spec.Policy.Exclude); validationErr != nil {
		return r.operationFailure(ctx, schema, fmt.Errorf("invalid reconciliation policy: %w", validationErr))
	}
	if schema.Status.Plan != nil {
		if bindingErr := r.ensureCurrentStatusExecutionBinding(schema, schema.Status.Plan); bindingErr != nil {
			return r.executionBindingChanged(ctx, schema, bindingErr)
		}
	}
	if schema.Status.Source.Verified {
		if policyErr := r.verifiedSourcePolicyError(ctx, schema); policyErr != nil {
			return r.verificationPolicyChanged(ctx, schema, policyErr)
		}
	}
	if schema.Spec.Suspend {
		before := schema.DeepCopy()
		schema.Status.Phase = operatorv1alpha1.PhaseSuspended
		schema.Status.ObservedGeneration = schema.Generation
		setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, operatorv1alpha1.ReasonRequested, "New database operations are suspended")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		return ctrl.Result{}, r.patchStatus(ctx, before, schema)
	}
	// After suspension and before any claim: a suspended resource runs nothing
	// and needs no verdict about the realm, and the durable mutation-safety
	// boundaries above have already returned for anything in flight.
	verdict, censusErr := takeRealmCensus(ctx, r.Client, schema.Namespace, schema.Spec.Target)
	if censusErr != nil {
		return ctrl.Result{}, censusErr
	}
	if refusal, refused := verdict.refusal(); refused {
		return r.schemaRealmBlocked(ctx, schema, refusal)
	}
	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionFalse, operatorv1alpha1.ReasonActive, "Reconciliation is active")
	if schema.Status.ObservedGeneration != schema.Generation {
		return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
	}

	switch schema.Status.Phase {
	case operatorv1alpha1.PhaseVerifying:
		return r.claim(ctx, schema, operatorv1alpha1.OperationVerify)
	case operatorv1alpha1.PhaseObserving, operatorv1alpha1.PhaseVerifyingConvergence:
		return r.claim(ctx, schema, operatorv1alpha1.OperationObserve)
	case operatorv1alpha1.PhasePlanning:
		return r.claim(ctx, schema, operatorv1alpha1.OperationPlan)
	case operatorv1alpha1.PhaseReadyToApply, operatorv1alpha1.PhaseAwaitingApproval:
		if due(schema.Status.NextReconciliationTime, now) {
			// Refresh the complete evidence chain before considering even an exact
			// approval or an automatic Apply. This prevents an expired plan from
			// racing a moved tag or database drift discovered by reconciliation.
			return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
		}
		return r.reconcileApproval(ctx, schema)
	case operatorv1alpha1.PhaseBlocked:
		if !due(schema.Status.NextReconciliationTime, now) {
			return requeueAtDeadline(schema.Status.NextReconciliationTime, r.now()), nil
		}
		// Every blocked decision is periodically refreshed through the complete
		// Resolve -> Verify -> Observe -> Plan pipeline. This observes moved tags,
		// policy changes, and external database drift without discarding current
		// evidence before the new resolution has been harvested.
		return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
	case operatorv1alpha1.PhaseInSync:
		if !due(schema.Status.NextReconciliationTime, now) {
			return requeueAtDeadline(schema.Status.NextReconciliationTime, r.now()), nil
		}
	case operatorv1alpha1.PhaseFailed:
		if !due(schema.Status.NextReconciliationTime, now) {
			return requeueAtDeadline(schema.Status.NextReconciliationTime, r.now()), nil
		}
	}

	// A changed desired reference or a regular interval always starts with a
	// fresh resolution. This is what makes mutable tags observable.
	return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
}

// schemaRealmBlocked refuses a claim the realm census does not allow: a
// database more than one resource claims, or a PtahRealm that does not admit
// this resource.
//
// Blocked with ApprovalRequired false is the fence the approval webhook reads,
// so no request beginning after this patch can authorize a plan. What ends the
// refusal is another object's change -- a peer's spec, or the realm's grant --
// and a peer's events do not reach this one, so the verdict is re-taken on a
// bounded cadence rather than waited on: the shorter of this resource's
// interval and a minute.
func (r *SchemaReconciler) schemaRealmBlocked(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	refusal realmRefusal,
) (ctrl.Result, error) {
	now := r.now()
	next := realmBlockDeadline(schema.Status.NextReconciliationTime, now, schema.Spec.Interval.Duration)
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.NextReconciliationTime = &next
	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionFalse, operatorv1alpha1.ReasonActive, "Reconciliation is active")
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, refusal.Reason, refusal.Approval)
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, refusal.Reason, refusal.Apply)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, refusal.Reason, refusal.Message)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return requeueAtDeadline(&next, r.now()), nil
}

// reconcileEngineSupport is deliberately after every durable mutation-safety
// boundary and before ordinary desired-state progress. A dispatched Apply and
// its read-only proof must finish first; an undispatched or read-only stale
// operation is retired by reconcileActive before this point. Once idle, the
// unsupported status closes approval admission before bounded approval cleanup
// and before any new Job can be claimed.
func (r *SchemaReconciler) reconcileEngineSupport(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, bool, error) {
	if databaseEngineSupported(schema.Spec.Target.Engine) {
		condition := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionEngineSupported)
		if condition != nil && condition.Status == metav1.ConditionTrue &&
			condition.Reason == string(operatorv1alpha1.ReasonSupportedEngine) {
			return ctrl.Result{}, false, nil
		}
		before := schema.DeepCopy()
		setCondition(
			schema,
			operatorv1alpha1.ConditionEngineSupported,
			metav1.ConditionTrue,
			operatorv1alpha1.ReasonSupportedEngine,
			fmt.Sprintf("Database engine %q is supported", schema.Spec.Target.Engine),
		)
		if apiequality.Semantic.DeepEqual(before.Status, schema.Status) {
			return ctrl.Result{}, false, nil
		}
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
	}

	message := fmt.Sprintf("Database engine %q is not supported", schema.Spec.Target.Engine)
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.NextReconciliationTime = nil
	setCondition(schema, operatorv1alpha1.ConditionEngineSupported, metav1.ConditionFalse, operatorv1alpha1.ReasonUnsupportedEngine, message)
	setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionUnknown, operatorv1alpha1.ReasonUnsupportedEngine, "Database access is disabled for an unsupported engine")
	setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, operatorv1alpha1.ReasonUnsupportedEngine, "Drift is unknown because the database engine is unsupported")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonUnsupportedEngine, "No plan can be produced for an unsupported engine")
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonUnsupportedEngine, "An unsupported engine cannot produce an approvable plan")
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonUnsupportedEngine, "No Apply operation is authorized for an unsupported engine")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonUnsupportedEngine, "Convergence is unknown because the database engine is unsupported")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonUnsupportedEngine, message)

	// Persist the status fence while retaining any current plan identity. The
	// approval webhook observes Phase=Blocked and ApprovalRequired=False, so no
	// request beginning after this patch can authorize a plan. Later passes
	// retire approvals one at a time; approval watch events also catch CREATEs
	// that crossed this fence before the plan pointer is eventually cleared.
	if !apiequality.Semantic.DeepEqual(before.Status, schema.Status) {
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
	}
	marked, err := r.markOneSchemaApprovalStaleWithReason(
		ctx,
		schema,
		operatorv1alpha1.ReasonUnsupportedEngine,
		message,
	)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if marked {
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
	}
	if schema.Status.Plan == nil {
		return ctrl.Result{}, true, nil
	}
	before = schema.DeepCopy()
	schema.Status.Plan = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
}

func databaseEngineSupported(engine operatorv1alpha1.DatabaseEngine) bool {
	switch engine {
	case operatorv1alpha1.DatabaseEnginePostgreSQL, operatorv1alpha1.DatabaseEngineMySQL:
		return true
	default:
		return false
	}
}

// rejectUnsupportedStoredControllerState is the first check after the direct
// API read. A manager that encounters state written by a newer controller must
// not rotate bindings, alter finalizers, renew or release Leases, or interpret
// an operation claim. Existing Job deadlines and Lease horizons provide
// containment until a capable manager resumes the object.
func (r *SchemaReconciler) rejectUnsupportedStoredControllerState(schema *operatorv1alpha1.PtahSchema) error {
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return err
	}
	if schema == nil {
		return fmt.Errorf("schema is unavailable")
	}

	type storedVersion struct {
		name    string
		version int32
	}
	versions := make([]storedVersion, 0, 4)
	if schema.Status.ExecutionBinding != nil {
		versions = append(versions, storedVersion{"status.executionBinding", schema.Status.ExecutionBinding.ControllerStateVersion})
	}
	if schema.Status.Plan != nil {
		versions = append(versions, storedVersion{"status.plan", schema.Status.Plan.ControllerStateVersion})
	}
	if schema.Status.Applied != nil {
		versions = append(versions, storedVersion{"status.applied", schema.Status.Applied.ControllerStateVersion})
	}
	if schema.Status.PendingObservation != nil {
		versions = append(versions, storedVersion{"status.pendingObservation.plan", schema.Status.PendingObservation.Plan.ControllerStateVersion})
	}
	for _, stored := range versions {
		if stored.version < 0 {
			return fmt.Errorf("stored %s controller state version %d is invalid; refusing to interpret or write PtahSchema state", stored.name, stored.version)
		}
		if stored.version > configured.ControllerStateVersion {
			return fmt.Errorf(
				"stored %s controller state version %d exceeds supported version %d; refusing to interpret or write PtahSchema state",
				stored.name,
				stored.version,
				configured.ControllerStateVersion,
			)
		}
	}
	return nil
}

func (r *SchemaReconciler) reconcilePendingObservation(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	pending := schema.Status.PendingObservation
	if pending == nil {
		return ctrl.Result{}, nil
	}
	acquired, requeue, err := r.acquirePendingLock(ctx, schema, pending)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	activePod, err := r.possibleApplyPodActive(ctx, schema, pending)
	if err != nil {
		return ctrl.Result{}, err
	}
	wait := until(pending.ObserveAfter, r.now())

	if schema.Spec.Suspend {
		before := schema.DeepCopy()
		schema.Status.Phase = operatorv1alpha1.PhaseSuspended
		setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, operatorv1alpha1.ReasonRequested, "New database operations are suspended")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		// Suspension prevents the read-only proof Job, but it cannot make an
		// unresolved Apply auditable. Keep renewing the original holder until
		// proof completes (or deletion explicitly abandons the resource), so an
		// intervening mutation cannot be mistaken for this plan's convergence.
		return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
	}

	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionFalse, operatorv1alpha1.ReasonActive, "Reconciliation is active")
	if activePod {
		return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
	}
	if wait > 0 {
		if wait > maxLockContentionPoll {
			wait = maxLockContentionPoll
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	operation := operatorv1alpha1.OperationObserve
	if pending.PlanRequired {
		operation = operatorv1alpha1.OperationPlan
	}
	return r.claim(ctx, schema, operation)
}

// reconcileExecutionBinding establishes one durable evidence epoch before any
// operation is claimed or any existing read-only result is accepted. The
// explicit component tuple is audit evidence; the opaque epoch prevents an old
// approval from becoming current again after a byte-identical rollback.
//
// Only the components that decide what a plan means when it runs are
// compared. A manager release that changes its own image, its revision or the
// runner image built beside it and nothing else finds the binding equal: the
// epoch, the current plan and any pending approval carry over, and work the
// previous manager dispatched is adopted as it stands.
func (r *SchemaReconciler) reconcileExecutionBinding(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, bool, error) {
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return ctrl.Result{}, true, err
	}
	// One retirement at a time: what the last rotation owes is worked off
	// before another rotation can replace the epoch the record names. Even a
	// configuration rolled back to the retired components waits, and then
	// claims an epoch of its own rather than reopening the retired one.
	if schema.Status.PendingBindingRetirement != nil {
		return r.reconcileBindingRetirement(ctx, schema)
	}

	current := schema.Status.ExecutionBinding
	if current != nil && validExecutionBindingID(current.Epoch) &&
		executionBindingComponentsEqual(current, configured) {
		if operation := schema.Status.ActiveOperation; operation != nil && operation.ExecutionBindingID != current.Epoch {
			result, err := r.executionBindingChanged(
				ctx,
				schema,
				fmt.Errorf("active %s operation belongs to an unprovable execution-binding epoch", operation.Type),
			)
			return result, true, err
		}
		return ctrl.Result{}, false, nil
	}

	operation := schema.Status.ActiveOperation
	if schemaOperation(operation).Mutating && schemaMayHaveDispatched(operation) {
		result, err := r.finishUncertainApplyForExecutionBindingChange(
			ctx,
			schema,
			configured,
			fmt.Errorf("execution binding changed after Apply dispatch"),
		)
		return result, true, err
	}

	if !hasExecutionBindingEvidence(schema) {
		binding, err := newExecutionBinding(configured)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		before := schema.DeepCopy()
		schema.Status.ExecutionBinding = binding
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
	}

	result, err := r.executionBindingChanged(
		ctx,
		schema,
		fmt.Errorf("configured execution components changed"),
	)
	return result, true, err
}

func (r *SchemaReconciler) configuredExecutionBinding() (*operatorv1alpha1.ExecutionBindingStatus, error) {
	if r.Jobs == nil {
		return nil, fmt.Errorf("Job builder is not configured")
	}
	controllerStateVersion, ptahVersion, executorImage, protocolVersion := r.Jobs.ExecutionBinding()
	if controllerStateVersion < 1 ||
		strings.TrimSpace(ptahVersion) == "" || strings.TrimSpace(ptahVersion) != ptahVersion ||
		strings.TrimSpace(executorImage) == "" || strings.TrimSpace(executorImage) != executorImage || protocolVersion < 1 {
		return nil, fmt.Errorf("Job builder execution binding is incomplete")
	}
	return &operatorv1alpha1.ExecutionBindingStatus{
		ControllerStateVersion: controllerStateVersion,
		PtahVersion:            ptahVersion, ExecutorImage: executorImage,
		RunnerProtocolVersion: protocolVersion,
	}, nil
}

// managerIdentity is what this manager records on the plans it publishes.
func (r *SchemaReconciler) managerIdentity() (controllerImage, controllerRevision, runnerImage string, err error) {
	if r.Jobs == nil {
		return "", "", "", fmt.Errorf("the Job builder is not configured")
	}
	controllerImage, controllerRevision, runnerImage = r.Jobs.ManagerIdentity()
	if !controllerImagePattern.MatchString(controllerImage) ||
		controllerstate.ValidateRevision(controllerRevision) != nil ||
		strings.TrimSpace(runnerImage) == "" || strings.TrimSpace(runnerImage) != runnerImage {
		return "", "", "", fmt.Errorf("the Job builder's manager identity is incomplete")
	}
	return controllerImage, controllerRevision, runnerImage, nil
}

func newExecutionBinding(configured *operatorv1alpha1.ExecutionBindingStatus) (*operatorv1alpha1.ExecutionBindingStatus, error) {
	if configured == nil {
		return nil, fmt.Errorf("configured execution binding is unavailable")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("create execution-binding ID: %w", err)
	}
	binding := configured.DeepCopy()
	binding.Epoch = "v1-" + hex.EncodeToString(nonce)
	return binding, nil
}

func validExecutionBindingID(id string) bool {
	if len(id) != len("v1-")+32 || !strings.HasPrefix(id, "v1-") {
		return false
	}
	encoded := strings.TrimPrefix(id, "v1-")
	if encoded != strings.ToLower(encoded) {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == 16
}

// executionBindingComponentsEqual compares what an epoch binds. Nothing in it
// names the manager's own release, so a manager-only upgrade compares equal.
func executionBindingComponentsEqual(
	left *operatorv1alpha1.ExecutionBindingStatus,
	right *operatorv1alpha1.ExecutionBindingStatus,
) bool {
	return left != nil && right != nil &&
		left.ControllerStateVersion == right.ControllerStateVersion &&
		left.PtahVersion == right.PtahVersion &&
		left.ExecutorImage == right.ExecutorImage &&
		left.RunnerProtocolVersion == right.RunnerProtocolVersion
}

func hasExecutionBindingEvidence(schema *operatorv1alpha1.PtahSchema) bool {
	if schema == nil {
		return false
	}
	return schema.Status.ActiveOperation != nil || schema.Status.PendingObservation != nil ||
		schema.Status.Plan != nil || schema.Status.Applied != nil ||
		schema.Status.ObservedGeneration != 0 || schema.Status.Phase != "" ||
		len(schema.Status.Conditions) != 0 ||
		!reflect.DeepEqual(schema.Status.Source, operatorv1alpha1.SchemaSourceStatus{}) ||
		!reflect.DeepEqual(schema.Status.Target, operatorv1alpha1.TargetStatus{})
}

func (r *SchemaReconciler) possibleApplyPodActive(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) (bool, error) {
	if pending == nil || pending.ApplyOperationID == "" {
		return false, fmt.Errorf("pending observation lacks an Apply operation identity")
	}
	// A retirement record that names this Apply's Job holds the proof back
	// until the Job is accounted for: its UID adopted or given up on, and its
	// cleanup scheduled. Each step is a status write of its own, so the pass
	// reports a possibly active Pod and the next one re-reads.
	retired := retiredApplyJob(schema, pending) != nil
	if pending.ApplyJobUID == "" {
		if !retired {
			// An uncertain create is protected by the immutable ObserveAfter
			// horizon instead of Pod discovery.
			return false, nil
		}
		return true, r.adoptRetiredApplyJobUID(ctx, schema, pending)
	}
	if pending.ApplyJobName == "" {
		return false, fmt.Errorf("pending observation has a Job UID without its immutable name")
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, pending.ApplyJobName, pending.ApplyJobUID)
	if err != nil {
		return false, err
	}
	evidence := podIdentityEvidence(pods)
	before := schema.DeepCopy()
	if mergePodEvidence(pending, evidence) {
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return false, err
		}
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return true, nil
		}
	}
	if retired {
		return true, r.cleanupRetiredApplyJob(ctx, schema, pending)
	}
	return false, nil
}

func (r *SchemaReconciler) reconcileDeletion(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(schema, activeOperationFinalizer) {
		return ctrl.Result{}, nil
	}
	if schema.Status.ActiveOperation != nil {
		operation := schema.Status.ActiveOperation
		if operation.LeaseContinuityLost {
			return r.recoverLeaseContinuity(ctx, schema)
		}
		job := &batchv1.Job{}
		err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}, job)
		dispatchedApplyUnknown := schemaOperation(operation).Mutating &&
			schemaMayHaveDispatched(operation) &&
			(apierrors.IsNotFound(err) || err == nil && (operation.JobUID != "" && operation.JobUID != job.UID || !ownedByUID(job.OwnerReferences, schema.UID)))
		// A running Job is waited on only while the claim holds the database
		// lock: an Apply, a Plan, or the Observe that proves an Apply. The lock
		// goes back when the claim is dropped, and another claimant must not
		// get it while this one's Pod can still reach the database. A Resolve,
		// a Verify or an ordinary Observe holds nothing, so its claim is
		// discarded and cascading deletion takes the Job, as a PtahMigration
		// discards a read-only claim. Waiting on it would hold the resource
		// for the Job's whole deadline when its Pod is refused at admission
		// and never runs.
		if err == nil && !dispatchedApplyUnknown && !jobTerminal(job) && schemaClaimHoldsLock(schema) {
			acquired, requeue, lockErr := r.acquireOperationLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("observe active Job during deletion: %w", err)
		}
		if dispatchedApplyUnknown {
			acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("dispatched Apply Job identity was lost during deletion"))
		}
		if schemaOperation(operation).Mutating && !dispatchedApplyUnknown {
			if err == nil {
				evidence, _, evidenceErr := r.collectTerminalPodEvidence(ctx, schema, job)
				if evidenceErr != nil &&
					!errors.Is(evidenceErr, errTerminalPodMultiplicity) &&
					!errors.Is(evidenceErr, errTerminalPodIntent) {
					return ctrl.Result{}, evidenceErr
				}
				if evidenceErr != nil || !evidence.Trusted {
					return r.finishUncertainApplyWithEvidence(
						ctx,
						schema,
						nil,
						fmt.Errorf("Apply executor termination is not proven during deletion"),
						evidence.PodUIDs,
						evidence.PodCount,
						true,
					)
				}
			}
		}
		// Nothing the claim dispatched can still be writing here. The Job of a
		// claim that holds the realm was waited on above until it stopped, and
		// an Apply whose executor did not provably stop left above as an
		// outcome nobody established.
		err = r.retireClaim(ctx, schema.DeepCopy(), schema, mutationlifecycle.DispositionDiscard)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.observeOperation(operation, telemetry.OperationCanceled)
	}
	if schema.Status.PendingObservation != nil {
		pending := schema.Status.PendingObservation
		acquired, requeue, err := r.acquirePendingLock(ctx, schema, pending)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		activePod, err := r.possibleApplyPodActive(ctx, schema, pending)
		if err != nil {
			return ctrl.Result{}, err
		}
		if activePod || until(pending.ObserveAfter, r.now()) > 0 {
			return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
		}
		before := schema.DeepCopy()
		if err := stagePendingLockRelease(schema, pending); err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingObservation = nil
		if err := r.patchStatus(ctx, before, schema); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if schema.Status.PendingLockRelease != nil {
			if err := r.completePendingLockRelease(ctx, schema); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, r.removeActiveFinalizer(ctx, schema)
}

func (r *SchemaReconciler) reconcileActive(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return ctrl.Result{}, nil
	}
	if operation.LeaseContinuityLost {
		return r.recoverLeaseContinuity(ctx, schema)
	}
	if schemaClaimServesProof(schema) {
		// A Job controller can create another exact-owner Apply Pod after the
		// first terminal attempt was recorded. Refresh that immutable evidence
		// before inspecting or consuming any post-Apply proof result. An active
		// attempt blocks proof consumption; newly observed terminal evidence
		// changes the proof fingerprint and makes the current result stale.
		acquired, requeue, lockErr := r.acquirePendingObservationLock(ctx, schema)
		if lockErr != nil {
			return ctrl.Result{}, lockErr
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		activePod, podErr := r.possibleApplyPodActive(ctx, schema, schema.Status.PendingObservation)
		if podErr != nil {
			return ctrl.Result{}, podErr
		}
		if activePod {
			return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
		}
		operation = schema.Status.ActiveOperation
	}
	if schemaOperation(operation).Mutating && !schemaMayHaveDispatched(operation) &&
		schema.Status.Plan != nil {
		if bindingErr := r.ensureCurrentStatusExecutionBinding(schema, schema.Status.Plan); bindingErr != nil {
			return r.executionBindingChanged(ctx, schema, bindingErr)
		}
	}
	if !schema.Spec.Suspend && schema.Status.Phase == operatorv1alpha1.PhaseFailed {
		failedRetryTime := r.now()
		if !due(schema.Status.NextReconciliationTime, failedRetryTime) {
			result := requeueAtDeadline(schema.Status.NextReconciliationTime, failedRetryTime)
			if schemaClaimServesProof(schema) {
				// Retry timers are user-configurable and may exceed the immutable
				// Apply Lease. Renew the same holder while proof is pending.
				if result.RequeueAfter > maxLockContentionPoll {
					result.RequeueAfter = maxLockContentionPoll
				}
			}
			return result, nil
		}
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}
	err := r.directReader().Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		if schemaOperation(operation).Mutating && schemaMayHaveDispatched(operation) {
			acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("dispatched Apply Job is missing and will not be recreated"))
		}
		return r.dispatch.Dispatch(ctx, &schemaDispatch{r: r, schema: schema})
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read active Job: %w", err)
	}
	// Suspension is the stop button, and a read-only operation has produced
	// nothing durable. Waiting for its dispatched Job to reach its own active
	// deadline holds a suspended schema for that entire budget, a quarter of an
	// hour by default. Discard it exactly as a changed input discards a
	// dispatched read-only operation: the Job keeps its own deadline, and its
	// Pods can no longer claim an operation this schema no longer has. Post-apply
	// proof is excluded because that work outranks suspension by design.
	if schema.Spec.Suspend && schema.Status.PendingObservation == nil && isReadOnlyOperation(operation) {
		return r.suspendActiveOperation(ctx, schema)
	}
	// The same decision both families make about the Job under a reserved
	// name, with the wording each one owes its reader kept here.
	verdict, cause := mutationlifecycle.VerdictFor(mutationlifecycle.JobClaim{
		Mutating:        schemaOperation(operation).Mutating,
		DispatchStarted: operation.DispatchStarted,
		RecordedJobUID:  string(operation.JobUID),
		Found:           true,
		FoundJobUID:     string(job.UID),
		OwnedExactly: exactControllerOwner(job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(), "PtahSchema", schema.Name, schema.UID),
	})
	switch verdict {
	case mutationlifecycle.VerdictUnaccounted:
		return r.finishUnknownRunningApply(ctx, schema, errors.New(unaccountedSchemaJobReason(cause)))
	case mutationlifecycle.VerdictRetry:
		return r.retryOperation(ctx, schema, nil, errors.New(retriedSchemaJobReason(cause)))
	}
	currentInputs, inputErr := r.operationInputFingerprint(schema, operation.Type)
	if inputErr == nil && currentInputs == operation.InputFingerprint {
		expectedJob, expectedErr := r.expectedJob(ctx, schema, operation)
		if expectedErr != nil {
			if schemaOperation(operation).Mutating {
				return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("rebuild immutable Apply Job intent: %w", expectedErr))
			}
			return r.retryOperation(ctx, schema, nil, fmt.Errorf("rebuild immutable Job intent: %w", expectedErr))
		}
		if intentErr := validateAdoptedJobIntent(job, expectedJob, schema, operation); intentErr != nil {
			if schemaOperation(operation).Mutating {
				return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("dispatched Apply Job intent changed: %w", intentErr))
			}
			return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job intent changed: %w", intentErr))
		}
	} else if envelopeErr := validateJobEnvelope(job, schema, operation); envelopeErr != nil {
		// The inputs moved, so the claim cannot rebuild its Job, but it still
		// fixes the Job's epoch, labels, annotations and Pod template. A Job
		// that fails them is not recorded, and is settled as one that fails
		// the rebuild is.
		if schemaOperation(operation).Mutating {
			return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("dispatched Apply Job is not its claim's: %w", envelopeErr))
		}
		return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job is not its claim's: %w", envelopeErr))
	}
	if operation.JobUID == "" {
		before := schema.DeepCopy()
		schema.Status.ActiveOperation.JobUID = job.UID
		if schemaOperation(operation).Mutating {
			schema.Status.ActiveOperation.DispatchStarted = true
		}
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		operation = schema.Status.ActiveOperation
	}
	if schemaClaimHoldsLock(schema) {
		acquired, requeue, lockErr := r.acquireOperationLock(ctx, schema)
		if lockErr != nil {
			return ctrl.Result{}, lockErr
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}
	if !jobTerminal(job) {
		if err := r.reportPodAdmission(ctx, schema, job); err != nil {
			return ctrl.Result{}, err
		}
		if durableDeliveryRequested(job) {
			engine := ""
			if operation.Target != nil {
				engine = string(operation.Target.Engine)
			}
			issued, issueErr := issueResultCredential(ctx, r.directReader(), r.ResultCredentials, resultEnrollmentHintsOf(&r.enrollmentHints), schema, "PtahSchema", job, operation.AdmissionSnapshot, operation.ExecutionBindingID, operation.InputFingerprint, string(schemaOperation(operation).Runner), operation.ID, engine)
			if issueErr != nil {
				r.event(schema, corev1.EventTypeWarning, "ResultCredentialFailed", "Result delivery credential is not ready; issuance will be retried")
			}
			if issueErr != nil || !issued {
				return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
			}
		}
		return ctrl.Result{RequeueAfter: activeJobPollInterval(schemaClaimHoldsLock(schema))}, nil
	}
	current, currentErr := r.operationInputFingerprint(schema, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = fmt.Errorf("operation inputs changed while the Job was running")
		}
		if mutationlifecycle.HarvestFailure(
			mutationlifecycle.FaultInputsChanged, schemaOperation(operation).Mutating,
		) == mutationlifecycle.DispositionUnaccounted {
			// Once an Apply Job exists, a mutation may have started even when
			// its formerly exact inputs became stale. Never classify that case
			// like an undispatched read-only operation: force fresh observation
			// before any later plan can run.
			return r.finishUncertainApply(ctx, schema, job, currentErr)
		}
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return r.discardStaleOperation(ctx, schema, currentErr)
	}

	evidence, err := r.terminalLogs(ctx, schema, job)
	if err != nil {
		if errors.Is(err, errResultReadCooling) {
			return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
		}
		if errors.Is(err, errTerminalPodPending) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if errors.Is(err, errTerminalPodMultiplicity) || errors.Is(err, errTerminalPodIntent) {
			failure := err
			if errors.Is(err, errTerminalPodMultiplicity) {
				failure = fmt.Errorf("Job produced multiple executor Pods")
			}
			if mutationlifecycle.HarvestFailure(
				mutationlifecycle.FaultPodMultiplicity, schemaOperation(operation).Mutating,
			) == mutationlifecycle.DispositionUnaccounted {
				return r.finishUncertainApplyWithEvidence(ctx, schema, job, failure, evidence.PodUIDs, evidence.PodCount, true)
			}
			return r.retryOperation(ctx, schema, job, failure)
		}
		if errors.Is(err, errResultReadRetry) {
			if errors.Is(err, context.DeadlineExceeded) {
				r.event(schema, corev1.EventTypeWarning, "ResultReadTimedOut",
					"reading the %s result took longer than its bound: %s",
					operation.Type, bounded(err.Error(), 512))
			} else {
				r.event(schema, corev1.EventTypeWarning, "ResultReadFailed",
					"reading the %s result failed and will be tried again: %s",
					operation.Type, bounded(err.Error(), 512))
			}
			return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	result, parseErr := evidence.parseResult(schemaOperation(operation).Runner, operation.ID)
	if requeue, wait := awaitFrameArrival(job, parseErr, r.now()); wait {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if refusal := runnerProtocolRefusal(result, parseErr); refusal != nil {
		r.event(schema, corev1.EventTypeWarning, "RunnerProtocolMismatch",
			"the %s runner refused the Job before starting the executor: %s", operation.Type, bounded(refusal.Error(), 512))
		if schemaOperation(operation).Mutating {
			// The refusal says this Pod started nothing, and it is the Pod's
			// own account. A Job may run its Pod more than once, so an Apply
			// still owes the read-only proof every Apply error owes; that
			// proof meets the same runner and is refused the same way, which
			// names the cause where a reader looks.
			return r.finishUncertainApplyWithEvidence(ctx, schema, job,
				fmt.Errorf("apply result is uncertain: %w", refusal), evidence.PodUIDs, evidence.PodCount, !evidence.Trusted)
		}
		return r.retryOperationAs(ctx, schema, job, operatorv1alpha1.ReasonRunnerProtocolMismatch, refusal, nil)
	}
	if parseErr != nil || !jobSucceeded(job) {
		if mutationlifecycle.HarvestFailure(
			mutationlifecycle.FaultUnreadableResult, schemaOperation(operation).Mutating,
		) == mutationlifecycle.DispositionUnaccounted {
			failure := parseErr
			if failure == nil {
				failure = fmt.Errorf("Apply Job did not complete successfully")
			}
			return r.finishUncertainApplyWithEvidence(
				ctx, schema, job, fmt.Errorf("apply result is uncertain: %w", failure),
				evidence.PodUIDs, evidence.PodCount, !evidence.Trusted,
			)
		}
		if parseErr != nil {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("read %s result: %w", operation.Type, parseErr))
		}
		return r.retryOperation(ctx, schema, job, fmt.Errorf("%s Job failed", operation.Type))
	}
	if result.Error != nil {
		err := fmt.Errorf("%s: %s", result.Error.Code, bounded(result.Error.Message, 512))
		if operation.Type == operatorv1alpha1.OperationVerify && result.Error.Code == "verification_refused" {
			localDigestPinRefusal := result.ChildExitCode == 0 && len(result.VerificationRequirements) == 1 &&
				result.VerificationRequirements[0] == "require_digest_pin"
			if result.Stdout != "" || (result.ChildExitCode != 2 && !localDigestPinRefusal) ||
				len(result.VerificationRequirements) == 0 ||
				result.ResolvedDigest != schema.Status.Source.Digest ||
				result.VerificationPolicyDigest == "" || operation.VerificationPolicyUID == "" ||
				result.VerificationPolicyDigest != operation.VerificationPolicyDigest {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("verification refusal evidence does not match the resolved artifact"))
			}
			if policyErr := r.verificationResultPolicyError(ctx, schema, result.VerificationPolicyDigest); policyErr != nil {
				if err := r.markJobHarvested(ctx, job); err != nil {
					return ctrl.Result{}, err
				}
				return r.verificationPolicyChanged(ctx, schema, policyErr)
			}
			return r.blockVerification(ctx, schema, job, result.VerificationRequirements, operation.VerificationPolicyUID, result.VerificationPolicyDigest)
		}
		// A fenced plan is not a run that went wrong: Ptah refused to compute a
		// plan that would change a table the policy protects, and it saved
		// nothing. Naming it keeps a reader from reading a refusal as a fault.
		if operation.Type == operatorv1alpha1.OperationPlan && result.Error.Code == "protected_table" {
			return r.refuseProtectedTable(ctx, schema, job, result.Error.Message)
		}
		if schemaOperation(operation).Mutating || result.Uncertain {
			// A terminal result belongs to only one Pod attempt. Kubernetes may
			// start a Job workload more than once, so no child-side pre-mutation
			// claim can prove that every attempt stayed pre-mutation. Once Job
			// dispatch was possible, every Apply error requires read-only proof.
			return r.finishUncertainApplyWithEvidence(ctx, schema, job, err, evidence.PodUIDs, evidence.PodCount, !evidence.Trusted)
		}
		return r.retryOperation(ctx, schema, job, err)
	}
	if result.Truncation != nil && result.Truncation.Stdout {
		return r.retryOperation(ctx, schema, job, fmt.Errorf("%s result was truncated", operation.Type))
	}
	if schemaOperation(operation).Mutating {
		if schema.Status.Plan == nil || result.CoordinationDigest != schema.Status.Plan.CoordinationDigest ||
			result.TargetIdentityDigest != schema.Status.Plan.TargetIdentityDigest {
			return r.finishUncertainApplyWithEvidence(
				ctx, schema, job, fmt.Errorf("apply target identity changed after approval"),
				evidence.PodUIDs, evidence.PodCount, !evidence.Trusted,
			)
		}
	}
	return r.consumeResultWithTransport(ctx, schema, job, result, evidence.PodUIDs, evidence.PodCount, evidence.Durable)
}

func (r *SchemaReconciler) recoverLeaseContinuity(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil || !operation.LeaseContinuityLost || !schemaClaimHoldsLock(schema) {
		return ctrl.Result{}, fmt.Errorf("database lock continuity recovery lacks an active locked operation")
	}
	acquired, requeue, err := r.acquireOperationLock(ctx, schema)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	job := &batchv1.Job{}
	jobErr := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}, job)
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return ctrl.Result{}, fmt.Errorf("read operation Job after database lock continuity loss: %w", jobErr)
	}
	honestJob := jobErr == nil && (operation.JobUID == "" || operation.JobUID == job.UID) &&
		exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema", schema.Name, schema.UID)
	if schemaOperation(operation).Mutating {
		if !honestJob || schema.DeletionTimestamp != nil {
			job = nil
		}
		return r.finishUncertainApply(ctx, schema, job, fmt.Errorf("database lock continuity was lost during Apply"))
	}
	if honestJob && !jobTerminal(job) {
		return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
	}
	if honestJob && schema.DeletionTimestamp == nil {
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}

	before := schema.DeepCopy()
	schema.Status.Plan = nil
	schema.Status.NextReconciliationTime = nil
	if schema.Status.PendingObservation != nil {
		schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonLeaseContinuityLost, "The result was discarded because the database lock epoch changed")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonLeaseContinuityLost, "A fresh observation is required after database lock continuity was lost")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonLeaseContinuityLost, "The operator is restarting read-only observation")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) verificationResultPolicyError(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	resultDigest string,
) error {
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationVerify ||
		operation.VerificationPolicyUID == "" || operation.VerificationPolicyDigest == "" {
		return fmt.Errorf("verification operation lacks an immutable policy binding")
	}
	current, err := policy.ConfigMapBinding(
		ctx,
		r.directReader(),
		schema.Namespace,
		schema.Spec.Desired.VerificationPolicyFrom,
	)
	if err != nil {
		return err
	}
	if resultDigest == "" || resultDigest != operation.VerificationPolicyDigest ||
		current.UID != operation.VerificationPolicyUID || current.Digest != operation.VerificationPolicyDigest {
		return fmt.Errorf("verification policy object changed while the verification Job was running")
	}
	return nil
}

func (r *SchemaReconciler) verifiedSourcePolicyError(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) error {
	if !schema.Status.Source.Verified || schema.Status.Source.VerificationPolicyUID == "" ||
		schema.Status.Source.VerificationPolicyDigest == "" {
		return fmt.Errorf("verified source lacks an immutable verification policy binding")
	}
	current, err := policy.ConfigMapBinding(
		ctx,
		r.directReader(),
		schema.Namespace,
		schema.Spec.Desired.VerificationPolicyFrom,
	)
	if err != nil {
		return err
	}
	if current.UID != schema.Status.Source.VerificationPolicyUID ||
		current.Digest != schema.Status.Source.VerificationPolicyDigest {
		return fmt.Errorf("verification policy object changed after artifact verification")
	}
	return nil
}

func pendingProofPolicyBindingError(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) error {
	if schema == nil || pending == nil || pending.Plan.VerificationPolicyUID == "" ||
		pending.Plan.VerificationPolicyDigest == "" {
		return fmt.Errorf("post-apply proof lacks an immutable verification policy binding")
	}
	// Execution-component rollover deliberately clears Source.Verified before
	// post-Apply proof runs. The immutable pending snapshot remains valid input
	// for attributing the already-dispatched Apply; it cannot authorize another
	// mutation, and current-source verification is refreshed afterward.
	if schema.Status.Source.VerificationPolicyUID != pending.Plan.VerificationPolicyUID ||
		schema.Status.Source.VerificationPolicyDigest != pending.Plan.VerificationPolicyDigest ||
		schema.Status.Source.Digest != pending.Source.Digest ||
		schema.Status.Source.ResolvedReference != pending.Source.ResolvedReference ||
		pending.Plan.ArtifactDigest != pending.Source.Digest ||
		pending.Plan.CoordinationDigest != pending.CoordinationDigest {
		return fmt.Errorf("post-apply proof policy or source snapshot is internally inconsistent")
	}
	return nil
}

func (r *SchemaReconciler) blockVerification(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	requirements []string,
	policyUID types.UID,
	policyDigest string,
) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	operation := schema.Status.ActiveOperation
	now := metav1.NewTime(r.now())
	next := metav1.NewTime(r.now().Add(interval(schema)))
	before := schema.DeepCopy()
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.LastAttemptTime = &now
	schema.Status.NextReconciliationTime = &next
	schema.Status.Source.Verified = false
	schema.Status.Source.VerifiedAt = nil
	schema.Status.Source.VerificationPolicyUID = policyUID
	schema.Status.Source.VerificationPolicyDigest = policyDigest
	schema.Status.Plan = nil
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	clearFailure(schema)
	message := fmt.Sprintf("Verification policy refused the artifact with %d finding(s)", len(requirements))
	if len(requirements) > 0 {
		message += ": " + strings.Join(requirements, ", ")
	}
	setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyRefused, bounded(message, 512))
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonArtifactUnverified, "No plan may be used for an artifact refused by policy")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyRefused, "In-sync status requires an artifact accepted by the current verification policy")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyRefused, "Artifact verification policy must be satisfied before database access")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionAccounted); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeWarning, "ArtifactVerificationRefused", "%s", bounded(message, 512))
	return ctrl.Result{RequeueAfter: interval(schema)}, nil
}

// refuseProtectedTable records a plan Ptah refused because it would change a
// table the policy fences off.
//
// It is a refusal rather than a failure of the run, and it carries no override:
// the fence is the statement that no approval, no allowDestructive and no
// severity makes this change permissible from the declarative path. So the
// conditions name it, the resource reads as blocked the way a refused
// verification policy does, and no failure is reported for a run that did
// exactly what the policy asked.
//
// It also ends the operation rather than retrying it. A retry would re-plan
// within the second and be refused again for as long as the fence and the
// artifact disagree, which is a loop that reads as progress and holds a
// database lock on every pass. The refusal stands until the fence goes or the
// artifact stops asking for the change, and the blocked interval is when this
// operator looks for either.
func (r *SchemaReconciler) refuseProtectedTable(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	message string,
) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	// No plan survives the refusal, so an approval recorded for the plan this
	// operation replaced may not stay approvable.
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	operation := schema.Status.ActiveOperation
	now := metav1.NewTime(r.now())
	next := metav1.NewTime(r.now().Add(interval(schema)))
	before := schema.DeepCopy()
	schema.Status.Plan = nil
	schema.Status.LastAttemptTime = &now
	schema.Status.NextReconciliationTime = &next
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	clearFailure(schema)
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonProtectedTable,
		"No plan may change a table spec.policy.protectedTables fences off")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse,
		operatorv1alpha1.ReasonProtectedTable,
		"The artifact asks for a change to a protected table, so the managed scope cannot converge")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonProtectedTable,
		"Remove the protectedTables entry to plan the change, or write the rows as a migration")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionAccounted); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.event(schema, corev1.EventTypeWarning, "ProtectedTableRefused", "%s", bounded(message, 512))
	return requeueAtDeadline(&next, r.now()), nil
}

func (r *SchemaReconciler) suspendActiveOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseSuspended
	schema.Status.ObservedGeneration = schema.Generation
	if operation != nil && operation.Type == operatorv1alpha1.OperationResolve {
		markSourceRefreshSuspended(schema)
	}
	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, operatorv1alpha1.ReasonRequested, "New database operations are suspended")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationCanceled)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SchemaReconciler) consumeResult(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	result runner.Result,
	podUIDs []types.UID,
	podCount int32,
) (ctrl.Result, error) {
	return r.consumeResultWithTransport(ctx, schema, job, result, podUIDs, podCount, false)
}

func (r *SchemaReconciler) consumeResultWithTransport(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, result runner.Result, podUIDs []types.UID, podCount int32, durable bool) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	var observedDrift *bool
	var completedProofPolicyErr error
	var completedProofExecutionBindingErr error
	now := metav1.NewTime(r.now())
	schema.Status.LastAttemptTime = &now
	if schemaClaimServesProof(schema) {
		schema.Status.ObservedGeneration = schema.Status.PendingObservation.ApplyGeneration
	} else {
		schema.Status.ObservedGeneration = schema.Generation
	}
	clearFailure(schema)

	switch schema.Status.ActiveOperation.Type {
	case operatorv1alpha1.OperationResolve:
		if result.Stdout != "" || ocireference.ValidateResolution(
			schema.Spec.Desired.OCIRef, result.ResolvedReference, result.ResolvedDigest,
		) != nil {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("resolved reference does not match the requested source"))
		}
		changed := schema.Status.Source.Digest != result.ResolvedDigest || schema.Status.Source.RequestedReference != schema.Spec.Desired.OCIRef
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference: schema.Spec.Desired.OCIRef,
			ResolvedReference:  result.ResolvedReference,
			Digest:             result.ResolvedDigest,
			MediaType:          result.ResolvedMediaType,
			Size:               result.ResolvedSize,
			ResolvedAt:         &now,
		}
		if changed {
			if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
				return ctrl.Result{}, err
			}
			schema.Status.Plan = nil
			schema.Status.Applied = nil
		}
		schema.Status.Phase = operatorv1alpha1.PhaseVerifying
		// Persist when the next step became due before retiring this claim.
		schema.Status.NextReconciliationTime = &now
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionTrue, operatorv1alpha1.ReasonDigestPinned, "Desired OCI reference resolved to immutable content")
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, operatorv1alpha1.ReasonPending, "Resolved content has not been verified")
	case operatorv1alpha1.OperationVerify:
		if result.Stdout != "" || len(result.VerificationRequirements) != 0 || result.ResolvedDigest != schema.Status.Source.Digest ||
			result.ObservedArtifactType != dataplane.SchemaArtifactType || result.VerificationPolicyDigest == "" ||
			operation.VerificationPolicyUID == "" || result.VerificationPolicyDigest != operation.VerificationPolicyDigest {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("verification evidence does not match the resolved schema artifact"))
		}
		if policyErr := r.verificationResultPolicyError(ctx, schema, result.VerificationPolicyDigest); policyErr != nil {
			return r.verificationPolicyChanged(ctx, schema, policyErr)
		}
		schema.Status.Source.Verified = true
		schema.Status.Source.ArtifactType = result.ObservedArtifactType
		schema.Status.Source.VerificationPolicyUID = operation.VerificationPolicyUID
		schema.Status.Source.VerificationPolicyDigest = result.VerificationPolicyDigest
		schema.Status.Source.VerifiedAt = &now
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
		schema.Status.NextReconciliationTime = &now
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, operatorv1alpha1.ReasonPolicySatisfied, "Artifact type and verification policy were satisfied")
	case operatorv1alpha1.OperationObserve:
		if result.Stdout != "" || result.CoordinationDigest == "" || result.TargetIdentityDigest == "" ||
			result.DriftReportDigest == "" || result.DriftFindingCount < 0 {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("observation lacks credential-free target evidence"))
		}
		pending := schema.Status.PendingObservation
		engine := schema.Spec.Target.Engine
		var expectedCoordination string
		if pending != nil {
			engine = pending.Target.Engine
			expectedCoordination = pending.CoordinationDigest
			if pending.Plan.CoordinationDigest != expectedCoordination || result.CoordinationDigest != expectedCoordination ||
				result.TargetIdentityDigest != pending.Plan.TargetIdentityDigest {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("post-apply observation target does not match the applied plan"))
			}
		} else {
			var err error
			expectedCoordination, err = coordination.Digest(schema.Namespace, schema.Spec.Target)
			if err != nil {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("derive database coordination digest: %w", err))
			}
		}
		if result.CoordinationDigest != expectedCoordination {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("observation coordination realm does not match the configured target"))
		}
		if !dataplane.DialectMatches(string(engine), result.ObservedDialect) {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("observation dialect %q does not match target engine %q", result.ObservedDialect, engine))
		}
		findings := make([]operatorv1alpha1.DriftFindingStatus, len(result.DriftFindings))
		for index, finding := range result.DriftFindings {
			findings[index] = operatorv1alpha1.DriftFindingStatus{
				Category: finding.Category,
				Count:    finding.Count,
				Severity: finding.Severity,
			}
		}
		schema.Status.Target = operatorv1alpha1.TargetStatus{
			Engine: engine, CoordinationDigest: result.CoordinationDigest,
			IdentityDigest: result.TargetIdentityDigest, DriftReportDigest: result.DriftReportDigest,
			LastObservedAt: &now, HighestDriftSeverity: result.HighestDriftSeverity,
			DriftFindingCount: result.DriftFindingCount, DriftFindings: findings,
		}
		if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.Plan = nil
		setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionTrue, operatorv1alpha1.ReasonObserved, "Database schema was observed")
		setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, operatorv1alpha1.ReasonScopedPlanPending, "Raw drift was recorded; the authoritative managed scope is being planned")
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonScopedPlanPending, "Convergence is unknown until scoped planning completes")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonScopedPlanPending, "A read-only scoped plan is required")
		schema.Status.NextReconciliationTime = &now
		if pending == nil {
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
		} else {
			pending.PlanRequired = true
			schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		}
	case operatorv1alpha1.OperationPlan:
		pending := schema.Status.PendingObservation
		if pending == nil {
			if policyErr := r.verifiedSourcePolicyError(ctx, schema); policyErr != nil {
				return r.verificationPolicyChanged(ctx, schema, policyErr)
			}
		} else {
			if policyErr := pendingProofPolicyBindingError(schema, pending); policyErr != nil {
				return r.retryOperation(ctx, schema, job, policyErr)
			}
			// A newer selector or a delete-and-recreate policy must not abandon
			// post-Apply proof or release its Lease. Record the current-policy
			// result now, finish proof against the immutable snapshot, then make
			// the verified source stale in the same status transition.
			completedProofPolicyErr = r.verifiedSourcePolicyError(ctx, schema)
			completedProofExecutionBindingErr = r.ensureCurrentStatusExecutionBinding(schema, &pending.Plan)
		}
		engine := schema.Spec.Target.Engine
		expectedCoordination := schema.Status.Target.CoordinationDigest
		expectedTarget := schema.Status.Target.IdentityDigest
		expectedExclude := schema.Spec.Policy.Exclude
		if pending != nil {
			engine = pending.Target.Engine
			expectedCoordination = pending.CoordinationDigest
			expectedTarget = pending.Plan.TargetIdentityDigest
			expectedExclude = pending.Exclude
		}
		if result.CoordinationDigest == "" || result.TargetIdentityDigest == "" {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("planning target does not match the persisted observation binding"))
		}
		if result.CoordinationDigest != expectedCoordination || result.TargetIdentityDigest != expectedTarget {
			return r.reobserveAfterStalePlan(ctx, schema, job, fmt.Errorf("planning target changed after observation"))
		}
		if result.PlanOutcome != runner.PlanOutcomeChanges && result.PlanOutcome != runner.PlanOutcomeNoChanges {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("plan result has no explicit managed-scope outcome"))
		}
		if result.PlanOutcome == runner.PlanOutcomeNoChanges {
			if result.Stdout != "" || result.PlanContentDigest != "" {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("no-change plan result contains executable content"))
			}
			observedDrift = ptr(false)
			schema.Status.Plan = nil
			setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, operatorv1alpha1.ReasonScopedConverged, "The authoritative managed scope has no changes")
			setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonNoChanges, "No executable plan is required")
			if pending != nil && pending.Outcome == operatorv1alpha1.PendingObservationApplySucceeded && pending.Plan.Fingerprint != "" {
				schema.Status.Applied = appliedStatusFor(pending.Plan, now)
				schema.Status.Applied.DispatchedBy = pending.DispatchedBy.DeepCopy()
			}
			if (pending == nil || pending.ApplyGeneration == schema.Generation) &&
				completedProofPolicyErr == nil && completedProofExecutionBindingErr == nil {
				schema.Status.Phase = operatorv1alpha1.PhaseInSync
				schema.Status.LastSuccessfulReconciliation = &now
				next := metav1.NewTime(r.now().Add(interval(schema)))
				schema.Status.NextReconciliationTime = &next
				convergenceReason := operatorv1alpha1.ReasonScopedConverged
				convergenceMessage := "A stable scoped plan proves convergence"
				if pending != nil && pending.Outcome == operatorv1alpha1.PendingObservationOutcomeUnknown {
					convergenceReason = operatorv1alpha1.ReasonConvergedAfterUnknownOutcome
					convergenceMessage = "The managed scope converged, but no Apply attribution is recorded because execution or lock continuity was uncertain"
				}
				setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionTrue, convergenceReason, convergenceMessage)
				setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionTrue, operatorv1alpha1.ReasonInSync, "Schema is in sync")
			} else {
				schema.Status.Phase = operatorv1alpha1.PhasePending
				schema.Status.NextReconciliationTime = nil
				setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged, "The applied generation converged, but newer desired inputs must be reconciled")
				setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged, "A newer resource generation is pending")
			}
		} else {
			var planDocument []byte
			var err error
			if durable {
				planDocument = []byte(result.Stdout)
			} else {
				// The claim recorded, at dispatch, the digest of the key this Job
				// was sealed to. A mismatch means this process cannot be the one
				// that dispatched it -- most likely a restart generated a new key
				// in between -- so the payload is unreadable by construction, not
				// merely rejected. Plan is read-only: retrying costs a fresh Job
				// sealed to the current key, which is exactly what harvesting the
				// old one would have cost anyway.
				if operation.PlanSealPublicKeyDigest == "" ||
					operation.PlanSealPublicKeyDigest != planSealPublicKeyDigest(r.SealKey.PublicKey()) {
					return r.retryOperation(ctx, schema, job,
						fmt.Errorf("plan was sealed to a manager key this process does not hold"))
				}
				// The envelope inside the sealed payload names the exact operation
				// and Job it was sealed for. A NaCl sealed box carries no
				// associated data, so opening successfully proves only that this
				// process's key sealed it, never that it was sealed for this
				// harvest -- a validly sealed plan from another operation, or
				// another attempt of this one, would otherwise open and validate
				// just as well.
				planDocument, err = r.SealKey.OpenPlan(result.Stdout, planseal.Envelope{
					OperationID: operation.ID, JobName: operation.JobName,
				})
				if err != nil {
					return r.retryOperation(ctx, schema, job, fmt.Errorf("open sealed plan payload: %w", err))
				}
			}

			if result.PlanContentDigest == "" || result.PlanContentDigest != fingerprint.DigestBytes(planDocument) {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("plan result content digest is missing or mismatched"))
			}
			decoded, err := dataplane.DecodePlan(planDocument, string(engine))
			if err != nil {
				return r.retryOperation(ctx, schema, job, err)
			}
			if !reflect.DeepEqual(fingerprint.NormalizeSet(decoded.Exclude), fingerprint.NormalizeSet(expectedExclude)) {
				return r.retryOperation(ctx, schema, job, fmt.Errorf("plan exclusion scope does not match the persisted policy"))
			}
			count, severity := planDriftSummary(decoded)
			observedDrift = ptr(true)
			setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, operatorv1alpha1.ReasonScopedChanges, fmt.Sprintf("Managed scope requires %d schema statements; highest severity %s", count, severity))
			setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonScopedChanges, "The authoritative managed scope differs from the verified artifact")
			if pending != nil && (!pendingMatchesCurrentSchema(schema, pending) ||
				completedProofPolicyErr != nil || completedProofExecutionBindingErr != nil) {
				schema.Status.Plan = nil
				schema.Status.Phase = operatorv1alpha1.PhasePending
				schema.Status.NextReconciliationTime = nil
				setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged, "The proof plan belongs to an older desired generation")
				setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged, "A newer resource generation is pending")
			} else {
				published, err := r.publishPlan(ctx, schema, decoded, planDocument)
				if err != nil {
					return ctrl.Result{}, err
				}
				schema.Status.Plan = currentPlanStatus(published)
				setPlanPolicyStatus(schema, published)
				next := metav1.NewTime(now.Add(interval(schema)))
				schema.Status.NextReconciliationTime = &next
				setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, operatorv1alpha1.ReasonPublished, "Exact plan bytes are stored in immutable chunks")
			}
		}
		if pending != nil {
			if completedProofExecutionBindingErr != nil {
				// Proof and historical Apply attribution are committed, but the
				// applied plan belongs to an epoch a rotation has since retired. An
				// approval CREATE that passed admission before the Apply claim can
				// commit after that rotation's own sweep, so the applied plan's
				// approvals are swept once more. The approval boundary closed at the
				// rotation, so the sweep needs no record of its own: a pass that
				// stops before the write below reads this Job again and sweeps again.
				if err := r.markPlanApprovalsStaleWithReason(
					ctx,
					schema,
					&pending.Plan,
					operatorv1alpha1.ReasonExecutionBindingChanged,
					"The approved plan uses an execution binding that is no longer configured",
				); err != nil {
					return ctrl.Result{}, err
				}
				schema.Status.Plan = nil
				schema.Status.Phase = operatorv1alpha1.PhasePending
				schema.Status.NextReconciliationTime = nil
				markExecutionBindingRefreshRequired(schema)
			} else if completedProofPolicyErr != nil {
				schema.Status.Source.Verified = false
				schema.Status.Source.VerifiedAt = nil
				schema.Status.Source.VerificationPolicyUID = ""
				schema.Status.Source.VerificationPolicyDigest = ""
				schema.Status.Plan = nil
				schema.Status.NextReconciliationTime = nil
				if pending.ApplyGeneration == schema.Generation {
					schema.Status.Phase = operatorv1alpha1.PhaseVerifying
				} else {
					schema.Status.Phase = operatorv1alpha1.PhasePending
				}
				setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, bounded(completedProofPolicyErr.Error(), 512))
				setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "Any plan for the previous verification policy is stale")
				setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "Post-Apply proof completed, but the artifact requires verification against the current policy")
				setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "Artifact verification against the current policy is pending")
			}
			schema.Status.PendingObservation = nil
		}
	case operatorv1alpha1.OperationApply:
		// A successful process is evidence only. Convergence is established by
		// a new read-only observation, never by the Job exit code.
		pending, err := pendingObservationFor(
			schema, operation, job, operatorv1alpha1.PendingObservationApplySucceeded, nil, podUIDs, podCount,
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingObservation = pending
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonJobCompleted, "Apply Job completed; convergence observation is pending")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonVerifyingConvergence, "Apply completion has not yet been independently observed")
	}

	// A completed Apply hands the realm to the pending observation it wrote,
	// which inherited its epoch, so it releases nothing. A Plan that settled
	// the proof it was carrying out releases the proof's epoch, and one that
	// had no proof to settle releases its own.
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionAccounted); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		if observedDrift != nil {
			outcome := telemetry.DriftInSync
			if *observedDrift {
				outcome = telemetry.DriftDetected
			}
			r.Telemetry.ObserveDrift(schema.Spec.Target.Engine, outcome)
		}
		if schemaOperation(operation).Mutating {
			r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyCompleted)
		}
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	if operation != nil {
		ctrl.LoggerFrom(ctx).Info(
			"operation completed",
			"operation", operation.Type,
			"attempt", operation.Attempt,
			"phase", schema.Status.Phase,
		)
	}
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.event(schema, corev1.EventTypeNormal, "OperationCompleted", "%s operation completed", result.Operation)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) expectedJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) (*batchv1.Job, error) {
	if r.Jobs == nil || operation == nil {
		return nil, fmt.Errorf("Job builder or active operation is missing")
	}
	var plan *operatorv1alpha1.PtahSchemaPlan
	var err error
	if schemaOperation(operation).Mutating {
		plan, err = r.currentPlan(ctx, schema)
		if err != nil {
			return nil, err
		}
	}
	return r.Jobs.Build(schema, *operation, plan)
}

// refreshAdmissionSnapshot drops the admission snapshot of a claim whose Job
// does not exist, so the next pass resolves it again from the Pod template
// this manager builds. It does so once per claim, and records that it did in
// the same write: the refresh ends only because the builder builds the same
// Job for the same claim every time, and a claim whose template moves again
// is retired by the caller rather than refreshed forever.
//
// Its only caller has already established that nothing ran: the claim is
// read-only with no committed Job UID, or an Apply that never crossed its
// dispatch boundary, and no Job stands under the claimed name. The claim's
// inputs are unchanged too, because a changed input retires the claim before
// the template is rebuilt. What is left to differ is the manager that built
// the template: its recorded identity -- the manager annotations and the
// runner image -- and anything else the new release changed in the Job it
// builds. Neither binds the claim, the plan or the approval, and nothing was
// dispatched from the old template, so the snapshot is resolved again rather
// than the claim retired; retiring a read-only claim here would also retire
// the plan it refreshes.
func (r *SchemaReconciler) refreshAdmissionSnapshot(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	before := schema.DeepCopy()
	schema.Status.ActiveOperation.AdmissionSnapshot = nil
	schema.Status.ActiveOperation.AdmissionSnapshotRefreshed = true
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeNormal, "AdmissionSnapshotRefreshed",
		"%s Job Pod template changed before dispatch; resolving its admission snapshot again",
		schema.Status.ActiveOperation.Type)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// validateAdoptedJobIntent checks a live Job against the Job its claim
// authorizes, where an earlier manager of the same execution binding may have
// built it.
//
// The manager's recorded identity -- the controller image and revision
// annotations and the runner image -- binds nothing, so the rebuild takes it
// from the live Pod template and compares everything else exactly. The live
// Pod template must still be the one the claim's admission snapshot recorded
// before dispatch, as every Job a claim accepts must, which pins what was
// taken to the claim rather than to the object being checked.
//
// A Plan Job's seal key is taken from the live Job the same way, and pinned
// by the digest the claim recorded at dispatch rather than by the snapshot.
// Every process generates its own key, so the rebuild carries this process's
// key while the live Job carries the dispatching process's: after a
// leadership change or a restart mid-Plan the two differ on every pass, and
// a byte-for-byte comparison would refuse a Job the claim authorized.
//
// Adoption holds only when the two releases build the same Job apart from
// that identity. A release that also changed the Job or its Pod template fails
// here, and the caller treats the Job as one it cannot confirm: a dispatched
// Apply is settled as outcome unknown and the database is observed before
// anything else runs, and a read-only Job is run again under a new attempt.
func validateAdoptedJobIntent(
	actual, expected *batchv1.Job,
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) error {
	if operation == nil {
		return fmt.Errorf("no active operation claims the Job")
	}
	workload.CarryManagerIdentity(expected, actual)
	if err := workload.CarrySealedPlanKey(expected, actual, *operation); err != nil {
		return fmt.Errorf("adopted Job seal key is invalid: %w", err)
	}
	if schema == nil {
		return fmt.Errorf("no schema claims the Job")
	}
	return jobclaim.Match(actual, builtSchemaJobClaim(schema, operation, expected))
}

// validateJobEnvelope holds a live Job to its claim where the claim can no
// longer rebuild it, because the inputs it was made from have moved since
// dispatch. The Job is held to what the claim fixes without a rebuild: its
// name, owner and recorded UID, the epoch the claim was made under, the labels
// and annotations the claim fixes, and the Pod template its admission snapshot
// recorded. It is the match the controller-write webhook applies when it
// admits the Job's cleanup TTL. The binding in force is named, as the webhook
// names it for a claim of the current epoch; reconcileExecutionBinding has
// already retired a claim of any other.
func validateJobEnvelope(
	actual *batchv1.Job,
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) error {
	if schema == nil || operation == nil {
		return fmt.Errorf("no active operation claims the Job")
	}
	claim := jobclaim.SchemaOperation(schema, operation)
	claim.Binding = schema.Status.ExecutionBinding
	return jobclaim.Match(actual, claim)
}

func (r *SchemaReconciler) reconcileApproval(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	if schema.Status.Plan == nil {
		return r.claim(ctx, schema, operatorv1alpha1.OperationObserve)
	}
	plan, err := r.currentPlan(ctx, schema)
	if err != nil {
		return r.applyBecameStale(ctx, schema, err)
	}
	if err := r.ensureCurrentExecutionBinding(schema, plan); err != nil {
		return r.executionBindingChanged(ctx, schema, err)
	}
	policyBinding, err := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil || policyBinding.UID != plan.Spec.VerificationPolicyUID || policyBinding.Digest != plan.Spec.VerificationPolicyDigest {
		if err == nil {
			err = fmt.Errorf("verification policy object changed")
		}
		return r.verificationPolicyChanged(ctx, schema, err)
	}
	if plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive {
		return r.waitBlocked(ctx, schema, operatorv1alpha1.ReasonDestructiveChangesDisabled, "Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, "+
			"then either narrow the desired schema or set spec.policy.allowDestructive")
	}
	if schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever {
		return r.waitBlocked(ctx, schema, operatorv1alpha1.ReasonApplyDisabled, "Policy records plans but does not apply them")
	}

	requiresApproval := planRequiresApproval(schema, plan)
	if requiresApproval && schema.Status.Plan.Approval == nil {
		approval, err := r.findApproval(ctx, schema, plan)
		if err != nil {
			return ctrl.Result{}, err
		}
		now := r.now()
		if schema.Status.Phase == operatorv1alpha1.PhaseAwaitingApproval &&
			due(schema.Status.NextReconciliationTime, now) {
			return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
		}
		if approval == nil {
			before := schema.DeepCopy()
			if schema.Status.NextReconciliationTime == nil {
				next := metav1.NewTime(now.Add(interval(schema)))
				schema.Status.NextReconciliationTime = &next
			}
			schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
			if privilegesRequireApproval(schema, plan) {
				// The reason stays the one that says why an Always resource is
				// waiting at all, rather than giving way to the generic one.
				setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonPrivilegeChanges, privilegeApprovalMessage(plan))
			} else {
				setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonWaiting, "Create an approval bound to the current plan fingerprint")
			}
			if err := r.patchStatus(ctx, before, schema); err != nil {
				return ctrl.Result{}, err
			}
			return requeueAtDeadline(schema.Status.NextReconciliationTime, r.now()), nil
		}
		before := schema.DeepCopy()
		schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
			Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
		}
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		// Approval reservation and Apply claim are separate reconciliation
		// boundaries. A fresh pass must re-enter the generation gate and validate
		// the plan against one current resource snapshot before any mutation.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	if requiresApproval {
		valid, err := r.ensureCurrentApproval(ctx, schema, plan, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !valid {
			return r.approvalBecameInvalid(ctx, schema)
		}
	}
	claimedAt := r.now()
	if (schema.Status.Phase == operatorv1alpha1.PhaseReadyToApply ||
		schema.Status.Phase == operatorv1alpha1.PhaseAwaitingApproval) &&
		due(schema.Status.NextReconciliationTime, claimedAt) {
		return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
	}
	return r.claimAt(ctx, schema, operatorv1alpha1.OperationApply, claimedAt)
}

func (r *SchemaReconciler) claim(ctx context.Context, schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.OperationType) (ctrl.Result, error) {
	return r.claimAt(ctx, schema, operation, time.Time{})
}

func (r *SchemaReconciler) claimAt(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.OperationType,
	claimedAt time.Time,
) (ctrl.Result, error) {
	if schema.Status.ActiveOperation != nil {
		// Unreachable in the normal dispatch: reconcile's own ActiveOperation
		// check sends a resource carrying one to reconcileActive before this
		// function is ever called. Requeuing rather than erroring keeps that
		// true if it ever stops being true.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	kind := mutationlifecycle.SchemaOperation(operation)
	inputs, err := operationInputs(schema, operation)
	if err != nil {
		return r.operationFailure(ctx, schema, err)
	}
	var verificationPolicy policy.Binding
	if operation == operatorv1alpha1.OperationVerify {
		verificationPolicy, err = policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
		if err != nil {
			return r.operationFailure(ctx, schema, err)
		}
		inputs["verification_policy_uid"] = string(verificationPolicy.UID)
		inputs["verification_policy_digest"] = verificationPolicy.Digest
	}
	inputFingerprint, err := fingerprint.DigestCanonicalJSON(inputs)
	if err != nil {
		return ctrl.Result{}, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ctrl.Result{}, fmt.Errorf("create operation nonce: %w", err)
	}
	id, err := fingerprint.DigestCanonicalJSON(map[string]string{"input": inputFingerprint, "nonce": hex.EncodeToString(nonce)})
	if err != nil {
		return ctrl.Result{}, err
	}
	if claimedAt.IsZero() {
		claimedAt = r.now()
	}
	active := &operatorv1alpha1.ActiveOperationStatus{
		Type: operation, ID: id, InputFingerprint: inputFingerprint,
		StartedAt: metav1.NewTime(claimedAt), Attempt: 1,
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	if operation == operatorv1alpha1.OperationVerify {
		active.VerificationPolicyUID = verificationPolicy.UID
		active.VerificationPolicyDigest = verificationPolicy.Digest
	}
	if kind.HoldsLock(schema.Status.PendingObservation != nil) {
		active.LeaseEpoch = "v1-" + strings.TrimPrefix(id, "sha256:")[:32]
	}
	if operation == operatorv1alpha1.OperationObserve {
		if pending := schema.Status.PendingObservation; pending != nil {
			active.CoordinationDigest = pending.CoordinationDigest
			active.TargetIdentityDigest = pending.Plan.TargetIdentityDigest
			active.LeaseDurationSeconds = pending.LeaseDurationSeconds
			active.LeaseEpoch = pending.LeaseEpoch
			target := pending.Target
			active.Target = &target
			active.Source = pending.Source.DeepCopy()
			active.ObservationExclude = append([]string(nil), pending.Exclude...)
			active.ObservationProtectedTables = append([]string(nil), pending.ProtectedTables...)
			active.ObservationSeverity = pending.DriftSeverity
			active.ObservationDev = pending.Dev.DeepCopy()
			active.ObservationConnectTimeout = pending.ConnectTimeout
			active.ObservationLockTimeout = pending.LockTimeout
		} else {
			coordinationDigest, digestErr := coordination.Digest(schema.Namespace, schema.Spec.Target)
			if digestErr != nil {
				return r.operationFailure(ctx, schema, fmt.Errorf("derive observation coordination digest: %w", digestErr))
			}
			active.CoordinationDigest = coordinationDigest
			target := databaseTargetBinding(schema.Spec.Target)
			active.Target = &target
			active.Source = artifactAccessBinding(schema)
			active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
			active.ObservationProtectedTables = append([]string(nil), schema.Spec.Policy.ProtectedTables...)
			active.ObservationSeverity = schema.Spec.Policy.DriftSeverity
			active.ObservationDev = schema.Spec.Dev.DeepCopy()
			active.ObservationConnectTimeout = schema.Spec.Execution.ConnectTimeout
			active.ObservationLockTimeout = schema.Spec.Policy.LockTimeout
		}
	}
	if operation == operatorv1alpha1.OperationApply {
		if schema.Status.Plan == nil || schema.Status.Plan.CoordinationDigest == "" || schema.Status.Plan.TargetIdentityDigest == "" {
			return r.operationFailure(ctx, schema, fmt.Errorf("apply plan target identity is missing"))
		}
		active.CoordinationDigest = schema.Status.Plan.CoordinationDigest
		active.TargetIdentityDigest = schema.Status.Plan.TargetIdentityDigest
		active.LeaseDurationSeconds = int32(leaseDuration(schema) / time.Second)
		dispatchNotAfter := metav1.NewTime(active.StartedAt.Add(applyWindow(schema)))
		active.DispatchNotAfter = &dispatchNotAfter
		executionNotAfter := dispatchNotAfter.DeepCopy()
		active.ExecutionNotAfter = executionNotAfter
		active.TerminationGracePeriodSeconds = int64(applyTerminationGrace / time.Second)
		target := databaseTargetBinding(schema.Spec.Target)
		active.Target = &target
		active.Source = artifactAccessBinding(schema)
		active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
		active.ObservationProtectedTables = append([]string(nil), schema.Spec.Policy.ProtectedTables...)
		active.ObservationSeverity = schema.Spec.Policy.DriftSeverity
		active.ObservationDev = schema.Spec.Dev.DeepCopy()
		active.ObservationConnectTimeout = schema.Spec.Execution.ConnectTimeout
		active.ObservationLockTimeout = schema.Spec.Policy.LockTimeout
	}
	if operation == operatorv1alpha1.OperationPlan {
		pending := schema.Status.PendingObservation
		if pending != nil {
			active.CoordinationDigest = pending.CoordinationDigest
			active.TargetIdentityDigest = pending.Plan.TargetIdentityDigest
			active.LeaseDurationSeconds = pending.LeaseDurationSeconds
			active.LeaseEpoch = pending.LeaseEpoch
			target := pending.Target
			active.Target = &target
			active.Source = pending.Source.DeepCopy()
			active.ObservationExclude = append([]string(nil), pending.Exclude...)
			active.ObservationProtectedTables = append([]string(nil), pending.ProtectedTables...)
			active.ObservationSeverity = pending.DriftSeverity
			active.ObservationDev = pending.Dev.DeepCopy()
			active.ObservationConnectTimeout = pending.ConnectTimeout
			active.ObservationLockTimeout = pending.LockTimeout
		} else {
			if schema.Status.Target.CoordinationDigest == "" || schema.Status.Target.IdentityDigest == "" {
				return r.operationFailure(ctx, schema, fmt.Errorf("planning target identity is missing"))
			}
			active.CoordinationDigest = schema.Status.Target.CoordinationDigest
			active.TargetIdentityDigest = schema.Status.Target.IdentityDigest
			active.LeaseDurationSeconds = int32(leaseDuration(schema) / time.Second)
			target := databaseTargetBinding(schema.Spec.Target)
			active.Target = &target
			active.Source = artifactAccessBinding(schema)
			active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
			active.ObservationProtectedTables = append([]string(nil), schema.Spec.Policy.ProtectedTables...)
			active.ObservationSeverity = schema.Spec.Policy.DriftSeverity
			active.ObservationDev = schema.Spec.Dev.DeepCopy()
			active.ObservationConnectTimeout = schema.Spec.Execution.ConnectTimeout
			active.ObservationLockTimeout = schema.Spec.Policy.LockTimeout
		}
	}
	if r.Jobs == nil {
		return r.operationFailure(ctx, schema, fmt.Errorf("Job builder is not configured"))
	}
	active.JobName, err = r.Jobs.NameFor(schema, *active)
	if err != nil {
		return r.operationFailure(ctx, schema, fmt.Errorf("name %s Job: %w", operation, err))
	}
	if !controllerutil.ContainsFinalizer(schema, activeOperationFinalizer) {
		beforeMeta := schema.DeepCopy()
		controllerutil.AddFinalizer(schema, activeOperationFinalizer)
		if err := r.Client.Patch(ctx, schema, client.MergeFromWithOptions(beforeMeta, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("add active-operation finalizer: %w", err)
		}
	}
	before := schema.DeepCopy()
	schema.Status.ActiveOperation = active
	schema.Status.LastAttemptTime = ptrTime(active.StartedAt)
	schema.Status.ObservedGeneration = schema.Generation
	if pending := schema.Status.PendingObservation; pending != nil && kind.ServesProof {
		schema.Status.ObservedGeneration = pending.ApplyGeneration
	}
	schema.Status.NextReconciliationTime = nil
	if schema.Status.PendingObservation != nil && kind.ServesProof {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = kind.Phase
	}
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation))
	if operation == operatorv1alpha1.OperationResolve {
		markSourceRefreshPending(schema)
	}
	if operation == operatorv1alpha1.OperationApply {
		setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionTrue, operatorv1alpha1.ReasonApprovedPlan, "Applying the exact current plan")
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonSatisfied, "Current plan approval requirements are satisfied")
	}
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	ctrl.LoggerFrom(ctx).Info(
		"operation claimed",
		"operation", operation,
		"attempt", active.Attempt,
		"phase", schema.Status.Phase,
	)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) retryOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, failure error) (ctrl.Result, error) {
	return r.retryOperationAs(ctx, schema, job, operatorv1alpha1.ReasonOperationFailed, failure, nil)
}

// retryOperationAs is retryOperation with the failure named, and with a chance
// to record what else the failure means before the status is patched. A caller
// that knows why an operation failed can say so on the conditions a reader
// looks at, without a second path for releasing the lock and renaming the Job.
func (r *SchemaReconciler) retryOperationAs(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	reason operatorv1alpha1.ConditionReason,
	failure error,
	record func(*operatorv1alpha1.PtahSchema),
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if !isReadOnlyOperation(operation) {
		operationType := operatorv1alpha1.OperationType("")
		if operation != nil {
			operationType = operation.Type
		}
		return ctrl.Result{}, fmt.Errorf("retry requires an active read-only operation, got %q", operationType)
	}
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation.Attempt++
	operation.JobUID = ""
	operation.DispatchStarted = false
	// A new read-only attempt is a fresh admission boundary. Re-resolve the
	// cluster objects and admission configuration that shape its Pod instead of
	// pinning the new dispatch to the retired attempt's snapshot. Apply never
	// enters this retry path because its dispatch outcome may be ambiguous.
	operation.AdmissionSnapshot = nil
	// The failed attempt is retired and the claim goes on: what the attempt
	// held goes back in this write, and the next attempt takes the realm
	// again under an epoch of its own.
	released, err := stageRetirementRelease(before, schema, mutationlifecycle.DispositionDiscard)
	if err != nil {
		return ctrl.Result{}, err
	}
	if released == mutationlifecycle.OwnerClaim {
		operation.LeaseEpoch = ""
		operation.LeaseContinuityLost = false
	}
	name, err := r.Jobs.NameFor(schema, *operation)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("name retry Job after %v: %w", failure, err)
	}
	operation.JobName = name
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	next := metav1.NewTime(r.now().Add(failureRetry(schema)))
	schema.Status.NextReconciliationTime = &next
	setFailure(schema, reason, failure)
	// After the failure is recorded, so a caller reporting a refusal rather
	// than a fault can replace what it says.
	if record != nil {
		record(schema)
	}
	if operation.Type == operatorv1alpha1.OperationResolve {
		markSourceRefreshFailed(schema)
	}
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.StageForOperation(operation.Type), telemetry.FailureOperation)
	}
	r.event(schema, corev1.EventTypeWarning, "OperationFailed", "%s", bounded(failure.Error(), 512))
	return ctrl.Result{RequeueAfter: failureRetry(schema)}, nil
}

func (r *SchemaReconciler) finishUncertainApply(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	jobName := ""
	var jobUID types.UID
	if operation != nil {
		jobName = operation.JobName
		jobUID = operation.JobUID
	}
	if jobUID == "" && job != nil {
		jobName = job.Name
		jobUID = job.UID
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, jobName, jobUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	evidence := podIdentityEvidence(pods)
	return r.finishUncertainApplyWithEvidence(ctx, schema, job, failure, evidence.PodUIDs, evidence.PodCount, true)
}

func (r *SchemaReconciler) finishUncertainApplyForExecutionBindingChange(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	configured *operatorv1alpha1.ExecutionBindingStatus,
	failure error,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return ctrl.Result{}, fmt.Errorf("cannot retire a dispatched Apply without its active operation")
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, operation.JobName, operation.JobUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	binding, err := newExecutionBinding(configured)
	if err != nil {
		return ctrl.Result{}, err
	}
	evidence := podIdentityEvidence(pods)
	return r.finishUncertainApplyWithEvidenceAndBinding(
		ctx,
		schema,
		nil,
		failure,
		evidence.PodUIDs,
		evidence.PodCount,
		true,
		binding,
	)
}

func (r *SchemaReconciler) finishUncertainApplyWithEvidence(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	failure error,
	podUIDs []types.UID,
	podCount int32,
	waitForDispatchDeadline bool,
) (ctrl.Result, error) {
	return r.finishUncertainApplyWithEvidenceAndBinding(
		ctx,
		schema,
		job,
		failure,
		podUIDs,
		podCount,
		waitForDispatchDeadline,
		nil,
	)
}

func (r *SchemaReconciler) finishUncertainApplyWithEvidenceAndBinding(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	failure error,
	podUIDs []types.UID,
	podCount int32,
	waitForDispatchDeadline bool,
	replacementBinding *operatorv1alpha1.ExecutionBindingStatus,
) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	var observeAfter *metav1.Time
	if waitForDispatchDeadline {
		if operation == nil || operation.ExecutionNotAfter == nil || operation.ExecutionNotAfter.IsZero() ||
			operation.TerminationGracePeriodSeconds < 1 ||
			!operation.ExecutionNotAfter.After(operation.StartedAt.Time) {
			return ctrl.Result{}, fmt.Errorf("cannot persist outcome-unknown proof without the immutable Apply execution horizon")
		}
		proofTime := metav1.NewTime(operation.ExecutionNotAfter.Add(
			time.Duration(operation.TerminationGracePeriodSeconds) * time.Second,
		))
		observeAfter = &proofTime
	}
	pending, err := pendingObservationFor(
		schema, operation, job, operatorv1alpha1.PendingObservationOutcomeUnknown, observeAfter, podUIDs, podCount,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	schema.Status.PendingObservation = pending
	if replacementBinding != nil {
		schema.Status.PendingObservation.PlanRequired = false
		// The Apply's Job may still be running. The record names it, so its
		// cleanup is scheduled once it stops and before fresh proof can clear
		// the pending observation that authorizes that write.
		if err := rotateExecutionBinding(schema, replacementBinding, &operatorv1alpha1.RetiredJobStatus{
			Operation: operatorv1alpha1.OperationApply, Name: pending.ApplyJobName, UID: pending.ApplyJobUID,
		}); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	}
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonOutcomeUnknown, "Apply outcome is uncertain; only read-only observation is permitted")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonOutcomeUnknown, "Database state must be observed before any next action")
	setFailure(schema, operatorv1alpha1.ReasonApplyOutcomeUnknown, failure)
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionUnaccounted); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyUncertain)
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.FailureStageApply, telemetry.FailureUncertain)
	}
	r.observeOperation(operation, telemetry.OperationUncertain)
	r.event(schema, corev1.EventTypeWarning, "ApplyOutcomeUnknown", "Apply outcome is uncertain; observing database state")
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) finishUnknownRunningApply(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	failure error,
) (ctrl.Result, error) {
	acquired, requeue, err := r.acquireApplyLock(ctx, schema)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return r.finishUncertainApply(ctx, schema, nil, failure)
}

func pendingObservationFor(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
	outcome operatorv1alpha1.PendingObservationOutcome,
	observeAfter *metav1.Time,
	podUIDs []types.UID,
	podCount int32,
) (*operatorv1alpha1.PendingObservationStatus, error) {
	if operation == nil || operation.Type != operatorv1alpha1.OperationApply || operation.JobName == "" || operation.Target == nil || operation.Source == nil ||
		operation.CoordinationDigest == "" || operation.TargetIdentityDigest == "" || operation.LeaseDurationSeconds == 0 || operation.LeaseEpoch == "" {
		return nil, fmt.Errorf("cannot persist post-apply proof without the immutable Apply binding")
	}
	if schema.Status.Plan == nil || schema.Status.Plan.CoordinationDigest != operation.CoordinationDigest ||
		schema.Status.Plan.TargetIdentityDigest != operation.TargetIdentityDigest {
		return nil, fmt.Errorf("cannot persist post-apply proof without the immutable current plan")
	}
	if operation.Source.Digest != schema.Status.Plan.ArtifactDigest ||
		!strings.Contains(operation.Source.ResolvedReference, "@"+operation.Source.Digest) {
		return nil, fmt.Errorf("cannot persist post-apply proof without immutable artifact access")
	}
	plan := *schema.Status.Plan
	if schema.Status.Plan.Approval != nil {
		approval := *schema.Status.Plan.Approval
		plan.Approval = &approval
	}
	jobUID := operation.JobUID
	if job != nil && job.UID != "" {
		jobUID = job.UID
	}
	if podCount < int32(len(podUIDs)) || len(podUIDs) > 8 {
		return nil, fmt.Errorf("cannot persist post-apply proof with invalid Pod evidence")
	}
	target := *operation.Target
	return &operatorv1alpha1.PendingObservationStatus{
		Outcome: outcome, ApplyOperationID: operation.ID, ApplyJobName: operation.JobName, ApplyJobUID: jobUID,
		DispatchedBy:      dispatcherRecord(job),
		AdmissionSnapshot: operation.AdmissionSnapshot.DeepCopy(),
		ApplyPodUIDs:      append([]types.UID(nil), podUIDs...), ApplyPodCount: podCount,
		ApplyGeneration: schema.Status.ObservedGeneration, ObserveAfter: observeAfter, Plan: plan, Target: target,
		CoordinationDigest:   operation.CoordinationDigest,
		Source:               *operation.Source.DeepCopy(),
		Dev:                  operation.ObservationDev.DeepCopy(),
		Exclude:              append([]string(nil), operation.ObservationExclude...),
		ProtectedTables:      append([]string(nil), operation.ObservationProtectedTables...),
		DriftSeverity:        operation.ObservationSeverity,
		ConnectTimeout:       operation.ObservationConnectTimeout,
		LockTimeout:          operation.ObservationLockTimeout,
		LeaseDurationSeconds: operation.LeaseDurationSeconds,
		LeaseEpoch:           operation.LeaseEpoch,
	}, nil
}

func (r *SchemaReconciler) applyBecameStale(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	schema.Status.Plan = nil
	if schema.Status.PendingObservation != nil {
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStale, bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStalePlan, "The plan became stale before apply")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		stage := telemetry.FailureStagePlan
		if schemaOperation(operation).Mutating {
			stage = telemetry.FailureStageApply
			r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStale)
		}
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, stage, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// executionBindingChanged retires every claim and plan of the stored epoch and
// installs a fresh one. A dispatched Apply becomes outcome-unknown proof in
// the same write; anything else is rotated here, and what the retired epoch
// still owes goes into status.pendingBindingRetirement.
func (r *SchemaReconciler) executionBindingChanged(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	failure error,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	failureStage := telemetry.FailureStageController
	if operation != nil {
		failureStage = telemetry.StageForOperation(operation.Type)
	} else if schema.Status.Plan != nil {
		failureStage = telemetry.FailureStagePlan
	} else {
		switch schema.Status.Phase {
		case operatorv1alpha1.PhaseVerifying:
			failureStage = telemetry.FailureStageVerify
		case operatorv1alpha1.PhaseObserving, operatorv1alpha1.PhaseVerifyingConvergence:
			failureStage = telemetry.FailureStageObserve
		case operatorv1alpha1.PhasePlanning:
			failureStage = telemetry.FailureStagePlan
		}
	}
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return ctrl.Result{}, err
	}
	if schemaOperation(operation).Mutating && schemaMayHaveDispatched(operation) {
		return r.finishUncertainApplyForExecutionBindingChange(
			ctx,
			schema,
			configured,
			fmt.Errorf("execution binding changed after Apply dispatch: %w", failure),
		)
	}
	binding, err := newExecutionBinding(configured)
	if err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	// A read-only claim is kept, and goes on holding whatever it took, until
	// the Job it dispatched has stopped; the record names that Job, and
	// cleanupRetiredReadOnlyJob retires the claim. Anything else is retired
	// in this write: an Apply that never dispatched, since one that may have
	// became an outcome nobody established above, or a claim of a type this
	// binary has never heard of, which a stored object written by a newer
	// operator supplies. The first hands its Lease back in the write; the
	// second holds nothing this binary can hand back, because a type that is
	// neither mutating nor serving a proof owns no realm in its own right, and
	// releasing its epoch would hand back a database under work this binary
	// cannot reason about.
	var retainedJob *operatorv1alpha1.RetiredJobStatus
	retiring := false
	if isReadOnlyOperation(operation) {
		retainedJob = &operatorv1alpha1.RetiredJobStatus{
			Operation: operation.Type, Name: operation.JobName, UID: operation.JobUID,
		}
	} else if operation != nil {
		retiring = true
	} else if pending := schema.Status.PendingObservation; pending != nil &&
		pending.Outcome == operatorv1alpha1.PendingObservationOutcomeUnknown && pending.ApplyJobName != "" &&
		schema.Status.ExecutionBinding != nil && pending.Plan.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		// An Apply of the epoch being retired, settled as outcome-unknown
		// without its Job in hand -- a create whose result was uncertain, a Job
		// that could not be read -- can leave a Job no pass has harvested. The
		// record names it, so it is adopted and cleaned up before the proof. One
		// that was harvested already settles at once.
		retainedJob = &operatorv1alpha1.RetiredJobStatus{
			Operation: operatorv1alpha1.OperationApply, Name: pending.ApplyJobName, UID: pending.ApplyJobUID,
		}
	}
	if schema.Status.PendingObservation != nil {
		// Any Observe/Plan evidence produced by the retired components is
		// unprovable. Keep the immutable Apply evidence and Lease, but restart
		// its read-only proof from Observe under the new epoch.
		schema.Status.PendingObservation.PlanRequired = false
	}
	// This write closes the approval boundary: approval admission reads
	// PtahSchema directly from the API server, so a request that starts after
	// it cannot authorize the retired plan. Marking that plan's approvals stale
	// waits for the next pass, which reads the plan from the record.
	if err := rotateExecutionBinding(schema, binding, retainedJob); err != nil {
		return ctrl.Result{}, err
	}
	if retiring {
		err = r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard)
	} else {
		err = r.patchStatus(ctx, before, schema)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, failureStage, telemetry.FailureStaleInput)
		if schemaOperation(operation).Mutating {
			r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStale)
		}
	}
	r.observeOperation(operation, telemetry.OperationStale)
	r.event(schema, corev1.EventTypeWarning, "ExecutionBindingChanged", "The previous execution binding was retired; closing its approval boundary before starting a complete read-only refresh")
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// isReadOnlyOperation reports whether the claim is known not to change the
// database. It is not the negation of Mutating: a claim of a type this binary
// does not define is neither.
func isReadOnlyOperation(operation *operatorv1alpha1.ActiveOperationStatus) bool {
	return schemaOperation(operation).ReadOnly()
}

func (r *SchemaReconciler) markRecordedApprovalStale(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
	return r.markRecordedApprovalStaleWithReason(
		ctx,
		schema,
		operatorv1alpha1.ReasonPlanNoLongerCurrent,
		"The approved plan no longer matches the current database state",
	)
}

func (r *SchemaReconciler) markRecordedApprovalStaleWithReason(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	reason operatorv1alpha1.ConditionReason,
	message string,
) error {
	if schema.Status.Plan == nil || schema.Status.Plan.Approval == nil {
		return nil
	}
	recorded := schema.Status.Plan.Approval
	approval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: recorded.Name}, approval); err != nil {
		return client.IgnoreNotFound(err)
	}
	if approval.UID != recorded.UID {
		return nil
	}
	return r.markApprovalStaleWithReason(
		ctx,
		approval,
		reason,
		message,
	)
}

func (r *SchemaReconciler) markPlanApprovalsStaleWithReason(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.CurrentPlanStatus,
	reason operatorv1alpha1.ConditionReason,
	message string,
) error {
	if schema == nil || plan == nil {
		return nil
	}
	list := &operatorv1alpha1.PtahSchemaApprovalList{}
	// The durable schema epoch already closes the authorization boundary. Use a
	// namespace-wide direct read here only for best-effort audit cleanup of
	// approvals visible after that fence; correctness does not depend on LIST
	// quiescence.
	if err := r.directReader().List(ctx, list, client.InNamespace(schema.Namespace)); err != nil {
		return fmt.Errorf("list plan approvals for invalidation: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.SchemaRef.Name != schema.Name {
			continue
		}
		approval := &operatorv1alpha1.PtahSchemaApproval{}
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(&list.Items[i]), approval); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read plan approval for invalidation: %w", err)
		}
		if approval.DeletionTimestamp != nil ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) ||
			!approvalMatchesPlanStatus(approval, schema, plan) {
			continue
		}
		if err := r.markApprovalStaleWithReason(ctx, approval, reason, message); err != nil {
			return err
		}
	}
	return nil
}

// markOneSchemaApprovalStaleWithReason bounds status writes to one approval per
// reconciliation. Unlike plan-scoped cleanup, it deliberately needs no current
// plan pointer: an approval CREATE that crossed the durable unsupported-engine
// fence will enqueue another pass and is still retired as historical evidence.
func (r *SchemaReconciler) markOneSchemaApprovalStaleWithReason(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	reason operatorv1alpha1.ConditionReason,
	message string,
) (bool, error) {
	if schema == nil {
		return false, nil
	}
	list := &operatorv1alpha1.PtahSchemaApprovalList{}
	if err := r.directReader().List(ctx, list, client.InNamespace(schema.Namespace)); err != nil {
		return false, fmt.Errorf("list schema approvals for invalidation: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Spec.SchemaRef.Name != schema.Name || item.Spec.SchemaRef.UID != schema.UID ||
			item.DeletionTimestamp != nil ||
			meta.IsStatusConditionTrue(item.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			continue
		}
		approval := &operatorv1alpha1.PtahSchemaApproval{}
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(item), approval); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("read schema approval for invalidation: %w", err)
		}
		if approval.Spec.SchemaRef.Name != schema.Name || approval.Spec.SchemaRef.UID != schema.UID ||
			approval.DeletionTimestamp != nil ||
			meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			continue
		}
		if err := r.markApprovalStaleWithReason(ctx, approval, reason, message); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *SchemaReconciler) reobserveAfterStalePlan(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	failure error,
) (ctrl.Result, error) {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	operation := schema.Status.ActiveOperation
	before := schema.DeepCopy()
	schema.Status.Plan = nil
	if schema.Status.PendingObservation != nil {
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStaleObservation, bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStaleObservation, "Database state must be observed again before planning")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.FailureStagePlan, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.event(schema, corev1.EventTypeWarning, "PlanStale", "Database state changed while the plan was generated; observing again")
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) verificationPolicyChanged(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	if operation != nil && operation.Type == operatorv1alpha1.OperationPlan {
		schema.Status.PendingObservation = nil
	}
	schema.Status.Plan = nil
	schema.Status.Source.Verified = false
	schema.Status.Source.VerificationPolicyUID = ""
	schema.Status.Source.VerificationPolicyDigest = ""
	schema.Status.Phase = operatorv1alpha1.PhaseVerifying
	setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "The plan is stale because verification policy bytes changed")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "In-sync status requires verification against the current policy bytes")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyChanged, "Artifact verification must run again")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.FailureStageVerify, telemetry.FailurePolicyChanged)
		if schemaOperation(operation).Mutating {
			r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStale)
		}
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) operationFailure(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	next := metav1.NewTime(r.now().Add(failureRetry(schema)))
	schema.Status.NextReconciliationTime = &next
	if schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationResolve {
		markSourceRefreshFailed(schema)
	}
	setFailure(schema, operatorv1alpha1.ReasonConfigurationError, failure)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		stage := telemetry.FailureStageController
		if schema.Status.ActiveOperation != nil {
			stage = telemetry.StageForOperation(schema.Status.ActiveOperation.Type)
		}
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, stage, telemetry.FailureConfiguration)
	}
	return ctrl.Result{RequeueAfter: failureRetry(schema)}, nil
}

func (r *SchemaReconciler) discardStaleOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	if schemaClaimServesProof(schema) {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		schema.Status.NextReconciliationTime = nil
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofInputsChanged, "Post-apply observation will restart from its durable binding")
		// The proof keeps the realm it holds.
		if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
			return ctrl.Result{}, err
		}
		if r.Telemetry != nil {
			r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.StageForOperation(operation.Type), telemetry.FailureStaleInput)
		}
		r.observeOperation(operation, telemetry.OperationStale)
		// PendingObservation still owns the database-realm Lease and deletion
		// safety boundary. Only its terminal proof transition may remove the
		// finalizer.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	schema.Status.Plan = nil
	schema.Status.Source.Verified = false
	schema.Status.Phase = operatorv1alpha1.PhasePending
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonInputsChanged, "Operation result was discarded because desired inputs changed")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonInputsChanged, bounded(failure.Error(), 512))
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		stage := telemetry.FailureStageController
		if operation != nil {
			stage = telemetry.StageForOperation(operation.Type)
			if schemaOperation(operation).Mutating {
				r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStale)
			}
		}
		r.Telemetry.ObserveFailure(telemetry.FamilySchema, stage, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) publishPlan(ctx context.Context, schema *operatorv1alpha1.PtahSchema, decoded dataplane.PlanFile, content []byte) (*operatorv1alpha1.PtahSchemaPlan, error) {
	policyFingerprint, err := policyFingerprint(schema)
	if err != nil {
		return nil, err
	}
	contentDigest := fingerprint.DigestBytes(content)
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return nil, err
	}
	executionBinding := schema.Status.ExecutionBinding
	if executionBinding == nil || !validExecutionBindingID(executionBinding.Epoch) ||
		!executionBindingComponentsEqual(executionBinding, configured) {
		return nil, fmt.Errorf("durable execution binding is not current")
	}
	controllerImage, controllerRevision, runnerImage, err := r.managerIdentity()
	if err != nil {
		return nil, err
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: fingerprint.CurrentPlanContractVersion, SchemaRef: operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		ContentDigest:  contentDigest,
		ArtifactDigest: schema.Status.Source.Digest, CoordinationDigest: schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest:   schema.Status.Target.IdentityDigest,
		ActualStateFingerprint: decoded.FromFingerprint, DesiredStateFingerprint: decoded.ToFingerprint,
		PolicyFingerprint: policyFingerprint, VerificationPolicyUID: schema.Status.Source.VerificationPolicyUID,
		VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		ExecutionBindingID:       executionBinding.Epoch,
		ControllerImage:          controllerImage,
		ControllerRevision:       controllerRevision,
		ControllerStateVersion:   executionBinding.ControllerStateVersion,
		PtahVersion:              executionBinding.PtahVersion, ExecutorImage: executionBinding.ExecutorImage,
		RunnerImage: runnerImage, RunnerProtocolVersion: executionBinding.RunnerProtocolVersion,
		Dialect: decoded.Dialect, Destructive: decoded.Destructive, PrivilegeChanges: planPrivilegeChanges(decoded),
		StatementCount: int32(len(decoded.Statements)),
	}
	// The fingerprint is computed from the spec as it will be published, the
	// way the controller-write webhook recomputes it.
	spec.Fingerprint, err = planstore.Binding(schema.UID, spec).Fingerprint()
	if err != nil {
		return nil, err
	}
	desired, chunks, err := planstore.Prepare(schema, spec, content)
	if err != nil {
		return nil, err
	}
	return r.Plans.Publish(ctx, desired, chunks)
}

func (r *SchemaReconciler) currentPlan(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (*operatorv1alpha1.PtahSchemaPlan, error) {
	if schema.Status.Plan == nil {
		return nil, fmt.Errorf("schema has no current plan")
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: schema.Status.Plan.Name}, plan); err != nil {
		return nil, fmt.Errorf("read current plan: %w", err)
	}
	if err := fingerprint.ValidatePlanContractVersion(plan.Spec.ContractVersion); err != nil {
		return nil, fmt.Errorf("current plan contract is not supported: %w", err)
	}
	if plan.UID != schema.Status.Plan.UID || plan.DeletionTimestamp != nil || plan.Spec.SchemaRef.UID != schema.UID ||
		plan.Spec.Fingerprint != schema.Status.Plan.Fingerprint || plan.Spec.ArtifactDigest != schema.Status.Source.Digest ||
		plan.Spec.ExecutionBindingID == "" || plan.Spec.ExecutionBindingID != schema.Status.Plan.ExecutionBindingID ||
		schema.Status.ExecutionBinding == nil || plan.Spec.ExecutionBindingID != schema.Status.ExecutionBinding.Epoch ||
		plan.Spec.CoordinationDigest != schema.Status.Plan.CoordinationDigest ||
		plan.Spec.CoordinationDigest != schema.Status.Target.CoordinationDigest ||
		plan.Spec.TargetIdentityDigest != schema.Status.Plan.TargetIdentityDigest ||
		plan.Spec.TargetIdentityDigest != schema.Status.Target.IdentityDigest ||
		plan.Spec.VerificationPolicyUID != schema.Status.Plan.VerificationPolicyUID ||
		plan.Spec.VerificationPolicyUID != schema.Status.Source.VerificationPolicyUID ||
		plan.Spec.VerificationPolicyDigest != schema.Status.Source.VerificationPolicyDigest ||
		plan.Spec.ControllerImage == "" || plan.Spec.ControllerImage != schema.Status.Plan.ControllerImage ||
		plan.Spec.ControllerRevision == "" || plan.Spec.ControllerRevision != schema.Status.Plan.ControllerRevision ||
		plan.Spec.ControllerStateVersion < 1 || plan.Spec.ControllerStateVersion != schema.Status.Plan.ControllerStateVersion ||
		plan.Spec.ControllerStateVersion != schema.Status.ExecutionBinding.ControllerStateVersion {
		return nil, fmt.Errorf("current plan binding is stale")
	}
	currentPolicyFingerprint, err := policyFingerprint(schema)
	if err != nil || currentPolicyFingerprint != plan.Spec.PolicyFingerprint {
		return nil, fmt.Errorf("current reconciliation policy no longer matches the plan")
	}
	return plan, nil
}

func (r *SchemaReconciler) ensureCurrentExecutionBinding(
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.PtahSchemaPlan,
) error {
	if schema == nil || schema.Status.Plan == nil || schema.Status.ExecutionBinding == nil || plan == nil {
		return fmt.Errorf("current plan execution binding is unavailable")
	}
	if plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion {
		return fmt.Errorf("current plan execution binding uses unsupported write contract %d", plan.Spec.ContractVersion)
	}
	status := schema.Status.Plan
	if status.ExecutionBindingID == "" || status.ExecutionBindingID != plan.Spec.ExecutionBindingID ||
		status.ExecutionBindingID != schema.Status.ExecutionBinding.Epoch ||
		status.ControllerImage == "" || status.ControllerImage != plan.Spec.ControllerImage ||
		status.ControllerRevision == "" || status.ControllerRevision != plan.Spec.ControllerRevision ||
		status.ControllerStateVersion < 1 || status.ControllerStateVersion != plan.Spec.ControllerStateVersion ||
		status.PtahVersion != plan.Spec.PtahVersion ||
		status.ExecutorImage != plan.Spec.ExecutorImage ||
		status.RunnerImage != plan.Spec.RunnerImage ||
		status.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("current plan execution binding is stale")
	}
	return r.ensureCurrentStatusExecutionBinding(schema, status)
}

func (r *SchemaReconciler) ensureCurrentStatusExecutionBinding(
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.CurrentPlanStatus,
) error {
	if schema == nil || schema.Status.ExecutionBinding == nil || plan == nil {
		return fmt.Errorf("current plan execution binding is unavailable")
	}
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return err
	}
	status := schema.Status.ExecutionBinding
	if !validExecutionBindingID(status.Epoch) || !executionBindingComponentsEqual(status, configured) ||
		plan.ExecutionBindingID == "" || plan.ExecutionBindingID != status.Epoch ||
		plan.ControllerStateVersion < 1 || plan.ControllerStateVersion != status.ControllerStateVersion ||
		plan.PtahVersion != status.PtahVersion || plan.ExecutorImage != status.ExecutorImage ||
		plan.RunnerProtocolVersion != status.RunnerProtocolVersion {
		return fmt.Errorf("current plan execution binding is stale")
	}
	return nil
}

func (r *SchemaReconciler) findApproval(ctx context.Context, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) (*operatorv1alpha1.PtahSchemaApproval, error) {
	list := &operatorv1alpha1.PtahSchemaApprovalList{}
	if err := r.Client.List(ctx, list, client.InNamespace(schema.Namespace), client.MatchingFields{approvalSchemaIndex: schema.Name}); err != nil {
		return nil, fmt.Errorf("list plan approvals: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].CreationTimestamp.Before(&list.Items[j].CreationTimestamp) })
	var staleCandidate *operatorv1alpha1.PtahSchemaApproval
	var validCandidate *operatorv1alpha1.PtahSchemaApproval
	for i := range list.Items {
		candidate := &operatorv1alpha1.PtahSchemaApproval{}
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(&list.Items[i]), candidate); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if meta.IsStatusConditionTrue(candidate.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) || candidate.DeletionTimestamp != nil {
			continue
		}
		if meta.IsStatusConditionTrue(candidate.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
			// Consumed is durable historical evidence, not permission that can
			// authorize another dispatch. Once a different plan is current, retain
			// Consumed=True and additionally mark the old decision stale.
			if staleCandidate == nil && !approvalMatches(candidate, schema, plan) {
				staleCandidate = candidate
			}
			continue
		}
		if approvalMatches(candidate, schema, plan) {
			if validCandidate == nil {
				validCandidate = candidate
				continue
			}
			// Admission prevents queued approvals in normal operation, but two
			// concurrent CREATE requests can both pass their read boundary. Retire
			// duplicates one at a time before reserving the sole decision.
			if err := r.markApprovalStaleWithReason(
				ctx,
				candidate,
				operatorv1alpha1.ReasonSupersededApproval,
				"Another approval already reserves this exact immutable plan",
			); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if staleCandidate == nil {
			staleCandidate = candidate
		}
	}
	if validCandidate != nil {
		return validCandidate, nil
	}
	// A valid approval always wins regardless of historical backlog. Clean up
	// at most one stale object per reconciliation to bound API writes and Events.
	if staleCandidate != nil {
		if err := r.markApprovalStale(ctx, staleCandidate); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (r *SchemaReconciler) markApprovalStale(ctx context.Context, approval *operatorv1alpha1.PtahSchemaApproval) error {
	return r.markApprovalStaleWithReason(ctx, approval, operatorv1alpha1.ReasonPlanNoLongerCurrent, "The approval does not match the current immutable plan")
}

func (r *SchemaReconciler) markApprovalStaleWithReason(
	ctx context.Context,
	approval *operatorv1alpha1.PtahSchemaApproval,
	reason operatorv1alpha1.ConditionReason,
	message string,
) error {
	if meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		return nil
	}
	before := approval.DeepCopy()
	approval.Status.ObservedGeneration = approval.Generation
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalAccepted, Status: metav1.ConditionFalse,
		Reason: string(reason), Message: message,
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalStale, Status: metav1.ConditionTrue,
		Reason: string(reason), Message: message,
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("mark approval stale: %w", err)
	}
	r.event(approval, corev1.EventTypeWarning, "ApprovalStale", "%s", message)
	if r.Telemetry != nil {
		r.Telemetry.ObserveApproval(telemetry.FamilySchema, telemetry.ApprovalStale)
	}
	return nil
}

func (r *SchemaReconciler) markApprovalConsumed(ctx context.Context, approval *operatorv1alpha1.PtahSchemaApproval) error {
	stale := meta.FindStatusCondition(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
	if meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalAccepted) &&
		meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) &&
		stale != nil && stale.Status == metav1.ConditionFalse {
		return nil
	}
	before := approval.DeepCopy()
	approval.Status.ObservedGeneration = approval.Generation
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalAccepted, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonCurrentPlan), Message: "The approval exactly matches the current immutable plan",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalStale, Status: metav1.ConditionFalse,
		Reason: string(operatorv1alpha1.ReasonCurrentPlan), Message: "The approved plan is current",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonDispatchCommitted), Message: "The exact plan was committed to one Apply Job dispatch",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("mark approval consumed: %w", err)
	}
	return nil
}

func (r *SchemaReconciler) ensureCurrentApproval(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.PtahSchemaPlan,
	consume bool,
) (bool, error) {
	if schema.Status.Plan == nil || schema.Status.Plan.Approval == nil {
		return false, nil
	}
	recorded := schema.Status.Plan.Approval
	approval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: recorded.Name}, approval); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read accepted approval: %w", err)
	}
	if approval.UID != recorded.UID || approval.DeletionTimestamp != nil ||
		meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) ||
		!approvalMatches(approval, schema, plan) || !recordedApprovalMatches(recorded, approval) {
		return false, nil
	}
	if consume {
		if err := r.markApprovalConsumed(ctx, approval); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *SchemaReconciler) approvalBecameInvalid(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	before := schema.DeepCopy()
	if schema.Status.Plan != nil {
		schema.Status.Plan.Approval = nil
	}
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonApprovalRevoked, "The recorded approval is missing, replaced, or no longer matches the current plan")
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonApprovalRevoked, "No Apply Job was started with the invalid approval")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonAwaitingApproval, "The current plan requires a new approval")
	// No Apply Job was created under the approval, so an Apply claim hands its
	// Lease back in the write.
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionDiscard); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		if schemaOperation(operation).Mutating {
			r.Telemetry.ObserveApply(telemetry.FamilySchema, telemetry.ApplyStale)
			r.Telemetry.ObserveFailure(telemetry.FamilySchema, telemetry.FailureStageApply, telemetry.FailureStaleInput)
		}
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeWarning, "ApprovalRevoked", "The recorded approval became invalid before the Apply Job was created")
	return requeueAtDeadline(schema.Status.NextReconciliationTime, r.now()), nil
}

func (r *SchemaReconciler) waitBlocked(ctx context.Context, schema *operatorv1alpha1.PtahSchema, reason operatorv1alpha1.ConditionReason, message string) (ctrl.Result, error) {
	before := schema.DeepCopy()
	now := r.now()
	next := metav1.NewTime(now.Add(interval(schema)))
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	schema.Status.NextReconciliationTime = &next
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return requeueAtDeadline(&next, r.now()), nil
}

var (
	errTerminalPodPending      = errors.New("terminal Job pod is not yet available")
	errTerminalPodMultiplicity = errors.New("terminal Job has multiple executor Pods")
	errTerminalPodIntent       = errors.New("terminal Job pod does not match immutable intent")
)

type terminalEvidence struct {
	Durable     bool
	Result      *runner.Result
	ResultError error
	Logs        []byte
	PodUIDs     []types.UID
	PodCount    int32
	Trusted     bool
	// TerminationMessage is the executor container's termination message as
	// the kubelet recorded it in Pod status. It is set only where Trusted is:
	// a container that never terminated wrote none.
	TerminationMessage string
	// LogLost is set, and Logs empty, when the executor's log will not be read
	// again (readResultLog). It stands in for the parse error of a log that
	// holds no frame.
	LogLost *resultLogLost
}

func (r *SchemaReconciler) collectTerminalPodEvidence(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (terminalEvidence, *corev1.Pod, error) {
	var snapshot *operatorv1alpha1.PodAdmissionSnapshot
	if schema.Status.ActiveOperation != nil {
		snapshot = schema.Status.ActiveOperation.AdmissionSnapshot
	}
	return collectTerminalPodEvidence(ctx, r.directReader(), schema.Namespace, job, snapshot)
}

// collectTerminalPodEvidence selects the one Pod a result may be attributed to
// and records what the Job produced. A second Pod, a Pod outside the persisted
// admission envelope, or a missing snapshot is refused rather than read: a
// terminal result belongs to one attempt.
func collectTerminalPodEvidence(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	job *batchv1.Job,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) (terminalEvidence, *corev1.Pod, error) {
	if job == nil || job.Name == "" || job.UID == "" {
		return terminalEvidence{}, nil, fmt.Errorf("terminal Job lacks immutable identity")
	}
	pods, err := podsOwnedByJob(ctx, reader, namespace, job.Name, job.UID)
	if err != nil {
		return terminalEvidence{}, nil, err
	}
	evidence := podIdentityEvidence(pods)
	if len(pods) == 0 {
		return evidence, nil, nil
	}
	if len(pods) != 1 {
		return evidence, nil, errTerminalPodMultiplicity
	}
	selected := pods[0]
	if selected.UID == "" {
		return evidence, nil, fmt.Errorf("%w: Pod UID is empty", errTerminalPodIntent)
	}
	if snapshot == nil {
		return evidence, nil, fmt.Errorf("%w: active operation admission binding is missing", errTerminalPodIntent)
	}
	if err := validatePodIntent(selected, job, snapshot); err != nil {
		return evidence, nil, fmt.Errorf("%w: %v", errTerminalPodIntent, err)
	}
	for _, status := range selected.Status.ContainerStatuses {
		if status.Name == executorContainerName && status.State.Terminated != nil {
			evidence.Trusted = true
			evidence.TerminationMessage = status.State.Terminated.Message
			return evidence, selected, nil
		}
	}
	return evidence, selected, nil
}

func (r *SchemaReconciler) podsOwnedByJob(
	ctx context.Context,
	namespace string,
	jobName string,
	jobUID types.UID,
) ([]*corev1.Pod, error) {
	return podsOwnedByJob(ctx, r.directReader(), namespace, jobName, jobUID)
}

// podsOwnedByJob returns the Pods one exact Job owns, in a stable order. Both
// reconcilers attribute a result to a Pod this way, so it reads through a
// reader rather than through either of them.
func podsOwnedByJob(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	jobName string,
	jobUID types.UID,
) ([]*corev1.Pod, error) {
	if jobName == "" || jobUID == "" {
		return nil, nil
	}
	list := &corev1.PodList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list exact-owner Job pods: %w", err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if exactControllerOwner(pod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job", jobName, jobUID) {
			pods = append(pods, pod)
		}
	}
	sort.Slice(pods, func(i, j int) bool {
		left := string(pods[i].UID) + "\x00" + pods[i].Name
		right := string(pods[j].UID) + "\x00" + pods[j].Name
		return left < right
	})
	return pods, nil
}

func podIdentityEvidence(pods []*corev1.Pod) terminalEvidence {
	evidence := terminalEvidence{PodCount: int32(len(pods))}
	for _, pod := range pods {
		if len(evidence.PodUIDs) == 8 {
			break
		}
		if pod.UID != "" {
			evidence.PodUIDs = append(evidence.PodUIDs, pod.UID)
		}
	}
	return evidence
}

func mergePodEvidence(pending *operatorv1alpha1.PendingObservationStatus, current terminalEvidence) bool {
	if pending == nil {
		return false
	}
	seen := make(map[types.UID]struct{}, len(pending.ApplyPodUIDs)+len(current.PodUIDs))
	all := make([]types.UID, 0, len(pending.ApplyPodUIDs)+len(current.PodUIDs))
	for _, uid := range append(append([]types.UID(nil), pending.ApplyPodUIDs...), current.PodUIDs...) {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		all = append(all, uid)
	}
	sort.Slice(all, func(i, j int) bool { return string(all[i]) < string(all[j]) })
	distinctCount := int32(len(all))
	if len(all) > 8 {
		all = all[:8]
	}
	count := pending.ApplyPodCount
	if current.PodCount > count {
		count = current.PodCount
	}
	if distinctCount > count {
		count = distinctCount
	}
	outcome := pending.Outcome
	if count > 1 {
		outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
	}
	if count == pending.ApplyPodCount && outcome == pending.Outcome && reflect.DeepEqual(all, pending.ApplyPodUIDs) {
		return false
	}
	pending.ApplyPodCount = count
	pending.ApplyPodUIDs = all
	pending.Outcome = outcome
	return true
}

func (r *SchemaReconciler) terminalLogs(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (terminalEvidence, error) {
	if durableDeliveryRequested(job) {
		operation := schema.Status.ActiveOperation
		if operation == nil {
			return terminalEvidence{}, errors.New("active schema operation is missing")
		}
		engine := ""
		if operation.Target != nil {
			engine = strings.ToLower(string(operation.Target.Engine))
		}
		return durableTerminalResult(ctx, r.directReader(), r.Results, schema, "PtahSchema", job, operation.AdmissionSnapshot, resultconsumer.Request{ExecutionBindingID: operation.ExecutionBindingID, InputFingerprint: operation.InputFingerprint, Operation: string(schemaOperation(operation).Runner), OperationID: operation.ID, JobUID: operation.JobUID, Engine: engine})
	}
	evidence, selected, err := r.collectTerminalPodEvidence(ctx, schema, job)
	if err != nil {
		return evidence, err
	}
	if selected == nil {
		if r.now().Sub(schema.Status.ActiveOperation.StartedAt.Time) < terminalPodGrace {
			return evidence, errTerminalPodPending
		}
		// An empty byte slice is deliberately parsed as an uncertain/missing
		// frame after the grace period.
		return evidence, nil
	}
	if !evidence.Trusted {
		// A failed init container means the executor has no log stream. Treat it
		// as a missing frame so read-only operations retry and apply remains
		// uncertain, instead of looping forever on pod/log BadRequest.
		return evidence, nil
	}
	if r.Logs == nil {
		return evidence, fmt.Errorf("pod log reader is not configured")
	}
	logs, err := readResultLog(ctx, r.Logs, resultLogFailuresOf(&r.resultLogs), r.now(), r.ResultReadTimeout,
		schemaResultReadBudget(schema), selected)
	var lost *resultLogLost
	if errors.As(err, &lost) {
		evidence.LogLost = lost
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	evidence.Logs = logs
	return evidence, nil
}

// schemaResultReadBudget is the Lease this read has to stay inside.
//
// A post-Apply Observe renews the Lease at the duration the Apply recorded in
// status.pendingObservation, not at whatever the spec says now, so that is the
// one to read when it is there. Otherwise the claim's own duration applies, and
// a read-only operation holding no Lease has no budget to derive.
func schemaResultReadBudget(schema *operatorv1alpha1.PtahSchema) time.Duration {
	if pending := schema.Status.PendingObservation; pending != nil {
		if budget := leaseReadBudget(pending.LeaseDurationSeconds); budget > 0 {
			return budget
		}
	}
	if operation := schema.Status.ActiveOperation; operation != nil {
		return leaseReadBudget(operation.LeaseDurationSeconds)
	}
	return 0
}

func (r *SchemaReconciler) markJobHarvested(ctx context.Context, job *batchv1.Job) error {
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

func (r *SchemaReconciler) removeActiveFinalizer(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
	latest := &operatorv1alpha1.PtahSchema{}
	if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(schema), latest); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := r.rejectUnsupportedStoredControllerState(latest); err != nil {
		return fmt.Errorf("recheck stored controller state before finalizer removal: %w", err)
	}
	if !controllerutil.ContainsFinalizer(latest, activeOperationFinalizer) {
		return nil
	}
	if latest.Status.ActiveOperation != nil || latest.Status.PendingObservation != nil ||
		latest.Status.PendingLockRelease != nil {
		return fmt.Errorf("active-operation finalizer still protects durable safety work")
	}
	before := latest.DeepCopy()
	controllerutil.RemoveFinalizer(latest, activeOperationFinalizer)
	if err := r.Client.Patch(ctx, latest, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("remove active-operation finalizer: %w", err)
	}
	return nil
}

func (r *SchemaReconciler) acquireApplyLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	if !schemaOperation(schema.Status.ActiveOperation).Mutating {
		return false, 0, fmt.Errorf("apply target lock inputs are incomplete")
	}
	return r.acquireClaimLock(ctx, schema)
}

// acquireClaimLock takes the realm under the active claim's own identity, the
// way a claim holds it when no proof is outstanding.
func (r *SchemaReconciler) acquireClaimLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return false, 0, fmt.Errorf("active operation is missing")
	}
	if operation.CoordinationDigest == "" || operation.LeaseDurationSeconds == 0 {
		return false, 0, fmt.Errorf("%s target lock inputs are incomplete", strings.ToLower(string(operation.Type)))
	}
	return r.acquireActiveLock(ctx, schema, targetlock.Request{
		CoordinationNamespace: r.LockNamespace, CoordinationDigest: operation.CoordinationDigest,
		Holder:   targetlock.Holder{SchemaUID: schema.UID, OperationID: operation.ID},
		Duration: time.Duration(operation.LeaseDurationSeconds) * time.Second,
	})
}

// acquireOperationLock takes the realm the way the active claim holds it: on
// behalf of the proof it carries out, under its own identity, or not at all.
// The operation type's descriptor answers which, as it does when the claim is
// retired.
func (r *SchemaReconciler) acquireOperationLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	if schema.Status.ActiveOperation == nil {
		return false, 0, fmt.Errorf("active operation is missing")
	}
	switch {
	case schemaClaimServesProof(schema):
		return r.acquirePendingObservationLock(ctx, schema)
	case schemaClaimHoldsLock(schema):
		return r.acquireClaimLock(ctx, schema)
	default:
		return true, 0, nil
	}
}

// planSealPublicKeyDigest is the digest recorded on a Plan claim and compared
// against at harvest, over the exact bytes the Job's environment carries so
// the two are computed the same way wherever either is read.
func planSealPublicKeyDigest(key planseal.PublicKey) string {
	return fingerprint.DigestBytes([]byte(key.Encode()))
}

func (r *SchemaReconciler) acquirePendingObservationLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	pending := schema.Status.PendingObservation
	operation := schema.Status.ActiveOperation
	if pending == nil || operation == nil || !schemaOperation(operation).ServesProof {
		return false, 0, fmt.Errorf("pending observation lock inputs are incomplete")
	}
	return r.acquireActiveLock(ctx, schema, r.pendingLockRequest(schema, pending))
}

func (r *SchemaReconciler) acquirePendingLock(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) (bool, time.Duration, error) {
	if pending == nil || pending.CoordinationDigest == "" || pending.Plan.CoordinationDigest != pending.CoordinationDigest ||
		pending.ApplyOperationID == "" || pending.LeaseDurationSeconds == 0 {
		return false, 0, fmt.Errorf("pending observation lock inputs are incomplete")
	}
	request := r.pendingLockRequest(schema, pending)
	request.ExpectedEpoch = pending.LeaseEpoch
	acquired, requeue, epoch, continuityLost, err := r.acquireLockEpoch(ctx, request)
	if err != nil || !acquired {
		return acquired, requeue, err
	}
	if pending.LeaseEpoch == epoch && !continuityLost {
		return true, 0, nil
	}
	before := schema.DeepCopy()
	if pending.LeaseEpoch == "" || pending.LeaseEpoch != epoch || continuityLost {
		pending.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
		pending.PlanRequired = false
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonLeaseContinuityLost, "Convergence proof restarted after the database lock epoch changed")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonLeaseContinuityLost, "A fresh observation is required after lock continuity was lost")
	}
	pending.LeaseEpoch = epoch
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return false, 0, err
	}
	if before.Status.PendingObservation != nil && before.Status.PendingObservation.LeaseEpoch != "" {
		r.event(schema, corev1.EventTypeWarning, "LeaseContinuityLost", "Database lock continuity was lost; restarting read-only convergence proof")
	}
	return false, statusPatchRequeue, nil
}

func (r *SchemaReconciler) acquireActiveLock(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	request targetlock.Request,
) (bool, time.Duration, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return false, 0, fmt.Errorf("active operation is required for database lock acquisition")
	}
	request.ExpectedEpoch = operation.LeaseEpoch
	acquired, requeue, epoch, reportedContinuityLoss, err := r.acquireLockEpoch(ctx, request)
	if err != nil || !acquired {
		return acquired, requeue, err
	}
	expected := operation.LeaseEpoch
	pending := schema.Status.PendingObservation
	if pending != nil && schemaOperation(operation).ServesProof {
		if expected == "" {
			expected = pending.LeaseEpoch
		}
		if pending.LeaseEpoch != "" && operation.LeaseEpoch != "" && pending.LeaseEpoch != operation.LeaseEpoch {
			expected = operation.LeaseEpoch
		}
	}
	if expected == epoch && operation.LeaseEpoch == epoch {
		return true, 0, nil
	}

	before := schema.DeepCopy()
	continuityLost := expected == "" || reportedContinuityLoss || expected != epoch
	if continuityLost && expected != "" && !schemaMayHaveDispatched(operation) {
		// The first Lease creation necessarily has a new epoch. Because the
		// operation claim persisted its expected token before any dispatch
		// boundary, adopting the API-assigned epoch here cannot validate stale
		// work. Every later locked Job sets DispatchStarted before Create.
		continuityLost = false
	}
	operation.LeaseEpoch = epoch
	operation.LeaseContinuityLost = continuityLost
	if pending != nil && schemaOperation(operation).ServesProof {
		if pending.LeaseEpoch != "" && pending.LeaseEpoch != epoch {
			continuityLost = true
			operation.LeaseContinuityLost = true
			pending.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
			pending.PlanRequired = false
		}
		pending.LeaseEpoch = epoch
	}
	if continuityLost {
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonLeaseContinuityLost, "The operation result is invalid because the database lock epoch changed")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonLeaseContinuityLost, "A fresh observation is required after lock continuity was lost")
	}
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return false, 0, err
	}
	if continuityLost {
		r.event(schema, corev1.EventTypeWarning, "LeaseContinuityLost", "Database lock continuity changed; the active result will be discarded")
	}
	return false, statusPatchRequeue, nil
}

func (r *SchemaReconciler) acquireLockEpoch(ctx context.Context, request targetlock.Request) (bool, time.Duration, string, bool, error) {
	if r.Locks == nil {
		return false, 0, "", false, fmt.Errorf("database target locker is not configured")
	}
	result, err := r.Locks.Acquire(ctx, request)
	if err != nil {
		return false, 0, "", false, fmt.Errorf("acquire database target lock: %w", err)
	}
	if result.Acquired {
		if result.Epoch == "" {
			return false, 0, "", false, fmt.Errorf("database target lock returned an empty epoch")
		}
		return true, 0, result.Epoch, result.ContinuityLost, nil
	}
	if result.Contention == nil {
		return false, 0, "", false, fmt.Errorf("database target lock returned no acquisition result")
	}
	requeue := result.Contention.RequeueAfter
	if requeue < time.Second {
		requeue = time.Second
	}
	if requeue > maxLockContentionPoll {
		requeue = maxLockContentionPoll
	}
	return false, requeue, "", false, nil
}

func targetLockReleaseForOperation(
	operation *operatorv1alpha1.ActiveOperationStatus,
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

func targetLockReleaseForPending(
	pending *operatorv1alpha1.PendingObservationStatus,
) (*operatorv1alpha1.TargetLockReleaseStatus, error) {
	if pending == nil {
		return nil, fmt.Errorf("persist target lock release: post-apply %w", mutationlifecycle.ErrIncompleteBinding)
	}
	return mutationlifecycle.OwedRelease(mutationlifecycle.LockBinding{
		CoordinationDigest:   pending.CoordinationDigest,
		OperationID:          pending.ApplyOperationID,
		LeaseEpoch:           pending.LeaseEpoch,
		LeaseDurationSeconds: pending.LeaseDurationSeconds,
	})
}

// schemaRealmClaim describes the active claim to the shared realm-ownership
// decision. A Plan or an Observe is the sort of claim a pending observation
// carries out; an Apply holds the realm in its own right.
func schemaRealmClaim(schema *operatorv1alpha1.PtahSchema) mutationlifecycle.RealmClaim {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return mutationlifecycle.RealmClaim{ProofOutstanding: schema.Status.PendingObservation != nil}
	}
	return mutationlifecycle.RealmClaim{
		Mutating:         schemaOperation(operation).Mutating,
		ServesProof:      schemaOperation(operation).ServesProof,
		Locked:           operation.LeaseEpoch != "",
		ProofOutstanding: schema.Status.PendingObservation != nil,
	}
}

// retireClaim drops the active claim in one status write. The write carries
// everything the caller recorded on schema since before -- the phase, the
// conditions, the pending observation an Apply leaves -- and the database
// release the retirement owes, which mutationlifecycle.Retirement decides from
// the claim as it stood and why it is going. The release itself follows the
// write, and one that fails keeps its record for the top of the next pass.
//
// A schema keeps the realm with the pending observation an Apply leaves, so a
// run that may have changed the database hands nothing back here; the write
// that removes a pending observation ends the proof that held it, and hands
// back the proof's epoch.
func (r *SchemaReconciler) retireClaim(
	ctx context.Context,
	before, schema *operatorv1alpha1.PtahSchema,
	disposition mutationlifecycle.Disposition,
) error {
	if _, err := stageRetirementRelease(before, schema, disposition); err != nil {
		return err
	}
	schema.Status.ActiveOperation = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return err
	}
	if schema.Status.PendingLockRelease == nil {
		return nil
	}
	return r.completePendingLockRelease(ctx, schema)
}

// stageRetirementRelease records on schema the release that retiring the claim
// in before owes, and reports whose epoch that was. It is retireClaim's
// decision on its own, for the one caller that retires an attempt and keeps
// the claim: a read-only retry, which hands back what the failed attempt held
// and takes the realm again for the next.
func stageRetirementRelease(
	before, schema *operatorv1alpha1.PtahSchema,
	disposition mutationlifecycle.Disposition,
) (mutationlifecycle.RealmOwner, error) {
	retirement := mutationlifecycle.Retirement{
		Claim:            schemaRealmClaim(before),
		Disposition:      disposition,
		RecordHoldsRealm: true,
		EndsProof:        before.Status.PendingObservation != nil && schema.Status.PendingObservation == nil,
	}
	released := retirement.Releases()
	switch released {
	case mutationlifecycle.OwnerClaim:
		return released, stageOperationLockRelease(schema, before.Status.ActiveOperation)
	case mutationlifecycle.OwnerProof:
		return released, stagePendingLockRelease(schema, before.Status.PendingObservation)
	}
	return released, nil
}

func stageOperationLockRelease(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) error {
	if operation == nil || operation.LeaseEpoch == "" {
		return nil
	}
	release, err := targetLockReleaseForOperation(operation)
	if err != nil {
		return err
	}
	return stageTargetLockRelease(schema, release)
}

func stagePendingLockRelease(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) error {
	release, err := targetLockReleaseForPending(pending)
	if err != nil {
		return err
	}
	return stageTargetLockRelease(schema, release)
}

func stageTargetLockRelease(
	schema *operatorv1alpha1.PtahSchema,
	release *operatorv1alpha1.TargetLockReleaseStatus,
) error {
	if schema == nil || release == nil {
		return fmt.Errorf("persist target lock release: schema and release are required")
	}
	if schema.Status.PendingLockRelease != nil {
		return fmt.Errorf("persist target lock release: another release is already pending")
	}
	schema.Status.PendingLockRelease = release
	return nil
}

func (r *SchemaReconciler) reconcilePendingLockRelease(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	if err := r.completePendingLockRelease(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *SchemaReconciler) completePendingLockRelease(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) error {
	before := schema.DeepCopy()
	return mutationlifecycle.CompleteRelease(ctx, r.Locks, r.LockNamespace, schemaLockOwner{schema},
		func(ctx context.Context) error { return r.patchStatus(ctx, before, schema) })
}

func (r *SchemaReconciler) pendingLockRequest(schema *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus) targetlock.Request {
	return targetlock.Request{
		CoordinationNamespace: r.LockNamespace,
		CoordinationDigest:    pending.CoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID:   schema.UID,
			OperationID: pending.ApplyOperationID,
		},
		Duration: time.Duration(pending.LeaseDurationSeconds) * time.Second,
	}
}

func (r *SchemaReconciler) patchStatus(ctx context.Context, before, after *operatorv1alpha1.PtahSchema) error {
	if databaseEngineSupported(after.Spec.Target.Engine) {
		setCondition(
			after,
			operatorv1alpha1.ConditionEngineSupported,
			metav1.ConditionTrue,
			operatorv1alpha1.ReasonSupportedEngine,
			fmt.Sprintf("Database engine %q is supported", after.Spec.Target.Engine),
		)
	}
	if reflect.DeepEqual(before.Status, after.Status) {
		return nil
	}
	if err := r.Client.Status().Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("update PtahSchema status: %w", err)
	}
	r.observeStatusTransitions(before, after)
	return nil
}

func (r *SchemaReconciler) observeStatusTransitions(before, after *operatorv1alpha1.PtahSchema) {
	newPlan := planChanged(before.Status.Plan, after.Status.Plan)
	if newPlan && after.Status.Plan != nil && r.Telemetry != nil {
		r.Telemetry.ObservePlan(telemetry.FamilySchema, after.Spec.Target.Engine,
			telemetry.DestructivePlan(after.Status.Plan.Destructive))
	}
	approvalRequired := meta.IsStatusConditionTrue(after.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if conditionBecame(before.Status.Conditions, after.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "") ||
		newPlan && approvalRequired {
		message := "The current immutable plan requires approval"
		if plan := after.Status.Plan; plan != nil && len(plan.PrivilegeChanges) > 0 &&
			after.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyAlways {
			message = "The current immutable plan changes privileges (" + privilegeKindList(plan.PrivilegeChanges) +
				") and requires approval under apply policy Always"
		}
		r.event(after, corev1.EventTypeNormal, "ApprovalRequired", "%s", message)
		if r.Telemetry != nil {
			r.Telemetry.ObserveApproval(telemetry.FamilySchema, telemetry.ApprovalRequired)
		}
	}
	if approvalChanged(before.Status.Plan, after.Status.Plan) {
		r.event(after, corev1.EventTypeNormal, "ApprovalAccepted", "An authenticated approval was accepted for the current immutable plan")
		if r.Telemetry != nil {
			r.Telemetry.ObserveApproval(telemetry.FamilySchema, telemetry.ApprovalAccepted)
		}
	}
	if planInvalidated(before.Status.Plan, after.Status.Plan) {
		r.event(after, corev1.EventTypeWarning, "PlanStale", "The immutable plan became stale and will not be applied")
	}
	if conditionBecame(before.Status.Conditions, after.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, string(operatorv1alpha1.ReasonPolicyChanged)) {
		r.event(after, corev1.EventTypeWarning, "VerificationPolicyInvalidated", "Verification policy no longer matches the plan; artifact and plan verification were invalidated")
	}
}

func (r *SchemaReconciler) observeOperation(operation *operatorv1alpha1.ActiveOperationStatus, outcome telemetry.OperationOutcome) {
	if r.Telemetry == nil || operation == nil || operation.StartedAt.IsZero() {
		return
	}
	r.Telemetry.ObserveOperation(telemetry.FamilySchema, telemetry.OperationForSchema(operation.Type),
		outcome, r.now().Sub(operation.StartedAt.Time))
}

func planChanged(before, after *operatorv1alpha1.CurrentPlanStatus) bool {
	if after == nil {
		return false
	}
	return before == nil || before.UID != after.UID || before.Fingerprint != after.Fingerprint
}

func planInvalidated(before, after *operatorv1alpha1.CurrentPlanStatus) bool {
	if before == nil {
		return false
	}
	return after == nil || before.UID != after.UID || before.Fingerprint != after.Fingerprint
}

func approvalChanged(before, after *operatorv1alpha1.CurrentPlanStatus) bool {
	if after == nil || after.Approval == nil {
		return false
	}
	return before == nil || before.Approval == nil || before.Approval.UID != after.Approval.UID
}

func conditionBecame(before, after []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason string) bool {
	afterCondition := meta.FindStatusCondition(after, conditionType)
	if afterCondition == nil || afterCondition.Status != status || reason != "" && afterCondition.Reason != reason {
		return false
	}
	beforeCondition := meta.FindStatusCondition(before, conditionType)
	if beforeCondition == nil {
		return true
	}
	if beforeCondition.Status != status {
		return true
	}
	return reason != "" && beforeCondition.Reason != reason
}

func (r *SchemaReconciler) operationInputFingerprint(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.OperationType) (string, error) {
	inputs, err := operationInputs(schema, operation)
	if err != nil {
		return "", err
	}
	if operation == operatorv1alpha1.OperationVerify && schema.Status.ActiveOperation != nil &&
		schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationVerify {
		inputs["verification_policy_uid"] = string(schema.Status.ActiveOperation.VerificationPolicyUID)
		inputs["verification_policy_digest"] = schema.Status.ActiveOperation.VerificationPolicyDigest
	}
	return fingerprint.DigestCanonicalJSON(inputs)
}

func (r *SchemaReconciler) directReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *SchemaReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock().UTC()
	}
	return time.Now().UTC()
}

func (r *SchemaReconciler) event(object client.Object, eventType, reason, message string, arguments ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, message, arguments...)
	}
}

func (r *SchemaReconciler) SetupWithManager(manager ctrl.Manager) error {
	if problems := k8svalidation.IsDNS1123Label(r.LockNamespace); len(problems) != 0 {
		return fmt.Errorf("target lock namespace is invalid: %s", problems[0])
	}
	if err := r.AdmissionOptions.Validate(); err != nil {
		return fmt.Errorf("Pod admission configuration is invalid: %w", err)
	}
	if r.Client == nil {
		r.Client = manager.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = manager.GetAPIReader()
	}
	if r.Scheme == nil {
		r.Scheme = manager.GetScheme()
	}
	if r.Recorder == nil {
		r.Recorder = manager.GetEventRecorderFor("ptah-schema-controller")
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &operatorv1alpha1.PtahSchemaApproval{}, approvalSchemaIndex, func(object client.Object) []string {
		approval := object.(*operatorv1alpha1.PtahSchemaApproval)
		if approval.Spec.SchemaRef.Name == "" {
			return nil
		}
		return []string{approval.Spec.SchemaRef.Name}
	}); err != nil {
		return fmt.Errorf("index approvals by schema: %w", err)
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &operatorv1alpha1.PtahSchema{}, schemaPolicyIndex, func(object client.Object) []string {
		schema := object.(*operatorv1alpha1.PtahSchema)
		if schema.Spec.Desired.VerificationPolicyFrom.Name == "" {
			return nil
		}
		return []string{schema.Spec.Desired.VerificationPolicyFrom.Name}
	}); err != nil {
		return fmt.Errorf("index schemas by verification policy: %w", err)
	}
	mapApproval := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
		approval, ok := object.(*operatorv1alpha1.PtahSchemaApproval)
		if !ok || approval.Spec.SchemaRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: approval.Namespace, Name: approval.Spec.SchemaRef.Name}}}
	})
	mapVerificationPolicy := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		configMap, ok := object.(*corev1.ConfigMap)
		if !ok || configMap.Name == "" {
			return nil
		}
		schemas := &operatorv1alpha1.PtahSchemaList{}
		if err := r.Client.List(
			ctx,
			schemas,
			client.InNamespace(configMap.Namespace),
			client.MatchingFields{schemaPolicyIndex: configMap.Name},
		); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(schemas.Items))
		for i := range schemas.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&schemas.Items[i])})
		}
		return requests
	})
	primaryEvents := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{
		UpdateFunc: func(update event.UpdateEvent) bool {
			return update.ObjectOld.GetDeletionTimestamp() == nil && update.ObjectNew.GetDeletionTimestamp() != nil
		},
	})
	return ctrl.NewControllerManagedBy(manager).
		For(&operatorv1alpha1.PtahSchema{}, builder.WithPredicates(primaryEvents)).
		Owns(&batchv1.Job{}, builder.WithPredicates(operationJobEvents())).
		Watches(&operatorv1alpha1.PtahSchemaApproval{}, mapApproval).
		Watches(&corev1.ConfigMap{}, mapVerificationPolicy).
		Watches(&operatorv1alpha1.PtahRealm{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				return realmClaimantRequests(ctx, r.Client,
					func() client.ObjectList { return &operatorv1alpha1.PtahSchemaList{} }, object)
			})).
		Complete(r)
}

func databaseTargetBinding(target operatorv1alpha1.DatabaseTargetSpec) operatorv1alpha1.DatabaseTargetBinding {
	binding := operatorv1alpha1.DatabaseTargetBinding{Engine: target.Engine}
	target.URLFrom.DeepCopyInto(&binding.URLFrom)
	return binding
}

func operationInputs(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.OperationType) (map[string]any, error) {
	if schema == nil || schema.Status.ExecutionBinding == nil ||
		!validExecutionBindingID(schema.Status.ExecutionBinding.Epoch) {
		return nil, fmt.Errorf("durable execution binding is required before claiming %s", operation)
	}
	base := map[string]any{"generation": schema.Generation, "operation": operation, "schema_uid": schema.UID}
	base["execution_binding"] = *schema.Status.ExecutionBinding.DeepCopy()
	switch operation {
	case operatorv1alpha1.OperationResolve:
		base["requested_reference"] = schema.Spec.Desired.OCIRef
		base["registry_auth"] = schema.Spec.Desired.RegistryAuthFrom
		base["transport"] = schema.Spec.Desired.Transport
	case operatorv1alpha1.OperationVerify:
		if schema.Status.Source.Digest == "" || schema.Status.Source.ResolvedReference == "" {
			return nil, fmt.Errorf("resolved artifact is required before verification")
		}
		base["requested_reference"] = schema.Spec.Desired.OCIRef
		base["resolved_reference"] = schema.Status.Source.ResolvedReference
		base["digest"] = schema.Status.Source.Digest
		base["verification_policy"] = schema.Spec.Desired.VerificationPolicyFrom
	case operatorv1alpha1.OperationObserve:
		target := databaseTargetBinding(schema.Spec.Target)
		exclude := schema.Spec.Policy.Exclude
		severity := schema.Spec.Policy.DriftSeverity
		connectTimeout := schema.Spec.Execution.ConnectTimeout
		lockTimeout := schema.Spec.Policy.LockTimeout
		var coordinationDigest string
		if pending := schema.Status.PendingObservation; pending != nil {
			base["generation"] = pending.ApplyGeneration
			target = pending.Target
			exclude = pending.Exclude
			severity = pending.DriftSeverity
			connectTimeout = pending.ConnectTimeout
			lockTimeout = pending.LockTimeout
			coordinationDigest = pending.CoordinationDigest
			base["pending_observation"] = pending
			base["resolved_reference"] = pending.Source.ResolvedReference
			base["artifact_digest"] = pending.Source.Digest
			base["source_access"] = pending.Source
		} else {
			var err error
			coordinationDigest, err = coordination.Digest(schema.Namespace, schema.Spec.Target)
			if err != nil {
				return nil, fmt.Errorf("derive database coordination digest: %w", err)
			}
			if !schema.Status.Source.Verified {
				return nil, fmt.Errorf("verified artifact is required before observation")
			}
			base["resolved_reference"] = schema.Status.Source.ResolvedReference
			base["artifact_digest"] = schema.Status.Source.Digest
			base["source_access"] = artifactAccessBinding(schema)
		}
		base["target"] = target
		base["coordination_digest"] = coordinationDigest
		base["exclude"] = fingerprint.NormalizeSet(exclude)
		base["connect_timeout"] = connectTimeout
		base["lock_timeout"] = lockTimeout
		base["severity"] = severity
	case operatorv1alpha1.OperationPlan:
		if pending := schema.Status.PendingObservation; pending != nil {
			if !pending.PlanRequired {
				return nil, fmt.Errorf("post-apply observation is required before scoped planning")
			}
			base["generation"] = pending.ApplyGeneration
			base["pending_observation"] = pending
			base["resolved_reference"] = pending.Source.ResolvedReference
			base["artifact_digest"] = pending.Source.Digest
			base["source_access"] = pending.Source
			base["coordination_digest"] = pending.CoordinationDigest
			base["target_identity"] = pending.Plan.TargetIdentityDigest
			base["target"] = pending.Target
			base["exclude"] = fingerprint.NormalizeSet(pending.Exclude)
			base["protected_tables"] = fingerprint.NormalizeSet(pending.ProtectedTables)
			base["dev"] = pending.Dev
			base["connect_timeout"] = pending.ConnectTimeout
		} else {
			if !schema.Status.Source.Verified || schema.Status.Target.CoordinationDigest == "" ||
				schema.Status.Target.IdentityDigest == "" || schema.Status.Target.DriftReportDigest == "" {
				return nil, fmt.Errorf("verified source and target observation are required before planning")
			}
			base["resolved_reference"] = schema.Status.Source.ResolvedReference
			base["artifact_digest"] = schema.Status.Source.Digest
			base["source_access"] = artifactAccessBinding(schema)
			base["coordination_digest"] = schema.Status.Target.CoordinationDigest
			base["target_identity"] = schema.Status.Target.IdentityDigest
			base["target"] = databaseTargetBinding(schema.Spec.Target)
			base["exclude"] = fingerprint.NormalizeSet(schema.Spec.Policy.Exclude)
			base["protected_tables"] = fingerprint.NormalizeSet(schema.Spec.Policy.ProtectedTables)
			base["dev"] = schema.Spec.Dev
			base["connect_timeout"] = schema.Spec.Execution.ConnectTimeout
		}
	case operatorv1alpha1.OperationApply:
		if schema.Status.Plan == nil {
			return nil, fmt.Errorf("current plan is required before apply")
		}
		base["plan_fingerprint"] = schema.Status.Plan.Fingerprint
		base["plan_content_digest"] = schema.Status.Plan.ContentDigest
		base["coordination_digest"] = schema.Status.Plan.CoordinationDigest
		base["approval"] = schema.Status.Plan.Approval
		base["source_access"] = artifactAccessBinding(schema)
	default:
		return nil, fmt.Errorf("unsupported operation %q", operation)
	}
	return base, nil
}

func artifactAccessBinding(schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.OCIArtifactAccessBinding {
	if schema == nil {
		return nil
	}
	binding := &operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: schema.Status.Source.ResolvedReference,
		Digest:            schema.Status.Source.Digest,
	}
	if schema.Spec.Desired.RegistryAuthFrom != nil {
		binding.RegistryAuthFrom = schema.Spec.Desired.RegistryAuthFrom.DeepCopy()
	}
	schema.Spec.Desired.Transport.DeepCopyInto(&binding.Transport)
	return binding
}

func currentPlanStatus(plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.CurrentPlanStatus {
	return &operatorv1alpha1.CurrentPlanStatus{
		Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint, ContentDigest: plan.Spec.ContentDigest,
		ArtifactDigest: plan.Spec.ArtifactDigest, CoordinationDigest: plan.Spec.CoordinationDigest,
		TargetIdentityDigest:   plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint: plan.Spec.ActualStateFingerprint, DesiredStateFingerprint: plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint: plan.Spec.PolicyFingerprint, VerificationPolicyUID: plan.Spec.VerificationPolicyUID,
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID:       plan.Spec.ExecutionBindingID,
		ControllerImage:          plan.Spec.ControllerImage,
		ControllerRevision:       plan.Spec.ControllerRevision,
		ControllerStateVersion:   plan.Spec.ControllerStateVersion,
		PtahVersion:              plan.Spec.PtahVersion, ExecutorImage: plan.Spec.ExecutorImage, RunnerImage: plan.Spec.RunnerImage,
		RunnerProtocolVersion: plan.Spec.RunnerProtocolVersion, Destructive: plan.Spec.Destructive,
		PrivilegeChanges: slices.Clone(plan.Spec.PrivilegeChanges),
		StatementCount:   plan.Spec.StatementCount, CreatedAt: plan.CreationTimestamp,
	}
}

// planPrivilegeChanges carries the kinds the plan decoder read into the API
// type. The decoder's vocabulary and the CRD enum are the same list, which a
// test of the generated CRD holds them to.
// dispatcherRecord reads the manager that built and dispatched a harvested
// Job, from its Pod template. A Job's template cannot change after it is
// created, and the claim's admission snapshot digested it before dispatch, so
// what is read is what that manager wrote. A Job that is absent, or whose
// template records no complete identity, yields no record: the record is
// audit evidence, and its absence must not stop the harvest it belongs to.
func dispatcherRecord(job *batchv1.Job) *operatorv1alpha1.ManagerRecord {
	controllerImage, controllerRevision, runnerImage := workload.ManagerIdentityOf(job)
	if len(controllerImage) > 512 || !controllerImagePattern.MatchString(controllerImage) ||
		controllerstate.ValidateRevision(controllerRevision) != nil ||
		len(runnerImage) > 512 || !controllerImagePattern.MatchString(runnerImage) {
		return nil
	}
	return &operatorv1alpha1.ManagerRecord{
		ControllerImage:    controllerImage,
		ControllerRevision: controllerRevision,
		RunnerImage:        runnerImage,
	}
}

// runnerProtocolRefusal reports a runner that refused its Job because the Job
// was built for another runner protocol: a runner of this protocol that
// answered in its own frame, or a runner of another protocol that answered in
// the one document every protocol writes and reads the same way. Either way
// the runner started no executor, and the cause is the runner image the
// installation names rather than anything the resource asked for, so it is
// reported as that rather than as a failed operation.
func runnerProtocolRefusal(result runner.Result, parseErr error) error {
	if mismatch := (*runner.ProtocolMismatchError)(nil); errors.As(parseErr, &mismatch) {
		return mismatch
	}
	if parseErr == nil && result.Error != nil && result.Error.Code == runner.CodeRunnerProtocolMismatch {
		return fmt.Errorf("%w: %s", runner.ErrRunnerProtocolMismatch, bounded(result.Error.Message, 512))
	}
	return nil
}

// planReadingMatches decodes the stored plan bytes with this manager's
// classifier and refuses when the result differs from what the plan records:
// its dialect, whether it is destructive, which privileges it changes and how
// many statements it holds.
func planReadingMatches(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan, content []byte) error {
	decoded, err := dataplane.DecodePlan(content, string(schema.Spec.Target.Engine))
	if err != nil {
		return fmt.Errorf("this manager cannot read the stored plan: %w", err)
	}
	recorded := make([]string, 0, len(plan.Spec.PrivilegeChanges))
	for _, kind := range plan.Spec.PrivilegeChanges {
		recorded = append(recorded, string(kind))
	}
	switch {
	case decoded.Dialect != plan.Spec.Dialect:
		return fmt.Errorf("this manager reads the plan dialect as %q; the plan records %q", decoded.Dialect, plan.Spec.Dialect)
	case decoded.Destructive != plan.Spec.Destructive:
		return fmt.Errorf("this manager reads the plan as destructive=%t; the plan records destructive=%t",
			decoded.Destructive, plan.Spec.Destructive)
	case !slices.Equal(fingerprint.NormalizeSet(decoded.PrivilegeChanges), fingerprint.NormalizeSet(recorded)):
		return fmt.Errorf("this manager reads privilege changes %v; the plan records %v",
			fingerprint.NormalizeSet(decoded.PrivilegeChanges), fingerprint.NormalizeSet(recorded))
	case int64(len(decoded.Statements)) != int64(plan.Spec.StatementCount):
		return fmt.Errorf("this manager reads %d statements; the plan records %d", len(decoded.Statements), plan.Spec.StatementCount)
	}
	return nil
}

func planPrivilegeChanges(plan dataplane.PlanFile) []operatorv1alpha1.PrivilegeChange {
	if len(plan.PrivilegeChanges) == 0 {
		return nil
	}
	kinds := make([]operatorv1alpha1.PrivilegeChange, 0, len(plan.PrivilegeChanges))
	for _, kind := range plan.PrivilegeChanges {
		kinds = append(kinds, operatorv1alpha1.PrivilegeChange(kind))
	}
	return kinds
}

func appliedStatusFor(plan operatorv1alpha1.CurrentPlanStatus, now metav1.Time) *operatorv1alpha1.AppliedStatus {
	// The reference comes from the same snapshot as everything else here.
	// Reading it off status.plan instead would name whichever plan the
	// controller has reached by now, which is not the plan this record is about.
	return &operatorv1alpha1.AppliedStatus{
		ArtifactDigest:     plan.ArtifactDigest,
		PlanRef:            operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		PlanFingerprint:    plan.Fingerprint,
		CoordinationDigest: plan.CoordinationDigest, TargetIdentityDigest: plan.TargetIdentityDigest,
		ExecutionBindingID: plan.ExecutionBindingID,
		ControllerImage:    plan.ControllerImage,
		ControllerRevision: plan.ControllerRevision, ControllerStateVersion: plan.ControllerStateVersion,
		PtahVersion: plan.PtahVersion, ExecutorImage: plan.ExecutorImage, RunnerImage: plan.RunnerImage,
		RunnerProtocolVersion: plan.RunnerProtocolVersion, CompletedAt: now,
	}
}

func pendingMatchesCurrentSchema(schema *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus) bool {
	if schema == nil || pending == nil || pending.ApplyGeneration != schema.Generation || !schema.Status.Source.Verified ||
		schema.Status.Source.Digest != pending.Source.Digest || schema.Status.Source.ResolvedReference != pending.Source.ResolvedReference ||
		schema.Status.Source.VerificationPolicyUID != pending.Plan.VerificationPolicyUID ||
		schema.Status.Source.VerificationPolicyDigest != pending.Plan.VerificationPolicyDigest ||
		!reflect.DeepEqual(databaseTargetBinding(schema.Spec.Target), pending.Target) ||
		!reflect.DeepEqual(artifactAccessBinding(schema), pending.Source.DeepCopy()) ||
		!reflect.DeepEqual(schema.Spec.Dev, pending.Dev) ||
		!reflect.DeepEqual(fingerprint.NormalizeSet(schema.Spec.Policy.Exclude), fingerprint.NormalizeSet(pending.Exclude)) ||
		!reflect.DeepEqual(fingerprint.NormalizeSet(schema.Spec.Policy.ProtectedTables),
			fingerprint.NormalizeSet(pending.ProtectedTables)) ||
		schema.Spec.Policy.DriftSeverity != pending.DriftSeverity ||
		schema.Spec.Execution.ConnectTimeout != pending.ConnectTimeout || schema.Spec.Policy.LockTimeout != pending.LockTimeout {
		return false
	}
	coordinationDigest, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil || coordinationDigest != pending.CoordinationDigest {
		return false
	}
	policyDigest, err := policyFingerprint(schema)
	return err == nil && policyDigest == pending.Plan.PolicyFingerprint
}

func planDriftSummary(plan dataplane.PlanFile) (int32, string) {
	const maxInt32 = int32(^uint32(0) >> 1)
	count := int32(len(plan.Statements))
	if len(plan.Statements) > int(maxInt32) {
		count = maxInt32
	}
	rank := map[string]int{"safe": 1, "info": 2, "warning": 3, "error": 4, "destructive": 5}
	severity := "safe"
	for _, statement := range plan.Statements {
		candidate := strings.ToLower(statement.Severity)
		if rank[candidate] > rank[severity] {
			severity = candidate
		}
	}
	return count, severity
}

func approvalMatches(approval *operatorv1alpha1.PtahSchemaApproval, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) bool {
	if plan == nil || plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion {
		return false
	}
	return approvalMatchesPlanStatus(approval, schema, currentPlanStatus(plan))
}

// approvalMatchesPlanStatus reports whether an approval names exactly this
// schema's current plan: the schema by name and UID, the plan by name and UID,
// and the plan by its fingerprint. The fingerprint binds every input the plan
// was decided from and a plan is immutable, so nothing else about the plan is
// compared here: a plan computed under another artifact, state, policy or
// execution binding is another plan, with another fingerprint and another
// UID, and an approval that named this one does not name it.
func approvalMatchesPlanStatus(
	approval *operatorv1alpha1.PtahSchemaApproval,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.CurrentPlanStatus,
) bool {
	return approval != nil && schema != nil && plan != nil &&
		approval.Spec.SchemaRef.Name == schema.Name && approval.Spec.SchemaRef.UID == schema.UID &&
		approval.Spec.PlanRef.Name == plan.Name && approval.Spec.PlanRef.UID == plan.UID &&
		approval.Spec.PlanFingerprint != "" && approval.Spec.PlanFingerprint == plan.Fingerprint &&
		!approval.Spec.ApprovedAt.IsZero() && approval.Spec.Approver.Username != "" && approval.Spec.MutationRequestUID != ""
}

func recordedApprovalMatches(recorded *operatorv1alpha1.ConsumedApprovalStatus, approval *operatorv1alpha1.PtahSchemaApproval) bool {
	return recorded.Name == approval.Name && recorded.UID == approval.UID &&
		recorded.ApprovedAt.Time.Equal(approval.Spec.ApprovedAt.Time) && reflect.DeepEqual(recorded.Approver, approval.Spec.Approver)
}

func planRequiresApproval(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) bool {
	return schema.Spec.Policy.Apply != operatorv1alpha1.ApplyPolicyAlways || plan.Spec.Destructive ||
		len(plan.Spec.PrivilegeChanges) > 0
}

// privilegesRequireApproval reports whether the privilege class is what stands
// between this plan and an unattended apply. Under any other policy the plan
// waits for a person anyway, and the reason says that instead.
func privilegesRequireApproval(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) bool {
	return schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyAlways && len(plan.Spec.PrivilegeChanges) > 0
}

// privilegeApprovalMessage names the kinds of authority the plan changes and
// nothing the statements say: no object, no role, no text.
func privilegeApprovalMessage(plan *operatorv1alpha1.PtahSchemaPlan) string {
	return "The plan changes privileges (" + privilegeKindList(plan.Spec.PrivilegeChanges) + "), which apply policy " +
		"Always applies only with an approval bound to this plan; read them with kubectl ptah plan"
}

func privilegeKindList(changes []operatorv1alpha1.PrivilegeChange) string {
	kinds := make([]string, 0, len(changes))
	for _, kind := range changes {
		kinds = append(kinds, string(kind))
	}
	return strings.Join(kinds, ", ")
}

func setPlanPolicyStatus(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) {
	switch {
	case plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive:
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonDestructiveChangesDisabled, "Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, "+
			"then either narrow the desired schema or set spec.policy.allowDestructive")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyBlocked, "Plan is blocked by destructive-change policy")
	case schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever:
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled, "Policy records plans but does not apply them")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled, "Plan is ready but apply is disabled")
	case privilegesRequireApproval(schema, plan):
		schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonPrivilegeChanges, privilegeApprovalMessage(plan))
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonAwaitingApproval, "Plan is waiting for approval")
	case planRequiresApproval(schema, plan):
		schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady, "An exact immutable plan is ready for approval")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonAwaitingApproval, "Plan is waiting for approval")
	default:
		schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonNotRequired, "Policy permits this plan without a separate approval: it destroys nothing and changes no privilege")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyPending, "The exact plan is ready to apply automatically")
	}
}

// PolicyFingerprint is the policy binding a plan records, exported for the
// end-to-end fixture that stands up a plan this controller must accept. A
// second spelling of it there would be a fixture that agrees with nothing.
func PolicyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	return policyFingerprint(schema)
}

func policyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	return fingerprint.DigestCanonicalJSON(struct {
		Engine           operatorv1alpha1.DatabaseEngine `json:"engine"`
		AllowDestructive bool                            `json:"allow_destructive"`
		DriftSeverity    string                          `json:"drift_severity"`
		Exclude          []string                        `json:"exclude"`
		ProtectedTables  []string                        `json:"protected_tables"`
		LockTimeout      string                          `json:"lock_timeout"`
		TransactionMode  string                          `json:"transaction_mode"`
		ConnectTimeout   string                          `json:"connect_timeout"`
	}{
		Engine: schema.Spec.Target.Engine, AllowDestructive: schema.Spec.Policy.AllowDestructive,
		DriftSeverity: schema.Spec.Policy.DriftSeverity,
		Exclude:       fingerprint.NormalizeSet(schema.Spec.Policy.Exclude),
		// The fence is part of the policy a published plan was computed under:
		// editing it makes a pending plan and its approval stale, which is the
		// point. A plan computed without a fence must not execute under one.
		ProtectedTables: fingerprint.NormalizeSet(schema.Spec.Policy.ProtectedTables),
		LockTimeout:     schema.Spec.Policy.LockTimeout.Duration.String(),
		TransactionMode: schema.Spec.Policy.TransactionMode, ConnectTimeout: schema.Spec.Execution.ConnectTimeout.Duration.String(),
	})
}

func interval(schema *operatorv1alpha1.PtahSchema) time.Duration {
	if schema.Spec.Interval.Duration > 0 {
		return schema.Spec.Interval.Duration
	}
	return defaultInterval
}

func failureRetry(schema *operatorv1alpha1.PtahSchema) time.Duration {
	if schema.Spec.Execution.FailureRetryInterval.Duration > 0 {
		return schema.Spec.Execution.FailureRetryInterval.Duration
	}
	return defaultFailureRetry
}

func leaseDuration(schema *operatorv1alpha1.PtahSchema) time.Duration {
	return applyWindow(schema) + time.Minute
}

// applyWindow is how long an Apply is authorized for: the window the claim
// stamps into dispatchNotAfter, the child's own context deadline, and the Job's
// deadline. The Lease above outlives it by a minute, so nothing can still be
// running when the realm has moved on. A schema Apply takes none of the grace a
// migration Apply Job does; workload.JobDeadlineGrace says why.
func applyWindow(schema *operatorv1alpha1.PtahSchema) time.Duration {
	deadline := schema.Spec.Execution.ActiveDeadlineSeconds
	if deadline <= 0 {
		deadline = 900
	}
	return time.Duration(deadline) * time.Second
}

func due(next *metav1.Time, now time.Time) bool { return next == nil || !now.Before(next.Time) }

func until(next *metav1.Time, now time.Time) time.Duration {
	if next == nil || !now.Before(next.Time) {
		return 0
	}
	return next.Sub(now)
}

func requeueAtDeadline(next *metav1.Time, now time.Time) ctrl.Result {
	remaining := until(next, now)
	if remaining <= 0 {
		return ctrl.Result{RequeueAfter: dueRequeue}
	}
	return ctrl.Result{RequeueAfter: remaining}
}

func jobTerminal(job *batchv1.Job) bool {
	return conditionTrue(job.Status.Conditions, batchv1.JobComplete) || conditionTrue(job.Status.Conditions, batchv1.JobFailed)
}

func jobSucceeded(job *batchv1.Job) bool {
	return conditionTrue(job.Status.Conditions, batchv1.JobComplete)
}

func conditionTrue(conditions []batchv1.JobCondition, conditionType batchv1.JobConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func ownedByUID(references []metav1.OwnerReference, uid types.UID) bool {
	for _, reference := range references {
		if reference.UID == uid {
			return true
		}
	}
	return false
}

func exactControllerOwner(
	references []metav1.OwnerReference,
	apiVersion, kind, name string,
	uid types.UID,
) bool {
	if len(references) != 1 {
		return false
	}
	reference := references[0]
	return reference.APIVersion == apiVersion && reference.Kind == kind && reference.Name == name && reference.UID == uid &&
		reference.Controller != nil && *reference.Controller && reference.BlockOwnerDeletion != nil && *reference.BlockOwnerDeletion
}

// validateJobIntent holds a Job the controller read back to the Job the
// schema's active claim builds, by the rule the controller-write webhook
// applied when the Job was created.
func validateJobIntent(actual, expected *batchv1.Job, schema *operatorv1alpha1.PtahSchema) error {
	if schema == nil {
		return fmt.Errorf("no schema claims the Job")
	}
	return jobclaim.Match(actual, builtSchemaJobClaim(schema, schema.Status.ActiveOperation, expected))
}

// builtSchemaJobClaim is the claim operation makes on a Job the controller
// reads back, held to the Job the claim builds under the binding in force.
func builtSchemaJobClaim(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	expected *batchv1.Job,
) jobclaim.Claim {
	claim := jobclaim.SchemaOperation(schema, operation)
	claim.Binding = schema.Status.ExecutionBinding
	claim.Built, claim.Stored = expected, true
	return claim
}

func validatePodIntent(pod *corev1.Pod, job *batchv1.Job, snapshot *operatorv1alpha1.PodAdmissionSnapshot) error {
	return podintent.ValidateStoredPod(pod, job, snapshot)
}

func setCondition(schema *operatorv1alpha1.PtahSchema, conditionType string, status metav1.ConditionStatus, reason operatorv1alpha1.ConditionReason, message string) {
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: string(reason), Message: bounded(message, 1024),
		ObservedGeneration: schema.Generation, LastTransitionTime: metav1.Now(),
	})
}

func markSourceRefreshPending(schema *operatorv1alpha1.PtahSchema) {
	if schema.Status.Source.Digest == "" {
		return
	}
	setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionUnknown, operatorv1alpha1.ReasonRefreshing, "Refreshing the requested OCI reference; the retained digest remains historical evidence")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceRefreshPending, "Plan currentness is unknown until source refresh and read-only reconciliation complete")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceRefreshPending, "Convergence is unknown until source refresh and read-only reconciliation complete")
}

func markSourceRefreshFailed(schema *operatorv1alpha1.PtahSchema) {
	if schema.Status.Source.Digest == "" {
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionFalse, operatorv1alpha1.ReasonResolveFailed, "The requested OCI reference could not be resolved")
		setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSourceUnresolved, "No plan can be prepared until the requested OCI reference resolves")
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceResolutionUnknown, "Convergence cannot be determined without a resolved source")
		return
	}
	setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionUnknown, operatorv1alpha1.ReasonRefreshFailed, "The requested OCI reference could not be refreshed; the retained digest remains historical evidence")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceFreshnessUnknown, "Plan currentness is unknown because source freshness could not be established")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceFreshnessUnknown, "Convergence is unknown because source freshness could not be established")
}

func markSourceRefreshSuspended(schema *operatorv1alpha1.PtahSchema) {
	if schema.Status.Source.Digest == "" {
		return
	}
	setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionUnknown, operatorv1alpha1.ReasonRefreshSuspended, "Source refresh was suspended before dispatch; the retained digest remains historical evidence")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceFreshnessUnknown, "Plan currentness is unknown because source refresh was suspended")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonSourceFreshnessUnknown, "Convergence is unknown because source refresh was suspended")
}

func markExecutionBindingRefreshRequired(schema *operatorv1alpha1.PtahSchema) {
	schema.Status.Source.Verified = false
	schema.Status.Source.VerifiedAt = nil
	setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged, "The retained digest must be refreshed under the current execution binding")
	setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged, "The retained verification is historical evidence from the previous execution binding")
	setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged, "The retained target observation is historical evidence from the previous execution binding")
	setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged, "Managed drift must be observed and planned under the current execution binding")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged, "The previous plan is not valid under the current execution binding")
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged, "Any approval for the previous execution binding is stale")
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged, "Convergence must be verified under the current execution binding")
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged, "No Apply operation is authorized under the previous execution binding")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged, "A complete read-only reconciliation is required under the current execution binding")
}

func setFailure(schema *operatorv1alpha1.PtahSchema, reason operatorv1alpha1.ConditionReason, failure error) {
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, reason, bounded(failure.Error(), 1024))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "Reconciliation did not complete")
}

func clearFailure(schema *operatorv1alpha1.PtahSchema) {
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, operatorv1alpha1.ReasonSucceeded, "The latest operation completed")
}

func bounded(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func ptrTime(value metav1.Time) *metav1.Time { return &value }
func ptr[T any](value T) *T                  { return &value }

// unaccountedSchemaJobReason and retriedSchemaJobReason word what the verdict
// found, for a reader of the condition rather than for the decision.
func unaccountedSchemaJobReason(cause mutationlifecycle.JobCause) string {
	if cause == mutationlifecycle.CauseDisowned {
		return "dispatched Apply Job lost schema ownership"
	}
	return "dispatched Apply Job was replaced"
}

func retriedSchemaJobReason(cause mutationlifecycle.JobCause) string {
	if cause == mutationlifecycle.CauseDisowned {
		return "active Job is not owned by the schema UID"
	}
	return "active Job was replaced"
}

// reportPodAdmission writes the refusal condition for a schema whose Job has
// no Pod, or restores the in-progress condition once it has one. Ready is the
// condition it moves: the operation is still claimed and still in flight, and
// Ready=False with this reason is what tells the reader why nothing happens.
func (r *SchemaReconciler) reportPodAdmission(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) error {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return nil
	}
	change, err := judgePodAdmission(ctx, r.directReader(), job, r.now(),
		meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady))
	if err != nil || !change.changed {
		return err
	}
	before := schema.DeepCopy()
	if change.refused {
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonPodAdmissionRefused, change.message)
	} else {
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation.Type))
	}
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return err
	}
	if change.refused {
		r.event(schema, corev1.EventTypeWarning, "PodAdmissionRefused", "%s", change.message)
	}
	return nil
}
