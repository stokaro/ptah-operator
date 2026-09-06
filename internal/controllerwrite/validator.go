// Package controllerwrite validates the narrow set of workload and plan
// writes issued by the operator manager identity.
package controllerwrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	sigsjson "sigs.k8s.io/json"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	cleanupTTLSeconds     int32 = 300
	maxStrictDecodeErrors       = 4
)

var (
	jobResource = metav1.GroupVersionResource{
		Group: batchv1.GroupName, Version: batchv1.SchemeGroupVersion.Version, Resource: "jobs",
	}
	jobKind = metav1.GroupVersionKind{
		Group: batchv1.GroupName, Version: batchv1.SchemeGroupVersion.Version, Kind: "Job",
	}
	configMapResource = metav1.GroupVersionResource{Version: corev1.SchemeGroupVersion.Version, Resource: "configmaps"}
	configMapKind     = metav1.GroupVersionKind{Version: corev1.SchemeGroupVersion.Version, Kind: "ConfigMap"}
	planResource      = metav1.GroupVersionResource{
		Group:    operatorv1alpha1.GroupVersion.Group,
		Version:  operatorv1alpha1.GroupVersion.Version,
		Resource: "ptahschemaplans",
	}
	planKind = metav1.GroupVersionKind{
		Group:   operatorv1alpha1.GroupVersion.Group,
		Version: operatorv1alpha1.GroupVersion.Version,
		Kind:    "PtahSchemaPlan",
	}
)

// JobBuilder is the immutable Job construction contract used by the schema
// controller. The production dependency should be workload.Builder.
type JobBuilder interface {
	Build(
		schema *operatorv1alpha1.PtahSchema,
		operation operatorv1alpha1.ActiveOperationStatus,
		plan *operatorv1alpha1.PtahSchemaPlan,
	) (*batchv1.Job, error)
}

var _ JobBuilder = workload.Builder{}

// Validator checks requests using uncached API reads. Reader must be the
// manager's direct API reader, never its informer cache.
type Validator struct {
	Reader          client.Reader
	Jobs            JobBuilder
	ManagerUsername string
}

// ValidationHandler adapts Validator to controller-runtime admission.
type ValidationHandler struct {
	Validator *Validator
}

// Handle implements controller-runtime admission.Handler.
func (h *ValidationHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h == nil || h.Validator == nil {
		return cradmission.Errored(http.StatusInternalServerError, errors.New("controller write webhook is not initialized"))
	}
	if err := h.Validator.Validate(ctx, req.AdmissionRequest); err != nil {
		var failed *validationFailure
		if !errors.As(err, &failed) {
			return cradmission.Errored(http.StatusInternalServerError, err)
		}
		switch failed.kind {
		case failureBadRequest:
			return cradmission.Errored(http.StatusBadRequest, failed)
		case failureInternal:
			return cradmission.Errored(http.StatusInternalServerError, failed)
		default:
			return cradmission.Denied(failed.Error())
		}
	}
	return cradmission.Allowed("operator manager write matches its exact controller contract")
}

// Validate verifies one admission request. It performs no mutation and treats
// dry-run requests exactly like requests that may be persisted.
func (v *Validator) Validate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if v == nil || v.Reader == nil || v.Jobs == nil || strings.TrimSpace(v.ManagerUsername) == "" {
		return internalf("controller write validator is not initialized")
	}
	if req.UID == "" {
		return badRequestf("admission request UID is empty")
	}
	if req.UserInfo.Username != v.ManagerUsername {
		return denyf("request username is not the configured operator manager identity")
	}
	if req.SubResource != "" || req.RequestSubResource != "" {
		return denyf("operator manager writes to subresources are not permitted")
	}

	switch req.Resource {
	case jobResource:
		if err := validateRequestType(req, jobResource, jobKind); err != nil {
			return err
		}
		switch req.Operation {
		case admissionv1.Create:
			return v.validateJobCreate(ctx, req)
		case admissionv1.Update:
			return v.validateJobUpdate(ctx, req)
		default:
			return denyf("operator manager may only create Jobs or apply the exact terminal cleanup patch")
		}
	case configMapResource:
		if err := validateRequestType(req, configMapResource, configMapKind); err != nil {
			return err
		}
		if req.Operation != admissionv1.Create {
			return denyf("operator manager may only create immutable plan chunk ConfigMaps")
		}
		return v.validateConfigMapCreate(ctx, req)
	case planResource:
		if err := validateRequestType(req, planResource, planKind); err != nil {
			return err
		}
		if req.Operation != admissionv1.Create {
			return denyf("operator manager may only create immutable PtahSchemaPlan manifests")
		}
		return v.validatePlanCreate(ctx, req)
	default:
		return denyf("resource is outside the operator manager write contract")
	}
}

func validateRequestType(
	req admissionv1.AdmissionRequest,
	resource metav1.GroupVersionResource,
	kind metav1.GroupVersionKind,
) error {
	if req.Kind != kind {
		return denyf("admission request kind does not match its resource")
	}
	if req.RequestResource != nil && *req.RequestResource != resource {
		return denyf("converted or equivalent resource requests are not permitted")
	}
	if req.RequestKind != nil && *req.RequestKind != kind {
		return denyf("converted or equivalent kind requests are not permitted")
	}
	return nil
}

func (v *Validator) validateJobCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("Job create unexpectedly contains an old object")
	}
	job := &batchv1.Job{}
	if err := decodeObject(req.Object.Raw, job, jobKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &job.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(job.Status, batchv1.JobStatus{}) {
		return denyf("new Job must not inject status")
	}

	schemaOwner, err := exactControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
	)
	if err != nil {
		return denyf("Job does not have one exact PtahSchema controller owner: %v", err)
	}
	schema, err := v.readSchema(ctx, job.Namespace, schemaOwner)
	if err != nil {
		return err
	}
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.JobName != job.Name || operation.JobUID != "" {
		return denyf("Job does not match a not-yet-created active operation")
	}

	plan, err := v.planForJob(ctx, schema, operation)
	if err != nil {
		return err
	}
	expected, err := v.Jobs.Build(schema.DeepCopy(), *operation.DeepCopy(), plan)
	if err != nil {
		return denyf("active operation cannot reconstruct the submitted Job: %v", err)
	}
	if err := validateAdmissionSnapshot(operation, expected); err != nil {
		return denyf("active operation Pod admission snapshot is invalid: %v", err)
	}
	if err := validateJobIntent(job, expected, schema, true); err != nil {
		return denyf("Job is outside the active operation intent: %v", err)
	}
	return nil
}

func (v *Validator) validateJobUpdate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) == 0 {
		return badRequestf("Job update has no old object")
	}
	oldJob := &batchv1.Job{}
	if err := decodeObject(req.OldObject.Raw, oldJob, jobKind); err != nil {
		return fmt.Errorf("decode old Job: %w", err)
	}
	job := &batchv1.Job{}
	if err := decodeObject(req.Object.Raw, job, jobKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &job.ObjectMeta); err != nil {
		return err
	}
	if oldJob.Namespace != req.Namespace || oldJob.Name != req.Name || oldJob.UID == "" || oldJob.UID != job.UID {
		return denyf("Job update does not preserve the request namespace, name, and UID")
	}
	if oldJob.Spec.TTLSecondsAfterFinished != nil || job.Spec.TTLSecondsAfterFinished == nil ||
		*job.Spec.TTLSecondsAfterFinished != cleanupTTLSeconds {
		return denyf("Job update is not the exact nil-to-300 cleanup TTL transition")
	}
	owner, err := exactControllerOwner(
		oldJob.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
	)
	if err != nil {
		return denyf("old Job does not have one exact PtahSchema controller owner: %v", err)
	}
	newOwner, err := exactControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
	)
	if err != nil || !apiequality.Semantic.DeepEqual(owner, newOwner) {
		return denyf("Job cleanup update changed the PtahSchema controller owner")
	}
	schema, err := v.readSchemaForJobUpdate(ctx, oldJob.Namespace, owner)
	if err != nil {
		return err
	}
	operation := schema.Status.ActiveOperation
	if !jobTerminal(oldJob) &&
		(operation == nil || operation.Type != operatorv1alpha1.OperationApply || !currentApplyAnnotations(oldJob.Annotations)) {
		return denyf("Job cleanup TTL cannot be set before terminal status")
	}
	if predecessorApplyAnnotations(oldJob.Annotations) {
		if err := validatePredecessorApplyCleanup(schema, schema.Status.PendingObservation, oldJob); err != nil {
			return denyf("predecessor Apply Job cleanup is outside the fenced uncertain-Apply contract: %v", err)
		}
		if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
			return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
		}
		return nil
	}
	if schema.Status.ActiveOperation == nil && currentApplyAnnotations(oldJob.Annotations) {
		if err := validatePendingApplyJobCleanup(schema, schema.Status.PendingObservation, oldJob); err != nil {
			return denyf("current-format Apply Job cleanup is outside the fenced pending-observation contract: %v", err)
		}
		if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
			return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
		}
		return nil
	}
	if operation == nil || operation.JobName != oldJob.Name || operation.JobUID != oldJob.UID {
		return denyf("Job is not the exact active operation instance")
	}
	if predecessorReadOnlyAnnotations(oldJob.Annotations) {
		if err := validatePredecessorReadOnlyCleanup(schema, operation, oldJob); err != nil {
			return denyf("predecessor read-only Job cleanup is outside the retired operation contract: %v", err)
		}
		if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
			return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
		}
		return nil
	}
	if currentOperationAnnotations(oldJob.Annotations) {
		if err := validateClaimBoundJobCleanup(schema, operation, oldJob); err != nil {
			return denyf("Job cleanup is outside the persisted operation claim: %v", err)
		}
		if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
			return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
		}
		return nil
	}
	plan, err := v.planForJob(ctx, schema, operation)
	if err != nil {
		return err
	}
	expected, err := v.Jobs.Build(schema.DeepCopy(), *operation.DeepCopy(), plan)
	if err != nil {
		return denyf("active operation cannot reconstruct the terminal Job: %v", err)
	}
	if err := validateAdmissionSnapshot(operation, expected); err != nil {
		return denyf("active operation Pod admission snapshot is invalid: %v", err)
	}
	if err := validateJobIntent(oldJob, expected, schema, true); err != nil {
		return denyf("terminal Job is outside the active operation intent: %v", err)
	}
	if err := validateOnlyCleanupTTLChanged(oldJob, job); err != nil {
		return denyf("Job cleanup update changes fields outside the cleanup TTL: %v", err)
	}
	return nil
}

// validatePendingApplyJobCleanup authorizes only garbage-collection
// scheduling for a current-format Apply Job after its operation claim has
// atomically moved into pending-observation evidence. The persisted admission
// snapshot retains the executable Pod template identity after ActiveOperation
// is cleared; this path never authorizes dispatch or result attribution.
func validatePendingApplyJobCleanup(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	job *batchv1.Job,
) error {
	if schema == nil || pending == nil || job == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("pending Apply cleanup inputs are incomplete")
	}
	if schema.Status.ActiveOperation != nil || schema.Status.Phase != operatorv1alpha1.PhasePending ||
		pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.PlanRequired {
		return errors.New("schema is not at the fenced pending-Apply cleanup boundary")
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionApprovalRequired,
	} {
		condition := apiMeta.FindStatusCondition(schema.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
			return fmt.Errorf("schema condition %s does not prove execution-binding retirement", conditionType)
		}
	}
	if !isExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		!isExecutionBindingID(pending.Plan.ExecutionBindingID) ||
		pending.Plan.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		return errors.New("Apply Job does not belong to a distinct retired execution epoch")
	}
	if pending.ApplyOperationID == "" || pending.ApplyJobName == "" || pending.ApplyJobUID == "" ||
		job.Namespace != schema.Namespace || job.Name != pending.ApplyJobName || job.UID != pending.ApplyJobUID {
		return errors.New("Job name, namespace, or UID does not match the pending Apply evidence")
	}
	if _, err := exactNamedControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return fmt.Errorf("Job owner does not match the current schema UID: %w", err)
	}
	if err := podintent.ValidateSnapshot(pending.AdmissionSnapshot); err != nil {
		return fmt.Errorf("pending Apply Pod admission snapshot is invalid: %w", err)
	}

	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(pending.ApplyOperationID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return errors.New("Job labels do not match the pending Apply evidence")
	}
	if err := validateControllerEnvelopeValues(job.Annotations); err != nil {
		return err
	}
	if pending.Plan.Name == "" || pending.Plan.UID == "" || !isSHA256Digest(pending.Plan.Fingerprint) ||
		!isSHA256Digest(pending.Plan.ContentDigest) ||
		pending.Plan.ControllerImage != job.Annotations[workload.AnnotationControllerImage] ||
		pending.Plan.ControllerRevision != job.Annotations[workload.AnnotationControllerRevision] ||
		strconv.FormatInt(int64(pending.Plan.ControllerStateVersion), 10) !=
			job.Annotations[workload.AnnotationControllerStateVersion] ||
		pending.Plan.PtahVersion != job.Annotations[workload.AnnotationPtahVersion] {
		return errors.New("Apply Job does not match the immutable pending plan binding")
	}
	inputFingerprint := job.Annotations[workload.AnnotationInputFingerprint]
	if !isSHA256Digest(inputFingerprint) {
		return errors.New("Apply Job input fingerprint is invalid")
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             pending.ApplyOperationID,
		workload.AnnotationInputFingerprint:        inputFingerprint,
		workload.AnnotationPtahVersion:             pending.Plan.PtahVersion,
		workload.AnnotationExecutionBindingID:      pending.Plan.ExecutionBindingID,
		workload.AnnotationControllerImage:         pending.Plan.ControllerImage,
		workload.AnnotationControllerRevision:      pending.Plan.ControllerRevision,
		workload.AnnotationControllerStateVersion:  strconv.FormatInt(int64(pending.Plan.ControllerStateVersion), 10),
		workload.AnnotationPlanFingerprint:         pending.Plan.Fingerprint,
		workload.AnnotationPlanContentDigest:       pending.Plan.ContentDigest,
		workload.AnnotationAdmissionSnapshotDigest: pending.AdmissionSnapshot.Digest,
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) {
		return errors.New("Job annotations are not the exact pending Apply envelope")
	}
	if !reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return errors.New("Job Pod template annotations differ from the pending Apply envelope")
	}
	normalized := job.DeepCopy()
	if err := normalizeJobForComparison(normalized, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return errors.New("Job Pod template labels differ from the pending Apply evidence")
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	if err != nil {
		return fmt.Errorf("digest pending Apply Job Pod template: %w", err)
	}
	if templateDigest != pending.AdmissionSnapshot.TemplateDigest {
		return errors.New("Job Pod template does not match the pending Apply admission snapshot")
	}
	return nil
}

// validateClaimBoundJobCleanup authorizes only garbage-collection scheduling
// for a current-format Job whose immutable operation claim and pre-admission
// Pod template are still durably recorded in status. A current Apply may set
// the TTL while still running because Kubernetes starts that timer only after
// the Job becomes terminal; every other operation must already be terminal.
// This deliberately avoids reconstructing the Job from mutable desired inputs.
func validateClaimBoundJobCleanup(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) error {
	if schema == nil || operation == nil || job == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("operation cleanup inputs are incomplete")
	}
	expectedName, err := workload.NameFor(schema, *operation.DeepCopy())
	if err != nil {
		return fmt.Errorf("derive claimed Job name: %w", err)
	}
	if job.Namespace != schema.Namespace || operation.JobName != expectedName || job.Name != expectedName ||
		operation.JobUID == "" || operation.JobUID != job.UID {
		return errors.New("Job name, namespace, or UID does not match the persisted operation claim")
	}
	if _, err := exactNamedControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return fmt.Errorf("Job owner does not match the current schema UID: %w", err)
	}
	if operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		if err := validateCurrentExecutionEnvelope(schema.Status.ExecutionBinding, job.Annotations); err != nil {
			return err
		}
	} else if err := validateRetiredReadOnlyStatus(schema, operation); err != nil {
		return err
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return fmt.Errorf("persisted Pod admission snapshot is invalid: %w", err)
	}

	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return errors.New("Job labels do not match the persisted operation claim")
	}
	if err := validateControllerEnvelopeValues(job.Annotations); err != nil {
		return err
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             job.Annotations[workload.AnnotationPtahVersion],
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationControllerImage:         job.Annotations[workload.AnnotationControllerImage],
		workload.AnnotationControllerRevision:      job.Annotations[workload.AnnotationControllerRevision],
		workload.AnnotationControllerStateVersion:  job.Annotations[workload.AnnotationControllerStateVersion],
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	if operation.Type == operatorv1alpha1.OperationApply {
		plan := schema.Status.Plan
		if plan == nil || plan.Name == "" || plan.UID == "" || !isSHA256Digest(plan.Fingerprint) ||
			!isSHA256Digest(plan.ContentDigest) || plan.ExecutionBindingID != operation.ExecutionBindingID ||
			plan.ControllerImage != wantAnnotations[workload.AnnotationControllerImage] ||
			plan.ControllerRevision != wantAnnotations[workload.AnnotationControllerRevision] ||
			strconv.FormatInt(int64(plan.ControllerStateVersion), 10) !=
				wantAnnotations[workload.AnnotationControllerStateVersion] ||
			plan.PtahVersion != wantAnnotations[workload.AnnotationPtahVersion] {
			return errors.New("Apply Job does not match the immutable current plan binding")
		}
		wantAnnotations[workload.AnnotationPlanFingerprint] = plan.Fingerprint
		wantAnnotations[workload.AnnotationPlanContentDigest] = plan.ContentDigest
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) {
		return errors.New("Job annotations are not the exact current operation envelope")
	}
	if !reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return errors.New("Job Pod template annotations differ from the current operation envelope")
	}
	normalized := job.DeepCopy()
	if err := normalizeJobForComparison(normalized, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return errors.New("Job Pod template labels differ from the persisted operation claim")
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	if err != nil {
		return fmt.Errorf("digest claimed Job Pod template: %w", err)
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return errors.New("Job Pod template does not match the persisted admission snapshot")
	}
	return nil
}

func validateCurrentExecutionEnvelope(
	binding *operatorv1alpha1.ExecutionBindingStatus,
	annotations map[string]string,
) error {
	if binding == nil || !isExecutionBindingID(binding.Epoch) ||
		annotations[workload.AnnotationExecutionBindingID] != binding.Epoch ||
		annotations[workload.AnnotationControllerImage] != binding.ControllerImage ||
		annotations[workload.AnnotationControllerRevision] != binding.ControllerRevision ||
		annotations[workload.AnnotationControllerStateVersion] !=
			strconv.FormatInt(int64(binding.ControllerStateVersion), 10) ||
		annotations[workload.AnnotationPtahVersion] != binding.PtahVersion {
		return errors.New("Job annotations do not match the durable current execution binding")
	}
	return nil
}

func validateControllerEnvelopeValues(annotations map[string]string) error {
	controllerImage := annotations[workload.AnnotationControllerImage]
	imageName, imageDigest, found := strings.Cut(controllerImage, "@")
	if !found || imageName == "" || strings.ContainsAny(imageName, "@ \t\r\n\v\f") ||
		!isSHA256Digest(imageDigest) {
		return errors.New("Job controller image is not pinned by a lowercase SHA-256 digest")
	}
	if err := controllerstate.ValidateRevision(annotations[workload.AnnotationControllerRevision]); err != nil {
		return fmt.Errorf("Job controller revision is invalid: %w", err)
	}
	stateVersion := annotations[workload.AnnotationControllerStateVersion]
	parsedStateVersion, err := strconv.ParseInt(stateVersion, 10, 32)
	if err != nil || parsedStateVersion < 1 || strconv.FormatInt(parsedStateVersion, 10) != stateVersion {
		return errors.New("Job controller state version is not a canonical positive integer")
	}
	ptahVersion := annotations[workload.AnnotationPtahVersion]
	if ptahVersion == "" || strings.TrimSpace(ptahVersion) != ptahVersion || len(ptahVersion) > 128 {
		return errors.New("Job data-plane version is empty or ambiguous")
	}
	return nil
}

func validateRetiredReadOnlyStatus(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) error {
	if schema == nil || operation == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("retired operation status is incomplete")
	}
	if schema.Status.Phase != operatorv1alpha1.PhasePending {
		return errors.New("schema has not entered the durable execution-binding retirement fence")
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionApprovalRequired,
	} {
		condition := apiMeta.FindStatusCondition(schema.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
			return fmt.Errorf("schema condition %s does not prove execution-binding retirement", conditionType)
		}
	}
	switch operation.Type {
	case operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan:
	default:
		return fmt.Errorf("operation %q is not read-only", operation.Type)
	}
	if !isExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		!isExecutionBindingID(operation.ExecutionBindingID) ||
		operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		return errors.New("operation does not belong to a distinct retired execution epoch")
	}
	return nil
}

// validatePredecessorApplyCleanup authorizes only garbage-collection
// scheduling for the exact predecessor Apply Job persisted in outcome-unknown
// rollover evidence. It never reconstructs, reads, or accepts the Apply result.
func validatePredecessorApplyCleanup(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	job *batchv1.Job,
) error {
	if schema == nil || pending == nil || job == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("uncertain-Apply cleanup inputs are incomplete")
	}
	if schema.Status.ActiveOperation != nil || schema.Status.Phase != operatorv1alpha1.PhasePending ||
		pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.PlanRequired {
		return errors.New("schema is not at the fenced uncertain-Apply cleanup boundary")
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionApprovalRequired,
	} {
		condition := apiMeta.FindStatusCondition(schema.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
			return fmt.Errorf("schema condition %s does not prove execution-binding retirement", conditionType)
		}
	}
	if !isExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		!isExecutionBindingID(pending.Plan.ExecutionBindingID) ||
		pending.Plan.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		return errors.New("Apply Job does not belong to a distinct retired execution epoch")
	}
	if pending.ApplyOperationID == "" || pending.ApplyJobName == "" || pending.ApplyJobUID == "" ||
		job.Name != pending.ApplyJobName || job.UID != pending.ApplyJobUID {
		return errors.New("Job name or UID does not match the persisted uncertain Apply")
	}
	if _, err := exactNamedControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return fmt.Errorf("Job owner does not match the current schema UID: %w", err)
	}
	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(pending.ApplyOperationID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return errors.New("Job labels do not match the persisted uncertain Apply")
	}
	if !isSHA256Digest(pending.Plan.Fingerprint) || !isSHA256Digest(pending.Plan.ContentDigest) {
		return errors.New("persisted Apply plan digests are invalid")
	}
	ptahVersion := pending.Plan.PtahVersion
	if ptahVersion == "" || strings.TrimSpace(ptahVersion) != ptahVersion {
		return errors.New("persisted Apply data-plane version is empty or ambiguous")
	}
	inputFingerprint := job.Annotations[workload.AnnotationInputFingerprint]
	snapshotDigest := job.Annotations[workload.AnnotationAdmissionSnapshotDigest]
	if !isSHA256Digest(inputFingerprint) || !isSHA256Digest(snapshotDigest) {
		return errors.New("Job input or admission snapshot digest is invalid")
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             pending.ApplyOperationID,
		workload.AnnotationInputFingerprint:        inputFingerprint,
		workload.AnnotationPtahVersion:             ptahVersion,
		workload.AnnotationExecutionBindingID:      pending.Plan.ExecutionBindingID,
		workload.AnnotationPlanFingerprint:         pending.Plan.Fingerprint,
		workload.AnnotationPlanContentDigest:       pending.Plan.ContentDigest,
		workload.AnnotationAdmissionSnapshotDigest: snapshotDigest,
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) {
		return errors.New("Job annotations are not the exact supported predecessor Apply envelope")
	}
	if !reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return errors.New("Job Pod template annotations differ from the uncertain Apply envelope")
	}
	normalized := job.DeepCopy()
	if err := normalizeJobForComparison(normalized, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return errors.New("Job Pod template labels differ from the uncertain Apply envelope")
	}
	return nil
}

// validatePredecessorReadOnlyCleanup accepts only the supported predecessor's
// terminal read-only Job after the candidate has durably retired its execution
// epoch. It authorizes scheduling deletion, never consuming the old result.
func validatePredecessorReadOnlyCleanup(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
	job *batchv1.Job,
) error {
	if schema == nil || operation == nil || job == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("retired operation inputs are incomplete")
	}
	if schema.Status.Phase != operatorv1alpha1.PhasePending {
		return errors.New("schema has not entered the durable execution-binding retirement fence")
	}
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionApprovalRequired,
	} {
		condition := apiMeta.FindStatusCondition(schema.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != string(operatorv1alpha1.ReasonExecutionBindingChanged) {
			return fmt.Errorf("schema condition %s does not prove execution-binding retirement", conditionType)
		}
	}
	switch operation.Type {
	case operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan:
	default:
		return fmt.Errorf("operation %q is not read-only", operation.Type)
	}
	if !isExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		return errors.New("operation does not belong to a distinct retired execution epoch")
	}
	expectedName, err := workload.NameFor(schema, *operation.DeepCopy())
	if err != nil {
		return fmt.Errorf("derive retired Job name: %w", err)
	}
	if operation.JobName != expectedName || job.Name != expectedName ||
		operation.JobUID == "" || operation.JobUID != job.UID {
		return errors.New("Job name or UID does not match the retired operation claim")
	}
	if _, err := exactNamedControllerOwner(
		job.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return fmt.Errorf("Job owner does not match the current schema UID: %w", err)
	}
	if operation.AdmissionSnapshot == nil {
		return errors.New("retired operation has no Pod admission snapshot")
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return fmt.Errorf("retired Pod admission snapshot is invalid: %w", err)
	}

	wantLabels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		return errors.New("Job labels do not match the retired operation claim")
	}
	ptahVersion := job.Annotations[workload.AnnotationPtahVersion]
	if ptahVersion == "" || strings.TrimSpace(ptahVersion) != ptahVersion {
		return errors.New("Job data-plane version is empty or ambiguous")
	}
	wantAnnotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             ptahVersion,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	if !reflect.DeepEqual(job.Annotations, wantAnnotations) {
		return errors.New("Job annotations are not the exact supported predecessor envelope")
	}
	if !reflect.DeepEqual(job.Spec.Template.Annotations, wantAnnotations) {
		return errors.New("Job Pod template annotations differ from the retired operation envelope")
	}
	normalized := job.DeepCopy()
	if err := normalizeJobForComparison(normalized, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, wantLabels) {
		return errors.New("Job Pod template labels differ from the retired operation envelope")
	}
	snapshotTemplate := normalized.Spec.Template.DeepCopy()
	delete(snapshotTemplate.Annotations, workload.AnnotationAdmissionSnapshotDigest)
	templateDigest, err := podintent.DigestTemplate(snapshotTemplate)
	if err != nil {
		return fmt.Errorf("digest retired Job Pod template: %w", err)
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return errors.New("Job Pod template does not match the retired admission snapshot")
	}
	return nil
}

func predecessorReadOnlyAnnotations(annotations map[string]string) bool {
	if len(annotations) != 5 {
		return false
	}
	for _, key := range []string{
		workload.AnnotationOperationID,
		workload.AnnotationInputFingerprint,
		workload.AnnotationPtahVersion,
		workload.AnnotationExecutionBindingID,
		workload.AnnotationAdmissionSnapshotDigest,
	} {
		if _, found := annotations[key]; !found {
			return false
		}
	}
	return true
}

func currentOperationAnnotations(annotations map[string]string) bool {
	if len(annotations) != 8 && len(annotations) != 10 {
		return false
	}
	for _, key := range []string{
		workload.AnnotationOperationID,
		workload.AnnotationInputFingerprint,
		workload.AnnotationPtahVersion,
		workload.AnnotationExecutionBindingID,
		workload.AnnotationControllerImage,
		workload.AnnotationControllerRevision,
		workload.AnnotationControllerStateVersion,
		workload.AnnotationAdmissionSnapshotDigest,
	} {
		if _, found := annotations[key]; !found {
			return false
		}
	}
	_, hasPlanFingerprint := annotations[workload.AnnotationPlanFingerprint]
	_, hasPlanContentDigest := annotations[workload.AnnotationPlanContentDigest]
	return len(annotations) == 8 && !hasPlanFingerprint && !hasPlanContentDigest ||
		len(annotations) == 10 && hasPlanFingerprint && hasPlanContentDigest
}

func currentApplyAnnotations(annotations map[string]string) bool {
	return len(annotations) == 10 && currentOperationAnnotations(annotations)
}

func predecessorApplyAnnotations(annotations map[string]string) bool {
	if len(annotations) != 7 {
		return false
	}
	for _, key := range []string{
		workload.AnnotationOperationID,
		workload.AnnotationInputFingerprint,
		workload.AnnotationPtahVersion,
		workload.AnnotationExecutionBindingID,
		workload.AnnotationPlanFingerprint,
		workload.AnnotationPlanContentDigest,
		workload.AnnotationAdmissionSnapshotDigest,
	} {
		if _, found := annotations[key]; !found {
			return false
		}
	}
	return true
}

func isExecutionBindingID(value string) bool {
	if len(value) != len("v1-")+32 || !strings.HasPrefix(value, "v1-") {
		return false
	}
	for _, character := range value[len("v1-"):] {
		if character < '0' || character > '9' && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateAdmissionSnapshot(
	operation *operatorv1alpha1.ActiveOperationStatus,
	expected *batchv1.Job,
) error {
	if operation == nil || expected == nil {
		return errors.New("operation or reconstructed Job is missing")
	}
	if err := podintent.ValidateSnapshot(operation.AdmissionSnapshot); err != nil {
		return err
	}
	templateDigest, err := podintent.DigestTemplate(&expected.Spec.Template)
	if err != nil {
		return err
	}
	if templateDigest != operation.AdmissionSnapshot.TemplateDigest {
		return errors.New("snapshot template digest does not match the reconstructed Job")
	}
	return nil
}

func (v *Validator) planForJob(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) (*operatorv1alpha1.PtahSchemaPlan, error) {
	if operation.Type != operatorv1alpha1.OperationApply {
		return nil, nil
	}
	if schema.Status.Plan == nil || schema.Status.Plan.Name == "" || schema.Status.Plan.UID == "" {
		return nil, denyf("Apply operation has no immutable current plan reference")
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	key := client.ObjectKey{Namespace: schema.Namespace, Name: schema.Status.Plan.Name}
	if err := v.Reader.Get(ctx, key, plan); err != nil {
		return nil, internalf("directly read Apply plan %s/%s: %v", key.Namespace, key.Name, err)
	}
	if plan.UID != schema.Status.Plan.UID {
		return nil, denyf("Apply plan UID does not match the current plan reference")
	}
	if err := validatePlanMetadata(plan, schema); err != nil {
		return nil, denyf("Apply plan metadata is invalid: %v", err)
	}
	if err := validatePlanShape(plan, schema); err != nil {
		return nil, denyf("Apply plan manifest is invalid: %v", err)
	}
	if err := v.validatePersistedChunks(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func validateJobIntent(
	actual, expected *batchv1.Job,
	schema *operatorv1alpha1.PtahSchema,
	allowGeneratedIdentity bool,
) error {
	if actual == nil || expected == nil || schema == nil {
		return errors.New("Job intent inputs are incomplete")
	}
	if actual.Namespace != expected.Namespace || actual.Name != expected.Name {
		return errors.New("Job name or namespace differs from the reconstructed intent")
	}
	if _, err := exactNamedControllerOwner(
		actual.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Labels, expected.Labels) || !reflect.DeepEqual(actual.Annotations, expected.Annotations) {
		return errors.New("Job labels or annotations differ from the reconstructed intent")
	}

	actualMeta := actual.ObjectMeta.DeepCopy()
	expectedMeta := expected.ObjectMeta.DeepCopy()
	scrubCreateServerMetadata(actualMeta)
	scrubCreateServerMetadata(expectedMeta)
	if !reflect.DeepEqual(actualMeta, expectedMeta) {
		return errors.New("Job metadata differs from the reconstructed intent")
	}

	actualCopy := actual.DeepCopy()
	expectedCopy := expected.DeepCopy()
	if err := normalizeJobForComparison(actualCopy, allowGeneratedIdentity); err != nil {
		return err
	}
	if err := normalizeJobForComparison(expectedCopy, allowGeneratedIdentity); err != nil {
		return err
	}
	if !apiequality.Semantic.DeepEqualWithNilDifferentFromEmpty(actualCopy.Spec, expectedCopy.Spec) {
		return errors.New("Job spec differs from the reconstructed immutable intent")
	}
	return nil
}

func normalizeJobForComparison(job *batchv1.Job, allowGeneratedIdentity bool) error {
	if job == nil {
		return nil
	}
	normalizeServiceAccountAlias(&job.Spec.Template.Spec)
	if !allowGeneratedIdentity {
		return nil
	}
	generated := map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
		batchv1.JobNameLabel:       job.Name,
		"controller-uid":           string(job.UID),
		"job-name":                 job.Name,
	}
	if job.Spec.Selector != nil {
		if job.UID == "" || len(job.Spec.Selector.MatchExpressions) != 0 || len(job.Spec.Selector.MatchLabels) == 0 {
			return errors.New("Job selector is not the API-generated UID selector")
		}
		for key, value := range job.Spec.Selector.MatchLabels {
			expected, ok := generated[key]
			if !ok || value != expected || !strings.Contains(key, "controller-uid") {
				return errors.New("Job selector is not bound to its API-assigned UID")
			}
		}
		job.Spec.Selector = nil
	}
	for key, expected := range generated {
		if value, ok := job.Spec.Template.Labels[key]; ok {
			if job.UID == "" || value != expected {
				return errors.New("Job Pod template has an invalid API-generated identity label")
			}
			delete(job.Spec.Template.Labels, key)
		}
	}
	return nil
}

func normalizeServiceAccountAlias(spec *corev1.PodSpec) {
	if spec == nil {
		return
	}
	name := spec.ServiceAccountName
	if name == "" {
		name = spec.DeprecatedServiceAccount
	}
	if spec.DeprecatedServiceAccount == name {
		spec.DeprecatedServiceAccount = ""
	}
}

func validateOnlyCleanupTTLChanged(oldJob, job *batchv1.Job) error {
	oldCopy := oldJob.DeepCopy()
	newCopy := job.DeepCopy()
	oldCopy.Spec.TTLSecondsAfterFinished = newCopy.Spec.TTLSecondsAfterFinished
	scrubUpdateServerMetadata(&oldCopy.ObjectMeta)
	scrubUpdateServerMetadata(&newCopy.ObjectMeta)
	if !apiequality.Semantic.DeepEqualWithNilDifferentFromEmpty(oldCopy, newCopy) {
		return errors.New("candidate object is not otherwise identical to the old object")
	}
	return nil
}

func jobTerminal(job *batchv1.Job) bool {
	return jobConditionTrue(job, batchv1.JobComplete) || jobConditionTrue(job, batchv1.JobFailed)
}

func jobSucceeded(job *batchv1.Job) bool {
	return jobConditionTrue(job, batchv1.JobComplete) && !jobConditionTrue(job, batchv1.JobFailed)
}

func jobConditionTrue(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (v *Validator) validatePlanCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("PtahSchemaPlan create unexpectedly contains an old object")
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := decodeObject(req.Object.Raw, plan, planKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &plan.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(plan.Status, operatorv1alpha1.PtahSchemaPlanStatus{}) {
		return denyf("new PtahSchemaPlan must not inject status")
	}
	owner, err := exactControllerOwner(
		plan.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
	)
	if err != nil {
		return denyf("PtahSchemaPlan does not have one exact PtahSchema controller owner: %v", err)
	}
	schema, err := v.readSchema(ctx, plan.Namespace, owner)
	if err != nil {
		return err
	}
	if err := validatePlanMetadata(plan, schema); err != nil {
		return denyf("PtahSchemaPlan metadata is invalid: %v", err)
	}
	if err := validatePlanShape(plan, schema); err != nil {
		return denyf("PtahSchemaPlan manifest is invalid: %v", err)
	}
	if err := validatePlanPublicationContext(plan, schema); err != nil {
		return denyf("PtahSchemaPlan does not match the active Plan operation: %v", err)
	}
	if err := v.validatePlanSourceJob(ctx, schema); err != nil {
		return err
	}
	return nil
}

func (v *Validator) validateConfigMapCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("ConfigMap create unexpectedly contains an old object")
	}
	configMap := &corev1.ConfigMap{}
	if err := decodeObject(req.Object.Raw, configMap, configMapKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &configMap.ObjectMeta); err != nil {
		return err
	}
	owner, err := exactControllerOwner(
		configMap.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchemaPlan",
	)
	if err != nil {
		return denyf("ConfigMap does not have one exact PtahSchemaPlan controller owner: %v", err)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	key := client.ObjectKey{Namespace: configMap.Namespace, Name: owner.Name}
	if err := v.Reader.Get(ctx, key, plan); err != nil {
		return internalf("directly read plan manifest %s/%s: %v", key.Namespace, key.Name, err)
	}
	if plan.UID == "" || plan.UID != owner.UID {
		return denyf("ConfigMap owner does not match the current PtahSchemaPlan UID")
	}
	planOwner, err := exactControllerOwner(
		plan.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
	)
	if err != nil {
		return denyf("owning PtahSchemaPlan has no exact PtahSchema controller owner: %v", err)
	}
	schema, err := v.readSchema(ctx, plan.Namespace, planOwner)
	if err != nil {
		return err
	}
	if err := validatePlanMetadata(plan, schema); err != nil {
		return denyf("owning PtahSchemaPlan metadata is invalid: %v", err)
	}
	if err := validatePlanShape(plan, schema); err != nil {
		return denyf("owning PtahSchemaPlan manifest is invalid: %v", err)
	}
	if err := validatePlanPublicationContext(plan, schema); err != nil {
		return denyf("plan chunk does not belong to the active Plan operation: %v", err)
	}

	ref, ok := findChunkReference(plan.Spec.Chunks, configMap.Name)
	if !ok {
		return denyf("ConfigMap name is not present in the immutable plan chunk manifest")
	}
	if err := validateChunk(configMap, plan, ref, ""); err != nil {
		return denyf("ConfigMap does not match its immutable plan chunk reference: %v", err)
	}
	return nil
}

func validatePlanMetadata(plan *operatorv1alpha1.PtahSchemaPlan, schema *operatorv1alpha1.PtahSchema) error {
	if plan == nil || schema == nil {
		return errors.New("plan metadata inputs are incomplete")
	}
	if plan.Namespace != schema.Namespace || plan.Spec.SchemaRef.Name != schema.Name || plan.Spec.SchemaRef.UID != schema.UID {
		return errors.New("plan schema reference does not match its namespace and owner")
	}
	if plan.DeletionTimestamp != nil || plan.DeletionGracePeriodSeconds != nil {
		return errors.New("deleting plan cannot authorize controller writes")
	}
	if _, err := exactNamedControllerOwner(
		plan.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchema",
		schema.Name,
		schema.UID,
	); err != nil {
		return err
	}
	wantName, err := deterministicPlanName(plan.Spec.Fingerprint)
	if err != nil || plan.Name != wantName {
		return errors.New("plan name is not derived from its fingerprint")
	}
	expected := metav1.ObjectMeta{
		Namespace: plan.Namespace,
		Name:      plan.Name,
		Labels:    map[string]string{planstore.LabelSchema: schema.Name},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         operatorv1alpha1.GroupVersion.String(),
			Kind:               "PtahSchema",
			Name:               schema.Name,
			UID:                schema.UID,
			Controller:         boolPointer(true),
			BlockOwnerDeletion: boolPointer(true),
		}},
	}
	actual := plan.ObjectMeta.DeepCopy()
	scrubCreateServerMetadata(actual)
	if !reflect.DeepEqual(actual, &expected) {
		return errors.New("plan metadata contains fields outside the immutable manifest contract")
	}
	return nil
}

func validatePlanShape(plan *operatorv1alpha1.PtahSchemaPlan, schema *operatorv1alpha1.PtahSchema) error {
	if plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion {
		return fmt.Errorf("plan contract version %d is not current", plan.Spec.ContractVersion)
	}
	if plan.Spec.Size < 1 || plan.Spec.Size > plancontract.MaxExecutableBytes {
		return errors.New("plan size is outside the executable plan contract")
	}
	expectedChunks := int((plan.Spec.Size + int64(planstore.ChunkBytes) - 1) / int64(planstore.ChunkBytes))
	if expectedChunks < 1 || expectedChunks > planstore.MaxChunks || len(plan.Spec.Chunks) != expectedChunks {
		return errors.New("plan chunk count does not match its declared size")
	}
	remaining := plan.Spec.Size
	for index, ref := range plan.Spec.Chunks {
		expectedSize := min(remaining, int64(planstore.ChunkBytes))
		if ref.Index != int32(index) || ref.Name != fmt.Sprintf("%s-%03d", plan.Name, index) ||
			ref.Key != planstore.ChunkDataKey || int64(ref.Size) != expectedSize || !isSHA256Digest(ref.Digest) {
			return fmt.Errorf("plan chunk %d is not the deterministic size, name, key, index, and digest tuple", index)
		}
		remaining -= expectedSize
	}
	if remaining != 0 || !isSHA256Digest(plan.Spec.ContentDigest) || !isSHA256Digest(plan.Spec.ArtifactDigest) ||
		!isSHA256Digest(plan.Spec.CoordinationDigest) || !isSHA256Digest(plan.Spec.TargetIdentityDigest) ||
		!isSHA256Digest(plan.Spec.ActualStateFingerprint) || !isSHA256Digest(plan.Spec.DesiredStateFingerprint) ||
		!isSHA256Digest(plan.Spec.PolicyFingerprint) || !isSHA256Digest(plan.Spec.VerificationPolicyDigest) {
		return errors.New("plan contains an invalid digest binding")
	}
	if plan.Spec.StatementCount < 1 {
		return errors.New("plan statement count must be positive")
	}
	if !dataplane.DialectMatches(string(schema.Spec.Target.Engine), plan.Spec.Dialect) {
		return errors.New("plan dialect does not match the schema target engine")
	}
	policyDigest, err := schemaPolicyFingerprint(schema)
	if err != nil || policyDigest != plan.Spec.PolicyFingerprint {
		return errors.New("plan policy fingerprint does not match the current schema policy")
	}
	binding := fingerprint.PlanBinding{
		ContractVersion:          plan.Spec.ContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        plan.Spec.ContentDigest,
		ArtifactDigest:           plan.Spec.ArtifactDigest,
		CoordinationDigest:       plan.Spec.CoordinationDigest,
		TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
		DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint:        plan.Spec.PolicyFingerprint,
		VerificationPolicyUID:    string(plan.Spec.VerificationPolicyUID),
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID:       plan.Spec.ExecutionBindingID,
		ControllerImage:          plan.Spec.ControllerImage,
		ControllerRevision:       plan.Spec.ControllerRevision,
		ControllerStateVersion:   plan.Spec.ControllerStateVersion,
		PtahVersion:              plan.Spec.PtahVersion,
		ExecutorImage:            plan.Spec.ExecutorImage,
		RunnerImage:              plan.Spec.RunnerImage,
		RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
	}
	wantFingerprint, err := binding.Fingerprint()
	if err != nil || wantFingerprint != plan.Spec.Fingerprint {
		return errors.New("plan fingerprint does not match its complete approval binding")
	}
	return nil
}

func validatePlanPublicationContext(plan *operatorv1alpha1.PtahSchemaPlan, schema *operatorv1alpha1.PtahSchema) error {
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationPlan || operation.JobName == "" || operation.JobUID == "" {
		return errors.New("schema has no completed active Plan Job identity")
	}
	if operation.Source == nil || operation.Source.Digest != plan.Spec.ArtifactDigest ||
		operation.CoordinationDigest != plan.Spec.CoordinationDigest ||
		operation.TargetIdentityDigest != plan.Spec.TargetIdentityDigest ||
		operation.ExecutionBindingID != plan.Spec.ExecutionBindingID {
		return errors.New("plan differs from the active operation's immutable source, target, or execution binding")
	}
	binding := schema.Status.ExecutionBinding
	if binding == nil || binding.Epoch != plan.Spec.ExecutionBindingID ||
		binding.ControllerImage != plan.Spec.ControllerImage || binding.ControllerRevision != plan.Spec.ControllerRevision ||
		binding.ControllerStateVersion != plan.Spec.ControllerStateVersion || binding.PtahVersion != plan.Spec.PtahVersion ||
		binding.ExecutorImage != plan.Spec.ExecutorImage || binding.RunnerImage != plan.Spec.RunnerImage ||
		binding.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return errors.New("plan differs from the schema's durable execution binding")
	}
	if !schema.Status.Source.Verified || schema.Status.Source.Digest != plan.Spec.ArtifactDigest ||
		schema.Status.Source.VerificationPolicyUID != plan.Spec.VerificationPolicyUID ||
		schema.Status.Source.VerificationPolicyDigest != plan.Spec.VerificationPolicyDigest ||
		schema.Status.Target.CoordinationDigest != plan.Spec.CoordinationDigest ||
		schema.Status.Target.IdentityDigest != plan.Spec.TargetIdentityDigest {
		return errors.New("plan differs from the current verified source or target observation")
	}
	return nil
}

func (v *Validator) validatePlanSourceJob(ctx context.Context, schema *operatorv1alpha1.PtahSchema) error {
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationPlan || operation.JobName == "" || operation.JobUID == "" {
		return denyf("plan publication has no exact source Job identity")
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: schema.Namespace, Name: operation.JobName}
	if err := v.Reader.Get(ctx, key, job); err != nil {
		return internalf("directly read terminal Plan Job %s/%s: %v", key.Namespace, key.Name, err)
	}
	if job.UID != operation.JobUID || !jobSucceeded(job) || job.Spec.TTLSecondsAfterFinished == nil ||
		*job.Spec.TTLSecondsAfterFinished != cleanupTTLSeconds {
		return denyf("plan publication source Job is not the exact harvested successful operation instance")
	}
	expected, err := v.Jobs.Build(schema.DeepCopy(), *operation.DeepCopy(), nil)
	if err != nil {
		return denyf("active Plan operation cannot reconstruct its source Job: %v", err)
	}
	if err := validateAdmissionSnapshot(operation, expected); err != nil {
		return denyf("active Plan operation Pod admission snapshot is invalid: %v", err)
	}
	harvested := job.DeepCopy()
	harvested.Spec.TTLSecondsAfterFinished = nil
	if err := validateJobIntent(harvested, expected, schema, true); err != nil {
		return denyf("terminal Plan Job is outside its immutable operation intent: %v", err)
	}
	return nil
}

func (v *Validator) validatePersistedChunks(ctx context.Context, plan *operatorv1alpha1.PtahSchemaPlan) error {
	if len(plan.Status.PublishedChunks) != len(plan.Spec.Chunks) {
		return denyf("Apply plan does not bind every published chunk")
	}
	type loadedChunk struct {
		configMap *corev1.ConfigMap
		err       error
	}
	loaded := make([]loadedChunk, len(plan.Spec.Chunks))

	for index, ref := range plan.Spec.Chunks {
		published := plan.Status.PublishedChunks[index]
		if published.Index != int32(index) || published.Name != ref.Name || published.UID == "" {
			return denyf("Apply plan published chunk %d has an invalid identity binding", index)
		}
	}

	var reads sync.WaitGroup
	reads.Add(len(plan.Spec.Chunks))
	for index, ref := range plan.Spec.Chunks {
		published := plan.Status.PublishedChunks[index]
		go func() {
			defer reads.Done()

			configMap := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: plan.Namespace, Name: ref.Name}
			if err := v.Reader.Get(ctx, key, configMap); err != nil {
				loaded[index].err = internalf("directly read Apply plan chunk %s/%s: %v", key.Namespace, key.Name, err)
				return
			}
			if configMap.UID != published.UID {
				loaded[index].err = denyf("Apply plan chunk %d was replaced", index)
				return
			}
			if err := validateChunk(configMap, plan, ref, published.UID); err != nil {
				loaded[index].err = denyf("Apply plan chunk %d is invalid: %v", index, err)
				return
			}
			loaded[index].configMap = configMap
		}()
	}
	reads.Wait()

	var content bytes.Buffer
	for index, ref := range plan.Spec.Chunks {
		if loaded[index].err != nil {
			return loaded[index].err
		}
		configMap := loaded[index].configMap
		if configMap == nil {
			return internalf("Apply plan chunk %d completed without a result", index)
		}
		if content.Len()+len(configMap.BinaryData[ref.Key]) > int(plancontract.MaxExecutableBytes) {
			return denyf("Apply plan chunks exceed the executable plan size limit")
		}
		_, _ = content.Write(configMap.BinaryData[ref.Key])
	}
	if int64(content.Len()) != plan.Spec.Size || fingerprint.DigestBytes(content.Bytes()) != plan.Spec.ContentDigest {
		return denyf("Apply plan chunks do not reconstruct the immutable content binding")
	}
	return nil
}

func validateChunk(
	configMap *corev1.ConfigMap,
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	expectedUID types.UID,
) error {
	if configMap == nil || plan == nil {
		return errors.New("chunk validation inputs are incomplete")
	}
	if expectedUID != "" && configMap.UID != expectedUID {
		return errors.New("chunk UID does not match its committed identity")
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return errors.New("chunk is not immutable")
	}
	if len(configMap.Data) != 0 || len(configMap.BinaryData) != 1 {
		return errors.New("chunk must contain exactly one BinaryData value and no string data")
	}
	content, ok := configMap.BinaryData[ref.Key]
	if !ok || len(content) != int(ref.Size) || fingerprint.DigestBytes(content) != ref.Digest {
		return errors.New("chunk payload does not match its declared key, size, and digest")
	}
	if _, err := exactNamedControllerOwner(
		configMap.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchemaPlan",
		plan.Name,
		plan.UID,
	); err != nil {
		return err
	}
	expected := metav1.ObjectMeta{
		Namespace: configMap.Namespace,
		Name:      ref.Name,
		Labels: map[string]string{
			planstore.LabelPlan:   plan.Name,
			planstore.LabelSchema: plan.Spec.SchemaRef.Name,
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         operatorv1alpha1.GroupVersion.String(),
			Kind:               "PtahSchemaPlan",
			Name:               plan.Name,
			UID:                plan.UID,
			Controller:         boolPointer(true),
			BlockOwnerDeletion: boolPointer(true),
		}},
	}
	actual := configMap.ObjectMeta.DeepCopy()
	scrubCreateServerMetadata(actual)
	if !reflect.DeepEqual(actual, &expected) {
		return errors.New("chunk metadata contains fields outside the immutable storage contract")
	}
	return nil
}

func (v *Validator) readSchema(
	ctx context.Context,
	namespace string,
	owner metav1.OwnerReference,
) (*operatorv1alpha1.PtahSchema, error) {
	return v.readSchemaWithDeletionPolicy(ctx, namespace, owner, false)
}

func (v *Validator) readSchemaForJobUpdate(
	ctx context.Context,
	namespace string,
	owner metav1.OwnerReference,
) (*operatorv1alpha1.PtahSchema, error) {
	return v.readSchemaWithDeletionPolicy(ctx, namespace, owner, true)
}

func (v *Validator) readSchemaWithDeletionPolicy(
	ctx context.Context,
	namespace string,
	owner metav1.OwnerReference,
	allowDeleting bool,
) (*operatorv1alpha1.PtahSchema, error) {
	schema := &operatorv1alpha1.PtahSchema{}
	key := client.ObjectKey{Namespace: namespace, Name: owner.Name}
	if err := v.Reader.Get(ctx, key, schema); err != nil {
		return nil, internalf("directly read PtahSchema %s/%s: %v", key.Namespace, key.Name, err)
	}
	if schema.UID == "" || schema.UID != owner.UID {
		return nil, denyf("controller owner does not match the current PtahSchema UID")
	}
	if !allowDeleting && schema.DeletionTimestamp != nil {
		return nil, denyf("controller owner does not match a current non-deleting PtahSchema UID")
	}
	return schema, nil
}

func validateRequestIdentity(req admissionv1.AdmissionRequest, metadata *metav1.ObjectMeta) error {
	if len(req.Object.Raw) == 0 || metadata == nil {
		return badRequestf("admission request has no candidate object")
	}
	if req.Namespace == "" || req.Name == "" || metadata.Namespace != req.Namespace || metadata.Name != req.Name {
		return denyf("candidate namespace and name do not match the admission request")
	}
	return nil
}

func decodeObject(raw []byte, object any, kind metav1.GroupVersionKind) error {
	if len(raw) == 0 {
		return badRequestf("admission request has no candidate object")
	}
	strictErrors, err := sigsjson.UnmarshalStrict(raw, object)
	if err != nil {
		return badRequestf("decode %s candidate: %v", kind.Kind, err)
	}
	if len(strictErrors) != 0 {
		reported := strictErrors
		if len(reported) > maxStrictDecodeErrors {
			reported = reported[:maxStrictDecodeErrors]
		}
		joined := errors.Join(reported...)
		if omitted := len(strictErrors) - len(reported); omitted > 0 {
			joined = fmt.Errorf("%w (and %d more strict decoding errors)", joined, omitted)
		}
		return badRequestf("decode %s candidate strictly: %v", kind.Kind, joined)
	}
	typed, ok := object.(runtime.Object)
	if !ok {
		return badRequestf("candidate type metadata does not match %s", kind.String())
	}
	actualKind := typed.GetObjectKind().GroupVersionKind()
	if actualKind.Group != kind.Group || actualKind.Version != kind.Version || actualKind.Kind != kind.Kind {
		return badRequestf("candidate type metadata does not match %s", kind.String())
	}
	return nil
}

func exactControllerOwner(
	references []metav1.OwnerReference,
	apiVersion, kind string,
) (metav1.OwnerReference, error) {
	if len(references) != 1 {
		return metav1.OwnerReference{}, errors.New("ownership graph is not a singleton")
	}
	reference := references[0]
	if reference.APIVersion != apiVersion || reference.Kind != kind || reference.Name == "" || reference.UID == "" ||
		reference.Controller == nil || !*reference.Controller ||
		reference.BlockOwnerDeletion == nil || !*reference.BlockOwnerDeletion {
		return metav1.OwnerReference{}, errors.New("controller owner identity is incomplete")
	}
	return reference, nil
}

func exactNamedControllerOwner(
	references []metav1.OwnerReference,
	apiVersion, kind, name string,
	uid types.UID,
) (metav1.OwnerReference, error) {
	reference, err := exactControllerOwner(references, apiVersion, kind)
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	if reference.Name != name || reference.UID != uid {
		return metav1.OwnerReference{}, errors.New("controller owner name or UID does not match")
	}
	return reference, nil
}

func deterministicPlanName(planFingerprint string) (string, error) {
	if !isSHA256Digest(planFingerprint) {
		return "", errors.New("plan fingerprint is not a lowercase SHA-256 digest")
	}
	return "ptah-plan-" + planFingerprint[len("sha256:"):len("sha256:")+24], nil
}

func findChunkReference(
	references []operatorv1alpha1.PlanChunkReference,
	name string,
) (operatorv1alpha1.PlanChunkReference, bool) {
	for _, reference := range references {
		if reference.Name == name {
			return reference, true
		}
	}
	return operatorv1alpha1.PlanChunkReference{}, false
}

func schemaPolicyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
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
		DriftSeverity:   schema.Spec.Policy.DriftSeverity,
		Exclude:         fingerprint.NormalizeSet(schema.Spec.Policy.Exclude),
		LockTimeout:     schema.Spec.Policy.LockTimeout.Duration.String(),
		TransactionMode: schema.Spec.Policy.TransactionMode,
		ConnectTimeout:  schema.Spec.Execution.ConnectTimeout.Duration.String(),
	})
}

func isSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func scrubCreateServerMetadata(metadata *metav1.ObjectMeta) {
	metadata.UID = ""
	metadata.ResourceVersion = ""
	metadata.Generation = 0
	metadata.CreationTimestamp = metav1.Time{}
	metadata.ManagedFields = nil
}

func scrubUpdateServerMetadata(metadata *metav1.ObjectMeta) {
	metadata.ResourceVersion = ""
	metadata.Generation = 0
	metadata.ManagedFields = nil
}

func boolPointer(value bool) *bool { return &value }

type failureKind uint8

const (
	failureDenied failureKind = iota
	failureBadRequest
	failureInternal
)

type validationFailure struct {
	kind    failureKind
	message string
}

func (e *validationFailure) Error() string { return e.message }

func denyf(format string, args ...any) error {
	return &validationFailure{kind: failureDenied, message: fmt.Sprintf(format, args...)}
}

func badRequestf(format string, args ...any) error {
	return &validationFailure{kind: failureBadRequest, message: fmt.Sprintf(format, args...)}
}

func internalf(format string, args ...any) error {
	return &validationFailure{kind: failureInternal, message: fmt.Sprintf(format, args...)}
}
