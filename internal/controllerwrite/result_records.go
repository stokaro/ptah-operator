package controllerwrite

import (
	"context"
	"strings"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		if record.Spec.Type == "credential" {
			if !strings.HasPrefix(record.Name, jobconfig.SecretPrefix) {
				return denyf("credential record name is outside its reserved namespace")
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
				if err == nil && record.Spec.Type == "complete" {
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
		if old.Spec.Type == "credential" {
			err = resultcredentials.ValidateRecordDelete(ctx, v.Reader, old)
		} else {
			// No publication may be discarded before receipt consumption and retention
			// are integrated. Active-operation retirement alone is not sufficient.
			return denyf("result publication retention has not authorized deletion")
		}
	default:
		return denyf("unexpected result record operation")
	}
	if err != nil {
		// Never include credential material or parser diagnostics in a response.
		return denyf("result record write violates its immutable operation binding")
	}
	return nil
}
