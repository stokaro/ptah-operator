package podintent

import (
	"context"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials/binding"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
)

// Called only after the complete Job/Pod admission envelope has been checked.
// The metadata read uses the dedicated result API, never Secrets. The issuer
// and record webhook protect the canonical original-Pod pin.
func (h *ValidationHandler) validateResultCredential(ctx context.Context, operation admissionv1.Operation, pod *corev1.Pod, job *batchv1.Job, operationID string) *cradmission.Response {
	refs := binding.References(pod)
	if len(refs) == 0 && !jobconfig.UsesPodToken(job) {
		return nil
	}
	deny := func(message string) *cradmission.Response { r := cradmission.Denied(message); return &r }
	if len(job.OwnerReferences) != 1 {
		return deny("result credential requires one operation owner")
	}
	projection, err := jobconfig.Read(job, job.OwnerReferences[0].UID, operationID)
	if err != nil || projection.PodToken && len(refs) != 0 || !projection.PodToken && (len(refs) != 1 || refs[0] != projection.SecretName) {
		return deny("Pod does not have the exact result credential projection")
	}
	metadata := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "PtahResultRecord"}}
	err = h.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: projection.SecretName}, metadata)
	if apierrors.IsNotFound(err) {
		// The first Pod must exist before its UID can be pinned. Token runners
		// wait at receiver preflight; legacy runners wait for their Secret.
		return nil
	}
	if err != nil {
		r := cradmission.Errored(http.StatusServiceUnavailable, err)
		return &r
	}
	if operation == admissionv1.Create {
		return deny("result credential is already bound to the original Pod")
	}
	if !metadata.DeletionTimestamp.IsZero() || pod.UID == "" || metadata.Annotations[binding.PodUID] != string(pod.UID) || metadata.Annotations[binding.PodName] != pod.Name || metadata.Annotations[binding.JobUID] != string(job.UID) || metadata.Annotations[binding.OperationID] != operationID {
		return deny("result credential does not belong to this Pod")
	}
	return nil
}
