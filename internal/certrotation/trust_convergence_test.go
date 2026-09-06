package certrotation

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTrustRepairUsesEphemeralPositiveConvergenceProof(t *testing.T) {
	t.Parallel()

	fixture := newTrustConvergenceFixture(t, false)
	trace := &trustConvergenceTrace{}
	sink := &trustConvergenceSink{current: fixture.current, trace: trace}
	canary := newTrustConvergenceCanary(fixture.client, fixture.config, fixture.current, trace, "")
	prober := &trustConvergenceProber{current: fixture.current, trace: trace}
	rotator := newTrustConvergenceRotator(t, fixture, sink, canary, prober)

	if err := rotator.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}

	want := trustRepairTrace()
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("trust-repair trace mismatch\n got: %s\nwant: %s", strings.Join(got, " -> "), strings.Join(want, " -> "))
	}
	assertTrustConvergenceSinkProtocol(t, sink, canary, fixture.current)
	assertFinalBundles(t, fixture.client, fixture.config, fixture.current.caPEM)
	assertTrustRepairDidNotPersistEphemeralMaterial(t, fixture)
}

func TestServingRotationOrdersCurrentAndReplacementEndpointProofs(t *testing.T) {
	t.Parallel()

	fixture := newTrustConvergenceFixture(t, true)
	trace := &trustConvergenceTrace{}
	sink := &trustConvergenceSink{current: fixture.current, trace: trace}
	canary := newTrustConvergenceCanary(fixture.client, fixture.config, fixture.current, trace, "")
	prober := &trustConvergenceProber{
		current:                  fixture.current,
		trace:                    trace,
		currentProbeIdentityOnly: true,
	}
	recordTrustConvergencePrimaryWrite(t, fixture, trace)
	rotator := newTrustConvergenceRotator(t, fixture, sink, canary, prober)

	if err := rotator.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}

	want := []string{
		"sink:N",
		"canary:expansion:mutating",
		"canary:expansion:validating",
		"canary:expansion:wait",
		"probe:current",
		"primary:write",
		"probe:new",
		"sink:P",
		"canary:contraction:mutating",
		"canary:contraction:validating",
		"canary:contraction:wait",
		"sink:N",
		"canary:parked:mutating",
		"canary:parked:validating",
		"canary:parked:wait",
		"sink:clear",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("serving-rotation trace mismatch\n got: %s\nwant: %s", strings.Join(got, " -> "), strings.Join(want, " -> "))
	}
	updated := mustGetSecret(t, fixture.client, fixture.config)
	state, err := inspectSecret(updated, fixture.config, fixture.now)
	if err != nil {
		t.Fatalf("inspect replacement Secret: %v", err)
	}
	if !certificateRawEqual(state.current.ca, fixture.current.ca) {
		t.Fatal("serving-only rotation replaced the certificate authority")
	}
	if certificateRawEqual(state.current.leaf, fixture.current.leaf) {
		t.Fatal("serving-only rotation retained the expiring leaf")
	}
	assertTrustConvergenceSinkProtocol(t, sink, canary, fixture.current)
	assertFinalBundles(t, fixture.client, fixture.config, fixture.current.caPEM)
}

func TestTrustRepairReprovesExactContractionAfterCrash(t *testing.T) {
	t.Parallel()

	fixture := newTrustConvergenceFixture(t, false)
	firstTrace := &trustConvergenceTrace{}
	firstSink := &trustConvergenceSink{current: fixture.current, trace: firstTrace}
	firstCanary := newTrustConvergenceCanary(
		fixture.client,
		fixture.config,
		fixture.current,
		firstTrace,
		"canary:parked:mutating",
	)
	first := newTrustConvergenceRotator(
		t,
		fixture,
		firstSink,
		firstCanary,
		&trustConvergenceProber{current: fixture.current, trace: firstTrace},
	)

	err := first.reconcile(context.Background())
	if !errors.Is(err, errTrustConvergenceCrash) {
		t.Fatalf("first reconcile() error = %v, want injected crash", err)
	}
	wantFirst := append(trustRepairTrace()[:9],
		"sink:N",
		"canary:parked:mutating",
	)
	if got := firstTrace.snapshot(); !reflect.DeepEqual(got, wantFirst) {
		t.Fatalf("pre-crash trace mismatch\n got: %s\nwant: %s", strings.Join(got, " -> "), strings.Join(wantFirst, " -> "))
	}
	assertFinalBundles(t, fixture.client, fixture.config, fixture.current.caPEM)

	// The production entries are already byte-exact after contraction. A new
	// process must still replay live expansion, endpoint, contraction, and
	// parked evidence because the crash may have preceded API-server cache
	// convergence.
	recoveryTrace := &trustConvergenceTrace{}
	recoverySink := &trustConvergenceSink{current: fixture.current, trace: recoveryTrace}
	recoveryCanary := newTrustConvergenceCanary(
		fixture.client,
		fixture.config,
		fixture.current,
		recoveryTrace,
		"",
	)
	recovery := newTrustConvergenceRotator(
		t,
		fixture,
		recoverySink,
		recoveryCanary,
		&trustConvergenceProber{current: fixture.current, trace: recoveryTrace},
	)
	if err := recovery.reconcile(context.Background()); err != nil {
		t.Fatalf("recovery reconcile() error = %v", err)
	}
	if got, want := recoveryTrace.snapshot(), trustRepairTrace(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery treated exact production trust as a no-op\n got: %s\nwant: %s", strings.Join(got, " -> "), strings.Join(want, " -> "))
	}
	assertTrustConvergenceSinkProtocol(t, recoverySink, recoveryCanary, fixture.current)
	assertFinalBundles(t, fixture.client, fixture.config, fixture.current.caPEM)
	assertTrustRepairDidNotPersistEphemeralMaterial(t, fixture)
}

var errTrustConvergenceCrash = errors.New("injected trust-convergence crash")

type trustConvergenceFixture struct {
	config  Config
	now     time.Time
	current certificateMaterial
	client  *fake.Clientset
}

func newTrustConvergenceFixture(t *testing.T, servingRotation bool) trustConvergenceFixture {
	t.Helper()

	config := testConfig()
	createdAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	now := createdAt
	if servingRotation {
		now = createdAt.Add(25 * 24 * time.Hour)
	}
	current := mustGenerateMaterial(t, createdAt, config)
	legacy := mustGenerateMaterial(t, createdAt.Add(time.Minute), config)
	overlap, err := combineCABundles(legacy.caPEM, current.caPEM)
	if err != nil {
		t.Fatalf("build pre-repair trust bundle: %v", err)
	}
	endpoints := endpointSlice(config, readyEndpoint("10.0.0.10", "pod-a", "uid-a", config.Namespace))
	client := newTestClient(config, secretForMaterial(config, current), current.caPEM, endpoints)
	setManagedBundles(t, client, config, overlap, [][]byte{overlap, overlap})
	client.ClearActions()
	return trustConvergenceFixture{config: config, now: now, current: current, client: client}
}

func newTrustConvergenceRotator(
	t *testing.T,
	fixture trustConvergenceFixture,
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

func trustRepairTrace() []string {
	return []string{
		"sink:N",
		"canary:expansion:mutating",
		"canary:expansion:validating",
		"canary:expansion:wait",
		"probe:current",
		"sink:P",
		"canary:contraction:mutating",
		"canary:contraction:validating",
		"canary:contraction:wait",
		"sink:N",
		"canary:parked:mutating",
		"canary:parked:validating",
		"canary:parked:wait",
		"sink:clear",
	}
}

type trustConvergenceTrace struct {
	mu     sync.Mutex
	events []string
}

func (trace *trustConvergenceTrace) add(event string) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.events = append(trace.events, event)
}

func (trace *trustConvergenceTrace) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.events...)
}

type trustConvergenceStoredCertificate struct {
	certificatePEM []byte
	privateKeyPEM  []byte
}

type trustConvergenceSink struct {
	current certificateMaterial
	trace   *trustConvergenceTrace
	stored  []trustConvergenceStoredCertificate
	clears  int
}

func (sink *trustConvergenceSink) StoreCandidateCertificate(certificatePEM, privateKeyPEM []byte) error {
	if _, err := tls.X509KeyPair(certificatePEM, privateKeyPEM); err != nil {
		return fmt.Errorf("candidate listener key pair: %w", err)
	}
	leaf, _, err := parseSingleCertificate(certificatePEM)
	if err != nil {
		return fmt.Errorf("candidate listener certificate: %w", err)
	}
	label := "P"
	if leaf.CheckSignatureFrom(sink.current.ca) == nil {
		label = "N"
	}
	sink.trace.add("sink:" + label)
	sink.stored = append(sink.stored, trustConvergenceStoredCertificate{
		certificatePEM: append([]byte(nil), certificatePEM...),
		privateKeyPEM:  append([]byte(nil), privateKeyPEM...),
	})
	return nil
}

func (sink *trustConvergenceSink) ClearCandidateCertificate() {
	// reconcile() defensively clears an empty staging listener before it reads
	// the primary Secret. Keep the trace focused on the transition lifecycle.
	if len(sink.stored) == 0 {
		return
	}
	sink.trace.add("sink:clear")
	sink.clears++
}

type trustConvergenceCanary struct {
	delegate  admissionCanaryController
	currentCA []byte
	trace     *trustConvergenceTrace
	failAt    string
	proofCAs  [][]byte
}

func newTrustConvergenceCanary(
	client *fake.Clientset,
	config Config,
	current certificateMaterial,
	trace *trustConvergenceTrace,
	failAt string,
) *trustConvergenceCanary {
	return &trustConvergenceCanary{
		delegate:  newTestAdmissionCanaryController(client, config),
		currentCA: append([]byte(nil), current.caPEM...),
		trace:     trace,
		failAt:    failAt,
	}
}

func (canary *trustConvergenceCanary) PublishMutating(
	ctx context.Context,
	desired AdmissionCanaryDesiredState,
) error {
	if err := canary.record("mutating", desired); err != nil {
		return err
	}
	return canary.delegate.PublishMutating(ctx, desired)
}

func (canary *trustConvergenceCanary) PublishValidating(
	ctx context.Context,
	desired AdmissionCanaryDesiredState,
) error {
	if err := canary.record("validating", desired); err != nil {
		return err
	}
	return canary.delegate.PublishValidating(ctx, desired)
}

func (canary *trustConvergenceCanary) Wait(
	ctx context.Context,
	desired AdmissionCanaryDesiredState,
) error {
	if err := canary.record("wait", desired); err != nil {
		return err
	}
	return canary.delegate.Wait(ctx, desired)
}

func (canary *trustConvergenceCanary) record(operation string, desired AdmissionCanaryDesiredState) error {
	state, err := canary.stateName(desired)
	if err != nil {
		return err
	}
	event := "canary:" + state + ":" + operation
	canary.trace.add(event)
	if event == canary.failAt {
		return errTrustConvergenceCrash
	}
	return nil
}

func (canary *trustConvergenceCanary) stateName(desired AdmissionCanaryDesiredState) (string, error) {
	if desired.productionMode == admissionCanaryProductionContains &&
		caBundlesEqual(desired.productionBundle, canary.currentCA) &&
		caBundlesEqual(desired.canaryBundle, canary.currentCA) {
		return "expansion", nil
	}
	if desired.productionMode != admissionCanaryProductionExact ||
		!caBundlesEqual(desired.productionBundle, canary.currentCA) {
		return "", errors.New("unexpected trust-convergence production state")
	}
	if caBundlesEqual(desired.canaryBundle, canary.currentCA) {
		return "parked", nil
	}
	disjoint, err := certificateBundlesDisjoint(desired.productionBundle, desired.canaryBundle)
	if err != nil {
		return "", fmt.Errorf("parse trust-convergence contraction state: %w", err)
	}
	if !disjoint {
		return "", errors.New("trust-convergence contraction proof is not independent")
	}
	canary.proofCAs = append(canary.proofCAs, append([]byte(nil), desired.canaryBundle...))
	return "contraction", nil
}

type trustConvergenceProber struct {
	current                  certificateMaterial
	trace                    *trustConvergenceTrace
	currentProbeIdentityOnly bool
}

func (prober *trustConvergenceProber) Probe(_ context.Context, request probeRequest) error {
	if !caBundlesEqual(request.CACertificatePEM, prober.current.caPEM) {
		return errors.New("trust convergence probed the primary endpoint with a foreign CA")
	}
	label := "new"
	if certificateRawEqual(request.LeafCertificate, prober.current.leaf) {
		label = "current"
		if request.IdentityOnly != prober.currentProbeIdentityOnly {
			return fmt.Errorf(
				"current primary identity-only probe = %v, want %v",
				request.IdentityOnly,
				prober.currentProbeIdentityOnly,
			)
		}
	} else if request.IdentityOnly {
		return errors.New("replacement primary proof was identity-only")
	}
	prober.trace.add("probe:" + label)
	return nil
}

func recordTrustConvergencePrimaryWrite(
	t *testing.T,
	fixture trustConvergenceFixture,
	trace *trustConvergenceTrace,
) {
	t.Helper()

	fixture.client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		secret := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if secret.Name != fixture.config.SecretName {
			return false, nil, nil
		}
		state, err := inspectSecret(secret, fixture.config, fixture.now)
		if err != nil {
			t.Errorf("inspect primary write: %v", err)
			return false, nil, nil
		}
		if !certificateRawEqual(state.current.ca, fixture.current.ca) ||
			certificateRawEqual(state.current.leaf, fixture.current.leaf) {
			t.Error("primary write is not a serving-only replacement")
		}
		trace.add("primary:write")
		return false, nil, nil
	})
}

func assertTrustConvergenceSinkProtocol(
	t *testing.T,
	sink *trustConvergenceSink,
	canary *trustConvergenceCanary,
	current certificateMaterial,
) {
	t.Helper()

	if len(sink.stored) != 3 {
		t.Fatalf("candidate listener store count = %d, want 3", len(sink.stored))
	}
	if sink.clears != 1 {
		t.Fatalf("candidate listener clear count = %d, want 1", sink.clears)
	}
	if !bytes.Equal(sink.stored[0].certificatePEM, sink.stored[2].certificatePEM) ||
		!bytes.Equal(sink.stored[0].privateKeyPEM, sink.stored[2].privateKeyPEM) {
		t.Fatal("parked listener did not restore the exact expansion certificate")
	}
	if bytes.Equal(sink.stored[0].certificatePEM, sink.stored[1].certificatePEM) {
		t.Fatal("contraction proof reused the authoritative-CA listener certificate")
	}
	if len(canary.proofCAs) != 3 {
		t.Fatalf("observed contraction proof bundle count = %d, want one per M/V/Wait operation", len(canary.proofCAs))
	}
	for index := 1; index < len(canary.proofCAs); index++ {
		if !caBundlesEqual(canary.proofCAs[0], canary.proofCAs[index]) {
			t.Fatal("one contraction convergence sequence used multiple proof CAs")
		}
	}
	proofCA, _, err := parseSingleCertificate(canary.proofCAs[0])
	if err != nil {
		t.Fatalf("parse contraction proof CA: %v", err)
	}
	proofLeaf, _, err := parseSingleCertificate(sink.stored[1].certificatePEM)
	if err != nil {
		t.Fatalf("parse contraction proof listener: %v", err)
	}
	if err := proofLeaf.CheckSignatureFrom(proofCA); err != nil {
		t.Fatalf("contraction listener was not signed by the independently published proof CA: %v", err)
	}
	if proofLeaf.CheckSignatureFrom(current.ca) == nil {
		t.Fatal("contraction listener is also trusted by the authoritative production CA")
	}
}

func assertTrustRepairDidNotPersistEphemeralMaterial(t *testing.T, fixture trustConvergenceFixture) {
	t.Helper()

	if secret := mustGetSecret(t, fixture.client, fixture.config); !secretContainsMaterial(secret, fixture.current) {
		t.Fatal("trust repair changed authoritative Secret material")
	}
	staging, err := fixture.client.CoreV1().Secrets(fixture.config.Namespace).Get(
		context.Background(),
		fixture.config.StagingSecretName,
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get staging Secret: %v", err)
	}
	if len(staging.Data) != 0 {
		t.Fatalf("trust repair persisted %d ephemeral staging fields", len(staging.Data))
	}
	for _, action := range fixture.client.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "secrets" {
			t.Fatalf("trust repair persisted an ephemeral certificate through Secret %q", action.GetSubresource())
		}
	}
}
