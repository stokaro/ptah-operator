package resultcredentials

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"maps"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
)

// ValidateCreate checks the submitted bytes using the issuer's trust, without
// signing or writing anything. The caller must authenticate the manager and
// check the admission request's namespace/name. A missing issuer fails closed.
func (i *Issuer) ValidateCreate(ctx context.Context, secret *corev1.Secret) error {
	if i == nil || secret == nil || secret.GenerateName != "" || len(secret.Finalizers) != 0 || len(secret.StringData) != 0 {
		return ErrCredential
	}
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return ErrCredential
	}
	identity, err := resultdelivery.ClientIdentity(certificate)
	if err != nil {
		return ErrCredential
	}
	record := &api.PtahResultRecord{}
	if err := i.reader.Get(ctx, client.ObjectKeyFromObject(secret), record); err != nil {
		return err
	}
	stored, err := recordSecret(record)
	if err != nil {
		return err
	}
	if _, err := i.validate(stored, identity); err != nil {
		return err
	}
	if !maps.EqualFunc(stored.Data, secret.Data, bytes.Equal) || !reflect.DeepEqual(stored.Labels, secret.Labels) || !reflect.DeepEqual(stored.Annotations, secret.Annotations) || !reflect.DeepEqual(credentialProjection(stored).OwnerReferences, secret.OwnerReferences) {
		return ErrCredential
	}
	// Validate all remaining projection fields against the operation identity.
	// Its owner differs deliberately: the record retains the resource binding,
	// while the kubelet Secret pins that exact canonical record's UID.
	candidate := secret.DeepCopy()
	candidate.OwnerReferences = stored.OwnerReferences
	if candidate.UID == "" {
		candidate.UID = "admission-only"
	}
	if _, err := i.validate(candidate, identity); err != nil {
		return err
	}
	return (resultauthority.Authorizer{Reader: i.reader}).Check(ctx, identity)
}

// ValidateRecordCreate authenticates the canonical credential before it exists.
// The caller checks the manager identity and exact admission resource binding.
func (i *Issuer) ValidateRecordCreate(ctx context.Context, record *api.PtahResultRecord) error {
	if i == nil {
		return ErrCredential
	}
	secret, err := recordSecret(record)
	if err != nil {
		return err
	}
	if !bytes.Equal(secret.Data["ca.crt"], i.serverTrust) {
		return ErrCredential
	}
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return ErrCredential
	}
	identity, err := resultdelivery.ClientIdentity(certificate)
	if err != nil {
		return ErrCredential
	}
	if secret.UID == "" {
		secret.UID = "admission-only"
	}
	if len(secret.Labels) != 2 {
		return ErrCredential
	}
	if _, err := i.validate(secret, identity); err != nil {
		return err
	}
	// Trust overlap permits already-issued credentials, not enrollment by a
	// retired signer. A stale replica and a current replica must make the same
	// refusal after the rotator advances the direct-read policy.
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.CheckSignatureFrom(i.ca) != nil {
		return ErrCredential
	}
	if err := (resultauthority.Authorizer{Reader: i.reader}).Check(ctx, identity); err != nil {
		return err
	}
	return i.CheckEnrollment(ctx)
}

// ValidateRecordUpdate freezes metadata as well as the CRD's immutable spec.
func ValidateRecordUpdate(old, next *api.PtahResultRecord) error {
	if old == nil || next == nil || !reflect.DeepEqual(old.Spec, next.Spec) {
		return ErrCredential
	}
	if _, err := decodePodBinding(old, true); err == nil {
		if _, err := decodePodBinding(next, true); err != nil {
			return err
		}
		return ValidateUpdate(&corev1.Secret{ObjectMeta: *old.ObjectMeta.DeepCopy()}, &corev1.Secret{ObjectMeta: *next.ObjectMeta.DeepCopy()})
	}
	a, err := recordSecret(old)
	if err != nil {
		return err
	}
	b, err := recordSecret(next)
	if err != nil {
		return err
	}
	return ValidateUpdate(a, b)
}

func ValidateRecordDelete(ctx context.Context, reader client.Reader, record *api.PtahResultRecord) error {
	if _, err := decodePodBinding(record, true); err == nil {
		return validatePodBindingDelete(ctx, reader, record)
	}
	secret, err := recordSecret(record)
	if err != nil {
		return err
	}
	return ValidateDelete(ctx, reader, secret)
}

// ValidateUpdate protects the public binding and owner as well as the key.
// Kubernetes' immutable Secret flag protects data only, not this metadata.
func ValidateUpdate(old, next *corev1.Secret) error {
	if old == nil || next == nil || old.UID == "" || old.UID != next.UID {
		return ErrCredential
	}
	a, b := old.DeepCopy(), next.DeepCopy()
	// Foreground DELETE adds this API-controlled finalizer. The garbage
	// collector must be able to remove it without changing the frozen binding.
	if foregroundDeleting(a.ObjectMeta) && !b.DeletionTimestamp.IsZero() && len(b.Finalizers) == 0 {
		a.Finalizers = b.Finalizers
	}
	// API bookkeeping may change on an otherwise identical write. Deletion
	// state is API-controlled and DELETE is separately checked below.
	a.ResourceVersion, b.ResourceVersion = "", ""
	a.ManagedFields, b.ManagedFields = nil, nil
	a.DeletionTimestamp, b.DeletionTimestamp = nil, nil
	a.DeletionGracePeriodSeconds, b.DeletionGracePeriodSeconds = nil, nil
	if !reflect.DeepEqual(a, b) {
		return ErrCredential
	}
	return nil
}

func foregroundDeleting(meta metav1.ObjectMeta) bool {
	return !meta.DeletionTimestamp.IsZero() && len(meta.Finalizers) == 1 && meta.Finalizers[0] == metav1.FinalizerDeleteDependents
}

// ValidateDelete retains the attempt's first-Pod pin until the owning resource
// retires that operation or disappears. Pod/Job loss, expiry, spec changes and
// lease loss alone must never permit deleting and reminting this credential.
// Reader must bypass the cache. The owner may be terminating: active work still
// needs its pin, while a retired operation must not obstruct foreground GC.
func ValidateDelete(ctx context.Context, reader client.Reader, secret *corev1.Secret) error {
	if reader == nil || secret == nil || len(secret.OwnerReferences) != 1 || secret.Annotations[AnnotationOperationID] == "" {
		return ErrCredential
	}
	owner := secret.OwnerReferences[0]
	if owner.APIVersion != api.GroupVersion.String() || owner.UID == "" || owner.Name == "" || owner.Controller == nil || !*owner.Controller {
		return ErrCredential
	}
	if owner.Kind == "PtahResultRecord" {
		if owner.Name != secret.Name || owner.BlockOwnerDeletion == nil || *owner.BlockOwnerDeletion {
			return ErrCredential
		}
		record := &api.PtahResultRecord{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: secret.Namespace, Name: owner.Name}, record)
		if err == nil && record.UID == owner.UID {
			// The canonical record is the cleanup authority. Retiring SQL
			// authority alone must not orphan its still-retained projection.
			return errors.New("result credential record is still retained")
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	identity, err := storedIdentity(secret)
	if err != nil {
		return err
	}
	b := identity.Binding
	if owner.Kind != "PtahResultRecord" && (owner.Kind != b.Kind || owner.Name != b.Name || owner.UID != b.UID) {
		return ErrCredential
	}
	// Parent loss or replacement is not proof that recovery has finished.
	return resultretention.CheckUnpinned(ctx, reader, b)
}

// StoredRecordIdentity reads the immutable binding without granting delivery
// authority. Cleanup uses it after certificate or CA expiry. The caller must
// read the persisted, admission-protected record directly from the API.
func StoredRecordIdentity(record *api.PtahResultRecord) (resultdelivery.Identity, error) {
	if identity, err := decodePodBinding(record, true); err == nil {
		return identity, nil
	}
	secret, err := recordSecret(record)
	if err != nil || record.UID == "" {
		return resultdelivery.Identity{}, ErrCredential
	}
	identity, err := storedIdentity(secret)
	if err != nil {
		return resultdelivery.Identity{}, err
	}
	b := identity.Binding
	if len(secret.OwnerReferences) != 1 {
		return resultdelivery.Identity{}, ErrCredential
	}
	owner := secret.OwnerReferences[0]
	if owner.APIVersion != api.GroupVersion.String() || owner.Kind != b.Kind || owner.Name != b.Name || owner.UID != b.UID || owner.Controller == nil || !*owner.Controller || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return resultdelivery.Identity{}, ErrCredential
	}
	return identity, nil
}

func storedIdentity(secret *corev1.Secret) (resultdelivery.Identity, error) {
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	identity, err := resultdelivery.StoredClientIdentity(certificate)
	if err != nil {
		return resultdelivery.Identity{}, ErrCredential
	}
	b := identity.Binding
	if secret.Namespace != b.Namespace || secret.Name != jobconfig.CredentialName(b.UID, b.OperationID, b.JobName) ||
		!reflect.DeepEqual(secret.Annotations, annotations(identity)) {
		return resultdelivery.Identity{}, ErrCredential
	}
	return identity, nil
}
