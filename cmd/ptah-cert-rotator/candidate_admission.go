package main

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	sigsjson "sigs.k8s.io/json"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

const (
	candidateMutatingCanaryPath      = certrotation.AdmissionCanaryMutatingPath
	candidateValidatingCanaryPath    = certrotation.AdmissionCanaryValidatingPath
	candidateMutatingDenialMessage   = certrotation.AdmissionCanaryMutatingDenialMessage
	candidateValidatingDenialMessage = certrotation.AdmissionCanaryValidatingDenialMessage
	candidateProbeLabelKey           = certrotation.AdmissionCanaryMarkerLabel
	candidateProbeLabelValue         = certrotation.AdmissionCanaryMarkerLabelValue

	maximumCandidateAdmissionBodyBytes = 64 << 10
	candidateAdmissionErrorBody        = "invalid candidate admission request\n"
)

type candidateAdmissionConfig struct {
	ReleaseName            string
	Namespace              string
	ConfigMapName          string
	Username               string
	MutatingFieldManager   string
	ValidatingFieldManager string
}

func (c candidateAdmissionConfig) validate() error {
	serviceAccountName := strings.TrimPrefix(c.Username, "system:serviceaccount:"+c.Namespace+":")
	switch {
	case c.ReleaseName == "" || len(validation.IsDNS1123Subdomain(c.ReleaseName)) != 0:
		return errors.New("candidate admission release name must be a DNS subdomain")
	case c.Namespace == "":
		return errors.New("candidate admission namespace is required")
	case len(validation.IsDNS1123Label(c.Namespace)) != 0:
		return errors.New("candidate admission namespace must be a DNS label")
	case c.ConfigMapName == "":
		return errors.New("candidate admission ConfigMap name is required")
	case len(validation.IsDNS1123Subdomain(c.ConfigMapName)) != 0:
		return errors.New("candidate admission ConfigMap name must be a DNS subdomain")
	case c.Username == "":
		return errors.New("candidate admission username is required")
	case serviceAccountName == c.Username || serviceAccountName == "" || len(validation.IsDNS1123Subdomain(serviceAccountName)) != 0:
		return errors.New("candidate admission username must identify a ServiceAccount in its namespace")
	case c.MutatingFieldManager == "":
		return errors.New("candidate mutating field manager is required")
	case c.ValidatingFieldManager == "":
		return errors.New("candidate validating field manager is required")
	case c.MutatingFieldManager == c.ValidatingFieldManager:
		return errors.New("candidate admission field managers must be distinct")
	}
	if errs := metav1validation.ValidateFieldManager(c.MutatingFieldManager, field.NewPath("mutatingFieldManager")); len(errs) != 0 {
		return errors.New("candidate mutating field manager is invalid")
	}
	if errs := metav1validation.ValidateFieldManager(c.ValidatingFieldManager, field.NewPath("validatingFieldManager")); len(errs) != 0 {
		return errors.New("candidate validating field manager is invalid")
	}
	if c.MutatingFieldManager != certrotation.AdmissionCanaryMutatingFieldManager {
		return errors.New("candidate mutating field manager differs from the supported admission canary contract")
	}
	if c.ValidatingFieldManager != certrotation.AdmissionCanaryValidatingFieldManager {
		return errors.New("candidate validating field manager differs from the supported admission canary contract")
	}
	return nil
}

type candidateAdmissionHandler struct {
	config candidateAdmissionConfig
}

func newCandidateAdmissionHandler(config candidateAdmissionConfig) (http.Handler, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &candidateAdmissionHandler{config: config}, nil
}

func (h *candidateAdmissionHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	denialStatus, fieldManager, pathOK := h.contractForPath(request)
	if !pathOK {
		writeCandidateAdmissionError(writer, http.StatusNotFound)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeCandidateAdmissionError(writer, http.StatusMethodNotAllowed)
		return
	}
	if !exactCandidateContentType(request.Header) || request.Header.Get("Content-Encoding") != "" {
		writeCandidateAdmissionError(writer, http.StatusUnsupportedMediaType)
		return
	}

	defer request.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maximumCandidateAdmissionBodyBytes))
	if err != nil {
		writeCandidateAdmissionError(writer, http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if !unmarshalCandidateJSON(body, &review) {
		writeCandidateAdmissionError(writer, http.StatusBadRequest)
		return
	}
	marker, valid := h.validReview(&review, fieldManager)
	if !valid {
		writeCandidateAdmissionError(writer, http.StatusBadRequest)
		return
	}

	response := candidateDenialReview(review.Request, denialStatus(marker.Name, marker.UID))
	encoded, err := json.Marshal(response)
	if err != nil {
		writeCandidateAdmissionError(writer, http.StatusInternalServerError)
		return
	}
	setCandidateResponseHeaders(writer.Header(), "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

type candidateDenialStatusBuilder func(string, types.UID) *metav1.Status

func (h *candidateAdmissionHandler) contractForPath(
	request *http.Request,
) (candidateDenialStatusBuilder, string, bool) {
	if request.URL == nil || request.URL.RawQuery != "" || request.URL.RawPath != "" {
		return nil, "", false
	}
	switch request.URL.Path {
	case candidateMutatingCanaryPath:
		return certrotation.AdmissionCanaryMutatingDenialStatus, h.config.MutatingFieldManager, true
	case candidateValidatingCanaryPath:
		return certrotation.AdmissionCanaryValidatingDenialStatus, h.config.ValidatingFieldManager, true
	default:
		return nil, "", false
	}
}

func (h *candidateAdmissionHandler) validReview(
	review *admissionv1.AdmissionReview,
	fieldManager string,
) (*corev1.ConfigMap, bool) {
	if review.APIVersion != admissionv1.SchemeGroupVersion.String() || review.Kind != "AdmissionReview" ||
		review.Request == nil || review.Response != nil {
		return nil, false
	}
	request := review.Request
	if request.UID == "" || string(request.UID) != strings.TrimSpace(string(request.UID)) || len(request.UID) > 128 ||
		request.Kind != candidateConfigMapKind() || request.Resource != candidateConfigMapResource() ||
		request.RequestKind == nil || *request.RequestKind != candidateConfigMapKind() ||
		request.RequestResource == nil || *request.RequestResource != candidateConfigMapResource() ||
		request.SubResource != "" || request.RequestSubResource != "" ||
		request.Name != h.config.ConfigMapName || request.Namespace != h.config.Namespace ||
		request.Operation != admissionv1.Update || request.DryRun == nil || !*request.DryRun ||
		!validCandidateUser(request.UserInfo, h.config.Username) {
		return nil, false
	}
	if !validCandidateUpdateOptions(request.Options, fieldManager) {
		return nil, false
	}
	object, ok := h.decodeProbeConfigMap(request.Object)
	if !ok {
		return nil, false
	}
	oldObject, ok := h.decodeProbeConfigMap(request.OldObject)
	if !ok || object.UID != oldObject.UID || object.ResourceVersion != oldObject.ResourceVersion ||
		!object.CreationTimestamp.Equal(&oldObject.CreationTimestamp) {
		return nil, false
	}
	object.ManagedFields = nil
	oldObject.ManagedFields = nil
	return object, apiequality.Semantic.DeepEqual(object, oldObject)
}

func candidateConfigMapKind() metav1.GroupVersionKind {
	return metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
}

func candidateConfigMapResource() metav1.GroupVersionResource {
	return metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
}

func (h *candidateAdmissionHandler) decodeProbeConfigMap(raw runtime.RawExtension) (*corev1.ConfigMap, bool) {
	if len(raw.Raw) == 0 || raw.Object != nil {
		return nil, false
	}
	var configMap corev1.ConfigMap
	expected := certrotation.AdmissionCanaryMarker(h.config.Namespace, h.config.ConfigMapName, h.config.ReleaseName)
	if !unmarshalCandidateJSON(raw.Raw, &configMap) ||
		configMap.APIVersion != "v1" || configMap.Kind != "ConfigMap" ||
		configMap.Name != h.config.ConfigMapName || configMap.Namespace != h.config.Namespace ||
		configMap.GenerateName != "" || configMap.SelfLink != "" || configMap.UID == "" ||
		configMap.ResourceVersion == "" || configMap.Generation != 0 || configMap.CreationTimestamp.IsZero() ||
		configMap.DeletionTimestamp != nil || configMap.DeletionGracePeriodSeconds != nil ||
		!maps.Equal(configMap.Labels, expected.Labels) || !maps.Equal(configMap.Annotations, expected.Annotations) ||
		len(configMap.OwnerReferences) != 0 || len(configMap.Finalizers) != 0 ||
		configMap.Immutable == nil || !*configMap.Immutable || len(configMap.Data) != 0 || len(configMap.BinaryData) != 0 {
		return nil, false
	}
	return &configMap, true
}

func validCandidateUser(user authenticationv1.UserInfo, username string) bool {
	return user.Username == username
}

func validCandidateUpdateOptions(raw runtime.RawExtension, fieldManager string) bool {
	if len(raw.Raw) == 0 || raw.Object != nil {
		return false
	}
	var options metav1.UpdateOptions
	if !unmarshalCandidateJSON(raw.Raw, &options) {
		return false
	}
	return options.APIVersion == metav1.SchemeGroupVersion.String() && options.Kind == "UpdateOptions" &&
		slices.Equal(options.DryRun, []string{metav1.DryRunAll}) &&
		options.FieldManager == fieldManager && options.FieldValidation == metav1.FieldValidationStrict
}

func unmarshalCandidateJSON(data []byte, target any) bool {
	strictErrors, err := sigsjson.UnmarshalStrict(data, target)
	return err == nil && len(strictErrors) == 0
}

func candidateDenialReview(
	request *admissionv1.AdmissionRequest,
	status *metav1.Status,
) admissionv1.AdmissionReview {
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Response: &admissionv1.AdmissionResponse{
			UID:     request.UID,
			Allowed: false,
			Result:  status,
		},
	}
}

func exactCandidateContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	return len(values) == 1 && values[0] == "application/json"
}

func writeCandidateAdmissionError(writer http.ResponseWriter, status int) {
	setCandidateResponseHeaders(writer.Header(), "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, candidateAdmissionErrorBody)
}

func setCandidateResponseHeaders(header http.Header, contentType string) {
	header.Set("Cache-Control", "no-store")
	header.Set("Connection", "close")
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
}
