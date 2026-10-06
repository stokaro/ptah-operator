package resultcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodBindings preserves the first Pod authorized for an operation. Its immutable
// record contains public identity only: no token, certificate, or private key.
// TokenReview authenticates requests separately. Neither operation can replace
// this pin, and deleting the original Pod cannot enroll a replacement.
type PodBindings struct {
	Writer client.Client
	Reader client.Reader
}

func podBindingRecord(identity resultdelivery.Identity) (*api.PtahResultRecord, error) {
	if _, err := resultdelivery.CertificateURI(identity); err != nil {
		return nil, ErrCredential
	}
	data, err := json.Marshal(identity)
	if err != nil || len(data) > 4<<10 {
		return nil, ErrCredential
	}
	b := identity.Binding
	return &api.PtahResultRecord{
		ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace,
			Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName), Annotations: annotations(identity),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator", "app.kubernetes.io/component": "result-credential"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: b.Kind, Name: b.Name,
				UID: b.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}},
		},
		Spec: api.PtahResultRecordSpec{Type: "credential", Data: data},
	}, nil
}

// decodePodBinding also serves retention after the Pod, Job, or operation has
// disappeared. Decoding an admission-protected record grants no live authority.
func decodePodBinding(record *api.PtahResultRecord, stored bool) (resultdelivery.Identity, error) {
	if record == nil || record.Spec.Type != "credential" || len(record.Spec.Data) == 0 || len(record.Spec.Data) > 4<<10 ||
		record.GenerateName != "" || stored && record.UID == "" || len(record.Finalizers) != 0 && !foregroundDeleting(record.ObjectMeta) {
		return resultdelivery.Identity{}, ErrCredential
	}
	var identity resultdelivery.Identity
	if err := json.Unmarshal(record.Spec.Data, &identity); err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	expected, err := podBindingRecord(identity)
	if err != nil || !bytes.Equal(record.Spec.Data, expected.Spec.Data) || record.Namespace != expected.Namespace || record.Name != expected.Name ||
		!reflect.DeepEqual(record.Labels, expected.Labels) || !reflect.DeepEqual(record.Annotations, expected.Annotations) ||
		!reflect.DeepEqual(record.OwnerReferences, expected.OwnerReferences) {
		return resultdelivery.Identity{}, ErrCredential
	}
	return identity, nil
}

func (p PodBindings) Ensure(ctx context.Context, identity resultdelivery.Identity) (Credential, error) {
	if p.Writer == nil || p.Reader == nil {
		return Credential{}, ErrCredential
	}
	check := (resultauthority.Authorizer{Reader: p.Reader}).Check
	// Reading the immutable pin grants no authority. Check before creating a
	// missing pin and after verifying the persisted winner, just before return.
	expected, err := podBindingRecord(identity)
	if err != nil {
		return Credential{}, err
	}
	key := client.ObjectKeyFromObject(expected)
	stored := &api.PtahResultRecord{}
	err = p.Reader.Get(ctx, key, stored)
	if apierrors.IsNotFound(err) {
		if err := check(ctx, identity); err != nil {
			return Credential{}, err
		}
		if err := p.Writer.Create(ctx, expected); err != nil && !apierrors.IsAlreadyExists(err) {
			return Credential{}, err
		}
		// The persisted winner, not a CREATE response, fixes the first Pod.
		err = p.Reader.Get(ctx, key, stored)
	}
	if err != nil {
		return Credential{}, err
	}
	bound, err := decodePodBinding(stored, true)
	if err != nil || bound != identity || !stored.DeletionTimestamp.IsZero() {
		return Credential{}, ErrCredential
	}
	if err := check(ctx, identity); err != nil {
		return Credential{}, err
	}
	return Credential{Name: stored.Name, UID: stored.UID}, nil
}

func (p PodBindings) ValidateRecordCreate(ctx context.Context, record *api.PtahResultRecord) error {
	identity, err := decodePodBinding(record, false)
	if err != nil || !record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 0 {
		return ErrCredential
	}
	return (resultauthority.Authorizer{Reader: p.Reader}).Check(ctx, identity)
}

func (p PodBindings) AuthorizePublication(ctx context.Context, binding resultstore.Binding) (resultdelivery.Identity, error) {
	if p.Reader == nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	if _, err := resultstore.Name(binding); err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	stored := &api.PtahResultRecord{}
	key := client.ObjectKey{Namespace: binding.Namespace, Name: jobconfig.CredentialName(binding.UID, binding.OperationID, binding.JobName)}
	if err := p.Reader.Get(ctx, key, stored); err != nil {
		if apierrors.IsNotFound(err) {
			return resultdelivery.Identity{}, resultauthority.ErrNotReady
		}
		return resultdelivery.Identity{}, err
	}
	identity, err := decodePodBinding(stored, true)
	if err != nil || identity.Binding != binding || !stored.DeletionTimestamp.IsZero() {
		return resultdelivery.Identity{}, ErrCredential
	}
	if err := (resultauthority.Authorizer{Reader: p.Reader}).Check(ctx, identity); err != nil {
		return resultdelivery.Identity{}, err
	}
	return identity, nil
}

func validatePodBindingDelete(ctx context.Context, reader client.Reader, record *api.PtahResultRecord) error {
	identity, err := decodePodBinding(record, true)
	if err != nil {
		return err
	}
	return resultretention.CheckUnpinned(ctx, reader, identity.Binding)
}
