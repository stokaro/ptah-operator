package certrotation

// These white-box tests exercise durable boundaries between private rotation
// steps. The crash states cannot be injected through the package's public API.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"strings"
	"sync"
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
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config))
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})

	var mu sync.Mutex
	checked := make(map[string]bool)
	for _, resource := range []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"} {
		client.PrependReactor("update", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			if checked[resource] {
				return false, nil, nil
			}
			checked[resource] = true
			staging := mustGetTrackedSecret(t, client, config.Namespace, config.StagingSecretName)
			pending, err := decodePendingCandidate(staging.Data, config)
			if err != nil {
				t.Fatalf("decode staged candidate before %s update: %v", resource, err)
			}
			if pending.phase != stagingPhasePrepared {
				t.Errorf("staged phase before the first %s update = %q, want %q", resource, pending.phase, stagingPhasePrepared)
			}
			primary := mustGetTrackedSecret(t, client, config.Namespace, config.SecretName)
			if relation := relatePendingCandidate(primary, pending, config); relation != pendingBeforePrimaryWrite {
				t.Fatalf("pending relationship before %s update = %v, want before-primary-write", resource, relation)
			}
			return false, nil, nil
		})
	}

	result, err := rotator.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(checked) != 2 {
		t.Fatalf("rotation published trust through %d of the two webhook configurations", len(checked))
	}
	if result.RequeueAfter != config.CASwitchDelay {
		t.Fatalf("RequeueAfter = %s, want the CA switch delay %s", result.RequeueAfter, config.CASwitchDelay)
	}
	staged, err := decodePendingCandidate(mustGetStagingSecret(t, client, config).Data, config)
	if err != nil {
		t.Fatalf("decode expanded record: %v", err)
	}
	if staged.phase != stagingPhaseExpanded || !staged.expandedAt.Equal(now) {
		t.Fatalf("staged record = phase %q expanded at %s, want %q at %s", staged.phase, staged.expandedAt, stagingPhaseExpanded, now)
	}
}

func TestInterruptedCARotationReusesExactDurableCandidate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config))
	switchAt := mustExpandCATransition(t, client, config, now, &recordingProber{})
	first := mustNewTestRotator(t, client, config, switchAt, &recordingProber{err: errors.New("projection pending")})
	if _, err := first.Run(context.Background()); err == nil {
		t.Fatal("switch pass unexpectedly succeeded")
	}

	stagingAfterFailure := mustGetStagingSecret(t, client, config)
	pending, err := decodePendingCandidate(stagingAfterFailure.Data, config)
	if err != nil {
		t.Fatalf("decode durable pending candidate: %v", err)
	}
	primaryAfterFailure := mustGetSecret(t, client, config)
	if relation := relatePendingCandidate(primaryAfterFailure, pending, config); relation != pendingAfterPrimaryWrite {
		t.Fatalf("pending relationship after interrupted primary write = %v, want after-primary-write", relation)
	}
	updatesBeforeRecovery := countStagingUpdates(client.Actions(), config)

	config.ProbeTimeout = time.Second
	second := mustNewTestRotator(t, client, config, switchAt.Add(time.Minute), &recordingProber{})
	if _, err := second.Run(context.Background()); err != nil {
		t.Fatalf("recovery Run() error = %v", err)
	}
	if got := countStagingUpdates(client.Actions(), config) - updatesBeforeRecovery; got != 1 {
		t.Fatalf("recovery staging updates = %d, want the one clear", got)
	}
	if len(mustGetStagingSecret(t, client, config).Data) != 0 {
		t.Fatal("recovery did not clear staging data")
	}
	if !secretContainsMaterial(mustGetSecret(t, client, config), pending.material) {
		t.Fatal("recovery replaced the durable candidate with new primary material")
	}
	assertFinalBundles(t, client, config, pending.material.caPEM)
}

func TestInterruptedCARotationBeforePrimaryWriteReusesExactDurableCandidate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config))

	failFirstTrustWrite := true
	client.PrependReactor("update", "mutatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failFirstTrustWrite {
			failFirstTrustWrite = false
			return true, nil, errors.New("injected trust publication failure")
		}
		return false, nil, nil
	})
	first := mustNewTestRotator(t, client, config, now, &recordingProber{})
	if _, err := first.Run(context.Background()); err == nil {
		t.Fatal("first Run() unexpectedly succeeded")
	}

	stagingAfterFailure := mustGetStagingSecret(t, client, config)
	pending, err := decodePendingCandidate(stagingAfterFailure.Data, config)
	if err != nil {
		t.Fatalf("decode durable pending candidate: %v", err)
	}
	if pending.phase != stagingPhasePrepared {
		t.Fatalf("interrupted expansion recorded phase %q, want %q", pending.phase, stagingPhasePrepared)
	}
	if relation := relatePendingCandidate(mustGetSecret(t, client, config), pending, config); relation != pendingBeforePrimaryWrite {
		t.Fatalf("pending relationship after interrupted trust publication = %v, want before-primary-write", relation)
	}
	updatesBeforeRecovery := countStagingUpdates(client.Actions(), config)

	completeCATransition(t, client, config, now.Add(time.Minute), &recordingProber{})
	if got := countStagingUpdates(client.Actions(), config) - updatesBeforeRecovery; got != 2 {
		t.Fatalf("recovery staging updates = %d, want the expansion record and the clear", got)
	}
	if !secretContainsMaterial(mustGetSecret(t, client, config), pending.material) {
		t.Fatal("recovery replaced the durable candidate with new primary material")
	}
}

func TestPendingCandidateRejectsSourceSecretDriftBeforeWrites(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config))
	failOverlap := true
	client.PrependReactor("update", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failOverlap {
			failOverlap = false
			return true, nil, errors.New("injected overlap failure")
		}
		return false, nil, nil
	})
	if _, err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background()); err == nil {
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
	rotator := mustNewTestRotator(t, client, config, now.Add(time.Minute), &recordingProber{})
	_, err := rotator.Run(context.Background())
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
}

func TestPendingCandidateRejectsPostWriteForeignPrimaryMetadata(t *testing.T) {
	t.Parallel()
	config := testConfig()
	config.ProbeTimeout = 15 * time.Millisecond
	config.ProbeInterval = time.Millisecond
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config))
	switchAt := mustExpandCATransition(t, client, config, now, &recordingProber{})
	first := mustNewTestRotator(t, client, config, switchAt, &recordingProber{err: errors.New("projection pending")})
	if _, err := first.Run(context.Background()); err == nil {
		t.Fatal("switch pass unexpectedly succeeded")
	}

	primary := mustGetSecret(t, client, config)
	primary.Annotations = map[string]string{"operator.ptah.run/foreign": "true"}
	if _, err := client.CoreV1().Secrets(config.Namespace).Update(context.Background(), primary, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("add foreign primary metadata: %v", err)
	}
	actionStart := len(client.Actions())
	second := mustNewTestRotator(t, client, config, switchAt.Add(time.Minute), &recordingProber{})
	_, err := second.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "source contract") {
		t.Fatalf("Run() error = %v, want primary source-contract failure", err)
	}
	for _, action := range client.Actions()[actionStart:] {
		if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
			t.Fatalf("foreign primary metadata triggered unsafe update: %s", action.GetResource().Resource)
		}
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
			name:   "empty Helm release name",
			mutate: func(config *Config) { config.ReleaseName = "" },
			want:   "Helm release name",
		},
		{
			name:   "staging Secret aliases primary",
			mutate: func(config *Config) { config.StagingSecretName = config.SecretName },
			want:   "must differ",
		},
		{
			name:   "no CA switch delay",
			mutate: func(config *Config) { config.CASwitchDelay = 0 },
			want:   "CA switch delay",
		},
		{
			name:   "CA switch delay as long as the renewal threshold",
			mutate: func(config *Config) { config.CASwitchDelay = config.RenewalThreshold },
			want:   "CA switch delay",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			test.mutate(&config)
			_, err := New(fake.NewClientset(), config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
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
				secret.Labels["operator.ptah.run/foreign"] = "true"
			},
		},
		{
			name: "annotation",
			mutate: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"operator.ptah.run/foreign": "true"}
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
			_, err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
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
					secret.Labels["operator.ptah.run/foreign"] = "true"
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
			keyless := secretForMaterial(config, material)
			delete(keyless.Data, CAPrivateKeyKey)
			client := newTestClient(config, keyless, material.caPEM, twoReadyEndpoints(config))
			test.mutate(t, client, config)
			actionStart := len(client.Actions())
			rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})
			if _, err := rotator.Run(context.Background()); err == nil {
				t.Fatal("Run() unexpectedly accepted a foreign staging Secret")
			}
			for _, action := range client.Actions()[actionStart:] {
				if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
					t.Fatalf("foreign staging Secret triggered update to %s", action.GetResource().Resource)
				}
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
			Labels:          stagingSecretLabels(),
			Annotations:     helmOwnershipAnnotations(config),
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
		{name: "extra label", mutate: func(secret *corev1.Secret) { secret.Labels["operator.ptah.run/foreign"] = "true" }},
		{name: "missing Helm managed-by label", mutate: func(secret *corev1.Secret) {
			delete(secret.Labels, HelmManagedByLabel)
		}},
		{name: "wrong Helm managed-by label", mutate: func(secret *corev1.Secret) {
			secret.Labels[HelmManagedByLabel] = "foreign"
		}},
		{name: "foreign annotation", mutate: func(secret *corev1.Secret) {
			secret.Annotations = map[string]string{"operator.ptah.run/foreign": "true"}
		}},
		{name: "missing release name annotation", mutate: func(secret *corev1.Secret) {
			delete(secret.Annotations, HelmReleaseNameAnnotation)
		}},
		{name: "wrong release name annotation", mutate: func(secret *corev1.Secret) {
			secret.Annotations[HelmReleaseNameAnnotation] = "foreign"
		}},
		{name: "missing release namespace annotation", mutate: func(secret *corev1.Secret) {
			delete(secret.Annotations, HelmReleaseNamespaceAnnotation)
		}},
		{name: "wrong release namespace annotation", mutate: func(secret *corev1.Secret) {
			secret.Annotations[HelmReleaseNamespaceAnnotation] = "foreign"
		}},
		{name: "owner reference", mutate: func(secret *corev1.Secret) {
			secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign", Controller: &controller}}
		}},
		{name: "finalizer", mutate: func(secret *corev1.Secret) { secret.Finalizers = []string{"operator.ptah.run/foreign"} }},
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

func TestPrimarySecretMetadataRequiresExactHelmOwnership(t *testing.T) {
	t.Parallel()
	config := testConfig()
	base := secretForMaterial(config, certificateMaterial{})
	if err := validatePrimarySecretSource(base, config); err != nil {
		t.Fatalf("exact primary Secret metadata: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{name: "missing labels", mutate: func(secret *corev1.Secret) { secret.Labels = nil }},
		{name: "extra label", mutate: func(secret *corev1.Secret) { secret.Labels["operator.ptah.run/foreign"] = "true" }},
		{name: "wrong Helm manager", mutate: func(secret *corev1.Secret) { secret.Labels[HelmManagedByLabel] = "foreign" }},
		{name: "missing annotations", mutate: func(secret *corev1.Secret) { secret.Annotations = nil }},
		{name: "extra annotation", mutate: func(secret *corev1.Secret) { secret.Annotations["operator.ptah.run/foreign"] = "true" }},
		{name: "wrong release name", mutate: func(secret *corev1.Secret) { secret.Annotations[HelmReleaseNameAnnotation] = "foreign" }},
		{name: "wrong release namespace", mutate: func(secret *corev1.Secret) { secret.Annotations[HelmReleaseNamespaceAnnotation] = "foreign" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := base.DeepCopy()
			test.mutate(candidate)
			if err := validatePrimarySecretSource(candidate, config); err == nil {
				t.Fatal("validatePrimarySecretSource() accepted foreign ownership metadata")
			}
		})
	}
}

func TestPendingCandidateDecodeSeparatesDurableSafetyFromTemporalUsability(t *testing.T) {
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
		name            string
		config          Config
		now             time.Time
		wantTemporalErr bool
		wantPolicy      bool
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
			name:            "expired durable serving certificate",
			config:          originalConfig,
			now:             createdAt.Add(31 * 24 * time.Hour),
			wantTemporalErr: true,
		},
		{
			name:            "durable certificates are not yet valid",
			config:          originalConfig,
			now:             createdAt.Add(-certificateBackdate - time.Second),
			wantTemporalErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decoded, err := decodePendingCandidate(data, test.config)
			if err != nil {
				t.Fatalf("timeless decodePendingCandidate() error = %v", err)
			}
			temporalErr := pendingCandidateTemporalUsability(decoded, test.now)
			if (temporalErr != nil) != test.wantTemporalErr {
				t.Fatalf("pendingCandidateTemporalUsability() error = %v, wantError %v", temporalErr, test.wantTemporalErr)
			}
			if test.wantTemporalErr {
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

func TestUnusableRelatedPendingCandidateIsRetiredForRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		sourceState stagingSourceState
		afterWrite  bool
	}{
		{name: "before primary write", sourceState: stagingSourcePresent},
		{name: "after primary write", sourceState: stagingSourcePresent, afterWrite: true},
		{name: "missing primary", sourceState: stagingSourceMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			createdAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
			sourceMaterial := mustGenerateMaterial(t, createdAt, config)
			source := secretForMaterial(config, sourceMaterial)
			var pendingSource *corev1.Secret
			var primary *corev1.Secret
			switch test.sourceState {
			case stagingSourcePresent:
				pendingSource = source
				primary = source.DeepCopy()
			case stagingSourceMissing:
				pendingSource = nil
				primary = nil
			default:
				t.Fatalf("unsupported source state %q", test.sourceState)
			}
			pending, err := generatePendingCandidate(rand.Reader, createdAt, config, pendingSource)
			if err != nil {
				t.Fatalf("generatePendingCandidate() error = %v", err)
			}
			if test.afterWrite {
				primary = generatedSecret(config, pending.material)
				primary.UID = source.UID
				primary.ResourceVersion = "2"
			}

			client := newTestClient(config, primary, sourceMaterial.caPEM, twoReadyEndpoints(config))
			updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
				secret.Data = encodePendingCandidate(pending)
			})
			var primaryData map[string][]byte
			if primary != nil {
				primaryData = cloneBytesMap(mustGetSecret(t, client, config).Data)
			}
			actionStart := len(client.Actions())
			rotator := mustNewTestRotator(
				t,
				client,
				config,
				createdAt.Add(config.ServingCertificateValidity+time.Second),
				&recordingProber{},
			)
			_, err = rotator.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "retry reconciliation from authoritative primary state") {
				t.Fatalf("Run() error = %v, want retriable unusable-staging result", err)
			}
			if len(mustGetStagingSecret(t, client, config).Data) != 0 {
				t.Fatal("unusable related pending candidate was not retired")
			}
			for _, action := range client.Actions()[actionStart:] {
				if action.GetVerb() != "update" || action.GetResource().Resource == "leases" {
					continue
				}
				if action.GetResource().Resource != "secrets" ||
					action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name != config.StagingSecretName {
					t.Fatalf("unusable pending candidate triggered unsafe update to %s", action.GetResource().Resource)
				}
			}
			if primary != nil && !maps.EqualFunc(mustGetSecret(t, client, config).Data, primaryData, bytes.Equal) {
				t.Fatal("retiring unusable staging material changed the authoritative primary Secret")
			}
		})
	}
}

func TestUnusableUnrelatedPendingCandidateStaysFailClosed(t *testing.T) {
	t.Parallel()
	config := testConfig()
	createdAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	source := secretForMaterial(config, mustGenerateMaterial(t, createdAt, config))
	pending, err := generatePendingCandidate(rand.Reader, createdAt, config, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	foreign := secretForMaterial(config, mustGenerateMaterial(t, createdAt.Add(time.Hour), config))
	foreign.UID = source.UID
	foreign.ResourceVersion = "2"
	client := newTestClient(config, foreign, foreign.Data[CACertificateKey], twoReadyEndpoints(config))
	updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
		secret.Data = encodePendingCandidate(pending)
	})
	stagedData := cloneBytesMap(mustGetStagingSecret(t, client, config).Data)
	actionStart := len(client.Actions())

	_, err = mustNewTestRotator(
		t,
		client,
		config,
		createdAt.Add(config.ServingCertificateValidity+time.Second),
		&recordingProber{},
	).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unrelated to the current generated TLS Secret") {
		t.Fatalf("Run() error = %v, want unrelated durable-transition failure", err)
	}
	if !maps.EqualFunc(mustGetStagingSecret(t, client, config).Data, stagedData, bytes.Equal) {
		t.Fatal("unrelated unusable pending material was changed")
	}
	for _, action := range client.Actions()[actionStart:] {
		if action.GetVerb() == "update" && action.GetResource().Resource != "leases" {
			t.Fatalf("unrelated unusable pending material triggered update to %s", action.GetResource().Resource)
		}
	}
}

func TestUnusablePendingCandidateClearFailureRetainsRecord(t *testing.T) {
	t.Parallel()
	config := testConfig()
	createdAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	sourceMaterial := mustGenerateMaterial(t, createdAt, config)
	source := secretForMaterial(config, sourceMaterial)
	pending, err := generatePendingCandidate(rand.Reader, createdAt, config, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	client := newTestClient(config, source, sourceMaterial.caPEM, twoReadyEndpoints(config))
	updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
		secret.Data = encodePendingCandidate(pending)
	})
	stagedData := cloneBytesMap(mustGetStagingSecret(t, client, config).Data)
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if secret.Name == config.StagingSecretName {
			return true, nil, errors.New("injected staging clear failure")
		}
		return false, nil, nil
	})
	rotator := mustNewTestRotator(
		t,
		client,
		config,
		createdAt.Add(config.ServingCertificateValidity+time.Second),
		&recordingProber{},
	)
	_, err = rotator.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "clear unusable durable pending CA transition") {
		t.Fatalf("Run() error = %v, want failed exact-clear result", err)
	}
	if !maps.EqualFunc(mustGetStagingSecret(t, client, config).Data, stagedData, bytes.Equal) {
		t.Fatal("failed staging clear changed the durable pending record")
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

	first := mustNewTestRotator(t, client, currentConfig, now, &recordingProber{})
	_, err = first.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires immediate renewal") {
		t.Fatalf("first Run() error = %v, want immediate-renewal request", err)
	}
	if len(mustGetStagingSecret(t, client, currentConfig).Data) != 0 {
		t.Fatal("completed old-policy transition retained durable pending material")
	}
	assertFinalBundles(t, client, currentConfig, pending.material.caPEM)

	if _, err := mustNewTestRotator(t, client, currentConfig, now, &recordingProber{}).Run(context.Background()); err != nil {
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
	expanded := *pending
	expanded.phase = stagingPhaseExpanded
	expanded.expandedAt = now
	tests := []struct {
		name   string
		source *pendingCandidate
		mutate func(map[string][]byte)
	}{
		{
			name:   "unknown field",
			source: pending,
			mutate: func(data map[string][]byte) {
				data["foreign"] = []byte("data")
			},
		},
		{
			name:   "record of the one-pass format",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingFormatKey] = []byte("v2")
			},
		},
		{
			name:   "noncanonical source digest",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingSourceDigestKey] = []byte(strings.Repeat("A", stagingSourceDigestHexSize))
			},
		},
		{
			name:   "unknown operation",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingOperationKey] = []byte("foreign-operation")
			},
		},
		{
			name:   "unknown phase",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingPhaseKey] = []byte("foreign-phase")
			},
		},
		{
			name:   "one-pass phase",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingPhaseKey] = []byte("expansion-proven")
			},
		},
		{
			name:   "prepared record carries an expansion time",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingExpandedAtKey] = []byte("2026-09-05T12:00:00Z")
			},
		},
		{
			name:   "expanded record lacks an expansion time",
			source: &expanded,
			mutate: func(data map[string][]byte) {
				data[stagingExpandedAtKey] = nil
			},
		},
		{
			name:   "expansion time with a fraction of a second",
			source: &expanded,
			mutate: func(data map[string][]byte) {
				data[stagingExpandedAtKey] = []byte("2026-09-05T12:00:00.5Z")
			},
		},
		{
			name:   "expansion time outside UTC",
			source: &expanded,
			mutate: func(data map[string][]byte) {
				data[stagingExpandedAtKey] = []byte("2026-09-05T14:00:00+02:00")
			},
		},
		{
			name:   "expansion time that is not a time",
			source: &expanded,
			mutate: func(data map[string][]byte) {
				data[stagingExpandedAtKey] = []byte("six hours ago")
			},
		},
		{
			name:   "noncanonical transition digest",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingTransitionDigestKey] = []byte(strings.Repeat("A", stagingTransitionHexSize))
			},
		},
		{
			name:   "staged material changed without digest",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingServingKeyKey][len(data[stagingServingKeyKey])-2] ^= 1
			},
		},
		{
			name:   "CA key does not match",
			source: pending,
			mutate: func(data map[string][]byte) {
				data[stagingCAPrivateKeyKey] = append([]byte(nil), data[stagingServingKeyKey]...)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data := encodePendingCandidate(test.source)
			if _, err := decodePendingCandidate(data, config); err != nil {
				t.Fatalf("decodePendingCandidate() rejected the untampered record: %v", err)
			}
			test.mutate(data)
			if _, err := decodePendingCandidate(data, config); err == nil {
				t.Fatal("decodePendingCandidate() accepted tampered data")
			}
		})
	}
}

func TestPendingCandidateRecordBindsMaterialAndKeepsTheCursorOutside(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	source := secretForMaterial(config, mustGenerateMaterial(t, now, config))
	pending, err := generatePendingCandidate(rand.Reader, now, config, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	data := encodePendingCandidate(pending)
	if len(data) != stagingFieldCount {
		t.Fatalf("staging field count = %d, want %d", len(data), stagingFieldCount)
	}
	if string(data[stagingFormatKey]) != stagingFormat ||
		string(data[stagingOperationKey]) != string(stagingOperationCA) ||
		string(data[stagingPhaseKey]) != string(stagingPhasePrepared) ||
		len(data[stagingExpandedAtKey]) != 0 {
		t.Fatalf("new record tags are not exact: format=%q operation=%q phase=%q expanded-at=%q",
			data[stagingFormatKey], data[stagingOperationKey], data[stagingPhaseKey], data[stagingExpandedAtKey])
	}
	decoded, err := decodePendingCandidate(data, config)
	if err != nil {
		t.Fatalf("decodePendingCandidate() error = %v", err)
	}
	if decoded.transitionDigest != pendingCandidateDigest(decoded) || decoded.transitionDigest != pending.transitionDigest {
		t.Fatal("transition digest does not bind the exact decoded staged material")
	}

	expanded := *pending
	expanded.phase = stagingPhaseExpanded
	expanded.expandedAt = now.Add(3 * time.Hour)
	expandedData := encodePendingCandidate(&expanded)
	if got := string(expandedData[stagingExpandedAtKey]); got != "2026-09-05T15:00:00Z" {
		t.Fatalf("expanded-at = %q, want the whole-second UTC instant", got)
	}
	decodedExpanded, err := decodePendingCandidate(expandedData, config)
	if err != nil {
		t.Fatalf("decode expanded record: %v", err)
	}
	if decodedExpanded.transitionDigest != pending.transitionDigest {
		t.Fatal("recording the expansion changed the material transition digest")
	}
	if decodedExpanded.phase != stagingPhaseExpanded || !decodedExpanded.expandedAt.Equal(expanded.expandedAt) {
		t.Fatalf("decoded expanded record = phase %q at %s", decodedExpanded.phase, decodedExpanded.expandedAt)
	}
}

func TestPendingCandidateRejectsSameKeyReissuedCandidateCA(t *testing.T) {
	t.Parallel()

	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	source := secretForMaterial(config, mustGenerateMaterial(t, now, config))
	pending, err := generatePendingCandidate(rand.Reader, now, config, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	pending.material = mustReissueCAWithSubject(t, pending.material, "different-candidate-subject")
	pending.transitionDigest = pendingCandidateDigest(pending)

	if _, err := decodePendingCandidate(encodePendingCandidate(pending), config); err == nil ||
		!strings.Contains(err.Error(), "candidate primary serving material") {
		t.Fatalf("decodePendingCandidate() error = %v, want candidate issuer-chain rejection", err)
	}
}

func TestRecordExpansionPersistsTheCursorAndReanchorsIt(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	sourceMaterial := mustGenerateMaterial(t, now, config)
	source := secretForMaterial(config, sourceMaterial)
	pending, err := generatePendingCandidate(rand.Reader, now, config, source)
	if err != nil {
		t.Fatalf("generatePendingCandidate() error = %v", err)
	}
	client := newTestClient(config, source, sourceMaterial.caPEM, twoReadyEndpoints(config))
	updateStagingForTest(t, client, config, func(secret *corev1.Secret) {
		secret.Data = encodePendingCandidate(pending)
	})
	staging := mustGetStagingSecret(t, client, config)
	rotator := mustNewTestRotator(t, client, config, now, &recordingProber{})
	originalDigest := pending.transitionDigest

	if _, err := rotator.recordExpansion(context.Background(), staging, pending, now.Add(time.Millisecond)); err == nil {
		t.Fatal("recordExpansion() accepted an instant between whole seconds")
	}
	if pending.phase != stagingPhasePrepared || !pending.expandedAt.IsZero() {
		t.Fatalf("rejected record changed the in-memory cursor to %q at %s", pending.phase, pending.expandedAt)
	}

	for _, expandedAt := range []time.Time{now, now.Add(time.Hour)} {
		staging, err = rotator.recordExpansion(context.Background(), staging, pending, expandedAt)
		if err != nil {
			t.Fatalf("recordExpansion(%s) error = %v", expandedAt, err)
		}
		decoded, err := decodePendingCandidate(staging.Data, config)
		if err != nil {
			t.Fatalf("decode record expanded at %s: %v", expandedAt, err)
		}
		if decoded.phase != stagingPhaseExpanded || !decoded.expandedAt.Equal(expandedAt) ||
			pending.phase != stagingPhaseExpanded || !pending.expandedAt.Equal(expandedAt) {
			t.Fatalf("durable/in-memory cursor = %q at %s / %q at %s, want %q at %s",
				decoded.phase, decoded.expandedAt, pending.phase, pending.expandedAt, stagingPhaseExpanded, expandedAt)
		}
		if decoded.transitionDigest != originalDigest {
			t.Fatalf("recording the expansion at %s changed the transition digest", expandedAt)
		}
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
			keyless := secretForMaterial(config, material)
			delete(keyless.Data, CAPrivateKeyKey)
			client := newTestClient(config, keyless, material.caPEM, twoReadyEndpoints(config))
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
			_, err := mustNewTestRotator(t, client, config, now, &recordingProber{}).Run(context.Background())
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
				secret.Annotations = map[string]string{"operator.ptah.run/foreign": "true"}
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
			keyless := secretForMaterial(config, material)
			delete(keyless.Data, CAPrivateKeyKey)
			client := newTestClient(config, keyless, material.caPEM, twoReadyEndpoints(config))
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

			switchAt := mustExpandCATransition(t, client, config, now, &recordingProber{})
			_, err := mustNewTestRotator(t, client, config, switchAt, &recordingProber{}).Run(context.Background())
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
	keyless := secretForMaterial(config, material)
	delete(keyless.Data, CAPrivateKeyKey)
	client := newTestClient(config, keyless, material.caPEM, twoReadyEndpoints(config))
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

	completeCATransition(t, client, config, now, &recordingProber{})
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

			switchAt := mustExpandCATransition(t, client, config, now, &recordingProber{})
			_, err := mustNewTestRotator(t, client, config, switchAt, &recordingProber{}).Run(context.Background())
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
