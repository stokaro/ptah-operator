package controllerwrite

import (
	"context"
	"strings"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (v *Validator) validateResultCredentialRecord(ctx context.Context, req admissionv1.AdmissionRequest) error {
	kind := metav1.GroupVersionKind{Group: "operator.ptah.run", Version: "v1alpha1", Kind: "PtahResultRecord"}
	if err := validateRequestType(req, metav1.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahresultrecords"}, kind); err != nil {
		return err
	}
	if req.SubResource != "" || req.RequestSubResource != "" || (req.Name == "" && req.Operation != admissionv1.Delete) || req.Namespace == "" {
		return denyf("result credential request has no exact record identity")
	}
	decode := func(raw []byte) (*api.PtahResultRecord, error) {
		record := &api.PtahResultRecord{}
		if err := decodeObject(raw, record, kind); err != nil {
			return nil, err
		}
		if (req.Name != "" && record.Name != req.Name) || record.Namespace != req.Namespace || !strings.HasPrefix(record.Name, jobconfig.SecretPrefix) {
			return nil, denyf("Record is outside the result credential namespace")
		}
		return record, nil
	}
	var err error
	switch req.Operation {
	case admissionv1.Create:
		if req.UserInfo.Username != v.ManagerUsername || len(req.OldObject.Raw) != 0 {
			return denyf("only the configured operator manager may create result credentials")
		}
		record, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		err = v.ResultCredentials.ValidateRecordCreate(ctx, record)
	case admissionv1.Update:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		next, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		err = resultcredentials.ValidateRecordUpdate(old, next)
	case admissionv1.Delete:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		// Collection deletion supplies each old object but no request name.
		// Bind to that API-supplied object, which must have a persisted UID.
		if old.UID == "" {
			return denyf("result credential deletion has no persisted record identity")
		}
		err = resultcredentials.ValidateRecordDelete(ctx, v.Reader, old)
	default:
		return denyf("unexpected result credential operation")
	}
	if err != nil {
		// Never include credential material or parser diagnostics in a response.
		return denyf("result credential write violates its immutable operation binding")
	}
	return nil
}
