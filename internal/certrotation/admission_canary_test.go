package certrotation

// These tests intentionally use the package-internal publication snapshots and
// client factory. They exercise a security protocol whose exact readback and
// multi-object identity cannot be observed through the exported API alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/kubeapi"
)

func TestAdmissionCanaryPublishesExpansionAndContractionExactly(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	expansion := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)

	if err := fixture.canary.PublishMutating(context.Background(), expansion); err != nil {
		t.Fatalf("PublishMutating(expansion) error = %v", err)
	}
	if err := fixture.canary.PublishValidating(context.Background(), expansion); err != nil {
		t.Fatalf("PublishValidating(expansion) error = %v", err)
	}
	assertAdmissionCanaryPublication(t, fixture, expansion)

	updates := countAdmissionCanaryUpdates(fixture.client)
	if err := fixture.canary.PublishMutating(context.Background(), expansion); err != nil {
		t.Fatalf("idempotent PublishMutating(expansion) error = %v", err)
	}
	if err := fixture.canary.PublishValidating(context.Background(), expansion); err != nil {
		t.Fatalf("idempotent PublishValidating(expansion) error = %v", err)
	}
	if got := countAdmissionCanaryUpdates(fixture.client); got != updates {
		t.Fatalf("idempotent publication update count = %d, want %d", got, updates)
	}

	contraction := mustAdmissionCanaryContraction(t, fixture.newCA, fixture.proofCA)
	if err := fixture.canary.PublishMutating(context.Background(), contraction); err != nil {
		t.Fatalf("PublishMutating(contraction) error = %v", err)
	}
	if err := fixture.canary.PublishValidating(context.Background(), contraction); err != nil {
		t.Fatalf("PublishValidating(contraction) error = %v", err)
	}
	assertAdmissionCanaryPublication(t, fixture, contraction)

	parked, err := NewAdmissionCanaryParked(fixture.newCA)
	if err != nil {
		t.Fatalf("NewAdmissionCanaryParked() error = %v", err)
	}
	if err := fixture.canary.PublishMutating(context.Background(), parked); err != nil {
		t.Fatalf("PublishMutating(parked) error = %v", err)
	}
	if err := fixture.canary.PublishValidating(context.Background(), parked); err != nil {
		t.Fatalf("PublishValidating(parked) error = %v", err)
	}
	assertAdmissionCanaryPublication(t, fixture, parked)
}

func TestNewAdmissionCanaryRejectsTypedNilDependencies(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	provider := kubeapi.Provider(func(context.Context) (kubeapi.Snapshot, error) {
		return kubeapi.Snapshot{}, nil
	})
	var nilClient *fake.Clientset
	var nilProvider kubeapi.Provider

	for _, test := range []struct {
		name     string
		client   kubernetes.Interface
		provider kubeapi.Provider
	}{
		{name: "Kubernetes client", client: nilClient, provider: provider},
		{name: "API-server provider", client: client, provider: nilProvider},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewAdmissionCanary(test.client, test.provider, AdmissionCanaryConfig{}); err == nil ||
				err.Error() != "admission canary Kubernetes client and API-server provider are required" {
				t.Fatalf("NewAdmissionCanary() error = %v, want dependency rejection", err)
			}
		})
	}
}

func TestAdmissionCanaryStaticWebhookContractIsNarrowAndDisjoint(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	desired := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)
	mutating := fixture.canary.mutatingWebhook(desired.canaryBundle)
	validating := fixture.canary.validatingWebhook(desired.canaryBundle)

	if AdmissionCanaryMutatingFieldManager == AdmissionCanaryValidatingFieldManager {
		t.Fatal("mutating and validating canaries share a field manager")
	}
	for _, test := range []struct {
		name         string
		webhookName  string
		path         string
		fieldManager string
		client       admissionregistrationv1.WebhookClientConfig
		rules        []admissionregistrationv1.RuleWithOperations
		match        []admissionregistrationv1.MatchCondition
		failure      *admissionregistrationv1.FailurePolicyType
		matchPolicy  *admissionregistrationv1.MatchPolicyType
		sideEffects  *admissionregistrationv1.SideEffectClass
		timeout      *int32
		review       []string
	}{
		{
			name: "mutating", webhookName: mutating.Name, path: AdmissionCanaryMutatingPath,
			fieldManager: AdmissionCanaryMutatingFieldManager, client: mutating.ClientConfig,
			rules: mutating.Rules, match: mutating.MatchConditions, failure: mutating.FailurePolicy,
			matchPolicy: mutating.MatchPolicy, sideEffects: mutating.SideEffects,
			timeout: mutating.TimeoutSeconds, review: mutating.AdmissionReviewVersions,
		},
		{
			name: "validating", webhookName: validating.Name, path: AdmissionCanaryValidatingPath,
			fieldManager: AdmissionCanaryValidatingFieldManager, client: validating.ClientConfig,
			rules: validating.Rules, match: validating.MatchConditions, failure: validating.FailurePolicy,
			matchPolicy: validating.MatchPolicy, sideEffects: validating.SideEffects,
			timeout: validating.TimeoutSeconds, review: validating.AdmissionReviewVersions,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.client.Service == nil || test.client.Service.Name != fixture.config.CandidateServiceName ||
				test.client.Service.Namespace != fixture.config.ServiceNamespace ||
				test.client.Service.Path == nil || *test.client.Service.Path != test.path ||
				test.client.Service.Port == nil || *test.client.Service.Port != webhookServicePort ||
				test.client.URL != nil {
				t.Fatalf("%s canary client contract = %#v", test.name, test.client)
			}
			if len(test.rules) != 1 || !reflect.DeepEqual(test.rules[0], admissionCanaryRule()) {
				t.Fatalf("%s canary rules = %#v", test.name, test.rules)
			}
			if test.failure == nil || *test.failure != admissionregistrationv1.Fail ||
				test.matchPolicy == nil || *test.matchPolicy != admissionregistrationv1.Exact ||
				test.sideEffects == nil || *test.sideEffects != admissionregistrationv1.SideEffectClassNone ||
				test.timeout == nil || *test.timeout != admissionCanaryTimeoutSeconds ||
				!slices.Equal(test.review, []string{"v1"}) {
				t.Fatalf("%s canary fail-closed fields are incomplete", test.name)
			}
			if len(test.match) != 1 {
				t.Fatalf("%s canary match conditions = %#v", test.name, test.match)
			}
			expression := test.match[0].Expression
			for _, required := range []string{
				`request.operation == "UPDATE"`,
				`request.resource.group == ""`,
				`request.namespace == "` + fixture.config.MarkerNamespace + `"`,
				`request.name == "` + fixture.config.MarkerName + `"`,
				`request.userInfo.username == "system:serviceaccount:` + fixture.config.MarkerNamespace + `:` + fixture.config.ServiceAccountName + `"`,
				`request.dryRun == true`,
				`request.options.fieldManager == "` + test.fieldManager + `"`,
				`request.options.fieldValidation == "Strict"`,
			} {
				if !strings.Contains(expression, required) {
					t.Errorf("%s canary match expression lacks %q: %s", test.name, required, expression)
				}
			}
		})
	}
}

func TestAdmissionCanaryRefusesForeignCandidateServiceContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*admissionCanaryFixture)
		call   func(*admissionCanaryFixture, AdmissionCanaryDesiredState) error
		want   string
	}{
		{
			name: "mutating canary path drift",
			mutate: func(f *admissionCanaryFixture) {
				configuration := mustGetAdmissionCanaryMutating(t, f)
				entry := f.canary.mutatingWebhook(f.oldCA)
				entry.ClientConfig.Service.Path = admissionCanaryStringPointer("/foreign")
				configuration.Webhooks = append(configuration.Webhooks, entry)
				mustUpdateAdmissionCanaryMutating(t, f, configuration)
			},
			call: func(f *admissionCanaryFixture, desired AdmissionCanaryDesiredState) error {
				return f.canary.PublishMutating(context.Background(), desired)
			},
			want: "static contract",
		},
		{
			name: "foreign validating candidate target",
			mutate: func(f *admissionCanaryFixture) {
				configuration := mustGetAdmissionCanaryValidating(t, f)
				configuration.Webhooks = append(configuration.Webhooks, admissionregistrationv1.ValidatingWebhook{
					Name: "foreign.operator.ptah.dev",
					ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{
						Name: f.config.CandidateServiceName, Namespace: f.config.ServiceNamespace,
					}},
				})
				mustUpdateAdmissionCanaryValidating(t, f, configuration)
			},
			call: func(f *admissionCanaryFixture, desired AdmissionCanaryDesiredState) error {
				return f.canary.PublishValidating(context.Background(), desired)
			},
			want: "foreign validating webhook",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newAdmissionCanaryFixture(t)
			desired := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)
			test.mutate(fixture)
			updatesBefore := countAdmissionCanaryUpdates(fixture.client)
			err := test.call(fixture, desired)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("publication error = %v, want %q", err, test.want)
			}
			if got := countAdmissionCanaryUpdates(fixture.client); got != updatesBefore {
				t.Fatalf("foreign contract caused %d writes", got-updatesBefore)
			}
		})
	}
}

func TestAdmissionCanaryUpdateVerificationCoversFullObjectMetadata(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	desired := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)
	mutating, err := fixture.canary.buildMutatingCandidate(mustGetAdmissionCanaryMutating(t, fixture), desired)
	if err != nil {
		t.Fatalf("buildMutatingCandidate() error = %v", err)
	}
	if mutating.UID != "mutating-uid" || mutating.ResourceVersion != "1" {
		t.Fatalf("mutating candidate identity = %q/%q", mutating.UID, mutating.ResourceVersion)
	}
	mutatingResponse := mutating.DeepCopy()
	mutatingResponse.ResourceVersion = "2"
	mutatingResponse.Generation++
	if err := verifyMutatingUpdateResponse(mutating, mutatingResponse); err != nil {
		t.Fatalf("exact mutating update response: %v", err)
	}
	mutatingDrift := mutatingResponse.DeepCopy()
	mutatingDrift.Labels = map[string]string{"foreign": "value"}
	if err := verifyMutatingUpdateResponse(mutating, mutatingDrift); err == nil {
		t.Fatal("mutating update response metadata drift was accepted")
	}
	if err := verifyMutatingReadback(mutatingResponse, mutatingDrift); err == nil {
		t.Fatal("mutating readback metadata drift was accepted")
	}

	validating, err := fixture.canary.buildValidatingCandidate(mustGetAdmissionCanaryValidating(t, fixture), desired)
	if err != nil {
		t.Fatalf("buildValidatingCandidate() error = %v", err)
	}
	if validating.UID != "validating-uid" || validating.ResourceVersion != "1" {
		t.Fatalf("validating candidate identity = %q/%q", validating.UID, validating.ResourceVersion)
	}
	validatingResponse := validating.DeepCopy()
	validatingResponse.ResourceVersion = "2"
	validatingResponse.Generation++
	if err := verifyValidatingUpdateResponse(validating, validatingResponse); err != nil {
		t.Fatalf("exact validating update response: %v", err)
	}
	validatingDrift := validatingResponse.DeepCopy()
	validatingDrift.Annotations = map[string]string{"foreign": "value"}
	if err := verifyValidatingUpdateResponse(validating, validatingDrift); err == nil {
		t.Fatal("validating update response metadata drift was accepted")
	}
	if err := verifyValidatingReadback(validatingResponse, validatingDrift); err == nil {
		t.Fatal("validating readback metadata drift was accepted")
	}
}

func TestAdmissionCanaryDenialClassifierRequiresExactStatusFingerprint(t *testing.T) {
	t.Parallel()

	const markerName = "ptah-cert-canary"
	markerUID := types.UID("marker-uid")
	exact := admissionCanaryAPIError(
		AdmissionCanaryMutatingWebhookName,
		AdmissionCanaryMutatingDenialStatus(markerName, markerUID),
	)
	if !HasExactAdmissionCanaryMutatingDenial(fmt.Errorf("wrapped: %w", exact), markerName, markerUID) {
		t.Fatal("exact wrapped mutating denial was not recognized")
	}
	if HasExactAdmissionCanaryValidatingDenial(exact, markerName, markerUID) {
		t.Fatal("mutating denial proved the validating canary")
	}

	tests := []struct {
		name   string
		mutate func(*metav1.Status)
	}{
		{name: "wrong webhook", mutate: func(status *metav1.Status) {
			status.Message = strings.Replace(status.Message, AdmissionCanaryMutatingWebhookName, "foreign.operator.ptah.dev", 1)
		}},
		{name: "wrong status", mutate: func(status *metav1.Status) { status.Status = metav1.StatusSuccess }},
		{name: "wrong reason", mutate: func(status *metav1.Status) { status.Reason = metav1.StatusReasonForbidden }},
		{name: "wrong code", mutate: func(status *metav1.Status) { status.Code = http.StatusForbidden }},
		{name: "wrong marker uid", mutate: func(status *metav1.Status) { status.Details.UID = "replacement" }},
		{name: "wrong cause type", mutate: func(status *metav1.Status) {
			status.Details.Causes[0].Type = metav1.CauseTypeFieldValueNotFound
		}},
		{name: "extra cause", mutate: func(status *metav1.Status) {
			status.Details.Causes = append(status.Details.Causes, metav1.StatusCause{Message: "foreign"})
		}},
		{name: "metadata", mutate: func(status *metav1.Status) { status.ResourceVersion = "1" }},
		{name: "foreign type meta", mutate: func(status *metav1.Status) {
			status.APIVersion = "v2"
			status.Kind = "ForeignStatus"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status := exact.(*apierrors.StatusError).ErrStatus.DeepCopy()
			test.mutate(status)
			if HasExactAdmissionCanaryMutatingDenial(&apierrors.StatusError{ErrStatus: *status}, markerName, markerUID) {
				t.Fatal("foreign denial fingerprint was accepted")
			}
		})
	}
	if HasExactAdmissionCanaryMutatingDenial(errors.New(exact.Error()), markerName, markerUID) {
		t.Fatal("plain-text denial was accepted")
	}
}

func TestAdmissionCanaryEndpointProbeRequiresBothExactDenials(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	endpoint := kubeapi.Endpoint{Address: "192.0.2.10:6443", RESTConfig: &rest.Config{Host: "https://192.0.2.10:6443"}}
	exactResponses := func(marker *corev1.ConfigMap) map[string]error {
		return map[string]error{
			AdmissionCanaryMutatingFieldManager: admissionCanaryAPIError(
				AdmissionCanaryMutatingWebhookName,
				AdmissionCanaryMutatingDenialStatus(marker.Name, marker.UID),
			),
			AdmissionCanaryValidatingFieldManager: admissionCanaryAPIError(
				AdmissionCanaryValidatingWebhookName,
				AdmissionCanaryValidatingDenialStatus(marker.Name, marker.UID),
			),
		}
	}

	tests := []struct {
		name   string
		mutate func(*scriptedAdmissionCanaryMarkerClient)
		want   bool
	}{
		{name: "both exact denials", want: true},
		{name: "mutating admitted", mutate: func(client *scriptedAdmissionCanaryMarkerClient) {
			client.responses[AdmissionCanaryMutatingFieldManager] = nil
		}},
		{name: "mutating transport failure", mutate: func(client *scriptedAdmissionCanaryMarkerClient) {
			client.responses[AdmissionCanaryMutatingFieldManager] = errors.New("TLS handshake reset")
		}},
		{name: "validating foreign typed denial", mutate: func(client *scriptedAdmissionCanaryMarkerClient) {
			client.responses[AdmissionCanaryValidatingFieldManager] = admissionCanaryAPIError(
				"foreign.operator.ptah.dev",
				AdmissionCanaryValidatingDenialStatus(client.marker.Name, client.marker.UID),
			)
		}},
		{name: "stale marker", mutate: func(client *scriptedAdmissionCanaryMarkerClient) {
			client.marker.ResourceVersion = "older"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedAdmissionCanaryMarkerClient{
				marker:    fixture.marker.DeepCopy(),
				responses: exactResponses(fixture.marker),
			}
			if test.mutate != nil {
				test.mutate(client)
			}
			fixtureCopy := *fixture.canary
			fixtureCopy.directClientFactory = func(*rest.Config, string) (admissionCanaryMarkerClient, func(), error) {
				return client, func() {}, nil
			}
			proven, err := fixtureCopy.endpointProbe(fixture.marker)(context.Background(), endpoint)
			if err != nil {
				t.Fatalf("endpoint probe error = %v", err)
			}
			if proven != test.want {
				t.Fatalf("endpoint probe proven = %t, want %t", proven, test.want)
			}
			if test.want && !slices.Equal(client.fieldManagers(), []string{
				AdmissionCanaryMutatingFieldManager,
				AdmissionCanaryValidatingFieldManager,
			}) {
				t.Fatalf("endpoint probe field managers = %v", client.fieldManagers())
			}
		})
	}
}

func TestAdmissionCanaryStoredContractCombinesBothConfigurationsAndMarker(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	desired := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)
	mustPublishAdmissionCanary(t, fixture, desired)
	publication, err := fixture.canary.capturePublication(context.Background(), desired)
	if err != nil {
		t.Fatalf("capturePublication() error = %v", err)
	}
	probe := fixture.canary.storedContractProbe(publication)
	identity, proven, err := probe(context.Background())
	if err != nil || !proven || identity != publication.identity || !strings.HasPrefix(identity, "sha256:") {
		t.Fatalf("exact stored contract = (%q, %t, %v)", identity, proven, err)
	}

	validating := mustGetAdmissionCanaryValidating(t, fixture)
	validating.ResourceVersion = "changed-validating-rv"
	mustUpdateAdmissionCanaryValidating(t, fixture, validating)
	if _, proven, err := probe(context.Background()); err != nil || proven {
		t.Fatalf("validating-only drift = (%t, %v), want inconclusive", proven, err)
	}
	mustUpdateAdmissionCanaryValidating(t, fixture, publication.validating.DeepCopy())

	marker := fixture.marker.DeepCopy()
	marker.ResourceVersion = "changed-marker-rv"
	if _, err := fixture.client.CoreV1().ConfigMaps(fixture.config.MarkerNamespace).Update(
		context.Background(), marker, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("drift marker: %v", err)
	}
	if _, proven, err := probe(context.Background()); err != nil || proven {
		t.Fatalf("marker-only drift = (%t, %v), want inconclusive", proven, err)
	}
}

func TestAdmissionCanaryWaitClosesOnlyAfterCombinedProof(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	desired := mustAdmissionCanaryExpansion(t, fixture.oldCA, fixture.newCA)
	mustPublishAdmissionCanary(t, fixture, desired)
	direct := &scriptedAdmissionCanaryMarkerClient{
		marker: fixture.marker.DeepCopy(),
		responses: map[string]error{
			AdmissionCanaryMutatingFieldManager: admissionCanaryAPIError(
				AdmissionCanaryMutatingWebhookName,
				AdmissionCanaryMutatingDenialStatus(fixture.marker.Name, fixture.marker.UID),
			),
			AdmissionCanaryValidatingFieldManager: admissionCanaryAPIError(
				AdmissionCanaryValidatingWebhookName,
				AdmissionCanaryValidatingDenialStatus(fixture.marker.Name, fixture.marker.UID),
			),
		},
	}
	fixture.canary.directClientFactory = func(*rest.Config, string) (admissionCanaryMarkerClient, func(), error) {
		return direct, func() {}, nil
	}
	if err := fixture.canary.Wait(context.Background(), desired); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got := len(direct.fieldManagers()); got < 4 || got%2 != 0 {
		t.Fatalf("stability barrier dry-run count = %d, want at least two complete pairs", got)
	}
}

func TestAdmissionCanaryDirectClientUsesFreshClosedHTTP11Requests(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	var mu sync.Mutex
	var fieldManagers []string
	requestCount := 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requestCount++
		if request.ProtoMajor != 1 || !request.Close {
			t.Errorf("request %d used %s close=%t, want fresh HTTP/1.1", requestCount, request.Proto, request.Close)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			if err := json.NewEncoder(writer).Encode(fixture.marker); err != nil {
				t.Errorf("encode marker: %v", err)
			}
		case http.MethodPut:
			var marker corev1.ConfigMap
			if err := json.NewDecoder(request.Body).Decode(&marker); err != nil {
				t.Errorf("decode marker update: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if !sameAdmissionCanaryMarker(&marker, fixture.marker, fixture.config.ReleaseName) ||
				request.URL.Query().Get("dryRun") != metav1.DryRunAll ||
				request.URL.Query().Get("fieldValidation") != metav1.FieldValidationStrict {
				t.Errorf("inexact direct marker update: %#v query=%v", marker.ObjectMeta, request.URL.Query())
			}
			fieldManager := request.URL.Query().Get("fieldManager")
			fieldManagers = append(fieldManagers, fieldManager)
			var webhookName string
			var status *metav1.Status
			switch fieldManager {
			case AdmissionCanaryMutatingFieldManager:
				webhookName = AdmissionCanaryMutatingWebhookName
				status = AdmissionCanaryMutatingDenialStatus(marker.Name, marker.UID)
			case AdmissionCanaryValidatingFieldManager:
				webhookName = AdmissionCanaryValidatingWebhookName
				status = AdmissionCanaryValidatingDenialStatus(marker.Name, marker.UID)
			default:
				t.Errorf("unexpected field manager %q", fieldManager)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			status.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}
			status.Message = fmt.Sprintf("admission webhook %q denied the request: %s", webhookName, status.Message)
			writer.WriteHeader(http.StatusUnprocessableEntity)
			if err := json.NewEncoder(writer).Encode(status); err != nil {
				t.Errorf("encode denial: %v", err)
			}
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	certificate := server.Certificate()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	endpoint := kubeapi.Endpoint{
		Address: strings.TrimPrefix(server.URL, "https://"),
		RESTConfig: &rest.Config{
			Host: server.URL,
			ContentConfig: rest.ContentConfig{
				ContentType: runtime.ContentTypeJSON,
			},
			TLSClientConfig: rest.TLSClientConfig{
				CAData: caPEM,
			},
		},
	}
	observedMarker, err := fixture.canary.directGetMarker(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("direct marker GET error = %v", err)
	}
	if !sameAdmissionCanaryMarker(observedMarker, fixture.marker, fixture.config.ReleaseName) {
		t.Fatalf("direct marker GET = %#v, want %#v", observedMarker, fixture.marker)
	}
	proven, err := fixture.canary.endpointProbe(fixture.marker)(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("direct endpoint probe error = %v", err)
	}
	if !proven {
		mu.Lock()
		observedRequests := requestCount
		observedManagers := slices.Clone(fieldManagers)
		mu.Unlock()
		t.Fatalf("direct endpoint probe was inconclusive after %d requests with fieldManagers=%v", observedRequests, observedManagers)
	}
	mu.Lock()
	defer mu.Unlock()
	if requestCount != 4 || !slices.Equal(fieldManagers, []string{
		AdmissionCanaryMutatingFieldManager,
		AdmissionCanaryValidatingFieldManager,
	}) {
		t.Fatalf("direct requests = %d fieldManagers=%v", requestCount, fieldManagers)
	}
}

func TestRotatorRejectsCanaryForAnotherHelmRelease(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	config := testConfig()
	if _, err := New(fixture.client, config, &recordingCandidateSink{}, fixture.canary); err != nil {
		t.Fatalf("matching canary identity rejected: %v", err)
	}
	config.ReleaseName = "another-release"
	if _, err := New(fixture.client, config, &recordingCandidateSink{}, fixture.canary); err == nil ||
		!strings.Contains(err.Error(), "admission canary identity differs") {
		t.Fatalf("foreign canary release error = %v", err)
	}
}

func TestAdmissionCanaryMarkerAndDesiredStateRejectDrift(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	marker := fixture.marker.DeepCopy()
	if err := verifyAdmissionCanaryMarker(marker, fixture.config.MarkerNamespace, fixture.config.MarkerName, fixture.config.ReleaseName); err != nil {
		t.Fatalf("Helm-owned live marker rejected: %v", err)
	}
	marker.Annotations[HelmReleaseNameAnnotation] = "another-release"
	if err := verifyAdmissionCanaryMarker(marker, fixture.config.MarkerNamespace, fixture.config.MarkerName, fixture.config.ReleaseName); err == nil {
		t.Fatal("marker from another Helm release was accepted")
	}
	marker = fixture.marker.DeepCopy()
	marker.Annotations = map[string]string{"foreign": "value"}
	if err := verifyAdmissionCanaryMarker(marker, fixture.config.MarkerNamespace, fixture.config.MarkerName, fixture.config.ReleaseName); err == nil {
		t.Fatal("foreign marker annotation was accepted")
	}
	marker = fixture.marker.DeepCopy()
	marker.Immutable = nil
	if err := verifyAdmissionCanaryMarker(marker, fixture.config.MarkerNamespace, fixture.config.MarkerName, fixture.config.ReleaseName); err == nil {
		t.Fatal("mutable marker was accepted")
	}

	if _, err := NewAdmissionCanaryExpansion(fixture.newCA, fixture.newCA); err == nil {
		t.Fatal("same-CA expansion was accepted")
	}
	old := mustGenerateMaterial(t, time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC), testConfig())
	sameKeyNew := mustReissueCAWithSubject(t, old, old.ca.Subject.CommonName)
	if bytes.Equal(old.ca.Raw, sameKeyNew.ca.Raw) {
		t.Fatal("same-key expansion test did not reissue a distinct CA certificate")
	}
	if _, err := NewAdmissionCanaryExpansion(old.caPEM, sameKeyNew.caPEM); err == nil ||
		!strings.Contains(err.Error(), "distinct") {
		t.Fatalf("same-key expansion error = %v, want distinct-CA rejection", err)
	}
	if _, err := NewAdmissionCanaryContraction(fixture.newCA, fixture.newCA); err == nil {
		t.Fatal("non-independent contraction proof CA was accepted")
	}
	production := mustGenerateMaterial(t, time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC), testConfig())
	sameKeyProof := mustReissueCAWithSubject(t, production, "same-key-proof")
	if _, err := NewAdmissionCanaryContraction(production.caPEM, sameKeyProof.caPEM); err == nil ||
		!strings.Contains(err.Error(), "independent") {
		t.Fatalf("same-key contraction proof error = %v, want independence rejection", err)
	}
	if _, err := NewAdmissionCanaryExpansion([]byte("not PEM"), fixture.newCA); err == nil {
		t.Fatal("malformed expansion CA was accepted")
	}
}

func TestAdmissionCanaryExpansionWithoutOldCAKeepsExistingTrust(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionCanaryFixture(t)
	desired, err := NewAdmissionCanaryExpansion(nil, fixture.newCA)
	if err != nil {
		t.Fatalf("NewAdmissionCanaryExpansion(nil, newCA) error = %v", err)
	}
	if err := fixture.canary.PublishMutating(context.Background(), desired); err != nil {
		t.Fatalf("PublishMutating() error = %v", err)
	}
	configuration := mustGetAdmissionCanaryMutating(t, fixture)
	production := configuration.Webhooks[0].ClientConfig.CABundle
	if !caBundleContainsAllCertificates(production, fixture.oldCA) ||
		!caBundleContainsAllCertificates(production, fixture.newCA) {
		t.Fatal("bootstrap expansion did not preserve existing trust and append the new CA")
	}
	canary := configuration.Webhooks[len(configuration.Webhooks)-1]
	if canary.Name != AdmissionCanaryMutatingWebhookName || !slices.Equal(canary.ClientConfig.CABundle, desired.canaryBundle) {
		t.Fatal("bootstrap expansion canary does not trust exactly the new CA")
	}
}

type admissionCanaryFixture struct {
	canary  *AdmissionCanary
	client  *fake.Clientset
	config  AdmissionCanaryConfig
	marker  *corev1.ConfigMap
	oldCA   []byte
	newCA   []byte
	proofCA []byte
}

func newAdmissionCanaryFixture(t *testing.T) *admissionCanaryFixture {
	t.Helper()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	certificateConfig := testConfig()
	oldCA := mustGenerateMaterial(t, now, certificateConfig).caPEM
	newCA := mustGenerateMaterial(t, now.Add(time.Minute), certificateConfig).caPEM
	proofCA := mustGenerateMaterial(t, now.Add(2*time.Minute), certificateConfig).caPEM
	config := AdmissionCanaryConfig{
		ReleaseName:                    certificateConfig.ReleaseName,
		MarkerNamespace:                certificateConfig.Namespace,
		MarkerName:                     "ptah-certificate-rotation-canary",
		ServiceAccountName:             certificateConfig.SecretCreateServiceAccountName,
		MutatingWebhookConfiguration:   certificateConfig.MutatingWebhookConfiguration,
		MutatingWebhookNames:           slices.Clone(certificateConfig.MutatingWebhookNames),
		ValidatingWebhookConfiguration: certificateConfig.ValidatingWebhookConfiguration,
		ValidatingWebhookNames:         slices.Clone(certificateConfig.ValidatingWebhookNames),
		PrimaryServiceName:             certificateConfig.ServiceName,
		CandidateServiceName:           certificateConfig.CandidateServiceName,
		ServiceNamespace:               certificateConfig.ServiceNamespace,
		StabilityDuration:              time.Nanosecond,
		PollEvery:                      time.Nanosecond,
		RequestTimeout:                 time.Second,
	}
	serviceReference := func() *admissionregistrationv1.ServiceReference {
		return &admissionregistrationv1.ServiceReference{
			Name: certificateConfig.ServiceName, Namespace: certificateConfig.ServiceNamespace,
		}
	}
	foreignURL := "https://foreign.invalid/admit"
	objects := []runtime.Object{
		&admissionregistrationv1.MutatingWebhookConfiguration{
			TypeMeta: metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "MutatingWebhookConfiguration"},
			ObjectMeta: metav1.ObjectMeta{
				Name: config.MutatingWebhookConfiguration, UID: "mutating-uid", ResourceVersion: "1",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				{Name: config.MutatingWebhookNames[0], ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: slices.Clone(oldCA), Service: serviceReference(),
				}},
				{Name: "foreign-mutate.example.com", ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: &foreignURL}},
			},
		},
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			TypeMeta: metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingWebhookConfiguration"},
			ObjectMeta: metav1.ObjectMeta{
				Name: config.ValidatingWebhookConfiguration, UID: "validating-uid", ResourceVersion: "1",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{Name: config.ValidatingWebhookNames[0], ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: slices.Clone(oldCA), Service: serviceReference(),
				}},
				{Name: config.ValidatingWebhookNames[1], ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: slices.Clone(oldCA), Service: serviceReference(),
				}},
				{Name: "foreign-validate.example.com", ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: &foreignURL}},
			},
		},
	}
	marker := AdmissionCanaryMarker(config.MarkerNamespace, config.MarkerName, config.ReleaseName)
	marker.UID = "marker-uid"
	marker.ResourceVersion = "1"
	marker.CreationTimestamp = metav1.NewTime(now)
	objects = append(objects, marker)
	client := fake.NewClientset(objects...)
	provider := func(context.Context) (kubeapi.Snapshot, error) {
		return kubeapi.Snapshot{
			InventoryResourceVersion: "topology-rv",
			InventoryIdentity:        "topology-identity",
			Endpoints: []kubeapi.Endpoint{{
				Address: "192.0.2.10:6443", RESTConfig: &rest.Config{Host: "https://192.0.2.10:6443"},
			}},
		}, nil
	}
	canary, err := NewAdmissionCanary(client, provider, config)
	if err != nil {
		t.Fatalf("NewAdmissionCanary() error = %v", err)
	}
	return &admissionCanaryFixture{
		canary: canary, client: client, config: config, marker: marker.DeepCopy(),
		oldCA: oldCA, newCA: newCA, proofCA: proofCA,
	}
}

func mustAdmissionCanaryExpansion(t *testing.T, oldCA, newCA []byte) AdmissionCanaryDesiredState {
	t.Helper()
	desired, err := NewAdmissionCanaryExpansion(oldCA, newCA)
	if err != nil {
		t.Fatalf("NewAdmissionCanaryExpansion() error = %v", err)
	}
	return desired
}

func mustAdmissionCanaryContraction(t *testing.T, newCA, proofCA []byte) AdmissionCanaryDesiredState {
	t.Helper()
	desired, err := NewAdmissionCanaryContraction(newCA, proofCA)
	if err != nil {
		t.Fatalf("NewAdmissionCanaryContraction() error = %v", err)
	}
	return desired
}

func mustPublishAdmissionCanary(t *testing.T, fixture *admissionCanaryFixture, desired AdmissionCanaryDesiredState) {
	t.Helper()
	if err := fixture.canary.PublishMutating(context.Background(), desired); err != nil {
		t.Fatalf("PublishMutating() error = %v", err)
	}
	if err := fixture.canary.PublishValidating(context.Background(), desired); err != nil {
		t.Fatalf("PublishValidating() error = %v", err)
	}
}

func assertAdmissionCanaryPublication(t *testing.T, fixture *admissionCanaryFixture, desired AdmissionCanaryDesiredState) {
	t.Helper()
	mutating := mustGetAdmissionCanaryMutating(t, fixture)
	if err := fixture.canary.verifyMutatingPublication(mutating, desired); err != nil {
		t.Fatalf("verify mutating publication: %v", err)
	}
	validating := mustGetAdmissionCanaryValidating(t, fixture)
	if err := fixture.canary.verifyValidatingPublication(validating, desired); err != nil {
		t.Fatalf("verify validating publication: %v", err)
	}
	if mutating.Webhooks[1].Name != "foreign-mutate.example.com" ||
		validating.Webhooks[2].Name != "foreign-validate.example.com" {
		t.Fatal("publication changed unrelated webhook entries")
	}
	for _, webhook := range mutating.Webhooks {
		if webhook.Name == fixture.config.MutatingWebhookNames[0] && !desired.productionMatches(webhook.ClientConfig.CABundle) {
			t.Fatal("mutating production bundle does not match desired state")
		}
	}
	for _, webhook := range validating.Webhooks {
		if slices.Contains(fixture.config.ValidatingWebhookNames, webhook.Name) && !desired.productionMatches(webhook.ClientConfig.CABundle) {
			t.Fatalf("validating production webhook %q bundle does not match desired state", webhook.Name)
		}
	}
}

func countAdmissionCanaryUpdates(client *fake.Clientset) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" && (action.GetResource().Resource == "mutatingwebhookconfigurations" ||
			action.GetResource().Resource == "validatingwebhookconfigurations") {
			count++
		}
	}
	return count
}

func mustGetAdmissionCanaryMutating(t *testing.T, fixture *admissionCanaryFixture) *admissionregistrationv1.MutatingWebhookConfiguration {
	t.Helper()
	configuration, err := fixture.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), fixture.config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	return configuration
}

func mustGetAdmissionCanaryValidating(t *testing.T, fixture *admissionCanaryFixture) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	configuration, err := fixture.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), fixture.config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	return configuration
}

func mustUpdateAdmissionCanaryMutating(
	t *testing.T,
	fixture *admissionCanaryFixture,
	configuration *admissionregistrationv1.MutatingWebhookConfiguration,
) {
	t.Helper()
	if _, err := fixture.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(
		context.Background(), configuration, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("update MutatingWebhookConfiguration fixture: %v", err)
	}
}

func mustUpdateAdmissionCanaryValidating(
	t *testing.T,
	fixture *admissionCanaryFixture,
	configuration *admissionregistrationv1.ValidatingWebhookConfiguration,
) {
	t.Helper()
	if _, err := fixture.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), configuration, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("update ValidatingWebhookConfiguration fixture: %v", err)
	}
}

func admissionCanaryAPIError(webhookName string, response *metav1.Status) error {
	status := response.DeepCopy()
	status.Message = fmt.Sprintf("admission webhook %q denied the request: %s", webhookName, status.Message)
	return &apierrors.StatusError{ErrStatus: *status}
}

type scriptedAdmissionCanaryMarkerClient struct {
	mu        sync.Mutex
	marker    *corev1.ConfigMap
	getErr    error
	responses map[string]error
	updates   []metav1.UpdateOptions
	bodies    []*corev1.ConfigMap
}

func (client *scriptedAdmissionCanaryMarkerClient) Get(
	context.Context,
	string,
	metav1.GetOptions,
) (*corev1.ConfigMap, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.getErr != nil {
		return nil, client.getErr
	}
	return client.marker.DeepCopy(), nil
}

func (client *scriptedAdmissionCanaryMarkerClient) Update(
	_ context.Context,
	marker *corev1.ConfigMap,
	options metav1.UpdateOptions,
) (*corev1.ConfigMap, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.updates = append(client.updates, options)
	client.bodies = append(client.bodies, marker.DeepCopy())
	err, exists := client.responses[options.FieldManager]
	if !exists {
		return nil, errors.New("unexpected field manager")
	}
	if err != nil {
		return nil, err
	}
	return marker.DeepCopy(), nil
}

func (client *scriptedAdmissionCanaryMarkerClient) fieldManagers() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	fieldManagers := make([]string, 0, len(client.updates))
	for index, options := range client.updates {
		if !slices.Equal(options.DryRun, []string{metav1.DryRunAll}) ||
			options.FieldValidation != metav1.FieldValidationStrict {
			panic(fmt.Sprintf("update %d is not an exact strict dry-run", index))
		}
		fieldManagers = append(fieldManagers, options.FieldManager)
	}
	return fieldManagers
}

var _ admissionCanaryMarkerClient = (*scriptedAdmissionCanaryMarkerClient)(nil)
