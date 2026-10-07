// Package resultcleanup retires and collects runner evidence through direct API
// reads. It never deletes plans or reads/deletes Secret projections.
package resultcleanup

import (
	"context"
	"errors"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrRetained = errors.New("result evidence is not eligible for collection")

type Policy struct {
	Reader client.Reader
	Window time.Duration
	// Now is injectable for tests; production uses the wall clock.
	Now func() time.Time
}

func (p Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func RootBinding(record *api.PtahResultRecord) (resultstore.Binding, error) {
	if record == nil {
		return resultstore.Binding{}, resultretention.ErrRecord
	}
	switch record.Spec.Type {
	case "credential":
		identity, err := resultcredentials.StoredRecordIdentity(record)
		return identity.Binding, err
	case "intent":
		return resultstore.StoredBinding(record)
	case "retired":
		r, err := resultretention.Decode(record)
		return r.Binding, err
	default:
		return resultstore.Binding{}, resultretention.ErrRecord
	}
}

// AuthorizeDelete is also the admission verdict, regardless of the DELETE
// caller. Neither a collector's cached decision nor an administrator's identity
// replaces these current checks. Every API failure retains the evidence.
func (p Policy) AuthorizeDelete(ctx context.Context, record *api.PtahResultRecord) error {
	if p.Reader == nil || p.Window < resultretention.MinimumWindow || record == nil || record.UID == "" {
		return ErrRetained
	}
	b, err := RootBinding(record)
	var marker *api.PtahResultRecord
	if record.Spec.Type == "chunk" || record.Spec.Type == "complete" {
		if len(record.OwnerReferences) != 1 || record.OwnerReferences[0].Kind != "PtahResultRecord" {
			return ErrRetained
		}
		marker = &api.PtahResultRecord{}
		if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.OwnerReferences[0].Name + "-retired"}, marker); err != nil {
			if apierrors.IsNotFound(err) {
				return p.authorizeForeground(ctx, record)
			}
			return err
		}
		r, decodeErr := resultretention.Decode(marker)
		b, err = r.Binding, decodeErr
		if err != nil || !resultstore.MemberOf(record, b) {
			return ErrRetained
		}
	}
	if err != nil {
		return err
	}
	if marker == nil {
		name, err := resultretention.Name(b)
		if err != nil {
			return err
		}
		marker = &api.PtahResultRecord{}
		if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, marker); err != nil {
			if apierrors.IsNotFound(err) {
				return p.authorizeForeground(ctx, record)
			}
			return err
		}
	}
	r, err := resultretention.Decode(marker)
	if err != nil || r.Binding != b || (record.Name == r.Source.Name && record.UID != r.Source.UID) || (record.Spec.Type == "retired" && record.UID != marker.UID) {
		return ErrRetained
	}
	source := &api.PtahResultRecord{}
	sourceErr := p.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: r.Source.Name}, source)
	if sourceErr == nil {
		sourceBinding, err := RootBinding(source)
		if err != nil || source.UID != r.Source.UID || source.Spec.Type != r.Source.Type || sourceBinding != b {
			return ErrRetained
		}
	} else if !apierrors.IsNotFound(sourceErr) {
		return sourceErr
	}
	if record.Spec.Type == "chunk" || record.Spec.Type == "complete" {
		// An intent-sourced retirement already read this immutable owner above.
		// Reuse that one live reading within this verdict. Credential-sourced
		// retirement still needs a separate intent read; no verdict is cached
		// across deletions, and the current recovery pins are checked below.
		intent, err := source, sourceErr
		if record.OwnerReferences[0].Name != r.Source.Name {
			intent = &api.PtahResultRecord{}
			err = p.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: record.OwnerReferences[0].Name}, intent)
		}
		if err == nil {
			intentBinding, bindingErr := RootBinding(intent)
			if bindingErr != nil || intentBinding != b || intent.UID != record.OwnerReferences[0].UID {
				return ErrRetained
			}
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := resultretention.Eligible(ctx, p.Reader, marker, p.now(), p.Window); err != nil {
		return err
	}
	// A late API write also receives a complete retention window. It cannot
	// become immediately disposable by arriving after retirement was recorded.
	window := max(p.Window, time.Duration(r.RetentionSeconds)*time.Second)
	if record.CreationTimestamp.IsZero() || p.now().Before(record.CreationTimestamp.Add(window)) {
		return resultretention.ErrWindow
	}
	// Keep the fence while the original Job can authenticate its original Pod.
	// A replacement Job has a new UID and cannot revive the retired claim.
	if err := p.requireJobAbsent(ctx, b); err != nil {
		return err
	}
	return p.deletionOrder(ctx, record, b)
}

func (p Policy) requireJobAbsent(ctx context.Context, b resultstore.Binding) error {
	job := &batchv1.Job{}
	if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.JobName}, job); err == nil {
		if job.UID == b.JobUID {
			return ErrRetained
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (p Policy) deletionOrder(ctx context.Context, record *api.PtahResultRecord, b resultstore.Binding) error {
	intentName, _ := resultstore.Name(b)
	switch record.Spec.Type {
	case "chunk":
		return p.requireAbsent(ctx, b.Namespace, intentName+"-complete")
	case "complete":
		return nil
	case "intent":
		return p.requireNoMembers(ctx, b)
	case "credential":
		return p.requireAbsent(ctx, b.Namespace, intentName)
	case "retired":
		for _, name := range []string{intentName, jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)} {
			if err := p.requireAbsent(ctx, b.Namespace, name); err != nil {
				return err
			}
		}
		return p.requireNoMembers(ctx, b)
	default:
		return ErrRetained
	}
}

// AuthorizeForegroundRetirement permits only the start of a foreground
// deletion. Its API-controlled timestamp supplies the retirement clock when
// namespace termination forbids creating a separate marker. Admission protects
// the foreground finalizer until AuthorizeDelete permits destruction of bytes.
func (p Policy) AuthorizeForegroundRetirement(ctx context.Context, record *api.PtahResultRecord) error {
	if p.Reader == nil || p.Window < resultretention.MinimumWindow || record == nil || record.UID == "" ||
		!record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 0 ||
		(record.Spec.Type != "intent" && record.Spec.Type != "credential") {
		return ErrRetained
	}
	b, err := RootBinding(record)
	if err != nil {
		return err
	}
	name, _ := resultretention.Name(b)
	if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &api.PtahResultRecord{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrRetained
		}
		return err
	}
	if err := resultretention.CheckUnpinned(ctx, p.Reader, b); err != nil {
		return err
	}
	return p.requireJobAbsent(ctx, b)
}

func (p Policy) foregroundBinding(ctx context.Context, record *api.PtahResultRecord) (resultstore.Binding, error) {
	root := record
	if record.Spec.Type == "chunk" || record.Spec.Type == "complete" {
		if len(record.OwnerReferences) != 1 || record.OwnerReferences[0].Kind != "PtahResultRecord" {
			return resultstore.Binding{}, ErrRetained
		}
		root = &api.PtahResultRecord{}
		if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.OwnerReferences[0].Name}, root); err != nil {
			return resultstore.Binding{}, err
		}
		if root.Spec.Type != "intent" || root.UID != record.OwnerReferences[0].UID {
			return resultstore.Binding{}, ErrRetained
		}
	}
	b, err := RootBinding(root)
	if err != nil || (root.Spec.Type != "intent" && root.Spec.Type != "credential") ||
		root.DeletionTimestamp.IsZero() || len(root.Finalizers) != 1 || root.Finalizers[0] != metav1.FinalizerDeleteDependents ||
		(root != record && !resultstore.MemberOf(record, b)) {
		return resultstore.Binding{}, ErrRetained
	}
	if err := resultretention.CheckUnpinned(ctx, p.Reader, b); err != nil {
		return b, err
	}
	if err := p.requireJobAbsent(ctx, b); err != nil {
		return b, err
	}
	if record.CreationTimestamp.IsZero() || p.now().Before(root.DeletionTimestamp.Add(p.Window)) || p.now().Before(record.CreationTimestamp.Add(p.Window)) {
		return b, resultretention.ErrWindow
	}
	return b, nil
}

func (p Policy) authorizeForeground(ctx context.Context, record *api.PtahResultRecord) error {
	b, err := p.foregroundBinding(ctx, record)
	if err != nil {
		return err
	}
	return p.deletionOrder(ctx, record, b)
}

func (p Policy) requireAbsent(ctx context.Context, namespace, name string) error {
	err := p.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &api.PtahResultRecord{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		return ErrRetained
	}
	return err
}

func (p Policy) requireNoMembers(ctx context.Context, b resultstore.Binding) error {
	members, err := p.members(ctx, b)
	if err != nil {
		return err
	}
	if len(members) != 0 {
		return ErrRetained
	}
	return nil
}
