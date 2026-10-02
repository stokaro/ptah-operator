package controllerwrite

import (
	"context"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

func (v *Validator) validateResultCredential(ctx context.Context, req admissionv1.AdmissionRequest) error {
	kind := metav1.GroupVersionKind{Version: "v1", Kind: "Secret"}
	if err := validateRequestType(req, metav1.GroupVersionResource{Version: "v1", Resource: "secrets"}, kind); err != nil {
		return err
	}
	if req.SubResource != "" || req.RequestSubResource != "" || (req.Name == "" && req.Operation != admissionv1.Delete) || req.Namespace == "" {
		return denyf("result credential request has no exact Secret identity")
	}
	decode := func(raw []byte) (*corev1.Secret, error) {
		secret := &corev1.Secret{}
		if err := decodeObject(raw, secret, kind); err != nil {
			return nil, err
		}
		if (req.Name != "" && secret.Name != req.Name) || secret.Namespace != req.Namespace || !strings.HasPrefix(secret.Name, jobconfig.SecretPrefix) {
			return nil, denyf("Secret is outside the result credential namespace")
		}
		return secret, nil
	}
	var err error
	switch req.Operation {
	case admissionv1.Create:
		if req.UserInfo.Username != v.ManagerUsername || len(req.OldObject.Raw) != 0 {
			return denyf("only the configured operator manager may create result credentials")
		}
		secret, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		err = v.ResultCredentials.ValidateCreate(ctx, secret)
	case admissionv1.Update:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		next, decodeErr := decode(req.Object.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		err = resultcredentials.ValidateUpdate(old, next)
	case admissionv1.Delete:
		old, decodeErr := decode(req.OldObject.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		// Collection deletion supplies each old object but no request name.
		// Bind to that API-supplied object, which must have a persisted UID.
		if old.UID == "" {
			return denyf("result credential deletion has no persisted Secret identity")
		}
		err = resultcredentials.ValidateDelete(ctx, v.Reader, old)
	default:
		return denyf("unexpected result credential operation")
	}
	if err != nil {
		// Never include credential material or parser diagnostics in a response.
		return denyf("result credential write violates its immutable operation binding")
	}
	return nil
}
