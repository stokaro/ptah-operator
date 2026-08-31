package certrotation

// These tests intentionally use the package under test. The safety contract
// depends on failures between otherwise private transition steps; black-box
// tests cannot inject those interrupted states without weakening the API.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCertificateLifecycleRotations(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		now           time.Time
		mutateSecret  func(*corev1.Secret)
		wantCARotated bool
	}{
		{name: "near-expiry serving certificate", now: baseTime.Add(25 * 24 * time.Hour)},
		{name: "expired serving certificate", now: baseTime.Add(31 * 24 * time.Hour)},
		{
			name: "legacy Secret without CA private key",
			now:  baseTime,
			mutateSecret: func(secret *corev1.Secret) {
				delete(secret.Data, CAPrivateKeyKey)
			},
			wantCARotated: true,
		},
		{name: "near-expiry CA", now: baseTime.Add(359 * 24 * time.Hour), wantCARotated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			original := mustGenerateMaterial(t, baseTime, config)
			secret := secretForMaterial(config, original)
			if test.mutateSecret != nil {
				test.mutateSecret(secret)
			}
			client := newTestClient(config, secret, original.caPEM, twoReadyEndpoints(config))
			prober := &recordingProber{}
			rotator := mustNewTestRotator(t, client, config, test.now, prober)

			if err := rotator.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			updated := mustGetSecret(t, client, config)
			state, err := inspectSecret(updated, config, test.now)
			if err != nil {
				t.Fatalf("inspect updated Secret: %v", err)
			}
			if state.rotateCA || state.rotateServing {
				t.Fatalf("updated Secret still needs rotation: %+v", state)
			}
			caRotated := !bytes.Equal(original.ca.Raw, state.current.ca.Raw)
			if caRotated != test.wantCARotated {
				t.Fatalf("CA rotated = %v, want %v", caRotated, test.wantCARotated)
			}
			if bytes.Equal(original.leaf.Raw, state.current.leaf.Raw) {
				t.Fatal("serving certificate was not rotated")
			}
			assertFinalBundles(t, client, config, state.current.caPEM)
			assertProbedAddresses(t, prober.addresses(), "10.0.0.10:9443", "10.0.0.11:9443")
		})
	}
}

func TestNoopCurrentCertificateDoesNotProbeOrWrite(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, now, config)
	client := newTestClient(config, secretForMaterial(config, material), material.caPEM, twoReadyEndpoints(config))
	prober := &recordingProber{}
	rotator := mustNewTestRotator(t, client, config, now, prober)

	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := prober.addresses(); len(got) != 0 {
		t.Fatalf("probe addresses = %v, want none", got)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
			t.Fatalf("unexpected update action: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestMalformedManagedTrustIsRepairedFromAuthoritativeSecret(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mutating   func(certificateMaterial) []byte
		validating func(certificateMaterial) [][]byte
	}{
		{
			name:     "one empty validating entry",
			mutating: func(material certificateMaterial) []byte { return material.caPEM },
			validating: func(material certificateMaterial) [][]byte {
				return [][]byte{material.caPEM, nil}
			},
		},
		{
			name:     "every entry malformed",
			mutating: func(certificateMaterial) []byte { return []byte("not PEM") },
			validating: func(material certificateMaterial) [][]byte {
				// A valid certificate followed by garbage is rejected as a whole;
				// the valid prefix must not be salvaged into trusted output.
				partial := append(append([]byte(nil), material.caPEM...), []byte("trailing garbage")...)
				return [][]byte{nil, partial}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			material := mustGenerateMaterial(t, now, config)
			client := newTestClient(config, secretForMaterial(config, material), material.caPEM, twoReadyEndpoints(config))
			setManagedBundles(t, client, config, test.mutating(material), test.validating(material))
			prober := &recordingProber{}
			rotator := mustNewTestRotator(t, client, config, now, prober)

			if err := rotator.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			assertFinalBundles(t, client, config, material.caPEM)
			assertProbedAddresses(t, prober.addresses(), "10.0.0.10:9443", "10.0.0.11:9443")
			for _, action := range client.Actions() {
				if action.GetVerb() == "update" && action.GetResource().Resource == "secrets" {
					t.Fatal("trust repair unexpectedly changed authoritative Secret material")
				}
			}
		})
	}
}

func TestExpiredCertificateRotationsRepairMalformedManagedTrust(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		now           time.Time
		mutating      []byte
		validating    [][]byte
		wantCARotated bool
	}{
		{
			name:          "expired serving certificate and one-sided damage",
			now:           baseTime.Add(31 * 24 * time.Hour),
			mutating:      nil,
			validating:    [][]byte{[]byte("not PEM"), nil},
			wantCARotated: false,
		},
		{
			name:          "expired CA and all damaged bundles",
			now:           baseTime.Add(366 * 24 * time.Hour),
			mutating:      []byte("not PEM"),
			validating:    [][]byte{nil, []byte("also not PEM")},
			wantCARotated: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			original := mustGenerateMaterial(t, baseTime, config)
			client := newTestClient(config, secretForMaterial(config, original), original.caPEM, twoReadyEndpoints(config))
			setManagedBundles(t, client, config, test.mutating, test.validating)
			prober := &recordingProber{}
			rotator := mustNewTestRotator(t, client, config, test.now, prober)

			if err := rotator.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			updated := mustGetSecret(t, client, config)
			state, err := inspectSecret(updated, config, test.now)
			if err != nil {
				t.Fatalf("inspect updated Secret: %v", err)
			}
			if state.rotateCA || state.rotateServing {
				t.Fatalf("updated Secret still needs rotation: %+v", state)
			}
			if got := !certificateRawEqual(original.ca, state.current.ca); got != test.wantCARotated {
				t.Fatalf("CA rotated = %v, want %v", got, test.wantCARotated)
			}
			if certificateRawEqual(original.leaf, state.current.leaf) {
				t.Fatal("serving certificate was not rotated")
			}
			assertFinalBundles(t, client, config, state.current.caPEM)
			assertProbedAddresses(t, prober.addresses(), "10.0.0.10:9443", "10.0.0.11:9443")
		})
	}
}

func TestInterruptedBeforeSecondOverlapPublicationRecovers(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	failOnce := true
	client.PrependReactor("update", "validatingwebhookconfigurations", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failOnce {
			failOnce = false
			return true, nil, errors.New("injected validating overlap failure")
		}
		return false, nil, nil
	})

	first := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}
	if got := mustGetSecret(t, client, config).Data[CAPrivateKeyKey]; len(got) != 0 {
		t.Fatal("Secret changed before both webhook configurations trusted the replacement CA")
	}

	prober := &recordingProber{}
	second := mustNewTestRotator(t, client, config, baseTime.Add(time.Minute), prober)
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	updated := mustGetSecret(t, client, config)
	if len(updated.Data[CAPrivateKeyKey]) == 0 {
		t.Fatal("recovery did not persist the replacement CA private key")
	}
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
}

func TestInterruptedAfterSecretUpdateBeforeReloadRecovers(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	firstProbe := &recordingProber{err: errors.New("certificate projection has not reloaded")}
	first := mustNewTestRotator(t, client, config, baseTime, firstProbe)
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}
	updated := mustGetSecret(t, client, config)
	if len(updated.Data[CAPrivateKeyKey]) == 0 {
		t.Fatal("replacement material was not atomically persisted")
	}
	assertBundleCertificateCount(t, mutatingBundle(t, client, config), 2)
	assertBundleCertificateCount(t, validatingBundle(t, client, config), 2)

	config.ProbeTimeout = time.Second
	secondProbe := &recordingProber{}
	second := mustNewTestRotator(t, client, config, baseTime.Add(time.Minute), secondProbe)
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
}

func TestInterruptedOneSidedContractionRecovers(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	failContraction := true
	client.PrependReactor("update", "validatingwebhookconfigurations", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updated := action.(k8stesting.UpdateAction).GetObject().(*admissionregistrationv1.ValidatingWebhookConfiguration)
		bundle := updated.Webhooks[0].ClientConfig.CABundle
		certificates, err := parseCertificateBundle(bundle)
		if err == nil && len(certificates) == 1 && failContraction {
			failContraction = false
			return true, nil, errors.New("injected contraction failure")
		}
		return false, nil, nil
	})

	first := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}
	assertBundleCertificateCount(t, mutatingBundle(t, client, config), 1)
	assertBundleCertificateCount(t, validatingBundle(t, client, config), 2)

	prober := &recordingProber{}
	second := mustNewTestRotator(t, client, config, baseTime.Add(time.Minute), prober)
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	updated := mustGetSecret(t, client, config)
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
	assertProbedAddresses(t, prober.addresses(), "10.0.0.10:9443", "10.0.0.11:9443")
}

func TestInterruptedManagedEntryUpdateRecoversEveryNamedWebhook(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	oldMaterial := mustGenerateMaterial(t, baseTime.Add(-24*time.Hour), config)
	current := mustGenerateMaterial(t, baseTime, config)
	overlap, err := combineCABundles(oldMaterial.caPEM, current.caPEM)
	if err != nil {
		t.Fatalf("combine interrupted bundle: %v", err)
	}
	client := newTestClient(config, secretForMaterial(config, current), oldMaterial.caPEM, twoReadyEndpoints(config))
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	validating.Webhooks[0].ClientConfig.CABundle = overlap
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("seed interrupted ValidatingWebhookConfiguration: %v", err)
	}

	prober := &recordingProber{}
	rotator := mustNewTestRotator(t, client, config, baseTime, prober)
	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertFinalBundles(t, client, config, current.caPEM)
	assertProbedAddresses(t, prober.addresses(), "10.0.0.10:9443", "10.0.0.11:9443")
}

func TestMissingNamedWebhookStopsBeforeSecretTransition(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, material)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	validating.Webhooks = validating.Webhooks[:1]
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("remove required webhook: %v", err)
	}

	rotator := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	err = rotator.Run(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(config.ValidatingWebhookNames[1])) {
		t.Fatalf("Run() error = %v, want missing webhook name", err)
	}
	if got := mustGetSecret(t, client, config).Data[CAPrivateKeyKey]; len(got) != 0 {
		t.Fatal("Secret changed before every explicitly managed webhook was found")
	}
}

func TestUnmanagedSameServiceWebhookIsNotModified(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	validating.Webhooks = append(validating.Webhooks, admissionregistrationv1.ValidatingWebhook{
		Name: "unmanaged.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace,
			},
		},
	})
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("add unmanaged webhook: %v", err)
	}

	rotator := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	validating, err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get updated ValidatingWebhookConfiguration: %v", err)
	}
	if !caBundlesEqual(validating.Webhooks[2].ClientConfig.CABundle, original.caPEM) {
		t.Fatal("rotator modified a webhook outside the explicit managed name set")
	}
}

func TestEndpointSetMustBeReadyAndStable(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		endpoints *discoveryv1.EndpointSlice
		wantError string
	}{
		{name: "zero endpoints", endpoints: endpointSlice(config), wantError: "no ready Pod endpoints"},
		{
			name: "unready endpoint",
			endpoints: endpointSlice(config, discoveryv1.Endpoint{
				Addresses:  []string{"10.0.0.10"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPointer(false)},
				TargetRef:  podReference("pod-a", "uid-a", config.Namespace),
			}),
			wantError: "not explicitly ready",
		},
		{
			name: "duplicate address",
			endpoints: endpointSlice(config,
				readyEndpoint("10.0.0.10", "pod-a", "uid-a", config.Namespace),
				readyEndpoint("10.0.0.10", "pod-b", "uid-b", config.Namespace),
			),
			wantError: "appears more than once",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			material := mustGenerateMaterial(t, baseTime, config)
			legacy := secretForMaterial(config, material)
			delete(legacy.Data, CAPrivateKeyKey)
			client := newTestClient(config, legacy, material.caPEM, test.endpoints)
			rotator := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
			err := rotator.Run(context.Background())
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte(test.wantError)) {
				t.Fatalf("Run() error = %v, want substring %q", err, test.wantError)
			}
			assertBundleCertificateCount(t, mutatingBundle(t, client, config), 2)
			assertBundleCertificateCount(t, validatingBundle(t, client, config), 2)
		})
	}
}

func TestEndpointSnapshotChangeRetriesFullProof(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = time.Second
	config.ProbeInterval = time.Millisecond
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, material)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
	changed := false
	prober := &recordingProber{callback: func(request probeRequest) {
		if changed {
			return
		}
		changed = true
		slice, err := client.DiscoveryV1().EndpointSlices(config.Namespace).Get(context.Background(), "webhook-endpoints", metav1.GetOptions{})
		if err != nil {
			t.Errorf("get EndpointSlice during probe: %v", err)
			return
		}
		slice.Endpoints[0].Addresses = []string{"10.0.0.12"}
		if _, err := client.DiscoveryV1().EndpointSlices(config.Namespace).Update(context.Background(), slice, metav1.UpdateOptions{}); err != nil {
			t.Errorf("update EndpointSlice during probe: %v", err)
		}
	}}
	rotator := mustNewTestRotator(t, client, config, baseTime, prober)

	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	addresses := prober.addresses()
	if !slices.Contains(addresses, "10.0.0.10:9443") || !slices.Contains(addresses, "10.0.0.12:9443") {
		t.Fatalf("probe addresses = %v, want proof attempts before and after endpoint change", addresses)
	}
}

func TestLeaseSerializesRotators(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.AcquireTimeout = 20 * time.Millisecond
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, material)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
	entered := make(chan struct{})
	release := make(chan struct{})
	firstProbe := &recordingProber{callback: func(probeRequest) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}}
	first := mustNewTestRotator(t, client, config, baseTime, firstProbe)
	firstResult := make(chan error, 1)
	go func() { firstResult <- first.Run(context.Background()) }()
	<-entered

	secondConfig := config
	secondConfig.HolderIdentity = "job-b/uid-b"
	second := mustNewTestRotator(t, client, secondConfig, baseTime, &recordingProber{})
	if err := second.Run(context.Background()); err == nil {
		t.Fatal("concurrent Run() unexpectedly acquired the held Lease")
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
}

type recordingProber struct {
	mu       sync.Mutex
	requests []probeRequest
	err      error
	callback func(probeRequest)
}

func (prober *recordingProber) Probe(_ context.Context, request probeRequest) error {
	prober.mu.Lock()
	prober.requests = append(prober.requests, request)
	callback := prober.callback
	err := prober.err
	prober.mu.Unlock()
	if callback != nil {
		callback(request)
	}
	return err
}

func (prober *recordingProber) addresses() []string {
	prober.mu.Lock()
	defer prober.mu.Unlock()
	addresses := make([]string, 0, len(prober.requests))
	for _, request := range prober.requests {
		addresses = append(addresses, request.Address)
	}
	return addresses
}

func testConfig() Config {
	return Config{
		Namespace:                      "ptah-system",
		SecretName:                     "ptah-webhook-cert",
		LeaseName:                      "ptah-cert-rotation",
		MutatingWebhookConfiguration:   "ptah-approval",
		MutatingWebhookNames:           []string{"mapproval.operator.ptah.dev"},
		ValidatingWebhookConfiguration: "ptah-approval",
		ValidatingWebhookNames:         []string{"vapproval.operator.ptah.dev", "vpodintent.operator.ptah.dev"},
		ServiceName:                    "ptah-webhook",
		ServiceNamespace:               "ptah-system",
		EndpointPortName:               "https",
		HolderIdentity:                 "job-a/uid-a",
		RenewalThreshold:               7 * 24 * time.Hour,
		ServingCertificateValidity:     30 * 24 * time.Hour,
		CACertificateValidity:          365 * 24 * time.Hour,
		ProbeTimeout:                   time.Second,
		ProbeInterval:                  time.Millisecond,
		LeaseDuration:                  30 * time.Second,
		AcquireTimeout:                 time.Second,
	}
}

func mustGenerateMaterial(t *testing.T, now time.Time, config Config) certificateMaterial {
	t.Helper()
	material, err := generateMaterial(rand.Reader, now, config)
	if err != nil {
		t.Fatalf("generateMaterial() error = %v", err)
	}
	return material
}

func secretForMaterial(config Config, material certificateMaterial) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: config.SecretName, Namespace: config.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			CACertificateKey:        append([]byte(nil), material.caPEM...),
			CAPrivateKeyKey:         append([]byte(nil), material.caKeyPEM...),
			corev1.TLSCertKey:       append([]byte(nil), material.certPEM...),
			corev1.TLSPrivateKeyKey: append([]byte(nil), material.keyPEM...),
		},
	}
}

func newTestClient(
	config Config,
	secret *corev1.Secret,
	bundle []byte,
	endpointSlice *discoveryv1.EndpointSlice,
) *fake.Clientset {
	serviceReference := func() *admissionregistrationv1.ServiceReference {
		return &admissionregistrationv1.ServiceReference{Name: config.ServiceName, Namespace: config.ServiceNamespace}
	}
	validatingWebhooks := make([]admissionregistrationv1.ValidatingWebhook, 0, len(config.ValidatingWebhookNames))
	for _, name := range config.ValidatingWebhookNames {
		validatingWebhooks = append(validatingWebhooks, admissionregistrationv1.ValidatingWebhook{
			Name: name,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				CABundle: append([]byte(nil), bundle...), Service: serviceReference(),
			},
		})
	}
	objects := []runtime.Object{
		secret,
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: config.LeaseName, Namespace: config.Namespace}},
		&admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: config.MutatingWebhookConfiguration},
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name: config.MutatingWebhookNames[0],
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: append([]byte(nil), bundle...), Service: serviceReference(),
				},
			}},
		},
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: config.ValidatingWebhookConfiguration},
			Webhooks:   validatingWebhooks,
		},
	}
	if endpointSlice != nil {
		objects = append(objects, endpointSlice)
	}
	return fake.NewClientset(objects...)
}

func endpointSlice(config Config, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	portName := config.EndpointPortName
	port := int32(9443)
	protocol := corev1.ProtocolTCP
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-endpoints",
			Namespace: config.ServiceNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: config.ServiceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port}},
		Endpoints:   endpoints,
	}
}

func twoReadyEndpoints(config Config) *discoveryv1.EndpointSlice {
	return endpointSlice(config,
		readyEndpoint("10.0.0.10", "pod-a", "uid-a", config.Namespace),
		readyEndpoint("10.0.0.11", "pod-b", "uid-b", config.Namespace),
	)
}

func readyEndpoint(address, podName, podUID, namespace string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{address},
		Conditions: discoveryv1.EndpointConditions{Ready: boolPointer(true)},
		TargetRef:  podReference(podName, podUID, namespace),
	}
}

func podReference(name, uid, namespace string) *corev1.ObjectReference {
	return &corev1.ObjectReference{Kind: "Pod", Name: name, Namespace: namespace, UID: types.UID(uid)}
}

func boolPointer(value bool) *bool { return &value }

func mustNewTestRotator(
	t *testing.T,
	client *fake.Clientset,
	config Config,
	now time.Time,
	prober certificateProber,
) *Rotator {
	t.Helper()
	rotator, err := New(client, config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	rotator.now = func() time.Time { return now }
	rotator.probe = prober
	return rotator
}

func mustGetSecret(t *testing.T, client *fake.Clientset, config Config) *corev1.Secret {
	t.Helper()
	secret, err := client.CoreV1().Secrets(config.Namespace).Get(context.Background(), config.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	return secret
}

func setManagedBundles(
	t *testing.T,
	client *fake.Clientset,
	config Config,
	mutating []byte,
	validating [][]byte,
) {
	t.Helper()
	mutatingConfiguration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	for i := range mutatingConfiguration.Webhooks {
		if slices.Contains(config.MutatingWebhookNames, mutatingConfiguration.Webhooks[i].Name) {
			mutatingConfiguration.Webhooks[i].ClientConfig.CABundle = append([]byte(nil), mutating...)
		}
	}
	if _, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(
		context.Background(), mutatingConfiguration, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("update MutatingWebhookConfiguration: %v", err)
	}

	validatingConfiguration, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	if len(validating) != len(config.ValidatingWebhookNames) {
		t.Fatalf("test validating bundle count = %d, want %d", len(validating), len(config.ValidatingWebhookNames))
	}
	for i := range validatingConfiguration.Webhooks {
		index := slices.Index(config.ValidatingWebhookNames, validatingConfiguration.Webhooks[i].Name)
		if index >= 0 {
			validatingConfiguration.Webhooks[i].ClientConfig.CABundle = append([]byte(nil), validating[index]...)
		}
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validatingConfiguration, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("update ValidatingWebhookConfiguration: %v", err)
	}
}

func mutatingBundle(t *testing.T, client *fake.Clientset, config Config) []byte {
	t.Helper()
	webhook, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	return webhook.Webhooks[0].ClientConfig.CABundle
}

func validatingBundle(t *testing.T, client *fake.Clientset, config Config) []byte {
	t.Helper()
	webhook, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	bundles := make([][]byte, 0, len(webhook.Webhooks))
	for _, entry := range webhook.Webhooks {
		if slices.Contains(config.ValidatingWebhookNames, entry.Name) {
			bundles = append(bundles, entry.ClientConfig.CABundle)
		}
	}
	combined, err := combineCABundles(bundles...)
	if err != nil {
		t.Fatalf("combine ValidatingWebhookConfiguration bundles: %v", err)
	}
	return combined
}

func assertFinalBundles(t *testing.T, client *fake.Clientset, config Config, want []byte) {
	t.Helper()
	for name, got := range map[string][]byte{
		"mutating":   mutatingBundle(t, client, config),
		"validating": validatingBundle(t, client, config),
	} {
		if !caBundlesEqual(got, want) {
			t.Errorf("%s CA bundle does not equal the current Secret CA", name)
		}
		assertBundleCertificateCount(t, got, 1)
	}
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	for _, webhook := range validating.Webhooks {
		if slices.Contains(config.ValidatingWebhookNames, webhook.Name) && !caBundlesEqual(webhook.ClientConfig.CABundle, want) {
			t.Errorf("validating webhook %q did not receive the current Secret CA", webhook.Name)
		}
	}
}

func assertBundleCertificateCount(t *testing.T, bundle []byte, want int) {
	t.Helper()
	certificates, err := parseCertificateBundle(bundle)
	if err != nil {
		t.Fatalf("parse CA bundle: %v", err)
	}
	if got := len(certificates); got != want {
		t.Fatalf("CA bundle certificate count = %d, want %d", got, want)
	}
}

func assertProbedAddresses(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, address := range want {
		if !slices.Contains(got, address) {
			t.Errorf("probe addresses = %v, missing %q", got, address)
		}
		if _, _, err := net.SplitHostPort(address); err != nil {
			t.Fatalf("invalid test address %q: %v", address, err)
		}
	}
}
