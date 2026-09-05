package certrotation

// These white-box tests exercise durable boundaries between private rotation
// steps. The crash states cannot be injected through the package's public API.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCARotationStagesCandidateBeforePublishingTrust(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	sink := &recordingCandidateSink{}
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})
	rotator.candidateSink = sink

	overlapChecks := 0
	checked := make(map[string]bool)
	for _, resource := range []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"} {
		client.PrependReactor("update", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			if checked[resource] {
				return false, nil, nil
			}
			checked[resource] = true
			overlapChecks++
			staging := mustGetTrackedSecret(t, client, config.Namespace, config.StagingSecretName)
			pending, err := decodePendingCandidate(staging.Data, config, now)
			if err != nil {
				t.Fatalf("decode staged candidate before %s update: %v", resource, err)
			}
			primary := mustGetTrackedSecret(t, client, config.Namespace, config.SecretName)
			if relation := relatePendingCandidate(primary, pending, config); relation != pendingBeforePrimaryWrite {
				t.Fatalf("pending relationship before %s update = %v, want before-primary-write", resource, relation)
			}
			certificatePEM, privateKeyPEM, stores, _ := sink.snapshot()
			if stores != 1 || !bytes.Equal(certificatePEM, pending.listenerCertPEM) ||
				!bytes.Equal(privateKeyPEM, pending.listenerKeyPEM) {
				t.Fatalf("candidate sink before %s update has stores=%d and does not match durable material", resource, stores)
			}
			pair, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
			if err != nil {
				t.Fatalf("parse candidate-listener key pair: %v", err)
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				t.Fatalf("parse candidate-listener leaf: %v", err)
			}
			if err := leaf.VerifyHostname(config.CandidateServiceName + "." + config.Namespace + ".svc"); err != nil {
				t.Fatalf("candidate-listener DNS identity: %v", err)
			}
			if err := leaf.VerifyHostname(config.ServiceName + "." + config.ServiceNamespace + ".svc"); err == nil {
				t.Fatal("candidate-listener certificate also authenticates the primary Service")
			}
			if err := leaf.CheckSignatureFrom(pending.material.ca); err != nil {
				t.Fatalf("candidate-listener certificate is not signed by pending CA: %v", err)
			}
			return false, nil, nil
		})
	}

	if err := rotator.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if overlapChecks == 0 {
		t.Fatal("rotation published no webhook trust updates")
	}
	staging := mustGetStagingSecret(t, client, config)
	if len(staging.Data) != 0 {
		t.Fatalf("completed staging data has %d fields, want empty", len(staging.Data))
	}
	certificatePEM, privateKeyPEM, stores, clears := sink.snapshot()
	if len(certificatePEM) != 0 || len(privateKeyPEM) != 0 || stores != 1 || clears < 2 {
		t.Fatalf("completed candidate sink = cert:%d key:%d stores:%d clears:%d", len(certificatePEM), len(privateKeyPEM), stores, clears)
	}
}

func TestInterruptedCARotationReusesExactDurableCandidate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	first := mustNewTestRotator(t, client, config, now, &recordingProber{err: errors.New("projection pending")})
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}

	stagingAfterFailure := mustGetStagingSecret(t, client, config)
	pending, err := decodePendingCandidate(stagingAfterFailure.Data, config, now)
	if err != nil {
		t.Fatalf("decode durable pending candidate: %v", err)
	}
	primaryAfterFailure := mustGetSecret(t, client, config)
	if relation := relatePendingCandidate(primaryAfterFailure, pending, config); relation != pendingAfterPrimaryWrite {
		t.Fatalf("pending relationship after interrupted primary write = %v, want after-primary-write", relation)
	}
	stagedData := cloneBytesMap(stagingAfterFailure.Data)
	updatesBeforeRecovery := countStagingUpdates(client.Actions(), config)

	config.ProbeTimeout = time.Second
	secondSink := &recordingCandidateSink{}
	secondProber := &recordingProber{callback: func(probeRequest) {
		certificatePEM, privateKeyPEM, stores, _ := secondSink.snapshot()
		if stores != 1 || !bytes.Equal(certificatePEM, pending.listenerCertPEM) ||
			!bytes.Equal(privateKeyPEM, pending.listenerKeyPEM) {
			t.Error("recovery did not load the exact durable candidate-listener key pair")
		}
	}}
	second := mustNewTestRotator(t, client, config, now.Add(time.Minute), secondProber)
	second.candidateSink = secondSink
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	if got := countStagingUpdates(client.Actions(), config) - updatesBeforeRecovery; got != 1 {
		t.Fatalf("recovery staging updates = %d, want one clear without candidate regeneration", got)
	}
	if maps.EqualFunc(mustGetStagingSecret(t, client, config).Data, stagedData, bytes.Equal) {
		t.Fatal("recovery left the durable candidate record in place")
	}
	if len(mustGetStagingSecret(t, client, config).Data) != 0 {
		t.Fatal("recovery did not clear staging data")
	}
	if !secretContainsMaterial(mustGetSecret(t, client, config), pending.material) {
		t.Fatal("recovery replaced the durable candidate with new primary material")
	}
}

func TestInterruptedCARotationBeforePrimaryWriteReusesExactDurableCandidate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))

	failFirstTrustWrite := true
	client.PrependReactor("update", "mutatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failFirstTrustWrite {
			failFirstTrustWrite = false
			return true, nil, errors.New("injected trust publication failure")
		}
		return false, nil, nil
	})
	firstSink := &recordingCandidateSink{}
	first := mustNewTestRotator(t, client, config, now, &recordingProber{})
	first.candidateSink = firstSink
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}

	stagingAfterFailure := mustGetStagingSecret(t, client, config)
	pending, err := decodePendingCandidate(stagingAfterFailure.Data, config, now)
	if err != nil {
		t.Fatalf("decode durable pending candidate: %v", err)
	}
	if relation := relatePendingCandidate(mustGetSecret(t, client, config), pending, config); relation != pendingBeforePrimaryWrite {
		t.Fatalf("pending relationship after interrupted trust publication = %v, want before-primary-write", relation)
	}
	stagedData := cloneBytesMap(stagingAfterFailure.Data)
	updatesBeforeRecovery := countStagingUpdates(client.Actions(), config)

	secondSink := &recordingCandidateSink{}
	second := mustNewTestRotator(t, client, config, now.Add(time.Minute), &recordingProber{})
	second.candidateSink = secondSink
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	if got := countStagingUpdates(client.Actions(), config) - updatesBeforeRecovery; got != 1 {
		t.Fatalf("recovery staging updates = %d, want one clear without candidate regeneration", got)
	}
	if maps.EqualFunc(mustGetStagingSecret(t, client, config).Data, stagedData, bytes.Equal) ||
		len(mustGetStagingSecret(t, client, config).Data) != 0 {
		t.Fatal("recovery did not retire the durable candidate record")
	}
	if !secretContainsMaterial(mustGetSecret(t, client, config), pending.material) {
		t.Fatal("recovery replaced the durable candidate with new primary material")
	}
	certificatePEM, privateKeyPEM, stores, clears := secondSink.snapshot()
	if len(certificatePEM) != 0 || len(privateKeyPEM) != 0 || stores != 1 || clears != 1 {
		t.Fatalf("recovery candidate sink = cert:%d key:%d stores:%d clears:%d", len(certificatePEM), len(privateKeyPEM), stores, clears)
	}
}

func TestPendingCandidateRejectsSourceSecretDriftBeforeWrites(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	failOverlap := true
	client.PrependReactor("update", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failOverlap {
			failOverlap = false
			return true, nil, errors.New("injected overlap failure")
		}
		return false, nil, nil
	})
	if err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}
	stagedBeforeDrift := cloneBytesMap(mustGetStagingSecret(t, client, config).Data)

	drifted := mustGetSecret(t, client, config)
	drifted.Data[corev1.TLSPrivateKeyKey] = append([]byte(nil), drifted.Data[corev1.TLSPrivateKeyKey]...)
	drifted.Data[corev1.TLSPrivateKeyKey][0] ^= 0xff
	if _, err := client.CoreV1().Secrets(config.Namespace).Update(context.Background(), drifted, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("drift source Secret: %v", err)
	}
	actionStart := len(client.Actions())
	sink := &recordingCandidateSink{}
	rotator := mustNewTestRotator(t, client, config, now.Add(time.Minute), &recordingProber{})
	rotator.candidateSink = sink
	err := rotator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unrelated to the current generated TLS Secret") {
		t.Fatalf("Run() error = %v, want unrelated durable-transition failure", err)
	}
	for _, action := range client.Actions()[actionStart:] {
		if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
			t.Fatalf("source drift triggered unsafe update: %s", action.GetResource().Resource)
		}
	}
	if !maps.EqualFunc(mustGetStagingSecret(t, client, config).Data, stagedBeforeDrift, bytes.Equal) {
		t.Fatal("source drift changed the durable pending record")
	}
	certificatePEM, privateKeyPEM, stores, _ := sink.snapshot()
	if len(certificatePEM) != 0 || len(privateKeyPEM) != 0 || stores != 0 {
		t.Fatal("source drift loaded unrelated candidate material into the listener boundary")
	}
}

func TestPendingCandidateRejectsPostWriteForeignPrimaryMetadata(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, original)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, original.caPEM, twoReadyEndpoints(config))
	first := mustNewTestRotator(t, client, config, now, &recordingProber{err: errors.New("projection pending")})
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}

	primary := mustGetSecret(t, client, config)
	primary.Annotations = map[string]string{"operator.ptah.dev/foreign": "true"}
	if _, err := client.CoreV1().Secrets(config.Namespace).Update(context.Background(), primary, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("add foreign primary metadata: %v", err)
	}
	actionStart := len(client.Actions())
	sink := &recordingCandidateSink{}
	second := mustNewTestRotator(t, client, config, now.Add(time.Minute), &recordingProber{})
	second.candidateSink = sink
	err := second.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "source contract") {
		t.Fatalf("Run() error = %v, want primary source-contract failure", err)
	}
	for _, action := range client.Actions()[actionStart:] {
		if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
			t.Fatalf("foreign primary metadata triggered unsafe update: %s", action.GetResource().Resource)
		}
	}
	_, _, stores, _ := sink.snapshot()
	if stores != 0 {
		t.Fatal("foreign primary metadata loaded candidate material")
	}
}

func TestCertificateRotationConfigRequiresStagingBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "empty staging Secret name",
			mutate: func(config *Config) { config.StagingSecretName = "" },
			want:   "staging Secret name",
		},
		{
			name:   "staging Secret aliases primary",
			mutate: func(config *Config) { config.StagingSecretName = config.SecretName },
			want:   "must differ",
		},
		{
			name:   "empty candidate Service name",
			mutate: func(config *Config) { config.CandidateServiceName = "" },
			want:   "candidate service name",
		},
		{
			name:   "candidate Service aliases primary",
			mutate: func(config *Config) { config.CandidateServiceName = config.ServiceName },
			want:   "must differ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			test.mutate(&config)
			_, err := New(fake.NewClientset(), config, &recordingCandidateSink{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := New(fake.NewClientset(), testConfig(), nil); err == nil || !strings.Contains(err.Error(), "sink is required") {
		t.Fatalf("New() nil-sink error = %v, want required sink", err)
	}
}

func TestPrimarySourceContractRejectsForeignShapeBeforeStaging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{
			name: "missing UID",
			mutate: func(secret *corev1.Secret) {
				secret.UID = ""
			},
		},
		{
			name: "missing resource version",
			mutate: func(secret *corev1.Secret) {
				secret.ResourceVersion = ""
			},
		},
		{
			name: "deletion in progress",
			mutate: func(secret *corev1.Secret) {
				deletionTime := metav1.NewTime(time.Date(2026, time.September, 5, 11, 0, 0, 0, time.UTC))
				secret.DeletionTimestamp = &deletionTime
			},
		},
		{
			name: "extra label",
			mutate: func(secret *corev1.Secret) {
				secret.Labels["operator.ptah.dev/foreign"] = "true"
			},
		},
		{
			name: "annotation",
			mutate: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"operator.ptah.dev/foreign": "true"}
			},
		},
		{
			name: "extra data field",
			mutate: func(secret *corev1.Secret) {
				secret.Data["foreign"] = []byte("data")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			material := mustGenerateMaterial(t, now, config)
			primary := secretForMaterial(config, material)
			delete(primary.Data, CAPrivateKeyKey)
			test.mutate(primary)
			client := newTestClient(config, primary, material.caPEM, twoReadyEndpoints(config))
			actionStart := len(client.Actions())
			err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "source contract") {
				t.Fatalf("Run() error = %v, want source-contract failure", err)
			}
			for _, action := range client.Actions()[actionStart:] {
				if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
					t.Fatalf("foreign primary source triggered update to %s", action.GetResource().Resource)
				}
			}
			if len(mustGetStagingSecret(t, client, config).Data) != 0 {
				t.Fatal("foreign primary source populated staging data")
			}
		})
	}
}

func TestStagingSecretContractFailsClosedBeforeCertificateWrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, *fake.Clientset, Config)
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				t.Helper()
				if err := client.CoreV1().Secrets(config.Namespace).Delete(context.Background(), config.StagingSecretName, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra label",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				t.Helper()
				updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
					secret.Labels["operator.ptah.dev/foreign"] = "true"
				})
			},
		},
		{
			name: "wrong type",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				t.Helper()
				updateStagingForTest(t, client, config, func(secret *corev1.Secret) { secret.Type = corev1.SecretTypeTLS })
			},
		},
		{
			name: "partial pending data",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				t.Helper()
				updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
					secret.Data = map[string][]byte{stagingFormatKey: []byte(stagingFormat)}
				})
			},
		},
		{
			name: "terminating",
			mutate: func(t *testing.T, client *fake.Clientset, config Config) {
				t.Helper()
				updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
					now := metav1.Now()
					secret.DeletionTimestamp = &now
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			material := mustGenerateMaterial(t, now, config)
			legacy := secretForMaterial(config, material)
			delete(legacy.Data, CAPrivateKeyKey)
			client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
			test.mutate(t, client, config)
			actionStart := len(client.Actions())
			sink := &recordingCandidateSink{}
			rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})
			rotator.candidateSink = sink
			if err := rotator.Run(context.Background()); err == nil {
				t.Fatal("Run() unexpectedly accepted a foreign staging Secret")
			}
			for _, action := range client.Actions()[actionStart:] {
				if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
					t.Fatalf("foreign staging Secret triggered update to %s", action.GetResource().Resource)
				}
			}
			certificatePEM, privateKeyPEM, stores, _ := sink.snapshot()
			if len(certificatePEM) != 0 || len(privateKeyPEM) != 0 || stores != 0 {
				t.Fatal("foreign staging Secret populated the candidate certificate sink")
			}
		})
	}
}

func TestStagingSecretMetadataRequiresExactLiveShape(t *testing.T) {
	t.Parallel()
	config := testConfig()
	base := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            config.StagingSecretName,
			Namespace:       config.Namespace,
			UID:             "staging-uid",
			ResourceVersion: "1",
			Labels: map[string]string{
				StagingSecretLabel: StagingSecretLabelValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := validateStagingSecretMetadata(base, config); err != nil {
		t.Fatalf("exact staging Secret metadata: %v", err)
	}
	controller := true
	immutable := false
	deletionTime := metav1.NewTime(time.Date(2026, time.September, 5, 11, 0, 0, 0, time.UTC))
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{name: "foreign name", mutate: func(secret *corev1.Secret) { secret.Name = "foreign" }},
		{name: "foreign namespace", mutate: func(secret *corev1.Secret) { secret.Namespace = "foreign" }},
		{name: "generateName", mutate: func(secret *corev1.Secret) { secret.GenerateName = "stage-" }},
		{name: "missing UID", mutate: func(secret *corev1.Secret) { secret.UID = "" }},
		{name: "missing resourceVersion", mutate: func(secret *corev1.Secret) { secret.ResourceVersion = "" }},
		{name: "deleting", mutate: func(secret *corev1.Secret) { secret.DeletionTimestamp = &deletionTime }},
		{name: "wrong type", mutate: func(secret *corev1.Secret) { secret.Type = corev1.SecretTypeTLS }},
		{name: "missing label", mutate: func(secret *corev1.Secret) { secret.Labels = nil }},
		{name: "extra label", mutate: func(secret *corev1.Secret) { secret.Labels["operator.ptah.dev/foreign"] = "true" }},
		{name: "annotation", mutate: func(secret *corev1.Secret) {
			secret.Annotations = map[string]string{"operator.ptah.dev/foreign": "true"}
		}},
		{name: "owner reference", mutate: func(secret *corev1.Secret) {
			secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign", Controller: &controller}}
		}},
		{name: "finalizer", mutate: func(secret *corev1.Secret) { secret.Finalizers = []string{"operator.ptah.dev/foreign"} }},
		{name: "immutable field", mutate: func(secret *corev1.Secret) { secret.Immutable = &immutable }},
		{name: "stringData", mutate: func(secret *corev1.Secret) { secret.StringData = map[string]string{"foreign": "value"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := base.DeepCopy()
			test.mutate(candidate)
			if err := validateStagingSecretMetadata(candidate, config); err == nil {
				t.Fatal("validateStagingSecretMetadata() accepted foreign live shape")
			}
		})
	}
	if err := validateStagingSecretMetadata(nil, config); err == nil {
		t.Fatal("validateStagingSecretMetadata() accepted nil")
	}
}

func TestPendingCandidateDecodeUsesDurableSafetyLimits(t *testing.T) {
	t.Parallel()
	originalConfig := testConfig()
	createdAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	source := secretForMaterial(originalConfig, mustGenerateMaterial(t, createdAt, originalConfig))
	pending, err := generatePendingCandidate(rand.Reader, createdAt, originalConfig, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	data := encodePendingCandidate(pending)

	tests := []struct {
		name       string
		config     Config
		now        time.Time
		wantErr    bool
		wantPolicy bool
	}{
		{
			name: "shorter current validity policy",
			config: func() Config {
				config := originalConfig
				config.CACertificateValidity = 180 * 24 * time.Hour
				config.ServingCertificateValidity = 20 * 24 * time.Hour
				return config
			}(),
			now:        createdAt,
			wantPolicy: true,
		},
		{
			name:       "inside current renewal margin",
			config:     originalConfig,
			now:        createdAt.Add(24 * 24 * time.Hour),
			wantPolicy: true,
		},
		{
			name:    "expired durable serving certificate",
			config:  originalConfig,
			now:     createdAt.Add(31 * 24 * time.Hour),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decoded, err := decodePendingCandidate(data, test.config, test.now)
			if (err != nil) != test.wantErr {
				t.Fatalf("decodePendingCandidate() error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			needsRenewal, err := pendingMaterialNeedsCurrentPolicyRenewal(decoded.material, test.config, test.now)
			if err != nil {
				t.Fatalf("pendingMaterialNeedsCurrentPolicyRenewal() error = %v", err)
			}
			if needsRenewal != test.wantPolicy {
				t.Fatalf("pendingMaterialNeedsCurrentPolicyRenewal() = %v, want %v", needsRenewal, test.wantPolicy)
			}
		})
	}
}

func TestCompletedOldPolicyPendingTransitionRequestsImmediateRenewal(t *testing.T) {
	t.Parallel()
	originalConfig := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	source := secretForMaterial(originalConfig, mustGenerateMaterial(t, now, originalConfig))
	pending, err := generatePendingCandidate(rand.Reader, now, originalConfig, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}

	currentConfig := originalConfig
	currentConfig.ServingCertificateValidity = 20 * 24 * time.Hour
	primary := generatedSecret(currentConfig, pending.material)
	primary.UID = source.UID
	primary.ResourceVersion = "1"
	client := newTestClient(currentConfig, primary, pending.material.caPEM, twoReadyEndpoints(currentConfig))
	updateStagingForTest(t, client, currentConfig, func(secret *corev1.Secret) {
		secret.Data = encodePendingCandidate(pending)
	})

	sink := &recordingCandidateSink{}
	first := mustNewTestRotator(t, client, currentConfig, now, &recordingProber{})
	first.candidateSink = sink
	err = first.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires immediate renewal") {
		t.Fatalf("first Run() error = %v, want immediate-renewal request", err)
	}
	if len(mustGetStagingSecret(t, client, currentConfig).Data) != 0 {
		t.Fatal("completed old-policy transition retained durable pending material")
	}
	certificatePEM, privateKeyPEM, _, _ := sink.snapshot()
	if len(certificatePEM) != 0 || len(privateKeyPEM) != 0 {
		t.Fatal("completed old-policy transition retained candidate-listener credentials")
	}
	assertFinalBundles(t, client, currentConfig, pending.material.caPEM)

	if err := mustNewTestRotator(t, client, currentConfig, now, &recordingProber{}).Run(context.Background()); err != nil {
		t.Fatalf("immediate renewal Run() error = %v", err)
	}
	renewed := mustGetSecret(t, client, currentConfig)
	if bytes.Equal(renewed.Data[corev1.TLSCertKey], pending.material.certPEM) {
		t.Fatal("immediate renewal retained the out-of-policy serving certificate")
	}
	if !bytes.Equal(renewed.Data[CACertificateKey], pending.material.caPEM) {
		t.Fatal("serving-certificate renewal unexpectedly replaced the current CA")
	}
}

func TestPendingCandidateRecordRejectsTampering(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	sourceMaterial := mustGenerateMaterial(t, now, config)
	pending, err := generatePendingCandidate(rand.Reader, now, config, secretForMaterial(config, sourceMaterial))
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
	}{
		{
			name: "unknown field",
			mutate: func(data map[string][]byte) {
				data["foreign"] = []byte("data")
			},
		},
		{
			name: "noncanonical source digest",
			mutate: func(data map[string][]byte) {
				data[stagingSourceDigestKey] = []byte(strings.Repeat("A", stagingSourceDigestHexSize))
			},
		},
		{
			name: "listener reuses primary certificate",
			mutate: func(data map[string][]byte) {
				data[stagingCandidateCertKey] = append([]byte(nil), data[stagingServingCertKey]...)
				data[stagingCandidateKeyKey] = append([]byte(nil), data[stagingServingKeyKey]...)
			},
		},
		{
			name: "CA key does not match",
			mutate: func(data map[string][]byte) {
				data[stagingCAPrivateKeyKey] = append([]byte(nil), data[stagingCandidateKeyKey]...)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data := encodePendingCandidate(pending)
			test.mutate(data)
			if _, err := decodePendingCandidate(data, config, now); err == nil {
				t.Fatal("decodePendingCandidate() accepted tampered data")
			}
		})
	}
}

func TestStagingSecretUpdateAcceptsOnlyExactUncertainReadback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		persistUpdate      bool
		successfulResponse bool
		replaceUID         bool
		wantEmpty          bool
		wantError          bool
	}{
		{name: "persisted response loss", persistUpdate: true},
		{name: "write did not land", wantEmpty: true, wantError: true},
		{name: "successful response has replacement UID", successfulResponse: true, replaceUID: true, wantEmpty: true, wantError: true},
		{name: "uncertain read-back has replacement UID", persistUpdate: true, replaceUID: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			material := mustGenerateMaterial(t, now, config)
			legacy := secretForMaterial(config, material)
			delete(legacy.Data, CAPrivateKeyKey)
			client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
			client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
				if secret.Name != config.StagingSecretName {
					return false, nil, nil
				}
				observed := secret.DeepCopy()
				if test.replaceUID {
					observed.UID = "replacement-staging-uid"
				}
				if test.successfulResponse {
					return true, observed, nil
				}
				if test.persistUpdate {
					resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
					if err := client.Tracker().Update(resource, observed, config.Namespace); err != nil {
						t.Fatalf("persist staging update behind response loss: %v", err)
					}
				}
				return true, nil, errors.New("injected response loss")
			})
			err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("Run() error = %v, wantError %v", err, test.wantError)
			}
			if test.wantError {
				assertFinalBundles(t, client, config, material.caPEM)
				if test.wantEmpty && len(mustGetStagingSecret(t, client, config).Data) != 0 {
					t.Fatal("failed uncertain write left unexpected staging data")
				}
			}
		})
	}
}

func TestPrimarySecretUpdateAcceptsOnlyExactPostWriteObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		successfulResponse bool
		mutate             func(*corev1.Secret)
	}{
		{
			name:               "successful response has replacement UID",
			successfulResponse: true,
			mutate: func(secret *corev1.Secret) {
				secret.UID = "replacement-primary-uid"
			},
		},
		{
			name:               "successful response has no resource version",
			successfulResponse: true,
			mutate: func(secret *corev1.Secret) {
				secret.ResourceVersion = ""
			},
		},
		{
			name:               "successful response has foreign metadata",
			successfulResponse: true,
			mutate: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"operator.ptah.dev/foreign": "true"}
			},
		},
		{
			name: "uncertain read-back has replacement UID",
			mutate: func(secret *corev1.Secret) {
				secret.UID = "replacement-primary-uid"
			},
		},
		{
			name: "uncertain read-back is being deleted",
			mutate: func(secret *corev1.Secret) {
				deletionTime := metav1.NewTime(time.Date(2026, time.September, 5, 11, 0, 0, 0, time.UTC))
				secret.DeletionTimestamp = &deletionTime
			},
		},
		{
			name: "uncertain read-back has an extra data field",
			mutate: func(secret *corev1.Secret) {
				secret.Data["foreign"] = []byte("data")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			material := mustGenerateMaterial(t, now, config)
			legacy := secretForMaterial(config, material)
			delete(legacy.Data, CAPrivateKeyKey)
			client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
			client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
				if secret.Name != config.SecretName {
					return false, nil, nil
				}
				observed := secret.DeepCopy()
				test.mutate(observed)
				if test.successfulResponse {
					return true, observed, nil
				}
				resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
				if err := client.Tracker().Update(resource, observed, config.Namespace); err != nil {
					t.Fatalf("persist foreign primary read-back: %v", err)
				}
				return true, nil, errors.New("injected response loss")
			})

			err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "atomically update generated TLS Secret") {
				t.Fatalf("Run() error = %v, want exact post-write contract failure", err)
			}
			if len(mustGetStagingSecret(t, client, config).Data) == 0 {
				t.Fatal("failed primary write discarded the durable pending record")
			}
			assertBundleCertificateCount(t, mutatingBundle(t, client, config), 2)
			assertBundleCertificateCount(t, validatingBundle(t, client, config), 2)
		})
	}
}

func TestPrimarySecretUpdateAcceptsExactUncertainReadback(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, now, config)
	legacy := secretForMaterial(config, material)
	delete(legacy.Data, CAPrivateKeyKey)
	client := newTestClient(config, legacy, material.caPEM, twoReadyEndpoints(config))
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if secret.Name != config.SecretName {
			return false, nil, nil
		}
		resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
		if err := client.Tracker().Update(resource, secret.DeepCopy(), config.Namespace); err != nil {
			t.Fatalf("persist exact primary read-back: %v", err)
		}
		return true, nil, errors.New("injected response loss")
	})

	if err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(mustGetStagingSecret(t, client, config).Data) != 0 {
		t.Fatal("completed exact uncertain write retained pending material")
	}
	updated := mustGetSecret(t, client, config)
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
}

func TestPrimarySecretCreateAcceptsOnlyExactLiveObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		successfulResponse bool
		mutate             func(*corev1.Secret)
		wantError          bool
	}{
		{
			name:               "successful response has no UID",
			successfulResponse: true,
			mutate:             func(secret *corev1.Secret) { secret.UID = "" },
			wantError:          true,
		},
		{
			name:               "successful response has no resource version",
			successfulResponse: true,
			mutate:             func(secret *corev1.Secret) { secret.ResourceVersion = "" },
			wantError:          true,
		},
		{
			name: "uncertain read-back is being deleted",
			mutate: func(secret *corev1.Secret) {
				deletionTime := metav1.NewTime(time.Date(2026, time.September, 5, 11, 0, 0, 0, time.UTC))
				secret.DeletionTimestamp = &deletionTime
			},
			wantError: true,
		},
		{
			name: "exact uncertain read-back",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			old := mustGenerateMaterial(t, now.Add(-time.Hour), config)
			client := newTestClient(config, nil, old.caPEM, twoReadyEndpoints(config))
			installEstablishedSecretCreateGuard(t, client, config)
			installSecretCreateAdmission(t, client, config)
			client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				options := action.(interface{ GetCreateOptions() metav1.CreateOptions }).GetCreateOptions()
				if len(options.DryRun) != 0 {
					return false, nil, nil
				}
				observed := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy()
				observed.UID = "created-primary-secret-uid"
				observed.ResourceVersion = "1"
				if test.mutate != nil {
					test.mutate(observed)
				}
				if test.successfulResponse {
					return true, observed, nil
				}
				resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
				if err := client.Tracker().Create(resource, observed, config.Namespace); err != nil {
					t.Fatalf("persist generated Secret behind response loss: %v", err)
				}
				return true, nil, errors.New("injected response loss")
			})

			err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("Run() error = %v, wantError %v", err, test.wantError)
			}
			if test.wantError {
				if len(mustGetStagingSecret(t, client, config).Data) == 0 {
					t.Fatal("rejected generated Secret discarded the durable pending record")
				}
				assertBundleCertificateCount(t, mutatingBundle(t, client, config), 2)
				assertBundleCertificateCount(t, validatingBundle(t, client, config), 2)
				return
			}
			if len(mustGetStagingSecret(t, client, config).Data) != 0 {
				t.Fatal("accepted exact uncertain create retained durable pending material")
			}
			created := mustGetSecret(t, client, config)
			assertFinalBundles(t, client, config, created.Data[CACertificateKey])
		})
	}
}

func countStagingUpdates(actions []k8stesting.Action, config Config) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() != "update" || action.GetResource().Resource != "secrets" {
			continue
		}
		update, ok := action.(k8stesting.UpdateAction)
		if !ok {
			continue
		}
		if update.GetObject().(*corev1.Secret).Name == config.StagingSecretName {
			count++
		}
	}
	return count
}

func mustGetStagingSecret(t *testing.T, client *fake.Clientset, config Config) *corev1.Secret {
	t.Helper()
	secret, err := client.CoreV1().Secrets(config.Namespace).Get(context.Background(), config.StagingSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get staging Secret: %v", err)
	}
	return secret
}

func mustGetTrackedSecret(t *testing.T, client *fake.Clientset, namespace, name string) *corev1.Secret {
	t.Helper()
	resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	object, err := client.Tracker().Get(resource, namespace, name)
	if err != nil {
		t.Fatalf("get tracked Secret %s/%s: %v", namespace, name, err)
	}
	return object.(*corev1.Secret).DeepCopy()
}

func updateStagingForTest(
	t *testing.T,
	client *fake.Clientset,
	config Config,
	mutate func(*corev1.Secret),
) {
	t.Helper()
	secret := mustGetStagingSecret(t, client, config)
	mutate(secret)
	if _, err := client.CoreV1().Secrets(config.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update staging Secret: %v", err)
	}
}
