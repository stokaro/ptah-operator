package podintent

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// ValidationHandler rejects an operator Pod before it can be scheduled when
// its final, post-mutation PodSpec falls outside the persisted admission
// snapshot. It reads only credential-free admission objects, Jobs, and
// PtahSchema status; it never reads Secrets.
type ValidationHandler struct {
	Reader  client.Reader
	Decoder cradmission.Decoder
}

// Handle implements controller-runtime admission.Handler.
func (h *ValidationHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("Pod intent webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Allowed("request does not mutate a Pod")
	}
	if req.SubResource != "" && req.SubResource != "ephemeralcontainers" && req.SubResource != "resize" {
		return cradmission.Denied("unexpected Pod subresource for operation-intent validation")
	}
	if req.Operation == admissionv1.Create && req.SubResource != "" {
		return cradmission.Denied("Pod subresources cannot be created through the operation-intent webhook")
	}
	pod := &corev1.Pod{}
	if err := h.Decoder.Decode(req, pod); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode Pod: %w", err))
	}
	if pod.Namespace == "" || pod.Namespace != req.Namespace {
		return cradmission.Denied("Pod namespace does not match the admission request")
	}
	var oldPod *corev1.Pod
	if req.Operation == admissionv1.Update {
		oldPod = &corev1.Pod{}
		if len(req.OldObject.Raw) == 0 {
			return cradmission.Denied("Pod update has no old object")
		}
		if err := h.Decoder.DecodeRaw(req.OldObject, oldPod); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old Pod: %w", err))
		}
		if req.Name == "" || pod.Name != req.Name || oldPod.Name != req.Name ||
			oldPod.Namespace != req.Namespace || pod.UID == "" || pod.UID != oldPod.UID {
			return cradmission.Denied("Pod update does not preserve the exact request name, namespace, and UID")
		}
		if exactJobTrackingFinalizerRemoval(oldPod, pod, req.SubResource) {
			return cradmission.Allowed("Job controller removed only its Pod tracking finalizer")
		}
	}

	jobOwner, ok := uniqueControllerReference(pod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job")
	if !ok {
		if managedPodIdentity(pod.Labels) || oldPod != nil && managedPodIdentity(oldPod.Labels) {
			return cradmission.Denied("managed Pod has no exact Job controller identity")
		}
		if oldPod != nil {
			if _, wasJobPod := uniqueControllerReference(oldPod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job"); wasJobPod {
				return cradmission.Denied("Pod update removed its exact Job owner")
			}
		}
		return cradmission.Allowed("Pod is not controlled by a Job")
	}
	job := &batchv1.Job{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: jobOwner.Name}, job); err != nil {
		return podIntentReadError("read owning Job", err)
	}
	if job.UID == "" || job.UID != jobOwner.UID {
		return cradmission.Denied("managed Pod owner does not match the current Job UID")
	}
	schemaOwner, ok := exactControllerReference(job.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema")
	if !ok {
		if managedPodIdentity(pod.Labels) || managedPodIdentity(job.Labels) ||
			oldPod != nil && managedPodIdentity(oldPod.Labels) {
			return cradmission.Denied("managed Pod Job has no exact PtahSchema controller identity")
		}
		return cradmission.Allowed("Job is not an operator schema workload")
	}
	if err := validateJobExecutionEnvelope(job); err != nil {
		return cradmission.Denied("managed Pod Job is outside the one-shot execution envelope: " + err.Error())
	}
	if len(pod.OwnerReferences) != 1 {
		return cradmission.Denied("managed Pod has ambiguous ownership")
	}
	if req.Operation == admissionv1.Create {
		if !apiequality.Semantic.DeepEqual(pod.Finalizers, []string{batchv1.JobTrackingFinalizer}) {
			return cradmission.Denied("managed Pod does not have the exact Job tracking finalizer")
		}
	} else if oldPod == nil || !apiequality.Semantic.DeepEqual(pod.Finalizers, oldPod.Finalizers) {
		return cradmission.Denied("managed Pod update changed finalizers outside Job cleanup")
	}
	if pod.Labels[workload.LabelManagedBy] != "ptah-operator" ||
		pod.Labels[workload.LabelComponent] != "schema-operation" {
		return cradmission.Denied("operator Pod removed its managed workload identity")
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: schemaOwner.Name}, schema); err != nil {
		return podIntentReadError("read owning PtahSchema", err)
	}
	if schema.UID == "" || schema.UID != schemaOwner.UID {
		return cradmission.Denied("managed Job owner does not match the current PtahSchema UID")
	}
	for key, value := range map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
		batchv1.JobNameLabel:       job.Name,
		"controller-uid":           string(job.UID),
		"job-name":                 job.Name,
	} {
		if job.Spec.Template.Labels[key] != value || pod.Labels[key] != value {
			return cradmission.Denied("managed Pod does not carry the exact generated Job identity")
		}
	}
	if req.Operation == admissionv1.Create {
		if !apiequality.Semantic.DeepEqual(pod.Labels, job.Spec.Template.Labels) ||
			!createAnnotationsMatch(pod.Annotations, job.Spec.Template.Annotations, operationSnapshot(schema)) {
			return cradmission.Denied("managed Pod metadata does not match the bound Job template")
		}
	} else if !containsStringMap(pod.Labels, job.Spec.Template.Labels) ||
		!containsStringMap(pod.Annotations, job.Spec.Template.Annotations) {
		return cradmission.Denied("managed Pod update changed bound Job metadata")
	}
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.AdmissionSnapshot == nil {
		return cradmission.Denied("managed Pod has no persisted admission snapshot")
	}
	if operation.JobName != job.Name || operation.JobUID != "" && operation.JobUID != job.UID {
		return cradmission.Denied("managed Pod does not match the active operation Job identity")
	}
	if operation.ID == "" || pod.Annotations[workload.AnnotationOperationID] != operation.ID ||
		job.Annotations[workload.AnnotationOperationID] != operation.ID ||
		pod.Labels[workload.LabelOperationID] != workload.OperationIDLabelValue(operation.ID) {
		return cradmission.Denied("managed Pod operation identity does not match status")
	}
	if pod.Labels[workload.LabelSchema] != schema.Name ||
		pod.Labels[workload.LabelOperation] != strings.ToLower(string(operation.Type)) {
		return cradmission.Denied("managed Pod operation labels do not match status")
	}
	digest := operation.AdmissionSnapshot.Digest
	if digest == "" || pod.Annotations[workload.AnnotationAdmissionSnapshotDigest] != digest ||
		job.Annotations[workload.AnnotationAdmissionSnapshotDigest] != digest ||
		job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] != digest {
		return cradmission.Denied("managed Pod admission digest does not match status and Job")
	}
	current, err := Resolve(ctx, h.Reader, pod.Namespace, &job.Spec.Template, Options{
		DefaultTolerationsEnabled:           operation.AdmissionSnapshot.DefaultTolerationsEnabled,
		DefaultNotReadyTolerationSeconds:    operation.AdmissionSnapshot.DefaultNotReadyTolerationSeconds,
		DefaultUnreachableTolerationSeconds: operation.AdmissionSnapshot.DefaultUnreachableTolerationSeconds,
		ExtendedResourceTolerationEnabled:   operation.AdmissionSnapshot.ExtendedResourceTolerationEnabled,
		AlwaysPullImagesEnabled:             operation.AdmissionSnapshot.AlwaysPullImagesEnabled,
	})
	if err != nil {
		return podIntentReadError("re-resolve current Pod admission bindings", err)
	}
	if current.Digest != digest {
		return cradmission.Denied("current Pod admission objects differ from the persisted snapshot")
	}
	if err := ValidatePodSpec(&pod.Spec, &job.Spec.Template, operation.AdmissionSnapshot); err != nil {
		return cradmission.Denied("managed Pod is outside the persisted admission envelope: " + err.Error())
	}
	return cradmission.Allowed("managed Pod matches the persisted admission envelope")
}

func managedPodIdentity(labels map[string]string) bool {
	return labels[workload.LabelManagedBy] == "ptah-operator" &&
		labels[workload.LabelComponent] == "schema-operation"
}

func validateJobExecutionEnvelope(job *batchv1.Job) error {
	if job == nil || job.UID == "" {
		return fmt.Errorf("Job identity is incomplete")
	}
	if job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 ||
		job.Spec.Completions == nil || *job.Spec.Completions != 1 {
		return fmt.Errorf("parallelism and completions must both be exactly one")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		return fmt.Errorf("backoff limit must be exactly zero")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 ||
		job.Spec.Template.Spec.ActiveDeadlineSeconds == nil ||
		*job.Spec.ActiveDeadlineSeconds != *job.Spec.Template.Spec.ActiveDeadlineSeconds {
		return fmt.Errorf("Job and Pod active deadlines must be equal and positive")
	}
	if job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed {
		return fmt.Errorf("Pod replacement policy must wait for a failed Pod")
	}
	if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode != batchv1.NonIndexedCompletion {
		return fmt.Errorf("completion mode must be non-indexed")
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		return fmt.Errorf("Job must not be suspended")
	}
	if job.Spec.ManualSelector != nil && *job.Spec.ManualSelector {
		return fmt.Errorf("manual Job selectors are not permitted")
	}
	if job.Spec.PodFailurePolicy != nil || job.Spec.SuccessPolicy != nil ||
		job.Spec.BackoffLimitPerIndex != nil || job.Spec.MaxFailedIndexes != nil {
		return fmt.Errorf("indexed and custom completion policies are not permitted")
	}
	if job.Spec.ManagedBy != nil {
		return fmt.Errorf("a custom Job controller is not permitted")
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		return fmt.Errorf("Job cleanup policy must not change before Pod admission")
	}
	if err := validateGeneratedJobSelector(job); err != nil {
		return err
	}
	return nil
}

func validateGeneratedJobSelector(job *batchv1.Job) error {
	if job.Spec.Selector == nil || len(job.Spec.Selector.MatchLabels) == 0 ||
		len(job.Spec.Selector.MatchExpressions) != 0 {
		return fmt.Errorf("Job selector is not the generated UID selector")
	}
	for key, value := range job.Spec.Selector.MatchLabels {
		if key != batchv1.ControllerUidLabel && key != "controller-uid" {
			return fmt.Errorf("Job selector contains a non-UID label")
		}
		if value != string(job.UID) {
			return fmt.Errorf("Job selector does not match the current Job UID")
		}
	}
	return nil
}

func containsStringMap(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func exactControllerReference(
	references []metav1.OwnerReference,
	apiVersion, kind string,
) (metav1.OwnerReference, bool) {
	if len(references) != 1 {
		return metav1.OwnerReference{}, false
	}
	reference := references[0]
	if reference.APIVersion != apiVersion || reference.Kind != kind || reference.Name == "" || reference.UID == "" ||
		reference.Controller == nil || !*reference.Controller {
		return metav1.OwnerReference{}, false
	}
	return reference, true
}

func uniqueControllerReference(
	references []metav1.OwnerReference,
	apiVersion, kind string,
) (metav1.OwnerReference, bool) {
	var found *metav1.OwnerReference
	for i := range references {
		reference := &references[i]
		if reference.APIVersion != apiVersion || reference.Kind != kind || reference.Name == "" || reference.UID == "" ||
			reference.Controller == nil || !*reference.Controller {
			continue
		}
		if found != nil {
			return metav1.OwnerReference{}, false
		}
		found = reference
	}
	if found == nil {
		return metav1.OwnerReference{}, false
	}
	return *found, true
}

func operationSnapshot(schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PodAdmissionSnapshot {
	if schema == nil || schema.Status.ActiveOperation == nil {
		return nil
	}
	return schema.Status.ActiveOperation.AdmissionSnapshot
}

func exactJobTrackingFinalizerRemoval(oldPod, newPod *corev1.Pod, subresource string) bool {
	if oldPod == nil || newPod == nil || subresource != "" {
		return false
	}
	oldOwner, oldOK := exactControllerReference(oldPod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job")
	newOwner, newOK := exactControllerReference(newPod.OwnerReferences, batchv1.SchemeGroupVersion.String(), "Job")
	if !oldOK || !newOK || !apiequality.Semantic.DeepEqual(oldOwner, newOwner) {
		return false
	}
	wantFinalizers := make([]string, 0, len(oldPod.Finalizers))
	removed := 0
	for _, finalizer := range oldPod.Finalizers {
		if finalizer == batchv1.JobTrackingFinalizer {
			removed++
			continue
		}
		wantFinalizers = append(wantFinalizers, finalizer)
	}
	if removed != 1 || !apiequality.Semantic.DeepEqual(newPod.Finalizers, wantFinalizers) {
		return false
	}
	oldCopy := oldPod.DeepCopy()
	newCopy := newPod.DeepCopy()
	oldCopy.Finalizers = append([]string(nil), wantFinalizers...)
	return apiequality.Semantic.DeepEqual(oldCopy, newCopy)
}

func createAnnotationsMatch(
	actual, expected map[string]string,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) bool {
	normalized := make(map[string]string, len(actual))
	for key, value := range actual {
		normalized[key] = value
	}
	const limitRangerAnnotation = "kubernetes.io/limit-ranger"
	if value, exists := normalized[limitRangerAnnotation]; exists {
		if !snapshotHasLimitRangeDefaults(snapshot) || !strings.HasPrefix(value, "LimitRanger plugin set: ") {
			return false
		}
		delete(normalized, limitRangerAnnotation)
	}
	return apiequality.Semantic.DeepEqual(normalized, expected)
}

func snapshotHasLimitRangeDefaults(snapshot *operatorv1alpha1.PodAdmissionSnapshot) bool {
	if snapshot == nil {
		return false
	}
	for _, limitRange := range snapshot.LimitRanges {
		if len(limitRange.DefaultRequests) != 0 || len(limitRange.DefaultLimits) != 0 {
			return true
		}
	}
	return false
}

func podIntentReadError(action string, err error) cradmission.Response {
	status := int32(http.StatusInternalServerError)
	if apierrors.IsNotFound(err) {
		status = http.StatusConflict
	}
	return cradmission.Errored(status, fmt.Errorf("%s: %w", action, err))
}
