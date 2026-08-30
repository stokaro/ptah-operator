// Package controller reconciles credential-isolated desired schema state.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

const (
	activeOperationFinalizer = "operator.ptah.dev/active-operation"
	executorContainerName    = "ptah"
	approvalSchemaIndex      = "spec.schemaRef.name"
	defaultInterval          = 10 * time.Minute
	defaultFailureRetry      = 30 * time.Second
	terminalPodGrace         = 10 * time.Second
	jobCleanupTTLSeconds     = int32(300)
)

// JobBuilder turns one already-persisted operation claim into a deterministic
// Job. It must never read Secret content.
type JobBuilder interface {
	NameFor(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error)
	Build(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus, plan *operatorv1alpha1.PtahSchemaPlan) (*batchv1.Job, error)
	ExecutionBinding() (ptahVersion, executorImage, runnerImage string, protocolVersion int32)
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
}

func (r *SchemaReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
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
	if schema.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, schema)
	}
	if schema.Status.ActiveOperation != nil {
		return r.reconcileActive(ctx, schema)
	}
	if controllerutil.ContainsFinalizer(schema, activeOperationFinalizer) {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := r.now()
	if schema.Spec.Suspend {
		before := schema.DeepCopy()
		schema.Status.Phase = operatorv1alpha1.PhaseSuspended
		schema.Status.ObservedGeneration = schema.Generation
		setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, "Requested", "New database operations are suspended")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "Suspended", "Reconciliation is suspended")
		return ctrl.Result{}, r.patchStatus(ctx, before, schema)
	}
	setCondition(schema, operatorv1alpha1.ConditionSuspended, metav1.ConditionFalse, "Active", "Reconciliation is active")
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
	case operatorv1alpha1.PhaseReadyToApply, operatorv1alpha1.PhaseAwaitingApproval, operatorv1alpha1.PhaseBlocked:
		return r.reconcileApproval(ctx, schema)
	case operatorv1alpha1.PhaseInSync:
		if !due(schema.Status.NextReconciliationTime, now) {
			return ctrl.Result{RequeueAfter: until(schema.Status.NextReconciliationTime, now)}, nil
		}
	case operatorv1alpha1.PhaseFailed:
		if !due(schema.Status.NextReconciliationTime, now) {
			return ctrl.Result{RequeueAfter: until(schema.Status.NextReconciliationTime, now)}, nil
		}
	}

	// A changed desired reference or a regular interval always starts with a
	// fresh resolution. This is what makes mutable tags observable.
	return r.claim(ctx, schema, operatorv1alpha1.OperationResolve)
}

func (r *SchemaReconciler) reconcileDeletion(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(schema, activeOperationFinalizer) {
		return ctrl.Result{}, nil
	}
	if schema.Status.ActiveOperation != nil {
		job := &batchv1.Job{}
		err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: schema.Status.ActiveOperation.JobName}, job)
		if err == nil && !jobTerminal(job) {
			if schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply {
				acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
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
		if schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply {
			if err := r.releaseApplyLock(ctx, schema); err != nil {
				return ctrl.Result{}, err
			}
		}
		before := schema.DeepCopy()
		operation := schema.Status.ActiveOperation
		schema.Status.ActiveOperation = nil
		if err := r.patchStatus(ctx, before, schema); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.observeOperation(operation, telemetry.OperationCanceled)
	}
	return ctrl.Result{}, r.removeActiveFinalizer(ctx, schema)
}

func (r *SchemaReconciler) reconcileActive(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation == nil {
		return ctrl.Result{}, nil
	}
	if schema.Status.Phase == operatorv1alpha1.PhaseFailed && !due(schema.Status.NextReconciliationTime, r.now()) {
		return ctrl.Result{RequeueAfter: until(schema.Status.NextReconciliationTime, r.now())}, nil
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: schema.Namespace, Name: operation.JobName}
	err := r.directReader().Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		current, currentErr := r.operationInputFingerprint(schema, operation.Type)
		if currentErr != nil || current != operation.InputFingerprint {
			if currentErr == nil {
				currentErr = fmt.Errorf("operation inputs changed after the claim")
			}
			return r.discardStaleOperation(ctx, schema, currentErr)
		}
		if operation.Type == operatorv1alpha1.OperationApply {
			acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
		}
		var plan *operatorv1alpha1.PtahSchemaPlan
		if operation.Type == operatorv1alpha1.OperationApply {
			plan, err = r.currentPlan(ctx, schema)
			if err != nil {
				return r.applyBecameStale(ctx, schema, err)
			}
			if _, err := r.Plans.Load(ctx, plan); err != nil {
				return r.applyBecameStale(ctx, schema, fmt.Errorf("verify plan storage: %w", err))
			}
			policyDigest, err := policy.ConfigMapDigest(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
			if err != nil || policyDigest != plan.Spec.VerificationPolicyDigest {
				if err == nil {
					err = fmt.Errorf("verification policy digest changed")
				}
				return r.verificationPolicyChanged(ctx, schema, err)
			}
			if planRequiresApproval(schema, plan) {
				valid, err := r.ensureCurrentApproval(ctx, schema, plan)
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
		job, err = r.Jobs.Build(schema, *operation, plan)
		if err != nil {
			return r.operationFailure(ctx, schema, fmt.Errorf("build %s Job: %w", operation.Type, err))
		}
		if job.Namespace != schema.Namespace || job.Name != operation.JobName {
			return ctrl.Result{}, fmt.Errorf("Job builder returned an object outside the operation claim")
		}
		if err := r.Client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create %s Job: %w", operation.Type, err)
		}
		if err := r.directReader().Get(ctx, key, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("read created %s Job: %w", operation.Type, err)
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
		return r.operationFailure(ctx, schema, fmt.Errorf("active Job was replaced"))
	}
	if !ownedByUID(job.OwnerReferences, schema.UID) {
		return r.operationFailure(ctx, schema, fmt.Errorf("active Job is not owned by the schema UID"))
	}
	if !jobTerminal(job) {
		if operation.Type == operatorv1alpha1.OperationApply {
			acquired, requeue, lockErr := r.acquireApplyLock(ctx, schema)
			if lockErr != nil {
				return ctrl.Result{}, lockErr
			}
			if !acquired {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	current, currentErr := r.operationInputFingerprint(schema, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = fmt.Errorf("operation inputs changed while the Job was running")
		}
		if operation.Type == operatorv1alpha1.OperationApply {
			if err := r.releaseApplyLock(ctx, schema); err != nil {
				return ctrl.Result{}, err
			}
			// Once an Apply Job exists, a mutation may have started even when
			// its formerly exact inputs became stale. Never classify that case
			// like an undispatched read-only operation: force fresh observation
			// before any later plan can run.
			return r.finishUncertainApply(ctx, schema, job, currentErr)
		}
		_ = r.markJobHarvested(ctx, job)
		return r.discardStaleOperation(ctx, schema, currentErr)
	}

	logs, err := r.terminalLogs(ctx, schema, job)
	if err != nil {
		if errors.Is(err, errTerminalPodPending) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	result, parseErr := runner.ParseResultFor(logs, runnerOperation(operation.Type), operation.ID)
	if parseErr != nil || !jobSucceeded(job) {
		if operation.Type == operatorv1alpha1.OperationApply {
			if err := r.releaseApplyLock(ctx, schema); err != nil {
				return ctrl.Result{}, err
			}
			failure := parseErr
			if failure == nil {
				failure = fmt.Errorf("Apply Job did not complete successfully")
			}
			return r.finishUncertainApply(ctx, schema, job, fmt.Errorf("apply result is uncertain: %w", failure))
		}
		if parseErr != nil {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("read %s result: %w", operation.Type, parseErr))
		}
		return r.retryOperation(ctx, schema, job, fmt.Errorf("%s Job failed", operation.Type))
	}
	if result.Error != nil {
		err := fmt.Errorf("%s: %s", result.Error.Code, bounded(result.Error.Message, 512))
		if operation.Type == operatorv1alpha1.OperationApply || result.Uncertain {
			if operation.Type == operatorv1alpha1.OperationApply {
				if releaseErr := r.releaseApplyLock(ctx, schema); releaseErr != nil {
					return ctrl.Result{}, releaseErr
				}
			}
			return r.finishUncertainApply(ctx, schema, job, err)
		}
		return r.retryOperation(ctx, schema, job, err)
	}
	if result.Truncation != nil && result.Truncation.Stdout {
		return r.retryOperation(ctx, schema, job, fmt.Errorf("%s result was truncated", operation.Type))
	}
	if operation.Type == operatorv1alpha1.OperationApply {
		if schema.Status.Plan == nil || result.TargetIdentityDigest != schema.Status.Plan.TargetIdentityDigest {
			if err := r.releaseApplyLock(ctx, schema); err != nil {
				return ctrl.Result{}, err
			}
			return r.finishUncertainApply(ctx, schema, job, fmt.Errorf("apply target identity changed after approval"))
		}
		if err := r.releaseApplyLock(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.consumeResult(ctx, schema, job, result)
}

func (r *SchemaReconciler) consumeResult(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, result runner.Result) (ctrl.Result, error) {
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	var observedDrift *bool
	now := metav1.NewTime(r.now())
	schema.Status.LastAttemptTime = &now
	schema.Status.ObservedGeneration = schema.Generation
	clearFailure(schema)

	switch schema.Status.ActiveOperation.Type {
	case operatorv1alpha1.OperationResolve:
		report, err := dataplane.DecodeResolve([]byte(result.Stdout))
		if err != nil {
			return r.retryOperation(ctx, schema, job, err)
		}
		if report.Reference != schema.Spec.Desired.OCIRef || result.ResolvedDigest != "" && result.ResolvedDigest != report.Digest {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("resolved reference does not match the requested source"))
		}
		changed := schema.Status.Source.Digest != report.Digest || schema.Status.Source.RequestedReference != schema.Spec.Desired.OCIRef
		schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference: schema.Spec.Desired.OCIRef,
			ResolvedReference:  report.PinnedReference,
			Digest:             report.Digest,
			MediaType:          report.MediaType,
			Size:               report.Size,
			ResolvedAt:         &now,
		}
		if changed {
			schema.Status.Plan = nil
			schema.Status.Applied = nil
		}
		schema.Status.Phase = operatorv1alpha1.PhaseVerifying
		setCondition(schema, operatorv1alpha1.ConditionArtifactResolved, metav1.ConditionTrue, "DigestPinned", "Desired OCI reference resolved to immutable content")
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "Pending", "Resolved content has not been verified")
	case operatorv1alpha1.OperationVerify:
		report, err := dataplane.DecodeVerify([]byte(result.Stdout))
		if err != nil {
			return r.retryOperation(ctx, schema, job, err)
		}
		if len(report.Findings) != 0 || report.Digest != schema.Status.Source.Digest || result.ResolvedDigest != schema.Status.Source.Digest ||
			result.ObservedArtifactType != dataplane.SchemaArtifactType || result.VerificationPolicyDigest == "" {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("verification evidence does not match the resolved schema artifact"))
		}
		schema.Status.Source.Verified = true
		schema.Status.Source.ArtifactType = result.ObservedArtifactType
		schema.Status.Source.VerificationPolicyDigest = result.VerificationPolicyDigest
		schema.Status.Source.VerifiedAt = &now
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
		setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied", "Artifact type and verification policy were satisfied")
	case operatorv1alpha1.OperationObserve:
		report, err := dataplane.DecodeDrift([]byte(result.Stdout), result.ChildExitCode)
		if err != nil {
			return r.retryOperation(ctx, schema, job, err)
		}
		if result.TargetIdentityDigest == "" || result.ObservedStateFingerprint == "" {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("observation lacks credential-free target evidence"))
		}
		observedDrift = ptr(report.Drift)
		wasVerifying := schema.Status.Phase == operatorv1alpha1.PhaseVerifyingConvergence
		schema.Status.Target = operatorv1alpha1.TargetStatus{
			Engine:                   schema.Spec.Target.Engine,
			IdentityDigest:           result.TargetIdentityDigest,
			ObservedStateFingerprint: result.ObservedStateFingerprint,
			LastObservedAt:           &now,
			HighestDriftSeverity:     report.HighestSeverity,
			DriftFindingCount:        findingCount(report),
		}
		setCondition(schema, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionTrue, "Observed", "Database schema was observed")
		if !report.Drift {
			schema.Status.Phase = operatorv1alpha1.PhaseInSync
			schema.Status.LastSuccessfulReconciliation = &now
			next := metav1.NewTime(r.now().Add(interval(schema)))
			schema.Status.NextReconciliationTime = &next
			setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "Converged", "Observed schema matches the verified artifact")
			setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionTrue, "ObservedConverged", "A post-operation observation proves convergence")
			setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionTrue, "InSync", "Schema is in sync")
			if wasVerifying && schema.Status.Plan != nil {
				schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
					ArtifactDigest: schema.Status.Plan.ArtifactDigest, PlanFingerprint: schema.Status.Plan.Fingerprint,
					TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest, PtahVersion: schema.Status.Plan.PtahVersion,
					ExecutorImage: schema.Status.Plan.ExecutorImage, RunnerImage: schema.Status.Plan.RunnerImage,
					RunnerProtocolVersion: schema.Status.Plan.RunnerProtocolVersion, CompletedAt: now,
				}
			}
		} else {
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.Plan = nil
			setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "Observed", fmt.Sprintf("Observed %d drift findings; highest severity %s", findingCount(report), report.HighestSeverity))
			setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, "DriftDetected", "Observed schema differs from the verified artifact")
			setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "DriftDetected", "A plan is required")
		}
	case operatorv1alpha1.OperationPlan:
		planDocument := []byte(result.Stdout)
		if result.TargetIdentityDigest != schema.Status.Target.IdentityDigest {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("planning target identity changed after observation"))
		}
		if result.PlanContentDigest == "" || result.PlanContentDigest != fingerprint.DigestBytes(planDocument) {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("plan result content digest is missing or mismatched"))
		}
		decoded, err := dataplane.DecodePlan(planDocument, string(schema.Spec.Target.Engine))
		if err != nil {
			return r.retryOperation(ctx, schema, job, err)
		}
		if decoded.FromFingerprint != schema.Status.Target.ObservedStateFingerprint {
			return r.retryOperation(ctx, schema, job, fmt.Errorf("plan source fingerprint is stale"))
		}
		published, err := r.publishPlan(ctx, schema, decoded, planDocument)
		if err != nil {
			return ctrl.Result{}, err
		}
		schema.Status.Plan = currentPlanStatus(published, now)
		setPlanPolicyStatus(schema, published)
		setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, "Published", "Exact plan bytes are stored in immutable chunks")
	case operatorv1alpha1.OperationApply:
		// A successful process is evidence only. Convergence is established by
		// a new read-only observation, never by the Job exit code.
		schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
		setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, "JobCompleted", "Apply Job completed; convergence observation is pending")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "VerifyingConvergence", "Apply completion has not yet been independently observed")
	}

	schema.Status.ActiveOperation = nil
	if err := r.patchStatus(ctx, before, schema); err != nil {
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
		if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
			r.Telemetry.ObserveApply(telemetry.ApplyCompleted)
		}
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	_ = r.markJobHarvested(ctx, job)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeNormal, "OperationCompleted", "%s operation completed", result.Operation)
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) reconcileApproval(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	if schema.Status.Plan == nil {
		return r.claim(ctx, schema, operatorv1alpha1.OperationObserve)
	}
	plan, err := r.currentPlan(ctx, schema)
	if err != nil {
		return r.applyBecameStale(ctx, schema, err)
	}
	policyDigest, err := policy.ConfigMapDigest(ctx, r.directReader(), schema.Namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil || policyDigest != plan.Spec.VerificationPolicyDigest {
		if err == nil {
			err = fmt.Errorf("verification policy digest changed")
		}
		return r.verificationPolicyChanged(ctx, schema, err)
	}
	if plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive {
		return r.waitBlocked(ctx, schema, "DestructiveChangesDisabled", "Plan contains destructive changes and policy disallows them")
	}
	if schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever {
		return r.waitBlocked(ctx, schema, "ApplyDisabled", "Policy records plans but does not apply them")
	}

	requiresApproval := planRequiresApproval(schema, plan)
	if requiresApproval && schema.Status.Plan.Approval == nil {
		approval, err := r.findApproval(ctx, schema, plan)
		if err != nil {
			return ctrl.Result{}, err
		}
		if approval == nil {
			before := schema.DeepCopy()
			schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
			setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "Waiting", "Create an approval bound to the current plan fingerprint")
			if err := r.patchStatus(ctx, before, schema); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: interval(schema)}, nil
		}
		before := schema.DeepCopy()
		schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
			Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
		}
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		fresh := &operatorv1alpha1.PtahSchema{}
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(schema), fresh); err != nil {
			return ctrl.Result{}, err
		}
		schema = fresh
	}
	if requiresApproval {
		valid, err := r.ensureCurrentApproval(ctx, schema, plan)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !valid {
			return r.approvalBecameInvalid(ctx, schema)
		}
	}
	return r.claim(ctx, schema, operatorv1alpha1.OperationApply)
}

func (r *SchemaReconciler) claim(ctx context.Context, schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.OperationType) (ctrl.Result, error) {
	if schema.Status.ActiveOperation != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	inputs, err := operationInputs(schema, operation)
	if err != nil {
		return r.operationFailure(ctx, schema, err)
	}
	inputFingerprint, err := fingerprint.DigestCanonicalJSON(inputs)
	if err != nil {
		return ctrl.Result{}, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return ctrl.Result{}, fmt.Errorf("create operation nonce: %w", err)
	}
	id, err := fingerprint.DigestCanonicalJSON(map[string]string{"input": inputFingerprint, "nonce": hex.EncodeToString(nonce)})
	if err != nil {
		return ctrl.Result{}, err
	}
	active := &operatorv1alpha1.ActiveOperationStatus{
		Type: operation, ID: id, InputFingerprint: inputFingerprint,
		StartedAt: metav1.NewTime(r.now()), Attempt: 1,
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
		if err := r.Client.Patch(ctx, schema, client.MergeFrom(beforeMeta)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add active-operation finalizer: %w", err)
		}
	}
	before := schema.DeepCopy()
	schema.Status.ActiveOperation = active
	schema.Status.LastAttemptTime = ptrTime(active.StartedAt)
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.NextReconciliationTime = nil
	if operation != operatorv1alpha1.OperationObserve || schema.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence {
		schema.Status.Phase = phaseFor(operation)
	}
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "OperationInProgress", fmt.Sprintf("%s operation is in progress", operation))
	if operation == operatorv1alpha1.OperationApply {
		setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionTrue, "ApprovedPlan", "Applying the exact current plan")
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "Satisfied", "Current plan approval requirements are satisfied")
	}
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) retryOperation(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	operation.Attempt++
	operation.JobUID = ""
	name, err := r.Jobs.NameFor(schema, *operation)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("name retry Job after %v: %w", failure, err)
	}
	operation.JobName = name
	schema.Status.Phase = operatorv1alpha1.PhaseFailed
	next := metav1.NewTime(r.now().Add(failureRetry(schema)))
	schema.Status.NextReconciliationTime = &next
	setFailure(schema, "OperationFailed", failure)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.StageForOperation(operation.Type), telemetry.FailureOperation)
	}
	_ = r.markJobHarvested(ctx, job)
	r.event(schema, corev1.EventTypeWarning, "OperationFailed", "%s", bounded(failure.Error(), 512))
	return ctrl.Result{RequeueAfter: failureRetry(schema)}, nil
}

func (r *SchemaReconciler) finishUncertainApply(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job, failure error) (ctrl.Result, error) {
	before := schema.DeepCopy()
	operation := schema.Status.ActiveOperation
	schema.Status.ActiveOperation = nil
	schema.Status.Phase = operatorv1alpha1.PhaseVerifyingConvergence
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, "OutcomeUnknown", "Apply outcome is uncertain; only read-only observation is permitted")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "OutcomeUnknown", "Database state must be observed before any next action")
	setFailure(schema, "ApplyOutcomeUnknown", failure)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if r.Telemetry != nil {
		r.Telemetry.ObserveApply(telemetry.ApplyUncertain)
		r.Telemetry.ObserveFailure(telemetry.FailureStageApply, telemetry.FailureUncertain)
	}
	r.observeOperation(operation, telemetry.OperationUncertain)
	_ = r.markJobHarvested(ctx, job)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.event(schema, corev1.EventTypeWarning, "ApplyOutcomeUnknown", "Apply outcome is uncertain; observing database state")
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) applyBecameStale(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply {
		if err := r.releaseApplyLock(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := schema.DeepCopy()
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "Stale", bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "StalePlan", "The plan became stale before apply")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
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
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SchemaReconciler) verificationPolicyChanged(ctx context.Context, schema *operatorv1alpha1.PtahSchema, failure error) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply {
		if err := r.releaseApplyLock(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := schema.DeepCopy()
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	schema.Status.Source.Verified = false
	schema.Status.Phase = operatorv1alpha1.PhaseVerifying
	setCondition(schema, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyChanged", bounded(failure.Error(), 512))
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "PolicyChanged", "The plan is stale because verification policy bytes changed")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyChanged", "Artifact verification must run again")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
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
	setFailure(schema, "ConfigurationError", failure)
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
	schema.Status.ActiveOperation = nil
	schema.Status.Plan = nil
	schema.Status.Source.Verified = false
	schema.Status.Phase = operatorv1alpha1.PhasePending
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "InputsChanged", "Operation result was discarded because desired inputs changed")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "InputsChanged", bounded(failure.Error(), 512))
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
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
	if r.Jobs == nil {
		return nil, fmt.Errorf("Job builder is not configured")
	}
	ptahVersion, executorImage, runnerImage, protocolVersion := r.Jobs.ExecutionBinding()
	binding := fingerprint.PlanBinding{
		ContractVersion: 1, SchemaUID: string(schema.UID), PlanContentDigest: contentDigest,
		ArtifactDigest: schema.Status.Source.Digest, TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		ActualStateFingerprint: decoded.FromFingerprint, DesiredStateFingerprint: decoded.ToFingerprint,
		PolicyFingerprint: policyFingerprint, VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		PtahVersion: ptahVersion, ExecutorImage: executorImage, RunnerImage: runnerImage, RunnerProtocolVersion: protocolVersion,
	}
	planFingerprint, err := binding.Fingerprint()
	if err != nil {
		return nil, err
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: 1, SchemaRef: operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		Fingerprint: planFingerprint, ContentDigest: contentDigest,
		ArtifactDigest: schema.Status.Source.Digest, TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		ActualStateFingerprint: decoded.FromFingerprint, DesiredStateFingerprint: decoded.ToFingerprint,
		PolicyFingerprint: policyFingerprint, VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		PtahVersion: ptahVersion, ExecutorImage: executorImage, RunnerImage: runnerImage, RunnerProtocolVersion: protocolVersion,
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
	if plan.UID != schema.Status.Plan.UID || plan.DeletionTimestamp != nil || plan.Spec.SchemaRef.UID != schema.UID ||
		plan.Spec.Fingerprint != schema.Status.Plan.Fingerprint || plan.Spec.ArtifactDigest != schema.Status.Source.Digest ||
		plan.Spec.TargetIdentityDigest != schema.Status.Target.IdentityDigest ||
		plan.Spec.ActualStateFingerprint != schema.Status.Target.ObservedStateFingerprint {
		return nil, fmt.Errorf("current plan binding is stale")
	}
	currentPolicyFingerprint, err := policyFingerprint(schema)
	if err != nil || currentPolicyFingerprint != plan.Spec.PolicyFingerprint {
		return nil, fmt.Errorf("current reconciliation policy no longer matches the plan")
	}
	return plan, nil
}

func (r *SchemaReconciler) findApproval(ctx context.Context, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) (*operatorv1alpha1.PtahSchemaApproval, error) {
	list := &operatorv1alpha1.PtahSchemaApprovalList{}
	if err := r.Client.List(ctx, list, client.InNamespace(schema.Namespace), client.MatchingFields{approvalSchemaIndex: schema.Name}); err != nil {
		return nil, fmt.Errorf("list plan approvals: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].CreationTimestamp.Before(&list.Items[j].CreationTimestamp) })
	var staleCandidate *operatorv1alpha1.PtahSchemaApproval
	for i := range list.Items {
		candidate := &operatorv1alpha1.PtahSchemaApproval{}
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(&list.Items[i]), candidate); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if meta.IsStatusConditionTrue(candidate.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
			meta.IsStatusConditionTrue(candidate.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) || candidate.DeletionTimestamp != nil {
			continue
		}
		if approvalMatches(candidate, schema, plan) {
			return candidate, nil
		}
		if staleCandidate == nil {
			staleCandidate = candidate
		}
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
	if meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		return nil
	}
	before := approval.DeepCopy()
	approval.Status.ObservedGeneration = approval.Generation
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalAccepted, Status: metav1.ConditionFalse,
		Reason: "PlanNoLongerCurrent", Message: "The approval does not match the current immutable plan",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalStale, Status: metav1.ConditionTrue,
		Reason: "PlanNoLongerCurrent", Message: "The approved plan is no longer current and cannot be applied",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("mark approval stale: %w", err)
	}
	r.event(approval, corev1.EventTypeWarning, "ApprovalStale", "The approved immutable plan is no longer current")
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
		Reason: "CurrentPlan", Message: "The approval exactly matches the current immutable plan",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalStale, Status: metav1.ConditionFalse,
		Reason: "CurrentPlan", Message: "The approved plan is current",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	meta.SetStatusCondition(&approval.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
		Reason: "ApplyClaimed", Message: "The exact plan was claimed for one apply operation",
		ObservedGeneration: approval.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
	if err := r.Client.Status().Patch(ctx, approval, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("mark approval consumed: %w", err)
	}
	return nil
}

func (r *SchemaReconciler) ensureCurrentApproval(ctx context.Context, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) (bool, error) {
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
	if err := r.markApprovalConsumed(ctx, approval); err != nil {
		return false, err
	}
	return true, nil
}

func (r *SchemaReconciler) approvalBecameInvalid(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	if operation != nil && operation.Type == operatorv1alpha1.OperationApply {
		if err := r.releaseApplyLock(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := schema.DeepCopy()
	if schema.Status.Plan != nil {
		schema.Status.Plan.Approval = nil
	}
	schema.Status.ActiveOperation = nil
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "ApprovalRevoked", "The recorded approval is missing, replaced, or no longer matches the current plan")
	setCondition(schema, operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, "ApprovalRevoked", "No Apply Job was started with the invalid approval")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "AwaitingApproval", "The current plan requires a new approval")
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
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
	return ctrl.Result{RequeueAfter: interval(schema)}, nil
}

func (r *SchemaReconciler) waitBlocked(ctx context.Context, schema *operatorv1alpha1.PtahSchema, reason, message string) (ctrl.Result, error) {
	before := schema.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval(schema)}, nil
}

var errTerminalPodPending = errors.New("terminal Job pod is not yet available")

func (r *SchemaReconciler) terminalLogs(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job) ([]byte, error) {
	if r.Logs == nil {
		return nil, fmt.Errorf("pod log reader is not configured")
	}
	pods := &corev1.PodList{}
	if err := r.directReader().List(ctx, pods, client.InNamespace(schema.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, fmt.Errorf("list active Job pods: %w", err)
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedByUID(pod.OwnerReferences, job.UID) {
			continue
		}
		if selected == nil || selected.CreationTimestamp.Before(&pod.CreationTimestamp) {
			selected = pod
		}
	}
	if selected == nil {
		if r.now().Sub(schema.Status.ActiveOperation.StartedAt.Time) < terminalPodGrace {
			return nil, errTerminalPodPending
		}
		// An empty byte slice is deliberately parsed as an uncertain/missing
		// frame after the grace period.
		return nil, nil
	}
	mainTerminated := false
	for _, status := range selected.Status.ContainerStatuses {
		if status.Name == executorContainerName && status.State.Terminated != nil {
			mainTerminated = true
			break
		}
	}
	if !mainTerminated {
		// A failed init container means the executor has no log stream. Treat it
		// as a missing frame so read-only operations retry and apply remains
		// uncertain, instead of looping forever on pod/log BadRequest.
		return nil, nil
	}
	return r.Logs.Read(ctx, schema.Namespace, selected.Name, executorContainerName)
}

func (r *SchemaReconciler) markJobHarvested(ctx context.Context, job *batchv1.Job) error {
	if job == nil || job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	before := job.DeepCopy()
	job.Spec.TTLSecondsAfterFinished = ptr(jobCleanupTTLSeconds)
	return r.Client.Patch(ctx, job, client.MergeFrom(before))
}

func (r *SchemaReconciler) removeActiveFinalizer(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
	latest := &operatorv1alpha1.PtahSchema{}
	if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(schema), latest); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(latest, activeOperationFinalizer) {
		return nil
	}
	before := latest.DeepCopy()
	controllerutil.RemoveFinalizer(latest, activeOperationFinalizer)
	if err := r.Client.Patch(ctx, latest, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("remove active-operation finalizer: %w", err)
	}
	return nil
}

func (r *SchemaReconciler) acquireApplyLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) (bool, time.Duration, error) {
	if r.Locks == nil {
		return false, 0, fmt.Errorf("database target locker is not configured")
	}
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationApply || schema.Status.Target.IdentityDigest == "" {
		return false, 0, fmt.Errorf("apply target lock inputs are incomplete")
	}
	result, err := r.Locks.Acquire(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace, TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		Holder:   targetlock.Holder{SchemaUID: schema.UID, OperationID: operation.ID},
		Duration: leaseDuration(schema),
	})
	if err != nil {
		return false, 0, fmt.Errorf("acquire database target lock: %w", err)
	}
	if result.Acquired {
		return true, 0, nil
	}
	if result.Contention == nil {
		return false, 0, fmt.Errorf("database target lock returned no acquisition result")
	}
	requeue := result.Contention.RequeueAfter
	if requeue < time.Second {
		requeue = time.Second
	}
	return false, requeue, nil
}

func (r *SchemaReconciler) releaseApplyLock(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
	if r.Locks == nil || schema.Status.ActiveOperation == nil || schema.Status.ActiveOperation.Type != operatorv1alpha1.OperationApply {
		return nil
	}
	if err := r.Locks.Release(ctx, targetlock.Request{
		CoordinationNamespace: r.LockNamespace, TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		Holder:   targetlock.Holder{SchemaUID: schema.UID, OperationID: schema.Status.ActiveOperation.ID},
		Duration: leaseDuration(schema),
	}); err != nil {
		return fmt.Errorf("release database target lock: %w", err)
	}
	return nil
}

func (r *SchemaReconciler) patchStatus(ctx context.Context, before, after *operatorv1alpha1.PtahSchema) error {
	if reflect.DeepEqual(before.Status, after.Status) {
		return nil
	}
	if err := r.Client.Status().Patch(ctx, after, client.MergeFrom(before)); err != nil {
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
	if conditionBecame(before.Status.Conditions, after.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyChanged") {
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
	mapApproval := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
		approval, ok := object.(*operatorv1alpha1.PtahSchemaApproval)
		if !ok || approval.Spec.SchemaRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: approval.Namespace, Name: approval.Spec.SchemaRef.Name}}}
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
		Complete(r)
}

func operationInputs(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.OperationType) (map[string]any, error) {
	base := map[string]any{"generation": schema.Generation, "operation": operation, "schema_uid": schema.UID}
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
		if !schema.Status.Source.Verified {
			return nil, fmt.Errorf("verified artifact is required before observation")
		}
		base["resolved_reference"] = schema.Status.Source.ResolvedReference
		base["artifact_digest"] = schema.Status.Source.Digest
		base["target"] = schema.Spec.Target
		base["ignore"] = fingerprint.NormalizeSet(schema.Spec.Policy.Ignore)
		base["severity"] = schema.Spec.Policy.DriftSeverity
	case operatorv1alpha1.OperationPlan:
		if schema.Status.Target.IdentityDigest == "" || schema.Status.Target.ObservedStateFingerprint == "" {
			return nil, fmt.Errorf("target observation is required before planning")
		}
		base["resolved_reference"] = schema.Status.Source.ResolvedReference
		base["target_identity"] = schema.Status.Target.IdentityDigest
		base["actual_state"] = schema.Status.Target.ObservedStateFingerprint
		base["exclude"] = fingerprint.NormalizeSet(schema.Spec.Policy.Exclude)
		base["dev"] = schema.Spec.Dev
	case operatorv1alpha1.OperationApply:
		if schema.Status.Plan == nil {
			return nil, fmt.Errorf("current plan is required before apply")
		}
		base["plan_fingerprint"] = schema.Status.Plan.Fingerprint
		base["plan_content_digest"] = schema.Status.Plan.ContentDigest
		base["approval"] = schema.Status.Plan.Approval
	default:
		return nil, fmt.Errorf("unsupported operation %q", operation)
	}
	return base, nil
}

func currentPlanStatus(plan *operatorv1alpha1.PtahSchemaPlan, now metav1.Time) *operatorv1alpha1.CurrentPlanStatus {
	return &operatorv1alpha1.CurrentPlanStatus{
		Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint, ContentDigest: plan.Spec.ContentDigest,
		ArtifactDigest: plan.Spec.ArtifactDigest, TargetIdentityDigest: plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint: plan.Spec.ActualStateFingerprint, DesiredStateFingerprint: plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint: plan.Spec.PolicyFingerprint, VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		PtahVersion: plan.Spec.PtahVersion, ExecutorImage: plan.Spec.ExecutorImage, RunnerImage: plan.Spec.RunnerImage,
		RunnerProtocolVersion: plan.Spec.RunnerProtocolVersion, Destructive: plan.Spec.Destructive,
		StatementCount: plan.Spec.StatementCount, CreatedAt: now,
	}
}

func approvalMatches(approval *operatorv1alpha1.PtahSchemaApproval, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) bool {
	return approval.Spec.SchemaRef.Name == schema.Name && approval.Spec.SchemaRef.UID == schema.UID &&
		approval.Spec.PlanRef.Name == plan.Name && approval.Spec.PlanRef.UID == plan.UID &&
		approval.Spec.PlanFingerprint == plan.Spec.Fingerprint && approval.Spec.ArtifactDigest == plan.Spec.ArtifactDigest &&
		approval.Spec.TargetIdentityDigest == plan.Spec.TargetIdentityDigest &&
		approval.Spec.ActualStateFingerprint == plan.Spec.ActualStateFingerprint &&
		approval.Spec.DesiredStateFingerprint == plan.Spec.DesiredStateFingerprint &&
		approval.Spec.PolicyFingerprint == plan.Spec.PolicyFingerprint &&
		approval.Spec.VerificationPolicyDigest == plan.Spec.VerificationPolicyDigest &&
		approval.Spec.PtahVersion == plan.Spec.PtahVersion && approval.Spec.ExecutorImage == plan.Spec.ExecutorImage &&
		approval.Spec.RunnerImage == plan.Spec.RunnerImage && approval.Spec.RunnerProtocolVersion == plan.Spec.RunnerProtocolVersion &&
		!approval.Spec.ApprovedAt.IsZero() && approval.Spec.Approver.Username != "" && approval.Spec.AdmissionRequestUID != ""
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
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "DestructiveChangesDisabled", "Plan contains destructive changes and policy disallows them")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyBlocked", "Plan is blocked by destructive-change policy")
	case schema.Spec.Policy.Apply == operatorv1alpha1.ApplyPolicyNever:
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "ApplyDisabled", "Policy records plans but does not apply them")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ApplyDisabled", "Plan is ready but apply is disabled")
	case planRequiresApproval(schema, plan):
		schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "PlanReady", "An exact immutable plan is ready for approval")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "AwaitingApproval", "Plan is waiting for approval")
	default:
		schema.Status.Phase = operatorv1alpha1.PhaseReadyToApply
		setCondition(schema, operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "NotRequired", "Policy permits this non-destructive plan without a separate approval")
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ApplyPending", "The exact plan is ready to apply automatically")
	}
}

func policyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	return fingerprint.DigestCanonicalJSON(struct {
		Engine           operatorv1alpha1.DatabaseEngine `json:"engine"`
		AllowDestructive bool                            `json:"allow_destructive"`
		DriftSeverity    string                          `json:"drift_severity"`
		Ignore           []string                        `json:"ignore"`
		Exclude          []string                        `json:"exclude"`
		LockTimeout      string                          `json:"lock_timeout"`
		TransactionMode  string                          `json:"transaction_mode"`
		ConnectTimeout   string                          `json:"connect_timeout"`
	}{
		Engine: schema.Spec.Target.Engine, AllowDestructive: schema.Spec.Policy.AllowDestructive,
		DriftSeverity: schema.Spec.Policy.DriftSeverity, Ignore: fingerprint.NormalizeSet(schema.Spec.Policy.Ignore),
		Exclude: fingerprint.NormalizeSet(schema.Spec.Policy.Exclude), LockTimeout: schema.Spec.Policy.LockTimeout.Duration.String(),
		TransactionMode: schema.Spec.Policy.TransactionMode, ConnectTimeout: schema.Spec.Execution.ConnectTimeout.Duration.String(),
	})
}

func findingCount(report dataplane.DriftReport) int32 {
	var count int32
	for _, finding := range report.Findings {
		if finding.Count > 0 && count <= int32(^uint32(0)>>1)-finding.Count {
			count += finding.Count
		}
	}
	return count
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

func setCondition(schema *operatorv1alpha1.PtahSchema, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: bounded(message, 1024),
		ObservedGeneration: schema.Generation, LastTransitionTime: metav1.Now(),
	})
}

func setFailure(schema *operatorv1alpha1.PtahSchema, reason string, failure error) {
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, reason, bounded(failure.Error(), 1024))
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "Reconciliation did not complete")
}

func clearFailure(schema *operatorv1alpha1.PtahSchema) {
	setCondition(schema, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, "Succeeded", "The latest operation completed")
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
