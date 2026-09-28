package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// status.pendingBindingRetirement is what an execution-binding rotation still
// owes the epoch it replaced. rotateExecutionBinding writes it in the status
// patch that installs the new epoch, and settleRetirement is the only place
// it is removed. Everything that decides whether to wait for, adopt or clean
// up something the retired epoch left reads it, and nothing reads a condition
// reason or the phase for that.
//
// The record owes at most two things, and each is removed as it is met:
//
//   - Plan: approvals of the retired plan are marked stale. reconcileBindingRetirement
//     does this before anything else, once the rotation that closed the
//     approval boundary is durable.
//   - Job: the Job the retired claim dispatched gets its cleanup TTL. A
//     read-only claim stays in status.activeOperation until then and is
//     retired by cleanupRetiredReadOnlyJob. An Apply is carried by
//     status.pendingObservation, whose pass renews the realm Lease and keeps
//     the Pod fence, so possibleApplyPodActive adopts its UID and schedules
//     its cleanup there, and claims no proof while the obligation stands.
//
// A second rotation waits until the record is gone, so the record always
// describes one retired epoch.

// retirementObligation is one thing a pending retirement owes.
type retirementObligation int

const (
	retiredPlanApprovals retirementObligation = iota + 1
	retiredJobCleanup
)

// rotateExecutionBinding installs a fresh epoch and, in the same status write,
// records what the epoch it replaces still owes: approvals of its plan to mark
// stale, and the Job its claim dispatched. The plan leaves status.plan here, so
// a plan that status names always belongs to the current epoch.
func rotateExecutionBinding(
	schema *operatorv1alpha1.PtahSchema,
	binding *operatorv1alpha1.ExecutionBindingStatus,
	job *operatorv1alpha1.RetiredJobStatus,
) error {
	if binding == nil || !validExecutionBindingID(binding.Epoch) {
		return fmt.Errorf("replacement execution binding is invalid")
	}
	if schema.Status.PendingBindingRetirement != nil {
		return fmt.Errorf("the previous execution-binding retirement is not settled")
	}
	retirement := &operatorv1alpha1.BindingRetirementStatus{Job: job}
	if plan := schema.Status.Plan; plan != nil {
		retirement.Plan = &operatorv1alpha1.RetiredPlanReference{
			Name: plan.Name, UID: plan.UID, Fingerprint: plan.Fingerprint,
		}
	}
	if retirement.Plan != nil || retirement.Job != nil {
		retired := schema.Status.ExecutionBinding
		if retired == nil || !validExecutionBindingID(retired.Epoch) {
			return fmt.Errorf("status carries a plan or an operation claim but no execution-binding epoch they belong to")
		}
		retirement.RetiredEpoch = retired.Epoch
		schema.Status.PendingBindingRetirement = retirement
	}
	schema.Status.Plan = nil
	schema.Status.ExecutionBinding = binding.DeepCopy()
	schema.Status.Phase = operatorv1alpha1.PhasePending
	schema.Status.NextReconciliationTime = nil
	markExecutionBindingRefreshRequired(schema)
	return nil
}

// settleRetirement marks obligations of the pending retirement met, and
// removes the record with its last one. It is the only place the record is
// cleared.
func settleRetirement(schema *operatorv1alpha1.PtahSchema, obligations ...retirementObligation) {
	retirement := schema.Status.PendingBindingRetirement
	if retirement == nil {
		return
	}
	for _, obligation := range obligations {
		switch obligation {
		case retiredPlanApprovals:
			retirement.Plan = nil
		case retiredJobCleanup:
			retirement.Job = nil
		}
	}
	if retirement.Plan == nil && retirement.Job == nil {
		schema.Status.PendingBindingRetirement = nil
	}
}

// reconcileBindingRetirement works off the pending retirement. It reports the
// pass unhandled only for an Apply Job, which the pending observation settles
// under its own Lease; the caller skips every rotation check meanwhile.
func (r *SchemaReconciler) reconcileBindingRetirement(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, bool, error) {
	switch {
	case schema.Status.PendingBindingRetirement.Plan != nil:
		result, err := r.retireBindingPlan(ctx, schema)
		return result, true, err
	case retiredReadOnlyJob(schema) != nil:
		result, err := r.cleanupRetiredReadOnlyJob(ctx, schema)
		return result, true, err
	case retiredApplyJob(schema, schema.Status.PendingObservation) != nil:
		return ctrl.Result{}, false, nil
	}
	// The Job the record names belongs to no claim this resource still
	// carries: a status that lost the claim, or a record naming another Job.
	// There is nothing left to clean up through it.
	before := schema.DeepCopy()
	settleRetirement(schema, retiredPlanApprovals, retiredJobCleanup)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
}

// retireBindingPlan marks stale the approvals that name the retired plan. It
// is audit, not a fence: approval admission reads this resource directly and
// refuses a plan of any epoch but the current one, so the rotation closed the
// boundary. The sweep runs only after that write is durable, and an approval
// that commits after the list it reads cannot authorize anything either.
func (r *SchemaReconciler) retireBindingPlan(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	retired := schema.Status.PendingBindingRetirement.Plan
	if err := r.markPlanApprovalsStaleWithReason(
		ctx,
		schema,
		&operatorv1alpha1.CurrentPlanStatus{Name: retired.Name, UID: retired.UID, Fingerprint: retired.Fingerprint},
		operatorv1alpha1.ReasonExecutionBindingChanged,
		"The approved plan uses an execution binding that is no longer configured",
	); err != nil {
		return ctrl.Result{}, err
	}
	before := schema.DeepCopy()
	settleRetirement(schema, retiredPlanApprovals)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	if schema.Status.ActiveOperation == nil && schema.Status.PendingObservation == nil &&
		schema.Status.PendingLockRelease == nil {
		if err := r.removeActiveFinalizer(ctx, schema); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// retiredReadOnlyJob is the record's Job when it is the one the read-only
// claim in status.activeOperation dispatched, and nil otherwise. It names the
// claim and nothing more; whether the Job may be touched is
// readOnlyJobEnvelopeMatches' decision.
func retiredReadOnlyJob(schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.RetiredJobStatus {
	retirement := schema.Status.PendingBindingRetirement
	operation := schema.Status.ActiveOperation
	if retirement == nil || retirement.Job == nil || !isReadOnlyOperation(operation) ||
		retirement.Job.Operation != operation.Type ||
		retirement.Job.Name == "" || retirement.Job.Name != operation.JobName {
		return nil
	}
	return retirement.Job
}

// retiredApplyJob is the record's Job when it is the Apply this pending
// observation carries, and nil otherwise. It names the Apply and nothing more;
// whether the Job may be touched is retiredPredecessorApplyJobMatches'
// decision.
func retiredApplyJob(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) *operatorv1alpha1.RetiredJobStatus {
	retirement := schema.Status.PendingBindingRetirement
	if retirement == nil || retirement.Job == nil || pending == nil || schema.Status.ActiveOperation != nil ||
		retirement.Job.Operation != operatorv1alpha1.OperationApply ||
		retirement.Job.Name == "" || retirement.Job.Name != pending.ApplyJobName {
		return nil
	}
	return retirement.Job
}

// cleanupRetiredReadOnlyJob retires the read-only claim a rotation kept, and
// schedules cleanup for the Job it dispatched. The claim stays in
// status.activeOperation until then: it is what holds the realm Lease for a
// Plan while the Job's Pod can still reach the database, and what authorizes
// the cleanup TTL write. It reads no result, because the retired epoch can no
// longer produce current evidence.
func (r *SchemaReconciler) cleanupRetiredReadOnlyJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
) (ctrl.Result, error) {
	operation := schema.Status.ActiveOperation
	retired := retiredReadOnlyJob(schema)
	if retired == nil {
		return ctrl.Result{}, fmt.Errorf("no retired read-only operation is pending cleanup")
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: retired.Name}, job)
	switch {
	case err == nil && retired.UID == "" && retiredPredecessorReadOnlyJobMatches(schema, operation, job):
		// A create the retired manager started can pass admission before the
		// rotation and commit after it. Record the UID first; the next pass is
		// then the same UID-bound cleanup as after an ordinary dispatch.
		before := schema.DeepCopy()
		operation.JobUID = job.UID
		retired.UID = job.UID
		if err := r.patchStatus(ctx, before, schema); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	case err == nil && retiredJobMayStillRun(schema, retired, job):
		// The claim is what holds a Plan's Lease while its Pod can still reach
		// the database, so it waits for a Job of this resource under the
		// recorded name to stop, whether or not the envelope below would let
		// the controller touch it.
		return ctrl.Result{RequeueAfter: maxLockContentionPoll}, nil
	case err == nil && retiredReadOnlyJobMatches(schema, operation, job):
		if err := r.markJobHarvested(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
	case err != nil && !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("read retired read-only Job: %w", err)
	}

	before := schema.DeepCopy()
	if mutationlifecycle.RealmHeldBy(schemaRealmClaim(schema)) == mutationlifecycle.OwnerClaim {
		if err := stageOperationLockRelease(schema, operation); err != nil {
			return ctrl.Result{}, err
		}
	}
	schema.Status.ActiveOperation = nil
	settleRetirement(schema, retiredJobCleanup)
	if err := r.patchStatus(ctx, before, schema); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// retiredJobMayStillRun reports whether job is one this resource dispatched
// under the name the record holds -- its UID the recorded one, or none
// recorded yet -- and has not reached a terminal condition. It asks nothing
// about the envelope: a Job whose envelope the controller cannot prove is
// never touched, but it can still be running.
func retiredJobMayStillRun(
	schema *operatorv1alpha1.PtahSchema,
	retired *operatorv1alpha1.RetiredJobStatus,
	job *batchv1.Job,
) bool {
	return retired != nil && job != nil && job.Name == retired.Name &&
		(retired.UID == "" || job.UID == retired.UID) && !jobTerminal(job) &&
		exactControllerOwner(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(),
			"PtahSchema", schema.Name, schema.UID)
}

// adoptRetiredApplyJobUID looks for the Job a retired Apply claim may have
// created when the claim recorded no UID. A create that passed admission
// before the rotation can commit after it, so the name is looked up until the
// Apply's own ObserveAfter horizon, which is also when proof may begin. A Job
// found with the claim's exact envelope has its UID recorded in the pending
// observation and the record in one write, and the Pod fence binds to it. None
// found by the horizon settles the obligation: from there the horizon protects
// the proof, as it does for any create whose result is unknown.
func (r *SchemaReconciler) adoptRetiredApplyJobUID(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) error {
	retired := retiredApplyJob(schema, pending)
	if retired == nil {
		return fmt.Errorf("no retired Apply Job is pending adoption")
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: retired.Name}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read late-committing retired Apply Job: %w", err)
	}
	before := schema.DeepCopy()
	switch {
	case err == nil && retiredPredecessorApplyJobMatches(schema, pending, job):
		pending.ApplyJobUID = job.UID
		retired.UID = job.UID
	case until(pending.ObserveAfter, r.now()) > 0:
		return nil
	default:
		settleRetirement(schema, retiredJobCleanup)
	}
	return r.patchStatus(ctx, before, schema)
}

// cleanupRetiredApplyJob schedules garbage collection for the Apply Job the
// retired epoch dispatched, once every Pod it owns has stopped. It reads no
// executor result and records no Apply: the pending observation stays
// outcome-unknown until a fresh read-only proof settles it.
func (r *SchemaReconciler) cleanupRetiredApplyJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
) error {
	retired := retiredApplyJob(schema, pending)
	if retired == nil {
		return fmt.Errorf("no retired Apply Job is pending cleanup")
	}
	job := &batchv1.Job{}
	err := r.directReader().Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: retired.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("read retired Apply Job: %w", err)
	case !retiredPredecessorApplyJobMatches(schema, pending, job):
		// A Job under the name that is not the one the retired claim
		// dispatched is not this resource's to touch.
	case job.Spec.TTLSecondsAfterFinished != nil:
		// A pass that stopped before settling the record already scheduled it.
	case !jobTerminal(job):
		// Fresh proof must not clear the pending observation -- the only record
		// that authorizes this write -- in the window between the last terminal
		// Pod and the Job controller's terminal condition.
		return nil
	default:
		if err := r.markJobHarvested(ctx, job); err != nil {
			return err
		}
	}
	before := schema.DeepCopy()
	settleRetirement(schema, retiredJobCleanup)
	return r.patchStatus(ctx, before, schema)
}

// retiredPredecessorApplyJobMatches holds the Apply Job a retirement record
// names to the exact envelope the retired claim dispatched: the record's name
// and UID, the retired epoch, and the pending observation's plan, operation
// and admission snapshot. Beside the keys the envelope fixes, the Job may
// carry what spec.execution.podMetadata declared when it was dispatched, by
// the rule the controller-write validator applies; the template digest pins
// what that was.
func retiredPredecessorApplyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	job *batchv1.Job,
) bool {
	retired := retiredApplyJob(schema, pending)
	if retired == nil || job == nil || job.UID == "" || schema.Status.ExecutionBinding == nil ||
		pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
		retired.UID != pending.ApplyJobUID || job.Name != retired.Name ||
		(retired.UID != "" && job.UID != retired.UID) ||
		pending.ApplyOperationID == "" ||
		!retiredEpochIs(schema, pending.Plan.ExecutionBindingID) ||
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
	if workload.ValidateClaimedMetadata(job.Labels, wantLabels) != nil ||
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
	workload.MarkMutatingOperation(wantAnnotations)
	// The manager that dispatched the Job is read from the Job: a plan names
	// the manager that published it, and a later manager of the same
	// execution binding may have applied it. What is read is held below to
	// the exact annotation set on the Job and its Pod template, and pinned by
	// the Pod template digest the claim persisted before dispatch.
	controllerImage := job.Annotations[workload.AnnotationControllerImage]
	controllerRevision := job.Annotations[workload.AnnotationControllerRevision]
	if pending.Plan.Name == "" || pending.Plan.UID == "" ||
		pending.AdmissionSnapshot == nil ||
		podintent.ValidateSnapshot(pending.AdmissionSnapshot) != nil ||
		pending.AdmissionSnapshot.Digest != snapshotDigest ||
		!controllerImagePattern.MatchString(controllerImage) ||
		controllerstate.ValidateRevision(controllerRevision) != nil ||
		pending.Plan.ControllerStateVersion < 1 {
		return false
	}
	wantAnnotations[workload.AnnotationControllerImage] = controllerImage
	wantAnnotations[workload.AnnotationControllerRevision] = controllerRevision
	wantAnnotations[workload.AnnotationControllerStateVersion] = strconv.FormatInt(
		int64(pending.Plan.ControllerStateVersion),
		10,
	)
	if !sha256DigestPattern.MatchString(inputFingerprint) ||
		!sha256DigestPattern.MatchString(snapshotDigest) ||
		workload.ValidateClaimedMetadata(job.Annotations, wantAnnotations) != nil ||
		!reflect.DeepEqual(job.Spec.Template.Annotations, job.Annotations) {
		return false
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedJobSelector(normalized); err != nil {
		return false
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, job.Labels) {
		return false
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	return err == nil && templateDigest == pending.AdmissionSnapshot.TemplateDigest
}

// readOnlyJobEnvelopeMatches holds the read-only Job a retirement record names
// to the exact envelope the workload builder writes, declared Pod metadata
// admitted as retiredPredecessorApplyJobMatches admits it. Its two callers differ in
// one thing: what they know about the committed Job UID. After an ordinary
// dispatch the claim and the record carry it and the live object must repeat
// it; after a cutover that lost it, neither carries one and the caller is
// about to reconstruct it from this object. Everything else stays one
// implementation, because a Job it rejects is never harvested: its cleanup is
// never scheduled and it outlives the release that created it.
func readOnlyJobEnvelopeMatches(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
	committedUID bool,
) bool {
	if schema == nil || operation == nil {
		return false
	}
	retired := retiredReadOnlyJob(schema)
	if retired == nil || job == nil || operation.ID == "" || job.UID == "" ||
		retired.Operation != operation.Type || retired.Name != operation.JobName ||
		retired.UID != operation.JobUID ||
		!retiredEpochIs(schema, operation.ExecutionBindingID) ||
		!exactControllerOwner(
			job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(),
			"PtahSchema",
			schema.Name,
			schema.UID,
		) {
		return false
	}
	if committedUID {
		if operation.JobUID == "" || operation.JobUID != job.UID {
			return false
		}
	} else if operation.JobUID != "" {
		return false
	}
	expectedName, err := workload.NameFor(schema, *operation.DeepCopy())
	if err != nil || operation.JobName != expectedName || job.Name != expectedName ||
		operation.AdmissionSnapshot == nil ||
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
	if workload.ValidateClaimedMetadata(job.Labels, wantLabels) != nil {
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
	if workload.ValidateClaimedMetadata(job.Annotations, wantAnnotations) != nil ||
		!reflect.DeepEqual(job.Spec.Template.Annotations, job.Annotations) {
		return false
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedJobSelector(normalized); err != nil ||
		!reflect.DeepEqual(normalized.Spec.Template.Labels, job.Labels) {
		return false
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	return err == nil && templateDigest == operation.AdmissionSnapshot.TemplateDigest
}

func retiredReadOnlyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) bool {
	return readOnlyJobEnvelopeMatches(schema, operation, job, true)
}

func retiredPredecessorReadOnlyJobMatches(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) bool {
	return readOnlyJobEnvelopeMatches(schema, operation, job, false)
}

// retiredEpochIs reports whether epoch is the one the pending retirement
// retired, and not the one in force.
func retiredEpochIs(schema *operatorv1alpha1.PtahSchema, epoch string) bool {
	retirement := schema.Status.PendingBindingRetirement
	return retirement != nil && schema.Status.ExecutionBinding != nil &&
		validExecutionBindingID(schema.Status.ExecutionBinding.Epoch) &&
		validExecutionBindingID(retirement.RetiredEpoch) && epoch == retirement.RetiredEpoch &&
		retirement.RetiredEpoch != schema.Status.ExecutionBinding.Epoch
}
