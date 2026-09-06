package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

func TestCandidateAdmissionHandlerReturnsExactTypedDenials(t *testing.T) {
	t.Parallel()

	config := testCandidateAdmissionConfig()
	handler, err := newCandidateAdmissionHandler(config)
	if err != nil {
		t.Fatalf("newCandidateAdmissionHandler() error = %v", err)
	}
	tests := []struct {
		name         string
		path         string
		fieldManager string
		message      string
		webhookName  string
		matches      func(error, string, types.UID) bool
	}{
		{
			name:         "mutating",
			path:         candidateMutatingCanaryPath,
			fieldManager: config.MutatingFieldManager,
			message:      candidateMutatingDenialMessage,
			webhookName:  certrotation.AdmissionCanaryMutatingWebhookName,
			matches:      certrotation.HasExactAdmissionCanaryMutatingDenial,
		},
		{
			name:         "validating",
			path:         candidateValidatingCanaryPath,
			fieldManager: config.ValidatingFieldManager,
			message:      candidateValidatingDenialMessage,
			webhookName:  certrotation.AdmissionCanaryValidatingWebhookName,
			matches:      certrotation.HasExactAdmissionCanaryValidatingDenial,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			review := testCandidateAdmissionReview(t, config, test.fieldManager)
			response := serveCandidateAdmission(t, handler, test.path, http.MethodPost, "application/json", mustCandidateJSON(t, review))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", response.Code, response.Body.String())
			}
			assertCandidateSecurityHeaders(t, response.Header(), "application/json")

			var got admissionv1.AdmissionReview
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode AdmissionReview response: %v", err)
			}
			if got.APIVersion != admissionv1.SchemeGroupVersion.String() || got.Kind != "AdmissionReview" || got.Request != nil {
				t.Fatalf("response envelope = %#v", got)
			}
			if got.Response == nil || got.Response.UID != review.Request.UID || got.Response.Allowed || got.Response.Result == nil {
				t.Fatalf("admission response = %#v", got.Response)
			}
			result := got.Response.Result
			if result.APIVersion != "v1" || result.Kind != "Status" || result.Status != metav1.StatusFailure ||
				result.Reason != metav1.StatusReasonInvalid || result.Code != http.StatusUnprocessableEntity ||
				result.Message != test.message {
				t.Fatalf("denial status = %#v", result)
			}
			if result.Details == nil || result.Details.Name != config.ConfigMapName || result.Details.Group != "" ||
				result.Details.Kind != "ConfigMap" || result.Details.UID != "config-map-uid" || len(result.Details.Causes) != 1 {
				t.Fatalf("denial details = %#v", result.Details)
			}
			cause := result.Details.Causes[0]
			if cause.Type != metav1.CauseTypeFieldValueInvalid || cause.Message != test.fieldManager ||
				cause.Field != "request.options.fieldManager" {
				t.Fatalf("denial cause = %#v", cause)
			}
			if got.Response.Patch != nil || got.Response.PatchType != nil || len(got.Response.Warnings) != 0 ||
				len(got.Response.AuditAnnotations) != 0 {
				t.Fatalf("denial contains unexpected mutation data: %#v", got.Response)
			}
			wrapped := result.DeepCopy()
			wrapped.Message = fmt.Sprintf(
				"admission webhook %q denied the request: %s",
				test.webhookName,
				result.Message,
			)
			if !test.matches(&apierrors.StatusError{ErrStatus: *wrapped}, config.ConfigMapName, "config-map-uid") {
				t.Fatal("listener denial did not satisfy the shared API-server proof classifier")
			}
		})
	}
}

func TestCandidateAdmissionHandlerFailsClosedWithoutDenialFingerprint(t *testing.T) {
	t.Parallel()

	config := testCandidateAdmissionConfig()
	handler, err := newCandidateAdmissionHandler(config)
	if err != nil {
		t.Fatalf("newCandidateAdmissionHandler() error = %v", err)
	}
	validReview := func(t *testing.T) admissionv1.AdmissionReview {
		t.Helper()
		return testCandidateAdmissionReview(t, config, config.MutatingFieldManager)
	}
	tests := []struct {
		name        string
		path        string
		method      string
		contentType string
		encoding    string
		body        func(*testing.T) []byte
		wantStatus  int
	}{
		{
			name: "unknown path", path: "/candidate/other", method: http.MethodPost, contentType: "application/json",
			body: func(t *testing.T) []byte { return mustCandidateJSON(t, validReview(t)) }, wantStatus: http.StatusNotFound,
		},
		{
			name: "query string", path: candidateMutatingCanaryPath + "?mode=probe", method: http.MethodPost, contentType: "application/json",
			body: func(t *testing.T) []byte { return mustCandidateJSON(t, validReview(t)) }, wantStatus: http.StatusNotFound,
		},
		{
			name: "wrong method", path: candidateMutatingCanaryPath, method: http.MethodGet, contentType: "application/json",
			body: func(t *testing.T) []byte { return mustCandidateJSON(t, validReview(t)) }, wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "content type parameter", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json; charset=utf-8",
			body: func(t *testing.T) []byte { return mustCandidateJSON(t, validReview(t)) }, wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "encoded body", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json", encoding: "gzip",
			body: func(t *testing.T) []byte { return mustCandidateJSON(t, validReview(t)) }, wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "malformed JSON", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json",
			body: func(*testing.T) []byte { return []byte(`{"apiVersion":`) }, wantStatus: http.StatusBadRequest,
		},
		{
			name: "multiple JSON objects", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json",
			body: func(t *testing.T) []byte { return append(mustCandidateJSON(t, validReview(t)), []byte(`{}`)...) }, wantStatus: http.StatusBadRequest,
		},
		{
			name: "unknown review field", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json",
			body: func(t *testing.T) []byte {
				valid := mustCandidateJSON(t, validReview(t))
				return append([]byte(`{"unexpected":true,`), valid[1:]...)
			}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate review field", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json",
			body: func(t *testing.T) []byte {
				valid := mustCandidateJSON(t, validReview(t))
				return append([]byte(`{"apiVersion":"admission.k8s.io/v1",`), valid[1:]...)
			}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversized body", path: candidateMutatingCanaryPath, method: http.MethodPost, contentType: "application/json",
			body: func(*testing.T) []byte { return bytes.Repeat([]byte{' '}, maximumCandidateAdmissionBodyBytes+1) }, wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(test.method, test.path, bytes.NewReader(test.body(t)))
			request.Header.Set("Content-Type", test.contentType)
			if test.encoding != "" {
				request.Header.Set("Content-Encoding", test.encoding)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.wantStatus, response.Body.String())
			}
			assertInvalidCandidateResponse(t, response)
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow header = %q, want POST", response.Header().Get("Allow"))
			}
		})
	}
}

func TestCandidateAdmissionHandlerRejectsContractDrift(t *testing.T) {
	t.Parallel()

	config := testCandidateAdmissionConfig()
	handler, err := newCandidateAdmissionHandler(config)
	if err != nil {
		t.Fatalf("newCandidateAdmissionHandler() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*testing.T, *admissionv1.AdmissionReview)
	}{
		{name: "wrong review version", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) {
			review.APIVersion = "admission.k8s.io/v1beta1"
		}},
		{name: "response supplied", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) {
			review.Response = &admissionv1.AdmissionResponse{}
		}},
		{name: "empty request UID", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.UID = "" }},
		{name: "wrong kind", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.Kind.Kind = "Secret" }},
		{name: "missing request kind", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.RequestKind = nil }},
		{name: "wrong resource", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.Resource.Resource = "secrets" }},
		{name: "missing request resource", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.RequestResource = nil }},
		{name: "subresource", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.SubResource = "status" }},
		{name: "wrong operation", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.Operation = admissionv1.Create }},
		{name: "not dry-run", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) {
			value := false
			review.Request.DryRun = &value
		}},
		{name: "wrong username", mutate: func(_ *testing.T, review *admissionv1.AdmissionReview) { review.Request.UserInfo.Username += "-other" }},
		{name: "wrong field manager", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			review.Request.Options = runtime.RawExtension{Raw: mustCandidateJSON(t, metav1.UpdateOptions{
				TypeMeta: metav1.TypeMeta{APIVersion: metav1.SchemeGroupVersion.String(), Kind: "UpdateOptions"},
				DryRun:   []string{metav1.DryRunAll}, FieldManager: config.ValidatingFieldManager, FieldValidation: metav1.FieldValidationStrict,
			})}
		}},
		{name: "non-strict field validation", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			review.Request.Options = runtime.RawExtension{Raw: mustCandidateJSON(t, metav1.UpdateOptions{
				TypeMeta: metav1.TypeMeta{APIVersion: metav1.SchemeGroupVersion.String(), Kind: "UpdateOptions"},
				DryRun:   []string{metav1.DryRunAll}, FieldManager: config.MutatingFieldManager,
			})}
		}},
		{name: "changed resource version", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.ResourceVersion = "different"
			review.Request.Object.Raw = mustCandidateJSON(t, object)
		}},
		{name: "mutable ConfigMap", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.Immutable = nil
			review.Request.Object.Raw = mustCandidateJSON(t, object)
		}},
		{name: "unexpected data", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.Data = map[string]string{"credential": "must-not-be-read"}
			review.Request.Object.Raw = mustCandidateJSON(t, object)
		}},
		{name: "extra label", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.Labels["extra"] = "value"
			review.Request.Object.Raw = mustCandidateJSON(t, object)
		}},
		{name: "annotation", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.Annotations = map[string]string{"unexpected": "value"}
			review.Request.Object.Raw = mustCandidateJSON(t, object)
		}},
		{name: "foreign Helm release on unchanged object", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			object.Annotations["meta.helm.sh/release-name"] = "another-release"
			review.Request.Object.Raw = mustCandidateJSON(t, object)
			review.Request.OldObject.Raw = mustCandidateJSON(t, object)
		}},
		{name: "missing Helm ownership on unchanged object", mutate: func(t *testing.T, review *admissionv1.AdmissionReview) {
			object := decodeCandidateConfigMap(t, review.Request.Object.Raw)
			delete(object.Labels, "app.kubernetes.io/managed-by")
			object.Annotations = nil
			review.Request.Object.Raw = mustCandidateJSON(t, object)
			review.Request.OldObject.Raw = mustCandidateJSON(t, object)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			review := testCandidateAdmissionReview(t, config, config.MutatingFieldManager)
			test.mutate(t, &review)
			response := serveCandidateAdmission(t, handler, candidateMutatingCanaryPath, http.MethodPost, "application/json", mustCandidateJSON(t, review))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %q", response.Code, response.Body.String())
			}
			assertInvalidCandidateResponse(t, response)
		})
	}
}

func TestCandidateAdmissionConfigValidation(t *testing.T) {
	t.Parallel()

	valid := testCandidateAdmissionConfig()
	tests := []struct {
		name   string
		mutate func(*candidateAdmissionConfig)
		want   string
	}{
		{name: "release", mutate: func(config *candidateAdmissionConfig) { config.ReleaseName = "" }, want: "release name must be a DNS subdomain"},
		{name: "namespace", mutate: func(config *candidateAdmissionConfig) { config.Namespace = "" }, want: "namespace is required"},
		{name: "ConfigMap", mutate: func(config *candidateAdmissionConfig) { config.ConfigMapName = "" }, want: "ConfigMap name is required"},
		{name: "username namespace", mutate: func(config *candidateAdmissionConfig) { config.Username = "system:serviceaccount:other:rotator" }, want: "username must identify"},
		{name: "mutating manager", mutate: func(config *candidateAdmissionConfig) { config.MutatingFieldManager = "" }, want: "mutating field manager is required"},
		{name: "validating manager", mutate: func(config *candidateAdmissionConfig) { config.ValidatingFieldManager = "" }, want: "validating field manager is required"},
		{name: "same managers", mutate: func(config *candidateAdmissionConfig) { config.ValidatingFieldManager = config.MutatingFieldManager }, want: "must be distinct"},
		{name: "manager too long", mutate: func(config *candidateAdmissionConfig) { config.MutatingFieldManager = strings.Repeat("x", 129) }, want: "mutating field manager is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := newCandidateAdmissionHandler(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newCandidateAdmissionHandler() error = %v, want substring %q", err, test.want)
			}
		})
	}
	if _, err := newCandidateAdmissionHandler(valid); err != nil {
		t.Fatalf("valid config error = %v", err)
	}
}

func testCandidateAdmissionConfig() candidateAdmissionConfig {
	return candidateAdmissionConfig{
		ReleaseName:            "ptah",
		Namespace:              "operator-system",
		ConfigMapName:          "ptah-certificate-canary",
		Username:               "system:serviceaccount:operator-system:ptah-cert-rotator",
		MutatingFieldManager:   "ptah-certificate-rotation-canary-mutate-v1",
		ValidatingFieldManager: "ptah-certificate-rotation-canary-validate-v1",
	}
}

func testCandidateAdmissionReview(
	t *testing.T,
	config candidateAdmissionConfig,
	fieldManager string,
) admissionv1.AdmissionReview {
	t.Helper()
	immutable := true
	object := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              config.ConfigMapName,
			Namespace:         config.Namespace,
			UID:               types.UID("config-map-uid"),
			ResourceVersion:   "42",
			CreationTimestamp: metav1.NewTime(time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)),
			Labels: map[string]string{
				candidateProbeLabelKey:         candidateProbeLabelValue,
				"app.kubernetes.io/managed-by": "Helm",
			},
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      config.ReleaseName,
				"meta.helm.sh/release-namespace": config.Namespace,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "helm", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1",
			}},
		},
		Immutable: &immutable,
	}
	oldObject := object.DeepCopy()
	object.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager: fieldManager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1",
	}}
	dryRun := true
	requestKind := candidateConfigMapKind()
	requestResource := candidateConfigMapResource()
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:             types.UID("review-uid"),
			Kind:            candidateConfigMapKind(),
			Resource:        candidateConfigMapResource(),
			RequestKind:     &requestKind,
			RequestResource: &requestResource,
			Name:            config.ConfigMapName,
			Namespace:       config.Namespace,
			Operation:       admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: config.Username,
				UID:      "service-account-uid",
				Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + config.Namespace, "system:authenticated"},
				Extra:    map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"rotator-0"}},
			},
			Object:    runtime.RawExtension{Raw: mustCandidateJSON(t, object)},
			OldObject: runtime.RawExtension{Raw: mustCandidateJSON(t, oldObject)},
			DryRun:    &dryRun,
			Options: runtime.RawExtension{Raw: mustCandidateJSON(t, metav1.UpdateOptions{
				TypeMeta:        metav1.TypeMeta{APIVersion: metav1.SchemeGroupVersion.String(), Kind: "UpdateOptions"},
				DryRun:          []string{metav1.DryRunAll},
				FieldManager:    fieldManager,
				FieldValidation: metav1.FieldValidationStrict,
			})},
		},
	}
}

func serveCandidateAdmission(
	t *testing.T,
	handler http.Handler,
	path string,
	method string,
	contentType string,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func mustCandidateJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal candidate admission fixture: %v", err)
	}
	return encoded
}

func decodeCandidateConfigMap(t *testing.T, raw []byte) *corev1.ConfigMap {
	t.Helper()
	var configMap corev1.ConfigMap
	if err := json.Unmarshal(raw, &configMap); err != nil {
		t.Fatalf("decode candidate ConfigMap fixture: %v", err)
	}
	return &configMap
}

func assertInvalidCandidateResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assertCandidateSecurityHeaders(t, response.Header(), "text/plain; charset=utf-8")
	if response.Body.String() != candidateAdmissionErrorBody {
		t.Fatalf("invalid response body = %q, want generic error", response.Body.String())
	}
	for _, fingerprint := range []string{candidateMutatingDenialMessage, candidateValidatingDenialMessage, "review-uid"} {
		if strings.Contains(response.Body.String(), fingerprint) {
			t.Fatalf("invalid response contains success fingerprint %q", fingerprint)
		}
	}
}

func assertCandidateSecurityHeaders(t *testing.T, header http.Header, contentType string) {
	t.Helper()
	if header.Get("Content-Type") != contentType || header.Get("Cache-Control") != "no-store" ||
		header.Get("Connection") != "close" || header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("response headers = %#v", header)
	}
}
