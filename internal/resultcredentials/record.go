package resultcredentials

import (
	"bytes"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The credential record is authoritative. The Secret is only a kubelet
// projection; neither issuance nor the Pod pin requires Secret GET permission.
func credentialRecord(secret *corev1.Secret) (*api.PtahResultRecord, error) {
	data, err := json.Marshal(secret.Data)
	if err != nil {
		return nil, err
	}
	return &api.PtahResultRecord{ObjectMeta: *secret.ObjectMeta.DeepCopy(),
		Spec: api.PtahResultRecordSpec{Type: "credential", Data: data}}, nil
}

func recordSecret(record *api.PtahResultRecord) (*corev1.Secret, error) {
	if record == nil || record.Spec.Type != "credential" || len(record.Spec.Data) == 0 || len(record.Spec.Data) > 512<<10 || record.GenerateName != "" {
		return nil, ErrCredential
	}
	if len(record.Finalizers) != 0 && !foregroundDeleting(record.ObjectMeta) {
		return nil, ErrCredential
	}
	var data map[string][]byte
	if err := json.Unmarshal(record.Spec.Data, &data); err != nil {
		return nil, ErrCredential
	}
	canonical, err := json.Marshal(data)
	if err != nil || !bytes.Equal(canonical, record.Spec.Data) {
		return nil, ErrCredential
	}
	return &corev1.Secret{ObjectMeta: *record.ObjectMeta.DeepCopy(),
		Type: corev1.SecretTypeTLS, Immutable: ptr.To(true), Data: data}, nil
}

// The projection belongs to the canonical record, not the operation resource.
// Deleting an eligible record lets Kubernetes collect its exact Secret without
// granting the manager Secret GET or DELETE. No Job owns either object.
func credentialProjection(secret *corev1.Secret) *corev1.Secret {
	out := secret.DeepCopy()
	out.ObjectMeta = metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace,
		Labels: out.Labels, Annotations: out.Annotations, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: api.GroupVersion.String(), Kind: "PtahResultRecord", Name: secret.Name,
			UID: secret.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
		}}}
	return out
}
