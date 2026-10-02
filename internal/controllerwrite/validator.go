// Package controllerwrite validates the narrow set of workload and plan
// writes issued by the operator manager identity, and protects reserved result
// credentials against writes from any principal.
package controllerwrite

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	sigsjson "sigs.k8s.io/json"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	cleanupTTLSeconds     = jobclaim.CleanupTTLSeconds
	maxStrictDecodeErrors = 4
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
	BuildMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
		plan *operatorv1alpha1.PtahMigrationPlan,
	) (*batchv1.Job, error)
	// ManagerIdentity is the manager's own release, which a plan it creates
	// records.
	ManagerIdentity() (controllerImage, controllerRevision, runnerImage string)
}

var _ JobBuilder = workload.Builder{}

type ResultCredentialValidator interface {
	ValidateCreate(context.Context, *corev1.Secret) error
	ValidateRecordCreate(context.Context, *operatorv1alpha1.PtahResultRecord) error
	AuthorizePublication(context.Context, resultstore.Binding) (resultdelivery.Identity, error)
}

// Validator checks requests using uncached API reads. Reader must be the
// manager's direct API reader, never its informer cache.
type Validator struct {
	Reader          client.Reader
	Jobs            JobBuilder
	ManagerUsername string
	// ResultCredentials remains nil until the installation has delivery trust.
	// Reserved credential writes are still guarded while issuance is disabled.
	ResultCredentials ResultCredentialValidator
	ResultCleanup     *resultcleanup.Policy
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
	return cradmission.Allowed("write matches the operator admission contract")
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
	if req.Resource == (metav1.GroupVersionResource{Version: "v1", Resource: "secrets"}) {
		return v.validateResultCredential(ctx, req)
	}
	if req.Resource == (metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"}) {
		return v.validateResultRecord(ctx, req)
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
			return denyf("operator manager may only create immutable plan projection ConfigMaps")
		}
		return v.validateProjectionCreate(ctx, req)
	case planChunkResource:
		if err := validateRequestType(req, planChunkResource, planChunkKind); err != nil {
			return err
		}
		if req.Operation != admissionv1.Create {
			return denyf("operator manager may only create immutable PtahSchemaPlanChunk objects")
		}
		return v.validatePlanChunkCreate(ctx, req)
	case planResource:
		if err := validateRequestType(req, planResource, planKind); err != nil {
			return err
		}
		if req.Operation != admissionv1.Create {
			return denyf("operator manager may only create immutable PtahSchemaPlan manifests")
		}
		return v.validatePlanCreate(ctx, req)
	case migrationPlanResource:
		if err := validateRequestType(req, migrationPlanResource, migrationPlanKind); err != nil {
			return err
		}
		if req.Operation != admissionv1.Create {
			return denyf("operator manager may only create immutable PtahMigrationPlan manifests")
		}
		return v.validateMigrationPlanCreate(ctx, req)
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

	owner, ownerKind, err := subjectOwner(job.OwnerReferences)
	if err != nil {
		return denyf("Job does not have one exact operator controller owner: %v", err)
	}
	if ownerKind == "PtahMigration" {
		return v.validateMigrationJobCreate(ctx, job, owner)
	}
	schema, err := v.readSchema(ctx, job.Namespace, owner)
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
	// The webhook request may land on a replica other than the one that
	// dispatched this Job, and every replica generates its own Plan seal key
	// at startup: rebuilding with this replica's own key would refuse a Job
	// this same manager just created. The claim's recorded digest, not
	// byte-equality with this process's key, is what proves live was sealed
	// to an authorized one.
	if err := workload.CarrySealedPlanKey(expected, job, *operation); err != nil {
		return denyf("Job seal key is invalid: %v", err)
	}
	// The claim has to have been made under the binding in force, and the Job
	// has to carry its epoch. The builder refuses a stale binding as well; the
	// matcher holds it without relying on that.
	claim := jobclaim.SchemaOperation(schema, operation)
	claim.Binding = schema.Status.ExecutionBinding
	claim.Built = expected
	if err := jobclaim.Match(job, claim); err != nil {
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
	owner, ownerKind, err := subjectOwner(oldJob.OwnerReferences)
	if err != nil {
		return denyf("old Job does not have one exact operator controller owner: %v", err)
	}
	if ownerKind == "PtahMigration" {
		return v.validateMigrationJobUpdate(ctx, oldJob, job, owner)
	}
	newOwner, newOwnerKind, err := subjectOwner(job.OwnerReferences)
	if err != nil || newOwnerKind != ownerKind || !apiequality.Semantic.DeepEqual(owner, newOwner) {
		return denyf("Job cleanup update changed the PtahSchema controller owner")
	}
	schema, err := v.readSchemaForJobUpdate(ctx, oldJob.Namespace, owner)
	if err != nil {
		return err
	}
	operation := schema.Status.ActiveOperation
	if !jobTerminal(oldJob) &&
		(operation == nil || !mutationlifecycle.SchemaOperation(operation.Type).Mutating ||
			!currentApplyAnnotations(oldJob.Annotations)) {
		return denyf("Job cleanup TTL cannot be set before terminal status")
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
	if !currentOperationAnnotations(oldJob.Annotations) {
		// Every Job the builder makes carries this envelope, and a rebuilt Job
		// is compared annotation for annotation, so rebuilding this claim's
		// Job -- a direct read of its plan first -- could only refuse it.
		return denyf("Job %s does not carry the operation envelope the controller writes on every Job it builds "+
			"(%d annotations for a read-only operation, %d for an Apply, beside what spec.execution.podMetadata declares; it has %d)",
			oldJob.Name, len(operationEnvelopeAnnotations), len(applyOperationAnnotations()), len(oldJob.Annotations))
	}
	if err := validateClaimBoundJobCleanup(schema, operation, oldJob); err != nil {
		return denyf("Job cleanup is outside the persisted operation claim: %v", err)
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
	if schema.Status.ActiveOperation != nil {
		return errors.New("schema carries an active operation, so no retired Apply is pending cleanup")
	}
	if pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		return errors.New("a retired Apply is pending as outcome-unknown, and this one is not")
	}
	retirement := schema.Status.PendingBindingRetirement
	if retirement == nil || retirement.Job == nil || retirement.Job.Operation != operatorv1alpha1.OperationApply ||
		retirement.Job.Name == "" || retirement.Job.Name != pending.ApplyJobName ||
		retirement.Job.UID == "" || retirement.Job.UID != pending.ApplyJobUID {
		return errors.New("no pending execution-binding retirement names this Apply Job")
	}
	if err := validateRetiredEpoch(schema, pending.Plan.ExecutionBindingID); err != nil {
		return fmt.Errorf("Apply Job %w", err)
	}
	return jobclaim.Match(job, jobclaim.PendingApply(schema, pending))
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
	if operation.JobName != expectedName || operation.JobUID == "" {
		return errors.New("the persisted operation claim does not name the Job it reserved")
	}
	claim := jobclaim.SchemaOperation(schema, operation)
	if operation.ExecutionBindingID == schema.Status.ExecutionBinding.Epoch {
		claim.Binding = schema.Status.ExecutionBinding
	} else if err := validateRetiredReadOnlyStatus(schema, operation); err != nil {
		return err
	}
	return jobclaim.Match(job, claim)
}

func validateRetiredReadOnlyStatus(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) error {
	if schema == nil || operation == nil || schema.Status.ExecutionBinding == nil {
		return errors.New("retired operation status is incomplete")
	}
	if !mutationlifecycle.SchemaOperation(operation.Type).ReadOnly() {
		return fmt.Errorf("operation %q is not read-only", operation.Type)
	}
	retirement := schema.Status.PendingBindingRetirement
	if retirement == nil || retirement.Job == nil || retirement.Job.Operation != operation.Type ||
		retirement.Job.Name == "" || retirement.Job.Name != operation.JobName ||
		retirement.Job.UID == "" || retirement.Job.UID != operation.JobUID {
		return errors.New("no pending execution-binding retirement names this operation's Job")
	}
	if err := validateRetiredEpoch(schema, operation.ExecutionBindingID); err != nil {
		return fmt.Errorf("operation %w", err)
	}
	return nil
}

// validateRetiredEpoch holds a retired claim's epoch to the one the pending
// retirement names, and that epoch to one that is no longer in force.
func validateRetiredEpoch(schema *operatorv1alpha1.PtahSchema, epoch string) error {
	retirement := schema.Status.PendingBindingRetirement
	if retirement == nil || schema.Status.ExecutionBinding == nil ||
		!isExecutionBindingID(schema.Status.ExecutionBinding.Epoch) ||
		!isExecutionBindingID(retirement.RetiredEpoch) || epoch != retirement.RetiredEpoch ||
		retirement.RetiredEpoch == schema.Status.ExecutionBinding.Epoch {
		return errors.New("does not belong to the execution epoch the pending retirement retired")
	}
	return nil
}

// operationEnvelopeAnnotations are the keys every operation Job carries.
var operationEnvelopeAnnotations = []string{
	workload.AnnotationOperationID,
	workload.AnnotationInputFingerprint,
	workload.AnnotationPtahVersion,
	workload.AnnotationExecutionBindingID,
	workload.AnnotationControllerImage,
	workload.AnnotationControllerRevision,
	workload.AnnotationControllerStateVersion,
	workload.AnnotationAdmissionSnapshotDigest,
}

// currentOperationAnnotations reports whether a Job carries the keys the
// builder writes -- the envelope alone for a read-only operation, or the
// Apply set -- and beyond them only what spec.execution.podMetadata may
// declare.
func currentOperationAnnotations(annotations map[string]string) bool {
	return carriesEnvelopeKeys(annotations, operationEnvelopeAnnotations) || currentApplyAnnotations(annotations)
}

// currentApplyAnnotations reports whether a Job carries the keys the builder
// writes on a schema Apply, and beyond them only declared ones.
func currentApplyAnnotations(annotations map[string]string) bool {
	return carriesEnvelopeKeys(annotations, applyOperationAnnotations())
}

// applyOperationAnnotations are the keys the builder writes on a schema
// Apply: the envelope, the plan binding, and the mutating-operation marks.
func applyOperationAnnotations() []string {
	keys := append([]string{workload.AnnotationPlanFingerprint, workload.AnnotationPlanContentDigest},
		operationEnvelopeAnnotations...)
	marks := map[string]string{}
	workload.MarkMutatingOperation(marks)
	for key := range marks {
		keys = append(keys, key)
	}
	return keys
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
	if err := v.validateApplyProjection(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
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
	// The plan records the manager that publishes it, and only this one is
	// publishing. Its chunks are held to the plan as it stands instead, so a
	// successor can finish a publication its predecessor began.
	if controllerImage, controllerRevision, runnerImage := v.Jobs.ManagerIdentity(); plan.Spec.ControllerImage != controllerImage ||
		plan.Spec.ControllerRevision != controllerRevision || plan.Spec.RunnerImage != runnerImage {
		return denyf("PtahSchemaPlan does not record the manager that publishes it")
	}
	if err := v.validatePlanSourceJob(ctx, schema); err != nil {
		return err
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
			int64(ref.Size) != expectedSize || !isSHA256Digest(ref.Digest) {
			return fmt.Errorf("plan chunk %d is not the deterministic size, name, index, and digest tuple", index)
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
	wantFingerprint, err := planstore.Binding(schema.UID, plan.Spec).Fingerprint()
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
		binding.ControllerStateVersion != plan.Spec.ControllerStateVersion || binding.PtahVersion != plan.Spec.PtahVersion ||
		binding.ExecutorImage != plan.Spec.ExecutorImage ||
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
	// An earlier manager of the same execution binding may have dispatched the
	// Plan Job. Its recorded identity is taken from the Job, and the snapshot
	// check that follows holds what was taken to the claim.
	workload.CarryManagerIdentity(expected, job)
	// The same manager may have restarted between dispatching this Plan Job
	// and this validation, generating a new seal key; or a different replica
	// dispatched it. Either way this process's own key is not what live was
	// sealed to. Checked against the claim's recorded digest, not trusted
	// outright, for the same reason CarrySealedPlanKey documents.
	if err := workload.CarrySealedPlanKey(expected, job, *operation); err != nil {
		return denyf("terminal Plan Job seal key is invalid: %v", err)
	}
	claim := jobclaim.SchemaOperation(schema, operation)
	claim.Binding = schema.Status.ExecutionBinding
	claim.Built, claim.Stored = expected, true
	if err := jobclaim.Match(job, claim); err != nil {
		return denyf("terminal Plan Job is outside its active Plan operation intent: %v", err)
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
	if job, isJob := object.(*batchv1.Job); isJob {
		if field := unreviewedJobField(job); field != "" {
			return badRequestf("decode %s candidate strictly: unknown field %q: the Job contract this webhook enforces was reviewed without it",
				kind.Kind, field)
		}
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

// SchemaPolicyFingerprint is the policy binding this admission side requires a
// plan to carry, exported for the fixture that stands up a plan it must accept.
// A fixture that spells the policy a second time agrees with nothing the day
// the policy grows a field.
func SchemaPolicyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
	return schemaPolicyFingerprint(schema)
}

func schemaPolicyFingerprint(schema *operatorv1alpha1.PtahSchema) (string, error) {
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
		DriftSeverity:   schema.Spec.Policy.DriftSeverity,
		Exclude:         fingerprint.NormalizeSet(schema.Spec.Policy.Exclude),
		ProtectedTables: fingerprint.NormalizeSet(schema.Spec.Policy.ProtectedTables),
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
