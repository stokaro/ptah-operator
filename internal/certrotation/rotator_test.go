package certrotation

// These tests intentionally use the package under test. The safety contract
// depends on failures between otherwise private transition steps; black-box
// tests cannot inject those interrupted states without weakening the API.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCertificateLifecycleRotations(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		now            time.Time
		materialConfig func(Config) Config
		mutateSecret   func(*corev1.Secret)
		wantCARotated  bool
	}{
		{name: "near-expiry serving certificate", now: baseTime.Add(25 * 24 * time.Hour)},
		{
			name: "near-expiry serving certificate in Opaque Secret",
			now:  baseTime.Add(25 * 24 * time.Hour),
			mutateSecret: func(secret *corev1.Secret) {
				secret.Type = corev1.SecretTypeOpaque
			},
		},
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
		{
			name: "serving certificate exceeds configured lifetime",
			now:  baseTime,
			materialConfig: func(config Config) Config {
				config.ServingCertificateValidity *= 2
				return config
			},
		},
		{
			name: "CA certificate exceeds configured lifetime",
			now:  baseTime,
			materialConfig: func(config Config) Config {
				config.CACertificateValidity *= 2
				return config
			},
			wantCARotated: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			materialConfig := config
			if test.materialConfig != nil {
				materialConfig = test.materialConfig(materialConfig)
			}
			original := mustGenerateMaterial(t, baseTime, materialConfig)
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

func TestExportedSecretCreateGuardContractVerifiers(t *testing.T) {
	t.Parallel()

	config := testConfig()
	client := fake.NewSimpleClientset()
	installUnestablishedSecretCreateGuard(t, client, config)
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
		context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(
		context.Background(), config.SecretCreatePolicyBindingName, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySecretCreatePolicyContract(policy, config); err != nil {
		t.Fatalf("verify exact policy: %v", err)
	}
	if err := VerifySecretCreateBindingContract(binding, config); err != nil {
		t.Fatalf("verify exact binding: %v", err)
	}
	if err := VerifySecretCreatePolicyContract(nil, config); err == nil {
		t.Fatal("nil policy was accepted")
	}
	if err := VerifySecretCreateBindingContract(nil, config); err == nil {
		t.Fatal("nil binding was accepted")
	}
	policy.Spec.Validations[0].Message = "foreign"
	if err := VerifySecretCreatePolicyContract(policy, config); err == nil {
		t.Fatal("foreign policy was accepted")
	}
	binding.Spec.PolicyName = "foreign"
	if err := VerifySecretCreateBindingContract(binding, config); err == nil {
		t.Fatal("foreign binding was accepted")
	}
}

func TestCertificateLifetimePolicyAllowsOnlyBoundedClockSkew(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	config := testConfig()
	clockSkew := min(maximumCertificatePolicyClockSkew, config.ServingCertificateValidity/10)
	tests := []struct {
		name               string
		additionalValidity time.Duration
		wantRotateServing  bool
	}{
		{
			name:               "within clock skew",
			additionalValidity: clockSkew,
		},
		{
			name:               "beyond clock skew",
			additionalValidity: clockSkew + time.Second,
			wantRotateServing:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			materialConfig := config
			materialConfig.ServingCertificateValidity += test.additionalValidity
			material := mustGenerateMaterial(t, now, materialConfig)

			state, err := inspectSecret(secretForMaterial(config, material), config, now)
			if err != nil {
				t.Fatalf("inspectSecret() error = %v", err)
			}
			if state.rotateCA {
				t.Fatal("CA unexpectedly requires rotation")
			}
			if state.rotateServing != test.wantRotateServing {
				t.Fatalf("serving rotation = %v, want %v", state.rotateServing, test.wantRotateServing)
			}
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

func TestValidMaterialWithWrongSecretTypeIsNormalizedWithoutRotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	for _, secretType := range []corev1.SecretType{corev1.SecretTypeOpaque, ""} {
		name := string(secretType)
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			material := mustGenerateMaterial(t, now, config)
			secret := secretForMaterial(config, material)
			secret.Type = secretType
			client := newTestClient(config, secret, material.caPEM, twoReadyEndpoints(config))
			prober := &recordingProber{}
			rotator := mustNewTestRotator(t, client, config, now, prober)

			if err := rotator.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			updated := mustGetSecret(t, client, config)
			if updated.Type != corev1.SecretTypeTLS {
				t.Fatalf("normalized Secret type = %q, want %q", updated.Type, corev1.SecretTypeTLS)
			}
			if !secretContainsMaterial(updated, material) {
				t.Fatal("normalization changed valid certificate material")
			}
			if got := prober.addresses(); len(got) != 0 {
				t.Fatalf("normalization probed endpoints = %v, want none", got)
			}
			secretUpdates := 0
			for _, action := range client.Actions() {
				if action.GetVerb() != "update" {
					continue
				}
				switch action.GetResource().Resource {
				case "secrets":
					secretUpdates++
				case "mutatingwebhookconfigurations", "validatingwebhookconfigurations":
					t.Fatalf("normalization unexpectedly updated %s", action.GetResource().Resource)
				}
			}
			if secretUpdates != 1 {
				t.Fatalf("Secret update count = %d, want 1", secretUpdates)
			}
		})
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

func TestMalformedSecretCARecoversOnlyLiveAuthenticObservedRoot(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	unrelated := mustGenerateMaterial(t, now.Add(time.Minute), config)
	secret := secretForMaterial(config, original)
	secret.Data[CACertificateKey] = []byte("not a certificate")
	mixed := append(append(append([]byte(nil), unrelated.caPEM...), original.caPEM...), []byte("trailing damage")...)
	client := newTestClient(config, secret, original.caPEM, twoReadyEndpoints(config))
	setManagedBundles(t, client, config, mixed, [][]byte{original.caPEM, original.caPEM})
	assertCANotCopiedToValidatingUpdates(t, client, unrelated.caPEM)
	prober := &recordingProber{}
	rotator := mustNewTestRotator(t, client, config, now, prober)

	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	updated := mustGetSecret(t, client, config)
	if bytes.Equal(updated.Data[CACertificateKey], original.caPEM) {
		t.Fatal("recovery did not replace the malformed Secret CA")
	}
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
	requests := prober.probeRequests()
	if !slices.ContainsFunc(requests, func(request probeRequest) bool {
		return request.IdentityOnly && caBundlesEqual(request.CACertificatePEM, original.caPEM)
	}) {
		t.Fatal("recovery did not prove the authentic observed CA against the live exact leaf")
	}
}

func TestMissingSecretIsRecreatedOnlyBehindEstablishedGuard(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
	installEstablishedSecretCreateGuard(t, client, config)
	installSecretCreateAdmission(t, client, config)
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	created := mustGetSecret(t, client, config)
	state, err := inspectSecret(created, config, now)
	if err != nil {
		t.Fatalf("inspect recreated Secret: %v", err)
	}
	if state.rotateCA || state.rotateServing {
		t.Fatalf("recreated Secret still needs rotation: %+v", state)
	}
	if !maps.Equal(created.Labels, map[string]string{GeneratedSecretLabel: GeneratedSecretLabelValue}) {
		t.Fatalf("recreated Secret labels = %v", created.Labels)
	}
	assertFinalBundles(t, client, config, state.current.caPEM)
}

func TestMissingSecretRecreationIsDisabledByDefault(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.RecreateMissingSecret = false
	config.SecretCreatePolicyName = ""
	config.SecretCreatePolicyBindingName = ""
	config.SecretCreateServiceAccountName = ""
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	err := rotator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recreation is disabled") {
		t.Fatalf("Run() error = %v, want disabled-recreation error", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "secrets" {
			t.Fatal("disabled missing-Secret recovery attempted CREATE")
		}
	}
	assertFinalBundles(t, client, config, old.caPEM)
}

func TestSecretCreateValidationExpressionHandlesOptionalGenerateName(t *testing.T) {
	t.Parallel()
	const want = "(!has(object.metadata.generateName) || object.metadata.generateName == '')"
	if expression := compactCEL(secretCreateValidationExpression(testConfig())); !strings.Contains(expression, want) {
		t.Fatalf("Secret CREATE validation expression = %q, want presence-safe generateName check %q", expression, want)
	}
}

func TestMissingSecretGuardMustBeEstablishedBeforeCreate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
	installUnestablishedSecretCreateGuard(t, client, config)
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	err := rotator.Run(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("not established")) {
		t.Fatalf("Run() error = %v, want unestablished guard", err)
	}
	if _, getErr := client.CoreV1().Secrets(config.Namespace).Get(context.Background(), config.SecretName, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("missing Secret was created without an established guard: %v", getErr)
	}
	assertFinalBundles(t, client, config, old.caPEM)
}

func TestMissingSecretGuardRejectsIndeterminateConditionBeforeCreate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
	installEstablishedSecretCreateGuard(t, client, config)
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
		context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get established Secret CREATE guard: %v", err)
	}
	policy.Status.Conditions = []metav1.Condition{{
		Type:   "Ready",
		Status: metav1.ConditionUnknown,
	}}
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().UpdateStatus(
		context.Background(), policy, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("set indeterminate Secret CREATE guard condition: %v", err)
	}
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	err = rotator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not true: Unknown") {
		t.Fatalf("Run() error = %v, want indeterminate guard rejection", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "secrets" {
			t.Fatal("indeterminate guard reached a Secret CREATE request")
		}
	}
}

func TestMissingSecretRejectsBroadenedGuardContractBeforeCreate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, *fake.Clientset, Config)
	}{
		{
			name: "missing exact ServiceAccount match condition",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
					context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard policy: %v", err)
				}
				policy.Spec.MatchConditions = nil
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(
					context.Background(), policy, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard policy: %v", err)
				}
			},
		},
		{
			name: "missing exact namespace selector",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(
					context.Background(), config.SecretCreatePolicyBindingName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard binding: %v", err)
				}
				binding.Spec.MatchResources.NamespaceSelector = nil
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(
					context.Background(), binding, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard binding: %v", err)
				}
			},
		},
		{
			name: "non-empty policy namespace selector",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
					context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard policy: %v", err)
				}
				policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"guard.ptah.dev/scope": "broadened"},
				}
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(
					context.Background(), policy, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard policy: %v", err)
				}
			},
		},
		{
			name: "non-empty policy object selector",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
					context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard policy: %v", err)
				}
				policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"guard.ptah.dev/scope": "broadened"},
				}
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(
					context.Background(), policy, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard policy: %v", err)
				}
			},
		},
		{
			name: "policy object selector match expression",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
					context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard policy: %v", err)
				}
				policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "guard.ptah.dev/scope",
						Operator: metav1.LabelSelectorOpExists,
					}},
				}
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(
					context.Background(), policy, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard policy: %v", err)
				}
			},
		},
		{
			name: "non-empty binding object selector",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(
					context.Background(), config.SecretCreatePolicyBindingName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard binding: %v", err)
				}
				binding.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"guard.ptah.dev/scope": "broadened"},
				}
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(
					context.Background(), binding, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard binding: %v", err)
				}
			},
		},
		{
			name: "binding namespace selector match expression",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(
					context.Background(), config.SecretCreatePolicyBindingName, metav1.GetOptions{},
				)
				if err != nil {
					t.Fatalf("get guard binding: %v", err)
				}
				binding.Spec.MatchResources.NamespaceSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
					Key:      "guard.ptah.dev/scope",
					Operator: metav1.LabelSelectorOpExists,
				}}
				if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(
					context.Background(), binding, metav1.UpdateOptions{},
				); err != nil {
					t.Fatalf("broaden guard binding: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
			old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
			client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
			installEstablishedSecretCreateGuard(t, client, config)
			test.mutate(t, client, config)
			rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

			err := rotator.Run(context.Background())
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte("guard")) {
				t.Fatalf("Run() error = %v, want guard contract rejection", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" && action.GetResource().Resource == "secrets" {
					t.Fatal("broadened guard reached a Secret CREATE request")
				}
			}
		})
	}
}

func TestConfigRejectsInvalidSecretCreateServiceAccountName(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.SecretCreateServiceAccountName = "Bad_Name"
	if _, err := New(fake.NewClientset(), config); err == nil ||
		!bytes.Contains([]byte(err.Error()), []byte("Secret CREATE ServiceAccount name")) {
		t.Fatalf("New() error = %v, want invalid Secret CREATE ServiceAccount name", err)
	}
}

func TestConfigRejectsSecretCreateGuardNamesWhenRecreationIsDisabled(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.RecreateMissingSecret = false
	if _, err := New(fake.NewClientset(), config); err == nil ||
		!strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("New() error = %v, want disabled guard-name rejection", err)
	}
}

func TestMissingSecretCreateRaceNeverOverwritesDifferentMaterial(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	racing := mustGenerateMaterial(t, now.Add(time.Minute), config)
	client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
	installEstablishedSecretCreateGuard(t, client, config)
	installSecretCreateAdmission(t, client, config)
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(interface{ GetCreateOptions() metav1.CreateOptions }).GetCreateOptions()
		if len(options.DryRun) != 0 {
			return false, nil, nil
		}
		if err := client.Tracker().Add(generatedSecret(config, racing)); err != nil {
			t.Fatalf("seed racing Secret: %v", err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, config.SecretName)
	})
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	err := rotator.Run(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("different material")) {
		t.Fatalf("Run() error = %v, want different-material race", err)
	}
	observed := mustGetSecret(t, client, config)
	if !secretContainsMaterial(observed, racing) {
		t.Fatal("rotator overwrote the racing Secret")
	}
	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("race recovery Run() error = %v", err)
	}
	assertFinalBundles(t, client, config, racing.caPEM)
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

func TestTrustRepairDoesNotContractBeforeEndpointProof(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
	current := mustGenerateMaterial(t, now, config)
	client := newTestClient(config, secretForMaterial(config, current), old.caPEM, twoReadyEndpoints(config))
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{err: errors.New("projection pending")})

	if err := rotator.Run(context.Background()); err == nil {
		t.Fatal("Run() unexpectedly succeeded without endpoint proof")
	}
	for name, bundle := range map[string][]byte{
		"mutating":   mutatingBundle(t, client, config),
		"validating": validatingBundle(t, client, config),
	} {
		if !caBundleContainsCertificate(bundle, old.caPEM) || !caBundleContainsCertificate(bundle, current.caPEM) {
			t.Errorf("%s trust contracted before endpoint proof", name)
		}
		assertBundleCertificateCount(t, bundle, 2)
	}
}

func TestCARotationPreservesEachEntryTrustUntilReplacementProof(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	current := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, current)
	delete(legacy.Data, CAPrivateKeyKey)
	localMutating := mustGenerateMaterial(t, now.Add(time.Minute), config)
	localValidatingA := mustGenerateMaterial(t, now.Add(2*time.Minute), config)
	localValidatingB := mustGenerateMaterial(t, now.Add(3*time.Minute), config)
	mutating := malformedBundleWithCertificates(t, current.caPEM, localMutating.caPEM)
	validatingA := append(
		[]byte("malformed prefix\n"),
		malformedBundleWithCertificates(t, current.caPEM, localValidatingA.caPEM)...,
	)
	validatingB := malformedBundleWithCertificates(t, current.caPEM, localValidatingB.caPEM)
	client := newTestClient(config, legacy, current.caPEM, twoReadyEndpoints(config))
	setManagedBundles(t, client, config, mutating, [][]byte{validatingA, validatingB})
	rotator := mustNewTestRotator(
		t,
		client,
		config,
		now,
		&recordingProber{err: errors.New("replacement not loaded")},
	)

	if err := rotator.Run(context.Background()); err == nil {
		t.Fatal("Run() unexpectedly succeeded without replacement endpoint proof")
	}
	nextCA := mustGetSecret(t, client, config).Data[CACertificateKey]
	entries := managedEntryBundles(t, client, config)
	wants := []certificateMaterial{localMutating, localValidatingA, localValidatingB}
	for i, bundle := range entries {
		for name, certificate := range map[string][]byte{
			"current":     current.caPEM,
			"replacement": nextCA,
			"entry-local": wants[i].caPEM,
		} {
			if !caBundleContainsCertificate(bundle, certificate) {
				t.Errorf("entry %d dropped %s trust before endpoint proof", i, name)
			}
		}
		for otherIndex, other := range wants {
			if otherIndex != i && caBundleContainsCertificate(bundle, other.caPEM) {
				t.Errorf("entry %d copied trust from entry %d", i, otherIndex)
			}
		}
		assertBundleCertificateCount(t, bundle, 3)
	}
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
		t.Fatal("Secret changed before every required webhook anchor was found")
	}
}

func TestRequiredWebhookOnDifferentPortStopsBeforeSecretTransition(t *testing.T) {
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
	differentPort := int32(8443)
	validating.Webhooks[0].ClientConfig.Service.Port = &differentPort
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("retarget required webhook port: %v", err)
	}

	rotator := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	err = rotator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), config.ValidatingWebhookNames[0]) {
		t.Fatalf("Run() error = %v, want required webhook name", err)
	}
	if got := mustGetSecret(t, client, config).Data[CAPrivateKeyKey]; len(got) != 0 {
		t.Fatal("Secret changed before the required webhook port was validated")
	}
}

func TestWebhookTargetsServiceRequiresSupportedPort(t *testing.T) {
	t.Parallel()
	config := testConfig()
	explicitSupportedPort := int32(443)
	differentPort := int32(8443)
	tests := []struct {
		name string
		port *int32
		want bool
	}{
		{name: "default port", want: true},
		{name: "explicit supported port", port: &explicitSupportedPort, want: true},
		{name: "different port", port: &differentPort, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clientConfig := admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Name: config.ServiceName, Namespace: config.ServiceNamespace, Port: test.port,
				},
			}
			if got := webhookTargetsService(clientConfig, config); got != test.want {
				t.Fatalf("webhookTargetsService() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAdditionalSameServiceWebhooksFollowRotation(t *testing.T) {
	t.Parallel()
	config := testConfig()
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	foreignURL := "https://example.invalid/validate"
	supportedPort := int32(443)
	differentPort := int32(8443)
	mutating, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	mutating.Webhooks = append(mutating.Webhooks, admissionregistrationv1.MutatingWebhook{
		Name: "future-mutating.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace, Port: &supportedPort,
			},
		},
	}, admissionregistrationv1.MutatingWebhook{
		Name: "different-port-mutating.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace, Port: &differentPort,
			},
		},
	}, admissionregistrationv1.MutatingWebhook{
		Name: "foreign-mutating.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: "other-service", Namespace: config.ServiceNamespace,
			},
		},
	}, admissionregistrationv1.MutatingWebhook{
		Name: "url-mutating.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			URL:      &foreignURL,
		},
	})
	if _, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(
		context.Background(), mutating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("add mutating forward-compatibility webhooks: %v", err)
	}
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	validating.Webhooks = append(validating.Webhooks, admissionregistrationv1.ValidatingWebhook{
		Name: "future.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace,
			},
		},
	}, admissionregistrationv1.ValidatingWebhook{
		Name: "different-port.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace, Port: &differentPort,
			},
		},
	}, admissionregistrationv1.ValidatingWebhook{
		Name: "foreign-service.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: "other-service", Namespace: config.ServiceNamespace,
			},
		},
	}, admissionregistrationv1.ValidatingWebhook{
		Name: "url.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			URL:      &foreignURL,
		},
	})
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(
		context.Background(), validating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("add forward-compatibility webhooks: %v", err)
	}

	rotator := mustNewTestRotator(t, client, config, baseTime, &recordingProber{})
	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	mutating, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get updated MutatingWebhookConfiguration: %v", err)
	}
	validating, err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get updated ValidatingWebhookConfiguration: %v", err)
	}
	replacementCA := mustGetSecret(t, client, config).Data[CACertificateKey]
	if !caBundlesEqual(mutating.Webhooks[1].ClientConfig.CABundle, replacementCA) {
		t.Fatal("rotator did not carry a future same-Service mutating webhook through the CA transition")
	}
	for _, index := range []int{2, 3, 4} {
		if !caBundlesEqual(mutating.Webhooks[index].ClientConfig.CABundle, original.caPEM) {
			t.Fatalf("rotator modified unrelated mutating webhook entry %d", index)
		}
	}
	if !caBundlesEqual(validating.Webhooks[2].ClientConfig.CABundle, replacementCA) {
		t.Fatal("rotator did not carry a future same-Service validating webhook through the CA transition")
	}
	for _, index := range []int{3, 4, 5} {
		if !caBundlesEqual(validating.Webhooks[index].ClientConfig.CABundle, original.caPEM) {
			t.Fatalf("rotator modified unrelated webhook entry %d", index)
		}
	}
}

func TestAdditionalSameServiceWebhooksRetainOverlapUntilEndpointProof(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	baseTime := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, baseTime, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))

	mutating, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	mutating.Webhooks = append(mutating.Webhooks, admissionregistrationv1.MutatingWebhook{
		Name: "future-mutating.operator.ptah.dev",
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: append([]byte(nil), original.caPEM...),
			Service: &admissionregistrationv1.ServiceReference{
				Name: config.ServiceName, Namespace: config.ServiceNamespace,
			},
		},
	})
	if _, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(
		context.Background(), mutating, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("add future mutating webhook: %v", err)
	}

	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	validating.Webhooks = append(validating.Webhooks, admissionregistrationv1.ValidatingWebhook{
		Name: "future-validating.operator.ptah.dev",
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
		t.Fatalf("add future validating webhook: %v", err)
	}

	rotator := mustNewTestRotator(
		t,
		client,
		config,
		baseTime,
		&recordingProber{err: errors.New("replacement not loaded")},
	)
	if err := rotator.Run(context.Background()); err == nil {
		t.Fatal("Run() unexpectedly succeeded without endpoint proof")
	}
	replacementCA := mustGetSecret(t, client, config).Data[CACertificateKey]
	mutating, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get updated MutatingWebhookConfiguration: %v", err)
	}
	validating, err = client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get updated ValidatingWebhookConfiguration: %v", err)
	}
	for name, bundle := range map[string][]byte{
		"mutating":   mutating.Webhooks[1].ClientConfig.CABundle,
		"validating": validating.Webhooks[2].ClientConfig.CABundle,
	} {
		if !caBundleContainsCertificate(bundle, original.caPEM) ||
			!caBundleContainsCertificate(bundle, replacementCA) {
			t.Errorf("future %s webhook did not retain both CA roots before endpoint proof", name)
		}
		assertBundleCertificateCount(t, bundle, 2)
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

func (prober *recordingProber) probeRequests() []probeRequest {
	prober.mu.Lock()
	defer prober.mu.Unlock()
	return slices.Clone(prober.requests)
}

func assertCANotCopiedToValidatingUpdates(t *testing.T, client *fake.Clientset, forbidden []byte) {
	t.Helper()
	checkBundle := func(bundle []byte) {
		if caBundleContainsCertificate(bundle, forbidden) {
			t.Error("rotator copied an unrelated observed CA into another managed entry")
		}
	}
	client.PrependReactor("update", "validatingwebhookconfigurations", func(action k8stesting.Action) (bool, runtime.Object, error) {
		configuration := action.(k8stesting.UpdateAction).GetObject().(*admissionregistrationv1.ValidatingWebhookConfiguration)
		for _, webhook := range configuration.Webhooks {
			checkBundle(webhook.ClientConfig.CABundle)
		}
		return false, nil, nil
	})
}

func installEstablishedSecretCreateGuard(t *testing.T, client *fake.Clientset, config Config) {
	t.Helper()
	installUnestablishedSecretCreateGuard(t, client, config)
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
		context.Background(), config.SecretCreatePolicyName, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get test Secret CREATE guard: %v", err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.TypeChecking = &admissionregistrationv1.TypeChecking{}
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().UpdateStatus(
		context.Background(), policy, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("establish test Secret CREATE guard: %v", err)
	}
}

func installUnestablishedSecretCreateGuard(t *testing.T, client *fake.Clientset, config Config) {
	t.Helper()
	failurePolicy := admissionregistrationv1.Fail
	scope := admissionregistrationv1.NamespacedScope
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: config.SecretCreatePolicyName, Generation: 1},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &failurePolicy,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"},
							Resources: []string{"secrets"}, Scope: &scope,
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "exact-certificate-rotator-service-account",
				Expression: "request.userInfo.username == 'system:serviceaccount:" +
					config.Namespace + ":" + config.SecretCreateServiceAccountName + "'",
			}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: secretCreateValidationExpression(config),
				Message:    secretCreateGuardDenialMessage,
			}},
		},
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: config.SecretCreatePolicyBindingName},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        config.SecretCreatePolicyName,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": config.Namespace},
				},
				ObjectSelector: &metav1.LabelSelector{},
			},
		},
	}
	if err := client.Tracker().Add(policy); err != nil {
		t.Fatalf("add test Secret CREATE guard policy: %v", err)
	}
	if err := client.Tracker().Add(binding); err != nil {
		t.Fatalf("add test Secret CREATE guard binding: %v", err)
	}
}

func installSecretCreateAdmission(t *testing.T, client *fake.Clientset, config Config) {
	t.Helper()
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(interface{ GetCreateOptions() metav1.CreateOptions }).GetCreateOptions()
		if len(options.DryRun) == 0 {
			return false, nil, nil
		}
		secret := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		if secretMatchesCreateContract(secret, config) {
			return true, secret.DeepCopy(), nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "secrets"},
			secret.Name,
			errors.New(secretCreateGuardDenialMessage),
		)
	})
}

func secretMatchesCreateContract(secret *corev1.Secret, config Config) bool {
	if secret.Name != config.SecretName || secret.Namespace != config.Namespace ||
		secret.GenerateName != "" || secret.Type != corev1.SecretTypeTLS ||
		!maps.Equal(secret.Labels, map[string]string{GeneratedSecretLabel: GeneratedSecretLabelValue}) ||
		len(secret.Annotations) != 0 || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 ||
		secret.Immutable != nil || len(secret.StringData) != 0 || len(secret.Data) != 4 {
		return false
	}
	for _, key := range []string{CACertificateKey, CAPrivateKeyKey, corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
		if len(secret.Data[key]) == 0 {
			return false
		}
	}
	return true
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
		SecretCreatePolicyName:         "ptah-cert-rotator",
		SecretCreatePolicyBindingName:  "ptah-cert-rotator",
		SecretCreateServiceAccountName: "ptah-cert-rotator",
		RecreateMissingSecret:          true,
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
	if secret != nil {
		objects = append(objects, secret)
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

func malformedBundleWithCertificates(t *testing.T, bundles ...[]byte) []byte {
	t.Helper()
	combined, err := combineCABundles(bundles...)
	if err != nil {
		t.Fatalf("combine malformed-bundle certificate candidates: %v", err)
	}
	return append(combined, []byte("malformed suffix\n")...)
}

func managedEntryBundles(t *testing.T, client *fake.Clientset, config Config) [][]byte {
	t.Helper()
	mutating, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		context.Background(), config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	validating, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}

	mutatingByName := make(map[string][]byte, len(mutating.Webhooks))
	for _, webhook := range mutating.Webhooks {
		mutatingByName[webhook.Name] = webhook.ClientConfig.CABundle
	}
	validatingByName := make(map[string][]byte, len(validating.Webhooks))
	for _, webhook := range validating.Webhooks {
		validatingByName[webhook.Name] = webhook.ClientConfig.CABundle
	}
	bundles := make([][]byte, 0, len(config.MutatingWebhookNames)+len(config.ValidatingWebhookNames))
	for _, name := range config.MutatingWebhookNames {
		bundles = append(bundles, mutatingByName[name])
	}
	for _, name := range config.ValidatingWebhookNames {
		bundles = append(bundles, validatingByName[name])
	}
	return bundles
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
