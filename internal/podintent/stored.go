package podintent

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// ValidateStoredPod holds a Pod to the persisted admission snapshot and exact
// owning Job. Controllers and result delivery use the same readback contract.
func ValidateStoredPod(
	pod *corev1.Pod,
	job *batchv1.Job,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	if pod == nil || job == nil || job.UID == "" ||
		!storedPodOwner(pod.OwnerReferences, job) {
		return fmt.Errorf("Pod is not controller-owned by the exact Job UID")
	}
	if err := ValidateGeneratedPodName(pod, job.Name); err != nil {
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
	if err := ValidatePodSpec(&pod.Spec, &job.Spec.Template, snapshot); err != nil {
		return fmt.Errorf("Pod workload spec does not match the validated Job template: %w", err)
	}
	return nil
}

func storedPodOwner(references []metav1.OwnerReference, job *batchv1.Job) bool {
	if len(references) != 1 {
		return false
	}
	ref := references[0]
	return ref.APIVersion == batchv1.SchemeGroupVersion.String() && ref.Kind == "Job" && ref.Name == job.Name && ref.UID == job.UID &&
		ref.Controller != nil && *ref.Controller && ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion
}

// ValidateActiveJob checks the one-shot execution envelope used at Pod
// admission. A Job already marked for cleanup cannot start a new publication.
func ValidateActiveJob(job *batchv1.Job) error { return validateJobExecutionEnvelope(job) }
