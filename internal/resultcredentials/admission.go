package resultcredentials

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
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
	// A CREATE review may precede UID assignment. This placeholder is confined
	// to validation; neither the persisted Secret nor its certificate uses it.
	candidate := secret.DeepCopy()
	if candidate.UID == "" {
		candidate.UID = "admission-only"
	}
	if _, err := i.validate(candidate, identity); err != nil {
		return err
	}
	return (resultauthority.Authorizer{Reader: i.reader}).Check(ctx, identity)
}

// ValidateUpdate protects the public binding and owner as well as the key.
// Kubernetes' immutable Secret flag protects data only, not this metadata.
func ValidateUpdate(old, next *corev1.Secret) error {
	if old == nil || next == nil || old.UID == "" || old.UID != next.UID {
		return ErrCredential
	}
	a, b := old.DeepCopy(), next.DeepCopy()
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
	key := client.ObjectKey{Namespace: secret.Namespace, Name: owner.Name}
	var object client.Object
	var activeID string
	var err error
	switch owner.Kind {
	case "PtahSchema":
		schema := &api.PtahSchema{}
		object = schema
		err = reader.Get(ctx, key, schema)
		if schema.Status.ActiveOperation != nil {
			activeID = schema.Status.ActiveOperation.ID
		}
	case "PtahMigration":
		migration := &api.PtahMigration{}
		object = migration
		err = reader.Get(ctx, key, migration)
		if migration.Status.ActiveOperation != nil {
			activeID = migration.Status.ActiveOperation.ID
		}
	default:
		return ErrCredential
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if object.GetUID() == owner.UID && activeID == secret.Annotations[AnnotationOperationID] {
		return errors.New("result credential belongs to an active operation")
	}
	return nil
}
