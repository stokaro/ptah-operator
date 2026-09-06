package certrotation

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPendingCATransitionUsesEvidenceOrderedProtocol(t *testing.T) {
	t.Parallel()

	fixture := newCATransitionFixture(t)
	trace := &caTransitionTrace{}
	sink := &caTransitionCandidateSink{pending: fixture.pending, trace: trace}
	canary := newCATransitionCanary(t, fixture.original.caPEM, fixture.pending, trace, "")
	prober := &caTransitionProber{pending: fixture.pending, trace: trace}
	recordCATransitionSecretWrites(t, fixture, trace)
	rotator := newCATransitionRotator(t, fixture, sink, canary, prober)

	if err := rotator.runPendingCATransition(
		context.Background(),
		fixture.staging,
		fixture.pending,
		fixture.original.caPEM,
	); err != nil {
		t.Fatalf("runPendingCATransition() error = %v", err)
	}

	want := []string{
		"sink:new",
		"canary:expansion:mutating",
		"phase:" + string(stagingPhaseExpansionMutatingStored),
		"canary:expansion:validating",
		"phase:" + string(stagingPhaseExpansionBothStored),
		"canary:expansion:wait",
		"phase:" + string(stagingPhaseExpansionProven),
		"primary:write",
		"phase:" + string(stagingPhasePrimaryWritten),
		"primary:probe",
		"phase:" + string(stagingPhasePrimaryServed),
		"sink:proof",
		"canary:contraction:mutating",
		"phase:" + string(stagingPhaseContractionMutatingStored),
		"canary:contraction:validating",
		"phase:" + string(stagingPhaseContractionBothStored),
		"canary:contraction:wait",
		"phase:" + string(stagingPhaseContractionProven),
		"sink:new",
		"canary:parked:mutating",
		"phase:" + string(stagingPhaseMutatingParked),
		"canary:parked:validating",
		"canary:parked:wait",
		"phase:" + string(stagingPhaseBothParked),
		"staging:clear",
		"sink:clear",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("transition trace mismatch\n got: %s\nwant: %s", strings.Join(got, " -> "), strings.Join(want, " -> "))
	}
	if data := mustGetStagingSecret(t, fixture.client, fixture.config).Data; len(data) != 0 {
		t.Fatalf("completed staging Secret has %d fields, want none", len(data))
	}
	if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), fixture.pending.material) {
		t.Fatal("completed primary Secret does not contain the durable candidate material")
	}
}

func TestPendingCATransitionFailurePreservesDurableStateAndRecoveryReplaysEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failAt    string
		wantPhase stagingPhase
	}{
		{
			name:      "expansion mutating publication",
			failAt:    "canary:expansion:mutating",
			wantPhase: stagingPhasePrepared,
		},
		{
			name:      "expansion validating publication",
			failAt:    "canary:expansion:validating",
			wantPhase: stagingPhaseExpansionMutatingStored,
		},
		{
			name:      "expansion convergence proof",
			failAt:    "canary:expansion:wait",
			wantPhase: stagingPhaseExpansionBothStored,
		},
		{
			name:      "primary serving proof",
			failAt:    "primary:probe",
			wantPhase: stagingPhasePrimaryWritten,
		},
		{
			name:      "contraction mutating publication",
			failAt:    "canary:contraction:mutating",
			wantPhase: stagingPhasePrimaryServed,
		},
		{
			name:      "contraction validating publication",
			failAt:    "canary:contraction:validating",
			wantPhase: stagingPhaseContractionMutatingStored,
		},
		{
			name:      "contraction convergence proof",
			failAt:    "canary:contraction:wait",
			wantPhase: stagingPhaseContractionBothStored,
		},
		{
			name:      "parked mutating publication",
			failAt:    "canary:parked:mutating",
			wantPhase: stagingPhaseContractionProven,
		},
		{
			name:      "parked validating publication",
			failAt:    "canary:parked:validating",
			wantPhase: stagingPhaseMutatingParked,
		},
		{
			name:      "parked convergence proof",
			failAt:    "canary:parked:wait",
			wantPhase: stagingPhaseMutatingParked,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCATransitionFixture(t)
			originalStagingData := cloneBytesMap(fixture.staging.Data)
			firstTrace := &caTransitionTrace{}
			firstSink := &caTransitionCandidateSink{pending: fixture.pending, trace: firstTrace}
			firstCanary := newCATransitionCanary(
				t,
				fixture.original.caPEM,
				fixture.pending,
				firstTrace,
				test.failAt,
			)
			firstProber := &caTransitionProber{
				pending: fixture.pending,
				trace:   firstTrace,
				fail:    test.failAt == "primary:probe",
			}
			if firstProber.fail {
				fixture.config.ProbeTimeout = 2 * time.Millisecond
				fixture.config.ProbeInterval = time.Millisecond
			}
			first := newCATransitionRotator(t, fixture, firstSink, firstCanary, firstProber)

			err := first.runPendingCATransition(
				context.Background(),
				fixture.staging,
				fixture.pending,
				fixture.original.caPEM,
			)
			if err == nil || !strings.Contains(err.Error(), caTransitionInjectedFailure.Error()) {
				t.Fatalf("first run error = %v, want injected failure", err)
			}

			stagingAfterFailure := mustGetStagingSecret(t, fixture.client, fixture.config)
			if len(stagingAfterFailure.Data) == 0 {
				t.Fatal("failed transition cleared its durable staging record")
			}
			pendingAfterFailure, err := decodePendingCandidate(
				stagingAfterFailure.Data,
				fixture.config,
			)
			if err != nil {
				t.Fatalf("decode pending candidate after failure: %v", err)
			}
			if pendingAfterFailure.phase != test.wantPhase {
				t.Fatalf("phase after failed evidence = %q, want %q", pendingAfterFailure.phase, test.wantPhase)
			}
			assertCATransitionStagingMaterialUnchanged(t, originalStagingData, stagingAfterFailure.Data)

			recoveryTrace := &caTransitionTrace{}
			recoverySink := &caTransitionCandidateSink{pending: pendingAfterFailure, trace: recoveryTrace}
			recoveryCanary := newCATransitionCanary(
				t,
				fixture.original.caPEM,
				pendingAfterFailure,
				recoveryTrace,
				"",
			)
			recoveryProber := &caTransitionProber{pending: pendingAfterFailure, trace: recoveryTrace}
			recovery := newCATransitionRotator(t, fixture, recoverySink, recoveryCanary, recoveryProber)
			rejectingRandom := &caTransitionRejectingReader{}
			recovery.random = rejectingRandom

			if err := recovery.reconcile(context.Background()); err != nil {
				t.Fatalf("recovery reconcile() error = %v", err)
			}
			if rejectingRandom.calls != 0 {
				t.Fatalf("recovery requested %d bytes of new randomness, want none", rejectingRandom.calls)
			}
			wantRecoveryTrace := []string{
				"sink:new",
				"canary:expansion:mutating",
				"canary:expansion:validating",
				"canary:expansion:wait",
				"primary:probe",
				"sink:proof",
				"canary:contraction:mutating",
				"canary:contraction:validating",
				"canary:contraction:wait",
				"sink:new",
				"canary:parked:mutating",
				"canary:parked:validating",
				"canary:parked:wait",
				"sink:clear",
			}
			if got := recoveryTrace.snapshot(); !reflect.DeepEqual(got, wantRecoveryTrace) {
				t.Fatalf(
					"recovery did not replay the complete live-evidence protocol\n got: %s\nwant: %s",
					strings.Join(got, " -> "),
					strings.Join(wantRecoveryTrace, " -> "),
				)
			}
			if data := mustGetStagingSecret(t, fixture.client, fixture.config).Data; len(data) != 0 {
				t.Fatalf("recovery left %d staging fields, want none", len(data))
			}
			if !secretContainsMaterial(
				mustGetSecret(t, fixture.client, fixture.config),
				pendingAfterFailure.material,
			) {
				t.Fatal("recovery replaced or failed to install the durable candidate material")
			}
		})
	}
}

var caTransitionInjectedFailure = errors.New("injected CA transition failure")

type caTransitionFixture struct {
	config   Config
	now      time.Time
	original certificateMaterial
	client   *fake.Clientset
	staging  *corev1.Secret
	pending  *pendingCandidate
}

func newCATransitionFixture(t *testing.T) caTransitionFixture {
	t.Helper()

	config := testConfig()
	now := time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	primary := secretForMaterial(config, original)
	pending, err := generatePendingCandidate(rand.Reader, now, config, primary)
	if err != nil {
		t.Fatalf("generate pending CA transition: %v", err)
	}
	endpoints := endpointSlice(config, readyEndpoint("10.0.0.10", "pod-a", "uid-a", config.Namespace))
	client := newTestClient(config, primary, original.caPEM, endpoints)
	staging := mustGetStagingSecret(t, client, config)
	staging.Data = encodePendingCandidate(pending)
	staging, err = client.CoreV1().Secrets(config.Namespace).Update(
		context.Background(),
		staging,
		metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("install pending CA transition fixture: %v", err)
	}
	return caTransitionFixture{
		config:   config,
		now:      now,
		original: original,
		client:   client,
		staging:  staging,
		pending:  pending,
	}
}

func newCATransitionRotator(
	t *testing.T,
	fixture caTransitionFixture,
	sink CandidateCertificateSink,
	canary admissionCanaryController,
	prober certificateProber,
) *Rotator {
	t.Helper()

	rotator, err := newRotator(fixture.client, fixture.config, sink, canary)
	if err != nil {
		t.Fatalf("newRotator() error = %v", err)
	}
	rotator.now = func() time.Time { return fixture.now }
	rotator.probe = prober
	return rotator
}

func recordCATransitionSecretWrites(t *testing.T, fixture caTransitionFixture, trace *caTransitionTrace) {
	t.Helper()

	fixture.client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		switch secret.Name {
		case fixture.config.SecretName:
			if !secretContainsMaterial(secret, fixture.pending.material) {
				t.Error("primary write did not contain the durable candidate material")
			}
			trace.add("primary:write")
		case fixture.config.StagingSecretName:
			if len(secret.Data) == 0 {
				trace.add("staging:clear")
				break
			}
			pending, err := decodePendingCandidate(secret.Data, fixture.config)
			if err != nil {
				t.Errorf("decode phase write: %v", err)
				break
			}
			trace.add("phase:" + string(pending.phase))
		}
		return false, nil, nil
	})
}

type caTransitionTrace struct {
	events []string
}

func (trace *caTransitionTrace) add(event string) {
	trace.events = append(trace.events, event)
}

func (trace *caTransitionTrace) snapshot() []string {
	return append([]string(nil), trace.events...)
}

type caTransitionCandidateSink struct {
	pending *pendingCandidate
	trace   *caTransitionTrace
}

func (sink *caTransitionCandidateSink) StoreCandidateCertificate(certificatePEM, privateKeyPEM []byte) error {
	switch {
	case bytes.Equal(certificatePEM, sink.pending.listenerCertPEM) &&
		bytes.Equal(privateKeyPEM, sink.pending.listenerKeyPEM):
		sink.trace.add("sink:new")
	case bytes.Equal(certificatePEM, sink.pending.proofListenerCertPEM) &&
		bytes.Equal(privateKeyPEM, sink.pending.proofListenerKeyPEM):
		sink.trace.add("sink:proof")
	default:
		return errors.New("candidate sink received material outside the durable transition")
	}
	return nil
}

func (sink *caTransitionCandidateSink) ClearCandidateCertificate() {
	sink.trace.add("sink:clear")
}

type caTransitionCanary struct {
	expansions  []AdmissionCanaryDesiredState
	contraction AdmissionCanaryDesiredState
	parked      AdmissionCanaryDesiredState
	trace       *caTransitionTrace
	failAt      string
}

func newCATransitionCanary(
	t *testing.T,
	oldCA []byte,
	pending *pendingCandidate,
	trace *caTransitionTrace,
	failAt string,
) *caTransitionCanary {
	t.Helper()

	expansion, err := NewAdmissionCanaryExpansion(oldCA, pending.material.caPEM)
	if err != nil {
		t.Fatalf("build expected expansion state: %v", err)
	}
	recoveryExpansion, err := NewAdmissionCanaryExpansion(nil, pending.material.caPEM)
	if err != nil {
		t.Fatalf("build expected post-write expansion state: %v", err)
	}
	contraction, err := NewAdmissionCanaryContraction(pending.material.caPEM, pending.proofCACertPEM)
	if err != nil {
		t.Fatalf("build expected contraction state: %v", err)
	}
	parked, err := NewAdmissionCanaryParked(pending.material.caPEM)
	if err != nil {
		t.Fatalf("build expected parked state: %v", err)
	}
	return &caTransitionCanary{
		expansions:  []AdmissionCanaryDesiredState{expansion, recoveryExpansion},
		contraction: contraction,
		parked:      parked,
		trace:       trace,
		failAt:      failAt,
	}
}

func (canary *caTransitionCanary) PublishMutating(
	_ context.Context,
	desired AdmissionCanaryDesiredState,
) error {
	return canary.record("mutating", desired)
}

func (canary *caTransitionCanary) PublishValidating(
	_ context.Context,
	desired AdmissionCanaryDesiredState,
) error {
	return canary.record("validating", desired)
}

func (canary *caTransitionCanary) Wait(_ context.Context, desired AdmissionCanaryDesiredState) error {
	return canary.record("wait", desired)
}

func (canary *caTransitionCanary) record(operation string, desired AdmissionCanaryDesiredState) error {
	state, err := canary.stateName(desired)
	if err != nil {
		return err
	}
	event := "canary:" + state + ":" + operation
	canary.trace.add(event)
	if event == canary.failAt {
		return caTransitionInjectedFailure
	}
	return nil
}

func (canary *caTransitionCanary) stateName(desired AdmissionCanaryDesiredState) (string, error) {
	for _, expansion := range canary.expansions {
		if equalAdmissionCanaryDesiredState(desired, expansion) {
			return "expansion", nil
		}
	}
	if equalAdmissionCanaryDesiredState(desired, canary.contraction) {
		return "contraction", nil
	}
	if equalAdmissionCanaryDesiredState(desired, canary.parked) {
		return "parked", nil
	}
	return "", errors.New("admission canary received an unexpected desired state")
}

func equalAdmissionCanaryDesiredState(left, right AdmissionCanaryDesiredState) bool {
	return left.productionMode == right.productionMode &&
		bytes.Equal(left.productionBundle, right.productionBundle) &&
		bytes.Equal(left.canaryBundle, right.canaryBundle)
}

type caTransitionProber struct {
	pending *pendingCandidate
	trace   *caTransitionTrace
	fail    bool
}

func (prober *caTransitionProber) Probe(_ context.Context, request probeRequest) error {
	if request.IdentityOnly ||
		!bytes.Equal(request.CACertificatePEM, prober.pending.material.caPEM) ||
		!certificateRawEqual(request.LeafCertificate, prober.pending.material.leaf) {
		return errors.New("primary probe did not request the exact durable candidate identity")
	}
	prober.trace.add("primary:probe")
	if prober.fail {
		return caTransitionInjectedFailure
	}
	return nil
}

type caTransitionRejectingReader struct {
	calls int
}

func (reader *caTransitionRejectingReader) Read(buffer []byte) (int, error) {
	reader.calls += len(buffer)
	return 0, errors.New("recovery attempted to generate replacement material")
}

func assertCATransitionStagingMaterialUnchanged(t *testing.T, before, after map[string][]byte) {
	t.Helper()

	if len(after) != len(before) {
		t.Fatalf("staging field count after failure = %d, want %d", len(after), len(before))
	}
	for key, want := range before {
		if key == stagingPhaseKey {
			continue
		}
		got, found := after[key]
		if !found {
			t.Fatalf("staging field %q disappeared after failure", key)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("staging field %q changed after failure", key)
		}
	}
	if got, want := string(after[stagingTransitionDigestKey]), string(before[stagingTransitionDigestKey]); got != want {
		t.Fatalf("transition digest after failure = %q, want %q", got, want)
	}
}
