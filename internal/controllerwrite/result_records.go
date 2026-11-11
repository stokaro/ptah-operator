package controllerwrite

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (v *Validator) validateResultRecord(ctx context.Context, req admissionv1.AdmissionRequest) error {
	kind := metav1.GroupVersionKind{Group: "operator.ptah.run", Version: "v1alpha1", Kind: "PtahResultRecord"}
	if err := validateRequestType(req, metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"}, kind); err != nil {
		return err
	}
	if req.SubResource != "" || req.RequestSubResource != "" || (req.Name == "" && req.Operation != admissionv1.Delete) || req.Namespace == "" {
		return denyf("result request has no exact record identity")
	}
	decode := func(raw []byte) (*api.PtahResultRecord, error) {
		record := &api.PtahResultRecord{}
		if err := decodeObject(raw, record, kind); err != nil {
			return nil, err
		}
		if (req.Name != "" && record.Name != req.Name) || record.Namespace != req.Namespace {
			return nil, denyf("result record differs from its request identity")
		}
		return record, nil
	}
	var err error
	switch req.Operation {
	case admissionv1.Create:
		if req.UserInfo.Username != v.ManagerUsername || len(req.OldObject.Raw) != 0 {
			return denyf("only the configured operator manager may create result records")
		}
		record, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		if record.Spec.Type == "retired" {
			// Retirement issues nothing and reads only the API, so it needs no
			// delivery trust: records outlive the setting that wrote them.
			err = v.validateResultRetirement(ctx, record)
		} else if record.Spec.Type == "credential" {
			if !strings.HasPrefix(record.Name, jobconfig.SecretPrefix) {
				return denyf("credential record name is outside its reserved namespace")
			}
			if v.ResultCredentials == nil {
				return denyf("result credential trust is not configured")
			}
			err = v.ResultCredentials.ValidateRecordCreate(ctx, record)
		} else {
			if v.ResultCredentials == nil {
				return denyf("result publication trust is not configured")
			}
			b, payload, validationErr := (resultstore.Store{Reader: v.Reader}).ValidateRecordCreate(ctx, record)
			err = validationErr
			if err == nil {
				identity, authorityErr := v.ResultCredentials.AuthorizePublication(ctx, b)
				err = authorityErr
				if err == nil && len(payload) != 0 {
					_, err = resultdelivery.Decode(identity, payload)
				}
			}
		}
	case admissionv1.Update:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		next, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		if old.Spec.Type == "credential" {
			err = resultcredentials.ValidateRecordUpdate(old, next)
		} else {
			err = resultstore.ValidateRecordUpdate(old, next)
		}
		// Foreground retirement may be the only persistent clock available in
		// a terminating namespace or under a full ResourceQuota. Even the garbage
		// collector must retain this API finalizer until the full window and
		// current pins permit deletion.
		// What this update asks for is the deletion, so that is what it is told.
		if err == nil && !old.DeletionTimestamp.IsZero() && len(old.Finalizers) == 1 &&
			old.Finalizers[0] == metav1.FinalizerDeleteDependents && len(next.Finalizers) == 0 {
			if err := v.authorizeResultDeletion(ctx, old); err != nil {
				return retentionDenial(old, err)
			}
		}
	case admissionv1.Delete:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		// Collection deletion supplies each old object but no request name.
		// Bind to that API-supplied object, which must have a persisted UID.
		if old.UID == "" {
			return denyf("result deletion has no persisted record identity")
		}
		if err := validateCredentialDeleteOptions(req); err != nil {
			return err
		}
		err = v.authorizeResultDeletion(ctx, old)
		var options metav1.DeleteOptions
		if err != nil && v.ResultCleanup != nil && len(req.Options.Raw) != 0 && json.Unmarshal(req.Options.Raw, &options) == nil &&
			options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground &&
			v.ResultCleanup.AuthorizeForegroundRetirement(ctx, old) == nil {
			err = nil
		}
		if err != nil {
			return retentionDenial(old, err)
		}
	default:
		return denyf("unexpected result record operation")
	}
	if err != nil {
		return resultWriteDenial("result record", req.Operation)
	}
	return nil
}

// authorizeResultDeletion asks the retention policy. With no policy nothing can
// authorize a deletion, whichever kind of record it is: the window has no other
// keeper.
func (v *Validator) authorizeResultDeletion(ctx context.Context, record *api.PtahResultRecord) error {
	if v.ResultCleanup == nil {
		return resultcleanup.ErrRetained
	}
	return v.ResultCleanup.AuthorizeDelete(ctx, record)
}

// resultWriteDenial words a refused create or update by what was refused. The
// reason stays out of the response: it can hold credential material or a parser
// diagnostic.
func resultWriteDenial(subject string, operation admissionv1.Operation) error {
	if operation == admissionv1.Create {
		return denyf("%s creation is not authorized for its operation", subject)
	}
	return denyf("%s update violates its immutable operation binding", subject)
}

// retentionDenial words a refusal by the retention policy. Only a refusal about
// time names a time. The instant comes from API server timestamps and the name
// from the stored object, so neither is credential material or a parser
// diagnostic. A namespace deletion quotes this message in its conditions, and
// the operations page says how to read it.
func retentionDenial(record *api.PtahResultRecord, err error) error {
	if ends, isTime := resultretention.WindowEnd(err); isTime {
		return denyf("result publication retention has not authorized deletion: record %s cannot be deleted before %s", record.Name, ends.UTC().Format(time.RFC3339))
	}
	return denyf("result publication retention has not authorized deletion")
}

func (v *Validator) validateResultRetirement(ctx context.Context, record *api.PtahResultRecord) error {
	r, err := resultretention.Decode(record)
	if err != nil || !record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 0 {
		return resultretention.ErrRecord
	}
	source := &api.PtahResultRecord{}
	if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: r.Source.Name}, source); err != nil {
		return err
	}
	if source.UID != r.Source.UID || source.Spec.Type != r.Source.Type {
		return resultretention.ErrRecord
	}
	var binding resultstore.Binding
	if source.Spec.Type == "credential" {
		identity, readErr := resultcredentials.StoredRecordIdentity(source)
		binding, err = identity.Binding, readErr
	} else {
		binding, err = resultstore.StoredBinding(source)
	}
	if err != nil || binding != r.Binding {
		return resultretention.ErrRecord
	}
	return resultretention.CheckUnpinned(ctx, v.Reader, binding)
}
