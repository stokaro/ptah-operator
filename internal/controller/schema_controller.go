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
	"sort"
	"strconv"
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
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/schemaselector"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	activeOperationFinalizer = "operator.ptah.dev/active-operation"
	executorContainerName    = "ptah"
	approvalSchemaIndex      = "spec.schemaRef.name"
	schemaPolicyIndex        = "spec.desired.verificationPolicyFrom.name"
	defaultInterval          = 10 * time.Minute
	defaultFailureRetry      = 30 * time.Second
	terminalPodGrace         = 10 * time.Second
	jobCleanupTTLSeconds     = int32(300)
	maxLockContentionPoll    = 5 * time.Second
	statusPatchRequeue       = time.Millisecond
	applyTerminationGrace    = 30 * time.Second
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

// SchemaReconciler implements the Resolve -> Verify -> Observe -> Plan ->
// Approval -> Apply -> Observe convergence state machine.
type SchemaReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Logs      PodLogReader
	Jobs      JobBuilder
	Plans     planstore.Store
	Locks     *targetlock.Locker
	// LockNamespace is one shared coordination namespace for every managed
	// PtahSchema, including schemas that live in different namespaces.
	LockNamespace string
	Clock         func() time.Time
	Telemetry     telemetry.Observer
	// AdmissionOptions must match cluster-wide built-in Pod admission settings.
	// The exact values are copied into each durable operation snapshot.
	AdmissionOptions podintent.Options
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
				"requeue", result.Requeue,
				"requeueAfter", result.RequeueAfter,
			)
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
		if executionBindingChangeFenced(schema) {
			return r.executionBindingChanged(ctx, schema, schema.Status.Plan, fmt.Errorf("execution-binding invalidation is pending"))
		}
		if bindingErr := r.ensureCurrentStatusExecutionBinding(schema, schema.Status.Plan); bindingErr != nil {
			return r.executionBindingChanged(ctx, schema, schema.Status.Plan, bindingErr)
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
		return ctrl.Result{Requeue: true}, true, nil
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
		return ctrl.Result{Requeue: true}, true, nil
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
		return ctrl.Result{Requeue: true}, true, nil
	}
	if schema.Status.Plan == nil {
		return ctrl.Result{}, true, nil
	}
	before = schema.DeepCopy()
	schema.Status.Plan = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
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
func (r *SchemaReconciler) reconcileExecutionBinding(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, bool, error) {
	configured, err := r.configuredExecutionBinding()
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if executionBindingRetiredReadOnlyCleanupPending(schema) {
		result, err := r.cleanupRetiredExecutionBindingOperation(ctx, schema)
		return result, true, err
	}

	// A previously persisted admission fence must finish before another rollout
	// can advance the epoch. This keeps the retired plan identity available for
	// best-effort audit cleanup without making cleanup completeness a safety
	// requirement.
	if executionBindingChangeFenced(schema) {
		result, err := r.executionBindingChanged(
			ctx,
			schema,
			schema.Status.Plan,
			fmt.Errorf("execution-binding invalidation is pending"),
		)
		return result, true, err
	}

	current := schema.Status.ExecutionBinding
	if current != nil && validExecutionBindingID(current.Epoch) &&
		executionBindingComponentsEqual(current, configured) {
		if operation := schema.Status.ActiveOperation; operation != nil && operation.ExecutionBindingID != current.Epoch {
			result, err := r.executionBindingChanged(
				ctx,
				schema,
				schema.Status.Plan,
				fmt.Errorf("active %s operation belongs to an unprovable execution-binding epoch", operation.Type),
			)
			return result, true, err
		}
		return ctrl.Result{}, false, nil
	}

	operation := schema.Status.ActiveOperation
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply &&
		(operation.DispatchStarted || operation.JobUID != "") {
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
		return ctrl.Result{Requeue: true}, true, nil
	}

	result, err := r.executionBindingChanged(
		ctx,
		schema,
		schema.Status.Plan,
		fmt.Errorf("configured execution components changed"),
	)
	return result, true, err
}

func (r *SchemaReconciler) configuredExecutionBinding() (*operatorv1alpha1.ExecutionBindingStatus, error) {
	if r.Jobs == nil {
		return nil, fmt.Errorf("Job builder is not configured")
	}
	controllerImage, controllerRevision, controllerStateVersion, ptahVersion, executorImage, runnerImage, protocolVersion := r.Jobs.ExecutionBinding()
	if !controllerImagePattern.MatchString(controllerImage) ||
		controllerstate.ValidateRevision(controllerRevision) != nil || controllerStateVersion < 1 ||
		strings.TrimSpace(ptahVersion) == "" || strings.TrimSpace(ptahVersion) != ptahVersion ||
		strings.TrimSpace(executorImage) == "" || strings.TrimSpace(executorImage) != executorImage ||
		strings.TrimSpace(runnerImage) == "" || strings.TrimSpace(runnerImage) != runnerImage || protocolVersion < 1 {
		return nil, fmt.Errorf("Job builder execution binding is incomplete")
	}
	return &operatorv1alpha1.ExecutionBindingStatus{
		ControllerImage: controllerImage, ControllerRevision: controllerRevision, ControllerStateVersion: controllerStateVersion,
		PtahVersion: ptahVersion, ExecutorImage: executorImage, RunnerImage: runnerImage,
		RunnerProtocolVersion: protocolVersion,
	}, nil
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

func executionBindingComponentsEqual(
	left *operatorv1alpha1.ExecutionBindingStatus,
	right *operatorv1alpha1.ExecutionBindingStatus,
) bool {
	return left != nil && right != nil &&
		left.ControllerImage == right.ControllerImage &&
		left.ControllerRevision == right.ControllerRevision &&
		left.ControllerStateVersion == right.ControllerStateVersion &&
		left.PtahVersion == right.PtahVersion &&
		left.ExecutorImage == right.ExecutorImage &&
		left.RunnerImage == right.RunnerImage &&
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
	if pending.ApplyJobUID == "" {
		adopted, err := r.adoptRetiredPredecessorApplyJobUID(ctx, schema, pending)
		if err != nil {
			return false, err
		}
		if adopted {
			// UID adoption is a durable boundary. Re-read the schema before Pod
			// discovery so every later decision is bound to the committed Job UID.
			return true, nil
		}
		// An uncertain create with no exact late-committing predecessor Job is
		// protected by the immutable ObserveAfter horizon instead of Pod discovery.
		return false, nil
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
	cleanupPending, err := r.cleanupRetiredPredecessorApplyJob(ctx, schema, pending)
	if err != nil {
		return false, err
	}
	if cleanupPending {
		// A successful cleanup patch is a durable boundary of its own. Re-read
		// before claiming or consuming read-only proof, while the exact fenced
		// Apply identity is still the only operation eligible for this update.
		return true, nil
	}
	return false, nil
}

func (r *SchemaReconciler) adoptRetiredPredecessorApplyJobUID(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) (bool, error) {
	if !predecessorApplyUIDAdoptionPending(schema, pending) {
		return false, nil
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace,
		Name:      pending.ApplyJobName,
	}, job)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read late-committing predecessor Apply Job: %w", err)
	}
	if !retiredPredecessorApplyJobMatches(schema, pending, job) {
		return false, nil
	}
	before := schema.DeepCopy()
	pending.ApplyJobUID = job.UID
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return false, err
	}
	return true, nil
}

// cleanupRetiredPredecessorApplyJob schedules garbage collection for the exact
// Apply Job that crossed an execution-binding transition. It deliberately
// reads no executor result and accepts no Apply attribution: PendingObservation
// remains outcome-unknown until a fresh read-only proof completes.
func (r *SchemaReconciler) cleanupRetiredPredecessorApplyJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) (bool, error) {
	if !predecessorApplyCleanupPending(schema, pending) {
		return false, nil
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace,
		Name:      pending.ApplyJobName,
	}, job)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read retired predecessor Apply Job: %w", err)
	}
	if !retiredPredecessorApplyJobMatches(schema, pending, job) {
		return false, nil
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		return false, nil
	}
	if !jobTerminal(job) {
		// Do not let fresh proof clear PendingObservation in the short window
		// between the last terminal Pod and the Job controller's terminal
		// condition. The exact retired Job remains fenced until cleanup is
		// durably scheduled.
		return true, nil
	}
	if err := r.markJobHarvested(ctx, job); err != nil {
		return false, err
	}
	return true, nil
}

func predecessorApplyCleanupPending(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) bool {
	return predecessorApplyRetirementPending(schema, pending) && pending.ApplyJobUID != ""
}

func predecessorApplyUIDAdoptionPending(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) bool {
	return predecessorApplyRetirementPending(schema, pending) && pending.ApplyJobUID == ""
}

func predecessorApplyRetirementPending(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) bool {
	if schema == nil || pending == nil || schema.Status.ExecutionBinding == nil ||
		schema.Status.ActiveOperation != nil || schema.Status.Phase != operatorv1alpha1.PhasePending ||
		pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.PlanRequired ||
		pending.ApplyOperationID == "" || pending.ApplyJobName == "" ||
		!validExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		!validExecutionBindingID(pending.Plan.ExecutionBindingID) ||
		pending.Plan.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch ||
		!executionBindingCleanupPending(schema) {
		return false
	}
	planReady := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionPlanReady)
	return planReady != nil && planReady.Status == metav1.ConditionFalse &&
		planReady.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
}

func retiredPredecessorApplyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	job *batchv1.Job,
) bool {
	if !predecessorApplyRetirementPending(schema, pending) || job == nil || job.UID == "" ||
		job.Name != pending.ApplyJobName ||
		(pending.ApplyJobUID != "" && job.UID != pending.ApplyJobUID) ||
		!exactControllerOwner(
			job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(),
			"PtahSchema",
			schema.Name,
			schema.UID,
		) {
		return false
	}
	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(pending.ApplyOperationID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) ||
		!sha256DigestPattern.MatchString(pending.Plan.Fingerprint) ||
		!sha256DigestPattern.MatchString(pending.Plan.ContentDigest) ||
		strings.TrimSpace(pending.Plan.PtahVersion) == "" ||
		pending.Plan.PtahVersion != strings.TrimSpace(pending.Plan.PtahVersion) {
		return false
	}
	inputFingerprint := job.Annotations[workload.AnnotationInputFingerprint]
	snapshotDigest := job.Annotations[workload.AnnotationAdmissionSnapshotDigest]
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             pending.ApplyOperationID,
		workload.AnnotationInputFingerprint:        inputFingerprint,
		workload.AnnotationPtahVersion:             pending.Plan.PtahVersion,
		workload.AnnotationExecutionBindingID:      pending.Plan.ExecutionBindingID,
		workload.AnnotationPlanFingerprint:         pending.Plan.Fingerprint,
		workload.AnnotationPlanContentDigest:       pending.Plan.ContentDigest,
		workload.AnnotationAdmissionSnapshotDigest: snapshotDigest,
	}
	currentFormat := len(job.Annotations) == 10
	switch {
	case len(job.Annotations) == 7:
		// The supported predecessor did not persist controller provenance in
		// its Job envelope. Keep that compatibility path exact and separate.
	case currentFormat:
		if pending.Plan.Name == "" || pending.Plan.UID == "" ||
			pending.AdmissionSnapshot == nil ||
			podintent.ValidateSnapshot(pending.AdmissionSnapshot) != nil ||
			pending.AdmissionSnapshot.Digest != snapshotDigest ||
			!controllerImagePattern.MatchString(pending.Plan.ControllerImage) ||
			controllerstate.ValidateRevision(pending.Plan.ControllerRevision) != nil ||
			pending.Plan.ControllerStateVersion < 1 {
			return false
		}
		wantAnnotations[workload.AnnotationControllerImage] = pending.Plan.ControllerImage
		wantAnnotations[workload.AnnotationControllerRevision] = pending.Plan.ControllerRevision
		wantAnnotations[workload.AnnotationControllerStateVersion] = strconv.FormatInt(
			int64(pending.Plan.ControllerStateVersion),
			10,
		)
	default:
		return false
	}
	if !sha256DigestPattern.MatchString(inputFingerprint) ||
		!sha256DigestPattern.MatchString(snapshotDigest) ||
		!reflect.DeepEqual(job.Annotations, wantAnnotations) ||
		!reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return false
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedJobSelector(normalized); err != nil {
		return false
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return false
	}
	if currentFormat {
		templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
		return err == nil && templateDigest == pending.AdmissionSnapshot.TemplateDigest
	}
	return true
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
		dispatchedApplyUnknown := operation.Type == operatorv1alpha1.OperationApply &&
			(operation.DispatchStarted || operation.JobUID != "") &&
			(apierrors.IsNotFound(err) || err == nil && (operation.JobUID != "" && operation.JobUID != job.UID || !ownedByUID(job.OwnerReferences, schema.UID)))
		if err == nil && !dispatchedApplyUnknown && !jobTerminal(job) {
			if operationNeedsTargetLock(schema) {
				acquired, requeue, lockErr := r.acquireOperationLock(ctx, schema)
				if lockErr != nil {
					return ctrl.Result{}, lockErr
				}
				if !acquired {
					return ctrl.Result{RequeueAfter: requeue}, nil
				}
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
		if operation.Type == operatorv1alpha1.OperationApply && !dispatchedApplyUnknown {
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
		before := schema.DeepCopy()
		if (operation.Type == operatorv1alpha1.OperationApply ||
			operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil) &&
			operation.LeaseEpoch != "" {
			if err := stageOperationLockRelease(schema, operation); err != nil {
				return ctrl.Result{}, err
			}
		}
		schema.Status.ActiveOperation = nil
		if err := r.patchStatus(ctx, before, schema); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if schema.Status.PendingLockRelease != nil {
			if err := r.completePendingLockRelease(ctx, schema); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
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
	if schema.Status.PendingObservation != nil &&
		(operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) {
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
	if operation.Type == operatorv1alpha1.OperationApply && !operation.DispatchStarted && operation.JobUID == "" &&
		schema.Status.Plan != nil {
		if bindingErr := r.ensureCurrentStatusExecutionBinding(schema, schema.Status.Plan); bindingErr != nil {
			return r.executionBindingChanged(ctx, schema, schema.Status.Plan, bindingErr)
		}
	}
	if !schema.Spec.Suspend && schema.Status.Phase == operatorv1alpha1.PhaseFailed {
		failedRetryTime := r.now()
		if !due(schema.Status.NextReconciliationTime, failedRetryTime) {
			result := requeueAtDeadline(schema.Status.NextReconciliationTime, failedRetryTime)
			if (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) &&
				schema.Status.PendingObservation != nil {
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
		if operation.Type == operatorv1alpha1.OperationApply && (operation.DispatchStarted || operation.JobUID != "") {
			acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("dispatched Apply Job is missing and will not be recreated"))
		}
		if schema.Spec.Suspend {
			return r.suspendUndispatchedOperation(ctx, schema)
		}
		current, currentErr := r.operationInputFingerprint(schema, operation.Type)
		if currentErr != nil || current != operation.InputFingerprint {
			if currentErr == nil {
				currentErr = fmt.Errorf("operation inputs changed after the claim")
			}
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.applyBecameStale(ctx, schema, currentErr)
			}
			return r.discardStaleOperation(ctx, schema, currentErr)
		}
		var plan *operatorv1alpha1.PtahSchemaPlan
		if operation.Type == operatorv1alpha1.OperationApply {
			plan, err = r.currentPlan(ctx, schema)
			if err != nil {
				return r.applyBecameStale(ctx, schema, err)
			}
			// An old execution binding must lose its authorization without
			// waiting for an unrelated holder of the database Lease. Releasing
			// this claim remains safe when it has not acquired the Lease yet.
			if err := r.ensureCurrentExecutionBinding(schema, plan); err != nil {
				return r.executionBindingChanged(ctx, schema, schema.Status.Plan, err)
			}
		}
		if operationNeedsTargetLock(schema) {
			acquired, requeue, lockErr := r.acquireOperationLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
		}
		if operation.Type == operatorv1alpha1.OperationVerify {
			binding, bindingErr := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
			if bindingErr != nil || binding.UID != operation.VerificationPolicyUID || binding.Digest != operation.VerificationPolicyDigest {
				if bindingErr == nil {
					bindingErr = fmt.Errorf("verification policy object changed after the operation claim")
				}
				return r.verificationPolicyChanged(ctx, schema, bindingErr)
			}
		}
		if operation.Type == operatorv1alpha1.OperationApply {
			if _, err := r.Plans.Load(ctx, plan); err != nil {
				return r.applyBecameStale(ctx, schema, fmt.Errorf("verify plan storage: %w", err))
			}
			policyBinding, err := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
			if err != nil || policyBinding.UID != plan.Spec.VerificationPolicyUID || policyBinding.Digest != plan.Spec.VerificationPolicyDigest {
				if err == nil {
					err = fmt.Errorf("verification policy object changed")
				}
				return r.verificationPolicyChanged(ctx, schema, err)
			}
			if planRequiresApproval(schema, plan) {
				valid, err := r.ensureCurrentApproval(ctx, schema, plan, false)
				if err != nil {
					return ctrl.Result{}, err
				}
				if !valid {
					return r.approvalBecameInvalid(ctx, schema)
				}
			}
		}
		if r.Jobs == nil {
			return ctrl.Result{}, fmt.Errorf("Job builder is not configured")
		}
		if operation.JobUID != "" {
			if !isReadOnlyOperation(operation) {
				return ctrl.Result{}, fmt.Errorf("missing %s Job has an unsupported persisted UID boundary", operation.Type)
			}
			pods, podsErr := r.podsOwnedByJob(ctx, schema.Namespace, operation.JobName, operation.JobUID)
			if podsErr != nil {
				return ctrl.Result{}, podsErr
			}
			for _, pod := range pods {
				if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
					// Job deletion does not prove that an already-created Pod has
					// stopped. Poll the exact owner UID until every old attempt is
					// terminal or gone before authorizing a new read-only dispatch.
					return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
				}
			}
			// A persisted UID proves that this attempt already crossed its
			// dispatch boundary. Job admission only permits CREATE while the
			// active claim has no UID, so durably advance to a fresh attempt and
			// deterministic name before a later reconciliation can recreate the
			// read-only work. Apply is handled above and is never recreated.
			return r.retryOperation(
				ctx,
				schema,
				nil,
				fmt.Errorf("%s Job %q with persisted UID %q is missing", operation.Type, operation.JobName, operation.JobUID),
			)
		}
		if operation.AdmissionSnapshot != nil {
			if snapshotErr := podintent.ValidateSnapshot(operation.AdmissionSnapshot); snapshotErr != nil {
				return r.operationFailure(ctx, schema, fmt.Errorf("validate persisted Pod admission snapshot: %w", snapshotErr))
			}
		}
		job, err = r.Jobs.Build(schema, *operation, plan)
		if err != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.applyBecameStale(ctx, schema, fmt.Errorf("build Apply Job: %w", err))
			}
			return r.operationFailure(ctx, schema, fmt.Errorf("build %s Job: %w", operation.Type, err))
		}
		if job.Namespace != schema.Namespace || job.Name != operation.JobName {
			return ctrl.Result{}, fmt.Errorf("Job builder returned an object outside the operation claim")
		}
		if operation.AdmissionSnapshot != nil {
			templateDigest, digestErr := podintent.DigestTemplate(&job.Spec.Template)
			if digestErr != nil {
				failure := fmt.Errorf("validate rebuilt Job Pod template: %w", digestErr)
				if operation.Type != operatorv1alpha1.OperationApply {
					return r.discardStaleOperation(ctx, schema, failure)
				}
				return r.operationFailure(ctx, schema, failure)
			}
			if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
				failure := fmt.Errorf("rebuilt Job Pod template differs from the persisted admission snapshot")
				if operation.Type != operatorv1alpha1.OperationApply {
					return r.discardStaleOperation(ctx, schema, failure)
				}
				return r.operationFailure(ctx, schema, failure)
			}
		}
		if operation.AdmissionSnapshot == nil {
			snapshot, snapshotErr := podintent.Resolve(
				ctx, r.directReader(), schema.Namespace, &job.Spec.Template, r.AdmissionOptions,
			)
			if snapshotErr != nil {
				return r.operationFailure(ctx, schema, fmt.Errorf("resolve Pod admission snapshot: %w", snapshotErr))
			}
			before := schema.DeepCopy()
			schema.Status.ActiveOperation.AdmissionSnapshot = snapshot
			if err := r.patchStatus(ctx, before, schema); err != nil {
				return ctrl.Result{}, err
			}
			// The snapshot is a separate durable boundary. In particular, an
			// Apply reconciliation must persist it before DispatchStarted and
			// before the one permitted Job create attempt.
			return ctrl.Result{Requeue: true}, nil
		}
		expectedJob := job.DeepCopy()
		if operationNeedsTargetLock(schema) && !operation.DispatchStarted {
			before := schema.DeepCopy()
			schema.Status.ActiveOperation.DispatchStarted = true
			if err := r.patchStatus(ctx, before, schema); err != nil {
				return ctrl.Result{}, err
			}
			operation = schema.Status.ActiveOperation
		}
		if operation.Type == operatorv1alpha1.OperationApply {
			if planRequiresApproval(schema, plan) {
				valid, err := r.ensureCurrentApproval(ctx, schema, plan, true)
				if err != nil {
					return ctrl.Result{}, err
				}
				if !valid {
					return r.approvalBecameInvalid(ctx, schema)
				}
			}
		}
		if err := r.Client.Create(ctx, job); err != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("Apply Job create result is uncertain: %w", err))
			}
			if apierrors.IsAlreadyExists(err) {
				return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job name was occupied during dispatch"))
			}
			return ctrl.Result{}, fmt.Errorf("create %s Job: %w", operation.Type, err)
		}
		if err := r.directReader().Get(ctx, key, job); err != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("cannot confirm dispatched Apply Job: %w", err))
			}
			return ctrl.Result{}, fmt.Errorf("read created %s Job: %w", operation.Type, err)
		}
		if err := validateJobIntent(job, expectedJob, schema); err != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUncertainApply(ctx, schema, nil, fmt.Errorf("dispatched Apply Job failed immutable intent validation: %w", err))
			}
			return r.retryOperation(ctx, schema, nil, fmt.Errorf("created Job failed immutable intent validation: %w", err))
		}
		before := schema.DeepCopy()
		schema.Status.ActiveOperation.JobUID = job.UID
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		r.event(schema, corev1.EventTypeNormal, "OperationStarted", "%s Job %s started", operation.Type, job.Name)
		if operation.Type == operatorv1alpha1.OperationApply && r.Telemetry != nil {
			r.Telemetry.ObserveApply(telemetry.ApplyStarted)
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read active Job: %w", err)
	}
	if operation.JobUID != "" && operation.JobUID != job.UID {
		if operation.Type == operatorv1alpha1.OperationApply {
			return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("dispatched Apply Job was replaced"))
		}
		return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job was replaced"))
	}
	if !exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema", schema.Name, schema.UID) {
		if operation.Type == operatorv1alpha1.OperationApply {
			return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("dispatched Apply Job lost schema ownership"))
		}
		return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job is not owned by the schema UID"))
	}
	currentInputs, inputErr := r.operationInputFingerprint(schema, operation.Type)
	if inputErr == nil && currentInputs == operation.InputFingerprint {
		expectedJob, expectedErr := r.expectedJob(ctx, schema, operation)
		if expectedErr != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("rebuild immutable Apply Job intent: %w", expectedErr))
			}
			return r.retryOperation(ctx, schema, nil, fmt.Errorf("rebuild immutable Job intent: %w", expectedErr))
		}
		if intentErr := validateJobIntent(job, expectedJob, schema); intentErr != nil {
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUnknownRunningApply(ctx, schema, fmt.Errorf("dispatched Apply Job intent changed: %w", intentErr))
			}
			return r.retryOperation(ctx, schema, nil, fmt.Errorf("active Job intent changed: %w", intentErr))
		}
	}
	if operation.JobUID == "" {
		before := schema.DeepCopy()
		schema.Status.ActiveOperation.JobUID = job.UID
		if operation.Type == operatorv1alpha1.OperationApply {
			schema.Status.ActiveOperation.DispatchStarted = true
		}
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		operation = schema.Status.ActiveOperation
	}
	if operationNeedsTargetLock(schema) {
		acquired, requeue, lockErr := r.acquireOperationLock(ctx, schema)
		if lockErr != nil {
			return ctrl.Result{}, lockErr
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}
	if !jobTerminal(job) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	current, currentErr := r.operationInputFingerprint(schema, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = fmt.Errorf("operation inputs changed while the Job was running")
		}
		if operation.Type == operatorv1alpha1.OperationApply {
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
		if errors.Is(err, errTerminalPodPending) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if errors.Is(err, errTerminalPodMultiplicity) || errors.Is(err, errTerminalPodIntent) {
			failure := err
			if errors.Is(err, errTerminalPodMultiplicity) {
				failure = fmt.Errorf("Job produced multiple executor Pods")
			}
			if operation.Type == operatorv1alpha1.OperationApply {
				return r.finishUncertainApplyWithEvidence(ctx, schema, job, failure, evidence.PodUIDs, evidence.PodCount, true)
			}
			return r.retryOperation(ctx, schema, job, failure)
		}
		return ctrl.Result{}, err
	}
	result, parseErr := runner.ParseResultFor(evidence.Logs, runnerOperation(operation.Type), operation.ID)
	if parseErr != nil || !jobSucceeded(job) {
		if operation.Type == operatorv1alpha1.OperationApply {
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
		if operation.Type == operatorv1alpha1.OperationApply || result.Uncertain {
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
	if operation.Type == operatorv1alpha1.OperationApply {
		if schema.Status.Plan == nil || result.CoordinationDigest != schema.Status.Plan.CoordinationDigest ||
			result.TargetIdentityDigest != schema.Status.Plan.TargetIdentityDigest {
			return r.finishUncertainApplyWithEvidence(
				ctx, schema, job, fmt.Errorf("apply target identity changed after approval"),
				evidence.PodUIDs, evidence.PodCount, !evidence.Trusted,
			)
		}
	}
	return r.consumeResult(ctx, schema, job, result, evidence.PodUIDs, evidence.PodCount)
}

func (r *SchemaReconciler) recoverLeaseContinuity(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil || !operation.LeaseContinuityLost || !operationNeedsTargetLock(schema) {
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
	if operation.Type == operatorv1alpha1.OperationApply {
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
	if operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil {
		release, err := targetLockReleaseForOperation(operation)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingLockRelease = release
	}
	schema.Status.ActiveOperation = nil
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
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{Requeue: true}, nil
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
	schema.Status.ActiveOperation = nil
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
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeWarning, "ArtifactVerificationRefused", "%s", bounded(message, 512))
	return ctrl.Result{RequeueAfter: interval(schema)}, nil
}

func (r *SchemaReconciler) suspendUndispatchedOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	before := schema.DeepCopy()
	if operation != nil && (operation.Type == operatorv1alpha1.OperationApply ||
		operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil) &&
		operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Phase = operatorv1alpha1.PhaseSuspended
	schema.Status.ObservedGeneration = schema.Generation
	if operation != nil && operation.Type == operatorv1alpha1.OperationResolve {
		markSourceRefreshSuspended(schema)
	}
	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, operatorv1alpha1.ReasonRequested, "New database operations are suspended")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
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
	if err := r.markJobHarvested(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	var observedDrift *bool
	var completedPending *operatorv1alpha1.PendingObservationStatus
	var completedProofPolicyErr error
	var completedProofExecutionBindingErr error
	now := metav1.NewTime(r.now())
	schema.Status.LastAttemptTime = &now
	if operation != nil && (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) &&
		schema.Status.PendingObservation != nil {
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
			expectedCoordination, err = fingerprint.DatabaseCoordinationDigest(string(engine), schema.Spec.Target.CoordinationKey)
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
			DriftFindingsTruncated: result.DriftFindingsTruncated,
		}
		if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.Plan = nil
		setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionTrue, operatorv1alpha1.ReasonObserved, "Database schema was observed")
		setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, operatorv1alpha1.ReasonScopedPlanPending, "Raw drift was recorded; the authoritative managed scope is being planned")
		setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonScopedPlanPending, "Convergence is unknown until scoped planning completes")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonScopedPlanPending, "A read-only scoped plan is required")
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
			planDocument := []byte(result.Stdout)
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
				schema.Status.Plan = currentPlanStatus(published, now)
				setPlanPolicyStatus(schema, published)
				next := metav1.NewTime(now.Add(interval(schema)))
				schema.Status.NextReconciliationTime = &next
				setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, operatorv1alpha1.ReasonPublished, "Exact plan bytes are stored in immutable chunks")
			}
		}
		if pending != nil {
			if completedProofExecutionBindingErr != nil {
				// Proof and historical Apply attribution are committed first. Preserve
				// the retired plan identity in a non-approvable state so a CREATE that
				// passed admission before the old Apply claim, but committed late, can
				// still be found and retired in the next reconciliation. ActiveOperation
				// is cleared in the same status patch below, so this completed proof is
				// never confused with an unresolved dispatched Apply.
				schema.Status.Plan = pending.Plan.DeepCopy()
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
			completedPending = pending.DeepCopy()
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

	if completedPending != nil {
		release, err := targetLockReleaseForPending(completedPending)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingLockRelease = release
	} else if operation != nil && operation.Type == operatorv1alpha1.OperationPlan {
		release, err := targetLockReleaseForOperation(operation)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingLockRelease = release
	}
	schema.Status.ActiveOperation = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		if observedDrift != nil {
			outcome := telemetry.DriftInSync
			if *observedDrift {
				outcome = telemetry.DriftDetected
			}
			r.Telemetry.ObserveDrift(schema.Spec.Target.Engine, outcome)
		}
		if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
			r.Telemetry.ObserveApply(telemetry.ApplyCompleted)
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
	return ctrl.Result{Requeue: true}, nil
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
	if operation.Type == operatorv1alpha1.OperationApply {
		plan, err = r.currentPlan(ctx, schema)
		if err != nil {
			return nil, err
		}
	}
	return r.Jobs.Build(schema, *operation, plan)
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
		return r.executionBindingChanged(ctx, schema, schema.Status.Plan, err)
	}
	policyBinding, err := policy.ConfigMapBinding(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil || policyBinding.UID != plan.Spec.VerificationPolicyUID || policyBinding.Digest != plan.Spec.VerificationPolicyDigest {
		if err == nil {
			err = fmt.Errorf("verification policy object changed")
		}
		return r.verificationPolicyChanged(ctx, schema, err)
	}
	if plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive {
		return r.waitBlocked(ctx, schema, operatorv1alpha1.ReasonDestructiveChangesDisabled, "Plan contains destructive changes and policy disallows them")
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
			setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonWaiting, "Create an approval bound to the current plan fingerprint")
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
		return ctrl.Result{Requeue: true}, nil
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
		return ctrl.Result{Requeue: true}, nil
	}
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
	if operation == operatorv1alpha1.OperationApply || operation == operatorv1alpha1.OperationPlan ||
		operation == operatorv1alpha1.OperationObserve && schema.Status.PendingObservation != nil {
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
			active.ObservationSeverity = pending.DriftSeverity
			active.ObservationDev = pending.Dev.DeepCopy()
			active.ObservationConnectTimeout = pending.ConnectTimeout
			active.ObservationLockTimeout = pending.LockTimeout
		} else {
			coordinationDigest, digestErr := fingerprint.DatabaseCoordinationDigest(string(schema.Spec.Target.Engine), schema.Spec.Target.CoordinationKey)
			if digestErr != nil {
				return r.operationFailure(ctx, schema, fmt.Errorf("derive observation coordination digest: %w", digestErr))
			}
			active.CoordinationDigest = coordinationDigest
			target := databaseTargetBinding(schema.Spec.Target)
			active.Target = &target
			active.Source = artifactAccessBinding(schema)
			active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
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
		dispatchNotAfter := metav1.NewTime(active.StartedAt.Add(leaseDuration(schema) - time.Minute))
		active.DispatchNotAfter = &dispatchNotAfter
		executionNotAfter := dispatchNotAfter.DeepCopy()
		active.ExecutionNotAfter = executionNotAfter
		active.TerminationGracePeriodSeconds = int64(applyTerminationGrace / time.Second)
		target := databaseTargetBinding(schema.Spec.Target)
		active.Target = &target
		active.Source = artifactAccessBinding(schema)
		active.ObservationExclude = append([]string(nil), schema.Spec.Policy.Exclude...)
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
	if pending := schema.Status.PendingObservation; pending != nil &&
		(operation == operatorv1alpha1.OperationObserve || operation == operatorv1alpha1.OperationPlan) {
		schema.Status.ObservedGeneration = pending.ApplyGeneration
	}
	schema.Status.NextReconciliationTime = nil
	if (operation == operatorv1alpha1.OperationObserve || operation == operatorv1alpha1.OperationPlan) &&
		schema.Status.PendingObservation != nil {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = phaseFor(operation)
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
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) retryOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, failure error) (ctrl.Result, error) {
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
	if operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil {
		release, err := targetLockReleaseForOperation(operation)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.PendingLockRelease = release
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
	setFailure(schema, operatorv1alpha1.ReasonOperationFailed, failure)
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
		r.Telemetry.ObserveFailure(telemetry.StageForOperation(operation.Type), telemetry.FailureOperation)
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
	if err := r.consumeRecordedApprovalAtDispatch(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
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
	schema.Status.ActiveOperation = nil
	schema.Status.PendingObservation = pending
	if replacementBinding != nil {
		if !validExecutionBindingID(replacementBinding.Epoch) {
			return ctrl.Result{}, fmt.Errorf("replacement execution binding is invalid")
		}
		schema.Status.ExecutionBinding = replacementBinding.DeepCopy()
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhasePending
		schema.Status.NextReconciliationTime = nil
		markExecutionBindingRefreshRequired(schema)
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	}
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonOutcomeUnknown, "Apply outcome is uncertain; only read-only observation is permitted")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonOutcomeUnknown, "Database state must be observed before any next action")
	setFailure(schema, operatorv1alpha1.ReasonApplyOutcomeUnknown, failure)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveApply(telemetry.ApplyUncertain)
		r.Telemetry.ObserveFailure(telemetry.FailureStageApply, telemetry.FailureUncertain)
	}
	r.observeOperation(operation, telemetry.OperationUncertain)
	r.event(schema, corev1.EventTypeWarning, "ApplyOutcomeUnknown", "Apply outcome is uncertain; observing database state")
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) consumeRecordedApprovalAtDispatch(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
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
	return r.markApprovalConsumed(ctx, approval)
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
		AdmissionSnapshot: operation.AdmissionSnapshot.DeepCopy(),
		ApplyPodUIDs:      append([]types.UID(nil), podUIDs...), ApplyPodCount: podCount,
		ApplyGeneration: schema.Status.ObservedGeneration, ObserveAfter: observeAfter, Plan: plan, Target: target,
		CoordinationDigest:   operation.CoordinationDigest,
		Source:               *operation.Source.DeepCopy(),
		Dev:                  operation.ObservationDev.DeepCopy(),
		Exclude:              append([]string(nil), operation.ObservationExclude...),
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
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply && operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	if schema.Status.PendingObservation != nil {
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStale, bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStalePlan, "The plan became stale before apply")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		stage := telemetry.FailureStagePlan
		if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
			stage = telemetry.FailureStageApply
			r.Telemetry.ObserveApply(telemetry.ApplyStale)
		}
		r.Telemetry.ObserveFailure(stage, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) executionBindingChanged(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.CurrentPlanStatus,
	failure error,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	failureStage := telemetry.FailureStageController
	if operation != nil {
		failureStage = telemetry.StageForOperation(operation.Type)
	} else if plan != nil {
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
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply &&
		(operation.DispatchStarted || operation.JobUID != "") {
		configured, err := r.configuredExecutionBinding()
		if err != nil {
			return ctrl.Result{}, err
		}
		return r.finishUncertainApplyForExecutionBindingChange(
			ctx,
			schema,
			configured,
			fmt.Errorf("execution binding changed after Apply dispatch: %w", failure),
		)
	}
	if !executionBindingChangeFenced(schema) {
		configured, err := r.configuredExecutionBinding()
		if err != nil {
			return ctrl.Result{}, err
		}
		binding, err := newExecutionBinding(configured)
		if err != nil {
			return ctrl.Result{}, err
		}
		before := schema.DeepCopy()
		retainReadOnlyOperation := isReadOnlyOperation(operation)
		if operation != nil && !retainReadOnlyOperation &&
			(operation.Type == operatorv1alpha1.OperationApply ||
				operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil) &&
			operation.LeaseEpoch != "" {
			if err := stageOperationLockRelease(schema, operation); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Close the admission boundary before touching approval objects. Approval
		// admission reads PtahSchema directly from the API server, so this durable
		// status transition prevents any request that starts after the patch from
		// authorizing the retired plan. Keep the plan identity for one best-effort
		// audit cleanup after restart. A CREATE that commits after that cleanup is
		// still non-authorizing because its retired epoch cannot match a later plan.
		schema.Status.ExecutionBinding = binding
		if !retainReadOnlyOperation {
			schema.Status.ActiveOperation = nil
		}
		if schema.Status.PendingObservation != nil {
			// Any Observe/Plan evidence produced by the retired components is
			// unprovable. Keep the immutable Apply evidence and Lease, but restart
			// its read-only proof from Observe under the new epoch.
			schema.Status.PendingObservation.PlanRequired = false
		}
		schema.Status.Phase = operatorv1alpha1.PhasePending
		schema.Status.NextReconciliationTime = nil
		markExecutionBindingRefreshRequired(schema)
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		if r.Telemetry != nil {
			r.Telemetry.ObserveFailure(failureStage, telemetry.FailureStaleInput)
			if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
				r.Telemetry.ObserveApply(telemetry.ApplyStale)
			}
		}
		r.observeOperation(operation, telemetry.OperationStale)
		r.event(schema, corev1.EventTypeWarning, "ExecutionBindingChanged", "The previous execution binding was retired; closing its approval boundary before starting a complete read-only refresh")
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.markPlanApprovalsStaleWithReason(
		ctx,
		schema,
		plan,
		operatorv1alpha1.ReasonExecutionBindingChanged,
		"The approved plan uses an execution binding that is no longer configured",
	); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	schema.Status.Plan = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{Requeue: true}, nil
}

func isReadOnlyOperation(operation *operatorv1alpha1.ActiveOperationStatus) bool {
	if operation == nil {
		return false
	}
	switch operation.Type {
	case operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan:
		return true
	default:
		return false
	}
}

func executionBindingRetiredReadOnlyCleanupPending(schema *operatorv1alpha1.PtahSchema) bool {
	if schema == nil || schema.Status.Phase != operatorv1alpha1.PhasePending ||
		!isReadOnlyOperation(schema.Status.ActiveOperation) || !executionBindingCleanupPending(schema) {
		return false
	}
	planReady := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionPlanReady)
	return planReady != nil && planReady.Status == metav1.ConditionFalse &&
		planReady.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
}

func (r *SchemaReconciler) cleanupRetiredExecutionBindingOperation(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if !executionBindingRetiredReadOnlyCleanupPending(schema) || operation == nil {
		return ctrl.Result{}, fmt.Errorf("retired execution-binding operation cleanup is not pending")
	}

	if operation.JobName != "" {
		job := &batchv1.Job{}
		err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}, job)
		switch {
		case err == nil && operation.JobUID == "" && retiredPredecessorReadOnlyJobMatches(schema, operation, job):
			// A predecessor CREATE can pass admission before the quiescence fence
			// and commit after the old controller's final status write. Persist the
			// fully reconstructed UID first; the following pass is then the same
			// strict, UID-bound cleanup update used after an ordinary dispatch.
			before := schema.DeepCopy()
			operation.JobUID = job.UID
			if err := r.patchStatus(ctx, before, schema); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		case err == nil && retiredReadOnlyJobMatches(schema, operation, job):
			// Scheduling cleanup is not result consumption. Do not inspect Pods or logs:
			// the old epoch can no longer produce current evidence.
			if !jobTerminal(job) {
				return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
			}
			if err := r.markJobHarvested(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		case err != nil && !apierrors.IsNotFound(err):
			return ctrl.Result{}, fmt.Errorf("read retired execution-binding Job: %w", err)
		}
	}

	before := schema.DeepCopy()
	if operation.Type == operatorv1alpha1.OperationPlan && schema.Status.PendingObservation == nil &&
		operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func retiredReadOnlyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) bool {
	if schema == nil || schema.Status.ExecutionBinding == nil || !isReadOnlyOperation(operation) || job == nil ||
		operation.ID == "" || job.UID == "" || operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch ||
		!validExecutionBindingID(operation.ExecutionBindingID) ||
		!exactControllerOwner(
			job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(),
			"PtahSchema",
			schema.Name,
			schema.UID,
		) {
		return false
	}
	expectedName, err := workload.NameFor(schema, *operation.DeepCopy())
	if err != nil || operation.JobName != expectedName || job.Name != expectedName ||
		operation.JobUID == "" || operation.JobUID != job.UID || operation.AdmissionSnapshot == nil ||
		podintent.ValidateSnapshot(operation.AdmissionSnapshot) != nil {
		return false
	}
	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return false
	}
	ptahVersion := job.Annotations[workload.AnnotationPtahVersion]
	if ptahVersion == "" || len(ptahVersion) > 128 || strings.TrimSpace(ptahVersion) != ptahVersion {
		return false
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             ptahVersion,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	switch len(job.Annotations) {
	case 5:
		// The supported predecessor did not persist controller provenance.
		// Keep its envelope exact and separate from the current format.
	case 8:
		controllerImage := job.Annotations[workload.AnnotationControllerImage]
		controllerRevision := job.Annotations[workload.AnnotationControllerRevision]
		controllerStateVersion := job.Annotations[workload.AnnotationControllerStateVersion]
		parsedStateVersion, parseErr := strconv.ParseInt(controllerStateVersion, 10, 32)
		if !controllerImagePattern.MatchString(controllerImage) ||
			controllerstate.ValidateRevision(controllerRevision) != nil || parseErr != nil ||
			parsedStateVersion < 1 || strconv.FormatInt(parsedStateVersion, 10) != controllerStateVersion {
			return false
		}
		wantAnnotations[workload.AnnotationControllerImage] = controllerImage
		wantAnnotations[workload.AnnotationControllerRevision] = controllerRevision
		wantAnnotations[workload.AnnotationControllerStateVersion] = controllerStateVersion
	default:
		return false
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) ||
		!reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return false
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedJobSelector(normalized); err != nil ||
		!reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return false
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	return err == nil && templateDigest == operation.AdmissionSnapshot.TemplateDigest
}

func retiredPredecessorReadOnlyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) bool {
	if schema == nil || schema.Status.ExecutionBinding == nil || !isReadOnlyOperation(operation) || job == nil ||
		operation.ID == "" || operation.JobUID != "" || job.UID == "" ||
		!validExecutionBindingID(operation.ExecutionBindingID) ||
		operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch ||
		!exactControllerOwner(
			job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(),
			"PtahSchema",
			schema.Name,
			schema.UID,
		) {
		return false
	}
	expectedName, err := workload.NameFor(schema, *operation.DeepCopy())
	if err != nil || operation.JobName != expectedName || job.Name != expectedName || operation.AdmissionSnapshot == nil {
		return false
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return false
	}
	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return false
	}
	ptahVersion := job.Annotations[workload.AnnotationPtahVersion]
	if ptahVersion == "" || strings.TrimSpace(ptahVersion) != ptahVersion {
		return false
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             ptahVersion,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) ||
		!reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return false
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedJobSelector(normalized); err != nil ||
		!reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return false
	}
	template := normalized.Spec.Template.DeepCopy()
	delete(template.Annotations, workload.AnnotationAdmissionSnapshotDigest)
	templateDigest, err := podintent.DigestTemplate(template)
	return err == nil && templateDigest == operation.AdmissionSnapshot.TemplateDigest
}

func executionBindingChangeFenced(schema *operatorv1alpha1.PtahSchema) bool {
	if schema == nil || schema.Status.Plan == nil || schema.Status.ActiveOperation != nil ||
		schema.Status.Phase != operatorv1alpha1.PhasePending {
		return false
	}
	planReady := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionPlanReady)
	return executionBindingCleanupPending(schema) &&
		planReady != nil && planReady.Status == metav1.ConditionFalse &&
		planReady.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
}

func executionBindingCleanupPending(schema *operatorv1alpha1.PtahSchema) bool {
	if schema == nil {
		return false
	}
	approvalRequired := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	return approvalRequired != nil && approvalRequired.Status == metav1.ConditionFalse &&
		approvalRequired.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
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
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	if operation != nil && operation.Type == operatorv1alpha1.OperationPlan &&
		schema.Status.PendingObservation == nil && operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	if schema.Status.PendingObservation != nil {
		schema.Status.PendingObservation.PlanRequired = false
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	} else {
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
	}
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStaleObservation, bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonStaleObservation, "Database state must be observed again before planning")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FailureStagePlan, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if schema.Status.PendingObservation == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.event(schema, corev1.EventTypeWarning, "PlanStale", "Database state changed while the plan was generated; observing again")
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) verificationPolicyChanged(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply && operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	if operation != nil && operation.Type == operatorv1alpha1.OperationPlan {
		if schema.Status.PendingObservation != nil {
			if err := stagePendingLockRelease(schema, schema.Status.PendingObservation); err != nil {
				return ctrl.Result{}, err
			}
		} else if operation.LeaseEpoch != "" {
			if err := stageOperationLockRelease(schema, operation); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	schema.Status.ActiveOperation = nil
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
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FailureStageVerify, telemetry.FailurePolicyChanged)
		if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
			r.Telemetry.ObserveApply(telemetry.ApplyStale)
		}
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
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
		r.Telemetry.ObserveFailure(stage, telemetry.FailureConfiguration)
	}
	return ctrl.Result{RequeueAfter: failureRetry(schema)}, nil
}

func (r *SchemaReconciler) discardStaleOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	if operation != nil && (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) &&
		schema.Status.PendingObservation != nil {
		schema.Status.ActiveOperation = nil
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		schema.Status.NextReconciliationTime = nil
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonProofInputsChanged, "Post-apply observation will restart from its durable binding")
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		if r.Telemetry != nil {
			r.Telemetry.ObserveFailure(telemetry.StageForOperation(operation.Type), telemetry.FailureStaleInput)
		}
		r.observeOperation(operation, telemetry.OperationStale)
		// PendingObservation still owns the database-realm Lease and deletion
		// safety boundary. Only its terminal proof transition may remove the
		// finalizer.
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	if operation != nil && operation.Type == operatorv1alpha1.OperationPlan && operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	schema.Status.Source.Verified = false
	schema.Status.Phase = operatorv1alpha1.PhasePending
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonInputsChanged, "Operation result was discarded because desired inputs changed")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonInputsChanged, bounded(failure.Error(), 512))
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		stage := telemetry.FailureStageController
		if operation != nil {
			stage = telemetry.StageForOperation(operation.Type)
			if operation.Type == operatorv1alpha1.OperationApply {
				r.Telemetry.ObserveApply(telemetry.ApplyStale)
			}
		}
		r.Telemetry.ObserveFailure(stage, telemetry.FailureStaleInput)
	}
	r.observeOperation(operation, telemetry.OperationStale)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
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
	binding := fingerprint.PlanBinding{
		ContractVersion: fingerprint.CurrentPlanContractVersion, SchemaUID: string(schema.UID), PlanContentDigest: contentDigest,
		ArtifactDigest: schema.Status.Source.Digest, CoordinationDigest: schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest:   schema.Status.Target.IdentityDigest,
		ActualStateFingerprint: decoded.FromFingerprint, DesiredStateFingerprint: decoded.ToFingerprint,
		PolicyFingerprint: policyFingerprint, VerificationPolicyUID: string(schema.Status.Source.VerificationPolicyUID),
		VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		ExecutionBindingID:       executionBinding.Epoch,
		ControllerImage:          executionBinding.ControllerImage,
		ControllerRevision:       executionBinding.ControllerRevision,
		ControllerStateVersion:   executionBinding.ControllerStateVersion,
		PtahVersion:              executionBinding.PtahVersion, ExecutorImage: executionBinding.ExecutorImage,
		RunnerImage: executionBinding.RunnerImage, RunnerProtocolVersion: executionBinding.RunnerProtocolVersion,
	}
	planFingerprint, err := binding.Fingerprint()
	if err != nil {
		return nil, err
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: fingerprint.CurrentPlanContractVersion, SchemaRef: operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		Fingerprint: planFingerprint, ContentDigest: contentDigest,
		ArtifactDigest: schema.Status.Source.Digest, CoordinationDigest: schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest:   schema.Status.Target.IdentityDigest,
		ActualStateFingerprint: decoded.FromFingerprint, DesiredStateFingerprint: decoded.ToFingerprint,
		PolicyFingerprint: policyFingerprint, VerificationPolicyUID: schema.Status.Source.VerificationPolicyUID,
		VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		ExecutionBindingID:       executionBinding.Epoch,
		ControllerImage:          executionBinding.ControllerImage,
		ControllerRevision:       executionBinding.ControllerRevision,
		ControllerStateVersion:   executionBinding.ControllerStateVersion,
		PtahVersion:              executionBinding.PtahVersion, ExecutorImage: executionBinding.ExecutorImage,
		RunnerImage: executionBinding.RunnerImage, RunnerProtocolVersion: executionBinding.RunnerProtocolVersion,
		Dialect: decoded.Dialect, Destructive: decoded.Destructive, StatementCount: int32(len(decoded.Statements)),
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
	if plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion ||
		plan.UID != schema.Status.Plan.UID || plan.DeletionTimestamp != nil || plan.Spec.SchemaRef.UID != schema.UID ||
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
		plan.Spec.ControllerImage != schema.Status.ExecutionBinding.ControllerImage ||
		plan.Spec.ControllerRevision != schema.Status.ExecutionBinding.ControllerRevision ||
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
		plan.ControllerImage == "" || plan.ControllerImage != status.ControllerImage ||
		plan.ControllerRevision == "" || plan.ControllerRevision != status.ControllerRevision ||
		plan.ControllerStateVersion < 1 || plan.ControllerStateVersion != status.ControllerStateVersion ||
		plan.PtahVersion != status.PtahVersion || plan.ExecutorImage != status.ExecutorImage ||
		plan.RunnerImage != status.RunnerImage || plan.RunnerProtocolVersion != status.RunnerProtocolVersion {
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
		r.Telemetry.ObserveApproval(telemetry.ApprovalStale)
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
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply && operation.LeaseEpoch != "" {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	if schema.Status.Plan != nil {
		schema.Status.Plan.Approval = nil
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonApprovalRevoked, "The recorded approval is missing, replaced, or no longer matches the current plan")
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, operatorv1alpha1.ReasonApprovalRevoked, "No Apply Job was started with the invalid approval")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonAwaitingApproval, "The current plan requires a new approval")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.PendingLockRelease != nil {
		if err := r.completePendingLockRelease(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Telemetry != nil {
		if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
			r.Telemetry.ObserveApply(telemetry.ApplyStale)
			r.Telemetry.ObserveFailure(telemetry.FailureStageApply, telemetry.FailureStaleInput)
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
	Logs     []byte
	PodUIDs  []types.UID
	PodCount int32
	Trusted  bool
}

func (r *SchemaReconciler) collectTerminalPodEvidence(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
) (terminalEvidence, *corev1.Pod, error) {
	if job == nil || job.Name == "" || job.UID == "" {
		return terminalEvidence{}, nil, fmt.Errorf("terminal Job lacks immutable identity")
	}
	pods, err := r.podsOwnedByJob(ctx, schema.Namespace, job.Name, job.UID)
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
	if schema.Status.ActiveOperation == nil {
		return evidence, nil, fmt.Errorf("%w: active operation admission binding is missing", errTerminalPodIntent)
	}
	if err := validatePodIntent(selected, job, schema.Status.ActiveOperation.AdmissionSnapshot); err != nil {
		return evidence, nil, fmt.Errorf("%w: %v", errTerminalPodIntent, err)
	}
	for _, status := range selected.Status.ContainerStatuses {
		if status.Name == executorContainerName && status.State.Terminated != nil {
			evidence.Trusted = true
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
	if jobName == "" || jobUID == "" {
		return nil, nil
	}
	list := &corev1.PodList{}
	if err := r.directReader().List(ctx, list, client.InNamespace(namespace)); err != nil {
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
	logs, err := r.Logs.Read(ctx, schema.Namespace, selected.Name, executorContainerName)
	if err != nil {
		return evidence, err
	}
	evidence.Logs = logs
	return evidence, nil
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
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationApply || operation.CoordinationDigest == "" || operation.LeaseDurationSeconds == 0 {
		return false, 0, fmt.Errorf("apply target lock inputs are incomplete")
	}
	return r.acquireActiveLock(ctx, schema, targetlock.Request{
		CoordinationNamespace: r.LockNamespace, CoordinationDigest: operation.CoordinationDigest,
		Holder:   targetlock.Holder{SchemaUID: schema.UID, OperationID: operation.ID},
		Duration: time.Duration(operation.LeaseDurationSeconds) * time.Second,
	})
}

func (r *SchemaReconciler) acquireOperationLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	if schema.Status.ActiveOperation == nil {
		return false, 0, fmt.Errorf("active operation is missing")
	}
	switch schema.Status.ActiveOperation.Type {
	case operatorv1alpha1.OperationApply:
		return r.acquireApplyLock(ctx, schema)
	case operatorv1alpha1.OperationObserve:
		return r.acquirePendingObservationLock(ctx, schema)
	case operatorv1alpha1.OperationPlan:
		if schema.Status.PendingObservation != nil {
			return r.acquirePendingObservationLock(ctx, schema)
		}
		operation := schema.Status.ActiveOperation
		if operation.CoordinationDigest == "" || operation.LeaseDurationSeconds == 0 {
			return false, 0, fmt.Errorf("plan target lock inputs are incomplete")
		}
		return r.acquireActiveLock(ctx, schema, targetlock.Request{
			CoordinationNamespace: r.LockNamespace, CoordinationDigest: operation.CoordinationDigest,
			Holder:   targetlock.Holder{SchemaUID: schema.UID, OperationID: operation.ID},
			Duration: time.Duration(operation.LeaseDurationSeconds) * time.Second,
		})
	default:
		return true, 0, nil
	}
}

func operationNeedsTargetLock(schema *operatorv1alpha1.PtahSchema) bool {
	if schema.Status.ActiveOperation == nil {
		return false
	}
	return schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply ||
		schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationPlan ||
		schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationObserve && schema.Status.PendingObservation != nil
}

func (r *SchemaReconciler) acquirePendingObservationLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	pending := schema.Status.PendingObservation
	operation := schema.Status.ActiveOperation
	if pending == nil || operation == nil ||
		(operation.Type != operatorv1alpha1.OperationObserve && operation.Type != operatorv1alpha1.OperationPlan) {
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
	if pending != nil && (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) {
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
	if continuityLost && expected != "" && !operation.DispatchStarted && operation.JobUID == "" {
		// The first Lease creation necessarily has a new epoch. Because the
		// operation claim persisted its expected token before any dispatch
		// boundary, adopting the API-assigned epoch here cannot validate stale
		// work. Every later locked Job sets DispatchStarted before Create.
		continuityLost = false
	}
	operation.LeaseEpoch = epoch
	operation.LeaseContinuityLost = continuityLost
	if pending != nil && (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) {
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
	if operation == nil || operation.CoordinationDigest == "" || operation.ID == "" ||
		operation.LeaseDurationSeconds == 0 || operation.LeaseEpoch == "" {
		return nil, fmt.Errorf("persist target lock release: operation lock binding is incomplete")
	}
	return &operatorv1alpha1.TargetLockReleaseStatus{
		CoordinationDigest:   operation.CoordinationDigest,
		OperationID:          operation.ID,
		LeaseDurationSeconds: operation.LeaseDurationSeconds,
		LeaseEpoch:           operation.LeaseEpoch,
	}, nil
}

func targetLockReleaseForPending(
	pending *operatorv1alpha1.PendingObservationStatus,
) (*operatorv1alpha1.TargetLockReleaseStatus, error) {
	if pending == nil || pending.CoordinationDigest == "" || pending.ApplyOperationID == "" ||
		pending.LeaseDurationSeconds == 0 || pending.LeaseEpoch == "" {
		return nil, fmt.Errorf("persist target lock release: post-apply lock binding is incomplete")
	}
	return &operatorv1alpha1.TargetLockReleaseStatus{
		CoordinationDigest:   pending.CoordinationDigest,
		OperationID:          pending.ApplyOperationID,
		LeaseDurationSeconds: pending.LeaseDurationSeconds,
		LeaseEpoch:           pending.LeaseEpoch,
	}, nil
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
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) completePendingLockRelease(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) error {
	release := schema.Status.PendingLockRelease
	if release == nil {
		return nil
	}
	if r.Locks == nil {
		return fmt.Errorf("release database target lock: target locker is not configured")
	}
	if release.CoordinationDigest == "" || release.OperationID == "" || release.LeaseDurationSeconds == 0 || release.LeaseEpoch == "" {
		return fmt.Errorf("release database target lock: persisted release binding is incomplete")
	}
	if err := r.Locks.Release(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace,
		CoordinationDigest:    release.CoordinationDigest,
		Holder: targetlock.Holder{
			SchemaUID:   schema.UID,
			OperationID: release.OperationID,
		},
		Duration:      time.Duration(release.LeaseDurationSeconds) * time.Second,
		ExpectedEpoch: release.LeaseEpoch,
	}); err != nil {
		return fmt.Errorf("release database target lock: %w", err)
	}
	before := schema.DeepCopy()
	schema.Status.PendingLockRelease = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return err
	}
	return nil
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
		r.Telemetry.ObservePlan(after.Spec.Target.Engine, after.Status.Plan.Destructive)
	}
	approvalRequired := meta.IsStatusConditionTrue(after.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	if conditionBecame(before.Status.Conditions, after.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "") ||
		newPlan && approvalRequired {
		r.event(after, corev1.EventTypeNormal, "ApprovalRequired", "The current immutable plan requires approval")
		if r.Telemetry != nil {
			r.Telemetry.ObserveApproval(telemetry.ApprovalRequired)
		}
	}
	if approvalChanged(before.Status.Plan, after.Status.Plan) {
		r.event(after, corev1.EventTypeNormal, "ApprovalAccepted", "An authenticated approval was accepted for the current immutable plan")
		if r.Telemetry != nil {
			r.Telemetry.ObserveApproval(telemetry.ApprovalAccepted)
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
	r.Telemetry.ObserveOperation(operation.Type, outcome, r.now().Sub(operation.StartedAt.Time))
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
		Owns(&batchv1.Job{}).
		Watches(&operatorv1alpha1.PtahSchemaApproval{}, mapApproval).
		Watches(&corev1.ConfigMap{}, mapVerificationPolicy).
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
			coordinationDigest, err = fingerprint.DatabaseCoordinationDigest(string(schema.Spec.Target.Engine), schema.Spec.Target.CoordinationKey)
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

func currentPlanStatus(plan *operatorv1alpha1.PtahSchemaPlan, now metav1.Time) *operatorv1alpha1.CurrentPlanStatus {
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
		StatementCount: plan.Spec.StatementCount, CreatedAt: now,
	}
}

func appliedStatusFor(plan operatorv1alpha1.CurrentPlanStatus, now metav1.Time) *operatorv1alpha1.AppliedStatus {
	return &operatorv1alpha1.AppliedStatus{
		ArtifactDigest: plan.ArtifactDigest, PlanFingerprint: plan.Fingerprint,
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
		schema.Spec.Policy.DriftSeverity != pending.DriftSeverity ||
		schema.Spec.Execution.ConnectTimeout != pending.ConnectTimeout || schema.Spec.Policy.LockTimeout != pending.LockTimeout {
		return false
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest(string(schema.Spec.Target.Engine), schema.Spec.Target.CoordinationKey)
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
	return approvalMatchesPlanStatus(approval, schema, currentPlanStatus(plan, metav1.Time{}))
}

func approvalMatchesPlanStatus(
	approval *operatorv1alpha1.PtahSchemaApproval,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.CurrentPlanStatus,
) bool {
	return approval != nil && schema != nil && plan != nil &&
		approval.Spec.SchemaRef.Name == schema.Name && approval.Spec.SchemaRef.UID == schema.UID &&
		approval.Spec.PlanRef.Name == plan.Name && approval.Spec.PlanRef.UID == plan.UID &&
		approval.Spec.PlanFingerprint == plan.Fingerprint && approval.Spec.ArtifactDigest == plan.ArtifactDigest &&
		approval.Spec.CoordinationDigest == plan.CoordinationDigest &&
		approval.Spec.TargetIdentityDigest == plan.TargetIdentityDigest &&
		approval.Spec.ActualStateFingerprint == plan.ActualStateFingerprint &&
		approval.Spec.DesiredStateFingerprint == plan.DesiredStateFingerprint &&
		approval.Spec.PolicyFingerprint == plan.PolicyFingerprint &&
		approval.Spec.VerificationPolicyUID == plan.VerificationPolicyUID &&
		approval.Spec.VerificationPolicyDigest == plan.VerificationPolicyDigest &&
		approval.Spec.ExecutionBindingID != "" && approval.Spec.ExecutionBindingID == plan.ExecutionBindingID &&
		approval.Spec.ControllerImage != "" && approval.Spec.ControllerImage == plan.ControllerImage &&
		approval.Spec.ControllerRevision != "" && approval.Spec.ControllerRevision == plan.ControllerRevision &&
		approval.Spec.ControllerStateVersion >= 1 && approval.Spec.ControllerStateVersion == plan.ControllerStateVersion &&
		approval.Spec.PtahVersion == plan.PtahVersion && approval.Spec.ExecutorImage == plan.ExecutorImage &&
		approval.Spec.RunnerImage == plan.RunnerImage && approval.Spec.RunnerProtocolVersion == plan.RunnerProtocolVersion &&
		!approval.Spec.ApprovedAt.IsZero() && approval.Spec.Approver.Username != "" && approval.Spec.MutationRequestUID != ""
}

func recordedApprovalMatches(recorded *operatorv1alpha1.ConsumedApprovalStatus, approval *operatorv1alpha1.PtahSchemaApproval) bool {
	return recorded.Name == approval.Name && recorded.UID == approval.UID &&
		recorded.ApprovedAt.Time.Equal(approval.Spec.ApprovedAt.Time) && reflect.DeepEqual(recorded.Approver, approval.Spec.Approver)
}

func planRequiresApproval(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) bool {
	return schema.Spec.Policy.Apply != operatorv1alpha1.ApplyPolicyAlways || plan.Spec.Destructive
}

func setPlanPolicyStatus(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) {
	switch {
	case plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive:
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonDestructiveChangesDisabled, "Plan contains destructive changes and policy disallows them")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyBlocked, "Plan is blocked by destructive-change policy")
	case schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever:
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled, "Policy records plans but does not apply them")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyDisabled, "Plan is ready but apply is disabled")
	case planRequiresApproval(schema, plan):
		schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonPlanReady, "An exact immutable plan is ready for approval")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonAwaitingApproval, "Plan is waiting for approval")
	default:
		schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonNotRequired, "Policy permits this non-destructive plan without a separate approval")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonApplyPending, "The exact plan is ready to apply automatically")
	}
}

func policyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	return fingerprint.DigestCanonicalJSON(struct {
		Engine           operatorv1alpha1.DatabaseEngine `json:"engine"`
		AllowDestructive bool                            `json:"allow_destructive"`
		DriftSeverity    string                          `json:"drift_severity"`
		Exclude          []string                        `json:"exclude"`
		LockTimeout      string                          `json:"lock_timeout"`
		TransactionMode  string                          `json:"transaction_mode"`
		ConnectTimeout   string                          `json:"connect_timeout"`
	}{
		Engine: schema.Spec.Target.Engine, AllowDestructive: schema.Spec.Policy.AllowDestructive,
		DriftSeverity: schema.Spec.Policy.DriftSeverity,
		Exclude:       fingerprint.NormalizeSet(schema.Spec.Policy.Exclude), LockTimeout: schema.Spec.Policy.LockTimeout.Duration.String(),
		TransactionMode: schema.Spec.Policy.TransactionMode, ConnectTimeout: schema.Spec.Execution.ConnectTimeout.Duration.String(),
	})
}

func phaseFor(operation operatorv1alpha1.OperationType) operatorv1alpha1.ReconciliationPhase {
	switch operation {
	case operatorv1alpha1.OperationResolve:
		return operatorv1alpha1.PhaseResolving
	case operatorv1alpha1.OperationVerify:
		return operatorv1alpha1.PhaseVerifying
	case operatorv1alpha1.OperationObserve:
		return operatorv1alpha1.PhaseObserving
	case operatorv1alpha1.OperationPlan:
		return operatorv1alpha1.PhasePlanning
	case operatorv1alpha1.OperationApply:
		return operatorv1alpha1.PhaseApplying
	default:
		return operatorv1alpha1.PhaseFailed
	}
}

func runnerOperation(operation operatorv1alpha1.OperationType) runner.Operation {
	return runner.Operation(strings.ToLower(string(operation)))
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
	deadline := schema.Spec.Execution.ActiveDeadlineSeconds
	if deadline <= 0 {
		deadline = 900
	}
	return time.Duration(deadline)*time.Second + time.Minute
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
		return ctrl.Result{Requeue: true}
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

func validateJobIntent(actual, expected *batchv1.Job, schema *operatorv1alpha1.PtahSchema) error {
	if actual == nil || expected == nil || schema == nil || actual.UID == "" {
		return fmt.Errorf("Job identity is incomplete")
	}
	if actual.Namespace != expected.Namespace || actual.Name != expected.Name ||
		!exactControllerOwner(actual.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema", schema.Name, schema.UID) {
		return fmt.Errorf("Job ownership does not match the schema controller binding")
	}
	if !reflect.DeepEqual(actual.Labels, expected.Labels) || !reflect.DeepEqual(actual.Annotations, expected.Annotations) {
		return fmt.Errorf("Job operation metadata does not match the immutable claim")
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
		return fmt.Errorf("Job workload spec does not match the immutable operation intent")
	}
	return nil
}

func normalizeSupportedServiceAccountAlias(spec *corev1.PodSpec) {
	if spec == nil {
		return
	}
	serviceAccountName := spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = spec.DeprecatedServiceAccount
	}
	if spec.DeprecatedServiceAccount == serviceAccountName {
		spec.DeprecatedServiceAccount = ""
	}
}

func normalizeGeneratedJobSelector(job *batchv1.Job) error {
	if job == nil {
		return nil
	}
	generated := map[string]string{
		"controller-uid":                     string(job.UID),
		"batch.kubernetes.io/controller-uid": string(job.UID),
		"job-name":                           job.Name,
		"batch.kubernetes.io/job-name":       job.Name,
	}
	if job.Spec.Selector != nil {
		if len(job.Spec.Selector.MatchExpressions) != 0 || len(job.Spec.Selector.MatchLabels) == 0 {
			return fmt.Errorf("Job selector is not the generated controller selector")
		}
		for key, value := range job.Spec.Selector.MatchLabels {
			expected, ok := generated[key]
			if !ok || value != expected || !strings.Contains(key, "controller-uid") {
				return fmt.Errorf("Job selector is not bound to its Kubernetes UID")
			}
		}
		job.Spec.Selector = nil
	}
	for key, value := range generated {
		if actual, ok := job.Spec.Template.Labels[key]; ok {
			if job.UID == "" || actual != value {
				return fmt.Errorf("Job Pod template has an invalid generated identity label")
			}
			delete(job.Spec.Template.Labels, key)
		}
	}
	return nil
}

func validatePodIntent(
	pod *corev1.Pod,
	job *batchv1.Job,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	if pod == nil || job == nil || job.UID == "" ||
		!exactControllerOwner(pod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job", job.Name, job.UID) {
		return fmt.Errorf("Pod is not controller-owned by the exact Job UID")
	}
	if err := podintent.ValidateGeneratedPodName(pod, job.Name); err != nil {
		return fmt.Errorf("Pod does not have the exact Job-generated name: %w", err)
	}
	for key, expected := range job.Spec.Template.Labels {
		if pod.Labels[key] != expected {
			return fmt.Errorf("Pod operation labels do not match the Job template")
		}
	}
	for key, expected := range job.Spec.Template.Annotations {
		if pod.Annotations[key] != expected {
			return fmt.Errorf("Pod operation annotations do not match the Job template")
		}
	}
	for key, expected := range map[string]string{
		"controller-uid":                     string(job.UID),
		"batch.kubernetes.io/controller-uid": string(job.UID),
		"job-name":                           job.Name,
		"batch.kubernetes.io/job-name":       job.Name,
	} {
		if value, ok := pod.Labels[key]; ok && value != expected {
			return fmt.Errorf("Pod has an invalid generated Job identity label")
		}
	}

	if snapshot == nil || job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] != snapshot.Digest {
		return fmt.Errorf("Job template is not bound to the persisted admission snapshot")
	}
	if err := podintent.ValidatePodSpec(&pod.Spec, &job.Spec.Template, snapshot); err != nil {
		return fmt.Errorf("Pod workload spec does not match the validated Job template: %w", err)
	}
	return nil
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
