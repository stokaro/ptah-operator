package certrotation

// These white-box tests drive one CA transition through its three durable
// steps -- expand, switch, retire -- with a fresh Rotator for every pass, the
// way a restarted rotator Pod would meet it, and at the instants where the
// schedule can go wrong.

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"slices"
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

func TestCATransitionExpandsSwitchesAndRetiresAcrossRestarts(t *testing.T) {
	t.Parallel()
	fixture := newCATransitionFixture(t)
	start := fixture.start
	delay := fixture.config.CASwitchDelay

	// Expand: both CAs in every entry, the Secret untouched, the switch due
	// one delay later.
	fixture.mustPass(t, start, delay)
	staged := fixture.staged(t)
	if staged.phase != stagingPhaseExpanded || !staged.expandedAt.Equal(start) {
		t.Fatalf("staged record = phase %q expanded at %s, want %q at %s", staged.phase, staged.expandedAt, stagingPhaseExpanded, start)
	}
	fixture.assertSourceUnchanged(t)
	fixture.assertEveryEntryTrusts(t, fixture.original.caPEM, staged.material.caPEM)

	// A restart anywhere inside the delay changes nothing and comes back at
	// the recorded switch time, not a full delay after the restart.
	for _, at := range []time.Time{start.Add(delay / 2), start.Add(delay - time.Nanosecond)} {
		writes := fixture.writesSince(len(fixture.client.Actions()))
		fixture.mustPass(t, at, start.Add(delay).Sub(at))
		if got := writes(); len(got) != 0 {
			t.Fatalf("pass at %s inside the delay wrote %v", at, got)
		}
		fixture.assertSourceUnchanged(t)
		if again := fixture.staged(t); again.transitionDigest != staged.transitionDigest || !again.expandedAt.Equal(start) {
			t.Fatalf("pass at %s changed the staged transition", at)
		}
	}

	// Switch and retire, in that order, at the recorded time.
	trace := fixture.traceWrites()
	fixture.mustPass(t, start.Add(delay), 0)
	if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), staged.material) {
		t.Fatal("the switch did not install the staged material")
	}
	assertFinalBundles(t, fixture.client, fixture.config, staged.material.caPEM)
	if len(mustGetStagingSecret(t, fixture.client, fixture.config).Data) != 0 {
		t.Fatal("the finished transition kept its staging record")
	}
	want := []string{
		"update secrets/" + fixture.config.SecretName,
		"update mutatingwebhookconfigurations/" + fixture.config.MutatingWebhookConfiguration,
		"update validatingwebhookconfigurations/" + fixture.config.ValidatingWebhookConfiguration,
		"update secrets/" + fixture.config.StagingSecretName,
	}
	if got := trace.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("switch pass writes = %v, want %v", got, want)
	}
	requests := fixture.prober.probeRequests()
	if len(requests) == 0 {
		t.Fatal("the switch pass retired the old CA without probing an endpoint")
	}
	for _, request := range requests {
		if request.IdentityOnly || !caBundlesEqual(request.CACertificatePEM, staged.material.caPEM) ||
			!certificateRawEqual(request.LeafCertificate, staged.material.leaf) {
			t.Fatal("an endpoint probe did not require the new leaf verified by the new CA")
		}
	}
}

func TestCATransitionResumesFromEveryInterruptedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// arm injects the failure that interrupts the transition. It runs
		// before the first pass and may change the prober of that pass.
		arm func(*testing.T, *caTransitionFixture) *recordingProber
		// failAtSwitch is set when the failure hits the switch pass rather
		// than the expansion pass.
		failAtSwitch bool
	}{
		{
			name: "record staged, trust publication failed",
			arm: func(_ *testing.T, fixture *caTransitionFixture) *recordingProber {
				fixture.failOnce("update", "mutatingwebhookconfigurations", nil)
				return fixture.prober
			},
		},
		{
			name: "trust published, expansion record lost",
			arm: func(_ *testing.T, fixture *caTransitionFixture) *recordingProber {
				fixture.failOnce("update", "secrets", func(secret *corev1.Secret) bool {
					return secret.Name == fixture.config.StagingSecretName &&
						string(secret.Data[stagingPhaseKey]) == string(stagingPhaseExpanded)
				})
				return fixture.prober
			},
		},
		{
			name:         "Secret switched, new certificate not yet served",
			failAtSwitch: true,
			arm: func(*testing.T, *caTransitionFixture) *recordingProber {
				return &recordingProber{err: errors.New("projection pending")}
			},
		},
		{
			name:         "old CA retired, record not cleared",
			failAtSwitch: true,
			arm: func(_ *testing.T, fixture *caTransitionFixture) *recordingProber {
				fixture.failOnce("update", "secrets", func(secret *corev1.Secret) bool {
					return secret.Name == fixture.config.StagingSecretName && len(secret.Data) == 0
				})
				return fixture.prober
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCATransitionFixture(t)
			fixture.config.ProbeTimeout = 15 * time.Millisecond
			start := fixture.start
			delay := fixture.config.CASwitchDelay
			failingProber := test.arm(t, fixture)

			resumeAt := start.Add(time.Minute)
			if test.failAtSwitch {
				fixture.mustPass(t, start, delay)
				if _, err := fixture.pass(start.Add(delay), failingProber); err == nil {
					t.Fatal("interrupted switch pass succeeded")
				}
				resumeAt = start.Add(delay + time.Minute)
			} else {
				if _, err := fixture.pass(start, failingProber); err == nil {
					t.Fatal("interrupted expansion pass succeeded")
				}
				fixture.assertSourceUnchanged(t)
			}
			staged := fixture.staged(t)

			// The replacement resumes the same transition. If the switch has not
			// happened, it waits a full delay from the expansion it can vouch for.
			fixture.config.ProbeTimeout = time.Second
			if !test.failAtSwitch {
				fixture.mustPass(t, resumeAt, delay)
				fixture.assertSourceUnchanged(t)
				if _, err := fixture.pass(resumeAt.Add(delay-time.Nanosecond), fixture.prober); err != nil {
					t.Fatalf("pass just before the switch: %v", err)
				}
				fixture.assertSourceUnchanged(t)
				resumeAt = resumeAt.Add(delay)
			}
			fixture.mustPass(t, resumeAt, 0)

			if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), staged.material) {
				t.Fatal("recovery installed material other than the first staged candidate")
			}
			assertFinalBundles(t, fixture.client, fixture.config, staged.material.caPEM)
			if len(mustGetStagingSecret(t, fixture.client, fixture.config).Data) != 0 {
				t.Fatal("recovery kept the staging record")
			}
		})
	}
}

func TestCATransitionContinuesWhenAThresholdIsReachedDuringIt(t *testing.T) {
	t.Parallel()

	t.Run("serving certificate enters its renewal threshold while the switch waits", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		// Three hours before the serving certificate enters its renewal
		// threshold, so it crosses it in the middle of the delay.
		start := fixture.original.leaf.NotAfter.Add(-fixture.config.RenewalThreshold - 3*time.Hour).Truncate(time.Second)
		delay := fixture.config.CASwitchDelay
		fixture.mustPass(t, start, delay)
		staged := fixture.staged(t)

		inside := start.Add(4 * time.Hour)
		state, err := inspectSecret(mustGetSecret(t, fixture.client, fixture.config), fixture.config, inside)
		if err != nil || !state.rotateServing {
			t.Fatalf("test precondition: serving certificate is not inside its threshold at %s (%v)", inside, err)
		}
		writes := fixture.writesSince(len(fixture.client.Actions()))
		fixture.mustPass(t, inside, start.Add(delay).Sub(inside))
		if got := writes(); len(got) != 0 {
			t.Fatalf("the threshold reached during the delay caused writes %v", got)
		}
		if again := fixture.staged(t); again.transitionDigest != staged.transitionDigest {
			t.Fatal("the threshold reached during the delay replaced the staged candidate")
		}
		fixture.mustPass(t, start.Add(delay), 0)
		if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), staged.material) {
			t.Fatal("the transition did not finish with its own candidate")
		}
	})

	t.Run("serving certificate expires while the switch waits", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		start := fixture.original.leaf.NotAfter.Add(-3 * time.Hour).Truncate(time.Second)
		delay := fixture.config.CASwitchDelay
		fixture.mustPass(t, start, delay)
		staged := fixture.staged(t)

		// Once nothing verifies the old certificate, admission through it has
		// stopped, and waiting out the rest of the delay only prolongs that.
		expired := fixture.original.leaf.NotAfter.Add(time.Second)
		fixture.mustPass(t, expired, 0)
		if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), staged.material) {
			t.Fatal("the switch did not happen once the old certificate expired")
		}
		if !expired.Before(start.Add(delay)) {
			t.Fatal("test precondition: the old certificate expired after the switch was due anyway")
		}
	})

	t.Run("the transition's own certificate enters the threshold before its switch", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		start := fixture.start
		fixture.mustPass(t, start, fixture.config.CASwitchDelay)
		staged := fixture.staged(t)

		// The rotator was away until the staged certificate itself needs
		// renewal. The transition still finishes first, and says so.
		late := staged.material.leaf.NotAfter.Add(-fixture.config.RenewalThreshold + time.Hour).Truncate(time.Second)
		if _, err := fixture.pass(late, fixture.prober); err == nil ||
			!strings.Contains(err.Error(), "requires immediate renewal") {
			t.Fatalf("late switch pass error = %v, want the immediate-renewal request", err)
		}
		if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), staged.material) {
			t.Fatal("the late pass did not finish the staged transition")
		}
		fixture.mustPass(t, late, 0)
		renewed := mustGetSecret(t, fixture.client, fixture.config)
		if !bytes.Equal(renewed.Data[CACertificateKey], staged.material.caPEM) ||
			bytes.Equal(renewed.Data[corev1.TLSCertKey], staged.material.certPEM) {
			t.Fatal("the renewal after the transition did not replace only the serving certificate")
		}
	})
}

func TestCATransitionSwitchTimeAtClockEdges(t *testing.T) {
	t.Parallel()

	t.Run("switch due to the nanosecond", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		delay := fixture.config.CASwitchDelay
		fixture.mustPass(t, fixture.start, delay)
		fixture.mustPass(t, fixture.start.Add(delay-time.Nanosecond), time.Nanosecond)
		fixture.assertSourceUnchanged(t)
		fixture.mustPass(t, fixture.start.Add(delay), 0)
		if len(mustGetStagingSecret(t, fixture.client, fixture.config).Data) != 0 {
			t.Fatal("the transition did not finish at its switch time")
		}
	})

	t.Run("expansion between whole seconds is dated at the next one", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		delay := fixture.config.CASwitchDelay
		started := fixture.start.Add(300 * time.Millisecond)
		recorded := fixture.start.Add(time.Second)
		fixture.mustPass(t, started, delay+700*time.Millisecond)
		if staged := fixture.staged(t); !staged.expandedAt.Equal(recorded) {
			t.Fatalf("expanded at %s, want %s", staged.expandedAt, recorded)
		}
		fixture.mustPass(t, recorded.Add(delay-time.Nanosecond), time.Nanosecond)
		fixture.assertSourceUnchanged(t)
		fixture.mustPass(t, recorded.Add(delay), 0)
	})

	t.Run("clock moved back after the expansion", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		delay := fixture.config.CASwitchDelay
		fixture.mustPass(t, fixture.start, delay)
		staged := fixture.staged(t)

		// The recorded instant now lies in the future. The dwell restarts from
		// the clock the rotator has, so it cannot stretch without limit.
		back := fixture.start.Add(-2 * time.Minute)
		fixture.mustPass(t, back, delay)
		reanchored := fixture.staged(t)
		if !reanchored.expandedAt.Equal(back) || reanchored.transitionDigest != staged.transitionDigest {
			t.Fatalf("clock moved back: staged expansion at %s, want the same transition re-dated to %s", reanchored.expandedAt, back)
		}
		fixture.mustPass(t, back.Add(delay-time.Second), time.Second)
		fixture.assertSourceUnchanged(t)
		fixture.mustPass(t, back.Add(delay), 0)
	})

	t.Run("clock moved back before the staged certificates", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		delay := fixture.config.CASwitchDelay
		fixture.mustPass(t, fixture.start, delay)
		staged := fixture.staged(t)

		// Staged certificates that are not yet valid cannot be switched to.
		// The record is cleared and the next pass stages afresh, so the new
		// candidate waits a full delay of its own.
		back := fixture.start.Add(-time.Hour)
		if _, err := fixture.pass(back, fixture.prober); err == nil ||
			!strings.Contains(err.Error(), "retry reconciliation from authoritative primary state") {
			t.Fatalf("pass before the staged certificates error = %v, want the record retired", err)
		}
		if len(mustGetStagingSecret(t, fixture.client, fixture.config).Data) != 0 {
			t.Fatal("the unusable record stayed staged")
		}
		fixture.mustPass(t, back, delay)
		restaged := fixture.staged(t)
		if restaged.transitionDigest == staged.transitionDigest || !restaged.expandedAt.Equal(back) {
			t.Fatal("the next pass did not stage and date a new transition")
		}
		fixture.assertSourceUnchanged(t)
		fixture.mustPass(t, back.Add(delay), 0)
		if !secretContainsMaterial(mustGetSecret(t, fixture.client, fixture.config), restaged.material) {
			t.Fatal("the switch did not install the new candidate")
		}
	})

	t.Run("clock jumped past the staged certificates", func(t *testing.T) {
		t.Parallel()
		fixture := newCATransitionFixture(t)
		fixture.mustPass(t, fixture.start, fixture.config.CASwitchDelay)
		staged := fixture.staged(t)
		after := staged.material.leaf.NotAfter.Add(time.Second)
		if _, err := fixture.pass(after, fixture.prober); err == nil ||
			!strings.Contains(err.Error(), "retry reconciliation from authoritative primary state") {
			t.Fatalf("pass after the staged certificates error = %v, want the record retired", err)
		}
		fixture.assertSourceUnchanged(t)
	})
}

func TestCATransitionRestartsTheDelayWhenAnEntryLostTheNewCA(t *testing.T) {
	t.Parallel()
	fixture := newCATransitionFixture(t)
	delay := fixture.config.CASwitchDelay
	fixture.mustPass(t, fixture.start, delay)
	staged := fixture.staged(t)

	// Something rewrote one entry to the old CA alone during the delay. Some
	// API server may have seen that entry without the new CA, so its dwell
	// starts again when the rotator puts the new CA back.
	setManagedBundles(t, fixture.client, fixture.config, mutatingBundle(t, fixture.client, fixture.config),
		[][]byte{fixture.original.caPEM, validatingEntryBundle(t, fixture, 1)})
	lost := fixture.start.Add(4 * time.Hour)
	fixture.mustPass(t, lost, delay)
	if restaged := fixture.staged(t); !restaged.expandedAt.Equal(lost) || restaged.transitionDigest != staged.transitionDigest {
		t.Fatalf("staged expansion at %s, want the same transition re-dated to %s", restaged.expandedAt, lost)
	}
	fixture.assertEveryEntryTrusts(t, fixture.original.caPEM, staged.material.caPEM)

	fixture.mustPass(t, fixture.start.Add(delay), lost.Add(delay).Sub(fixture.start.Add(delay)))
	fixture.assertSourceUnchanged(t)
	fixture.mustPass(t, lost.Add(delay), 0)
	assertFinalBundles(t, fixture.client, fixture.config, staged.material.caPEM)
}

func TestCATransitionSwitchesWithoutDelayWhenNothingVerifiesTheCurrentCertificate(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, now, config)
	foreign := mustGenerateMaterial(t, now, config)
	secret := secretForMaterial(config, original)
	secret.Data[CACertificateKey] = []byte("not a certificate")
	// No managed entry holds the CA that issued the serving certificate, so
	// no API server can verify it now and the switch has nothing to protect.
	client := newTestClient(config, secret, foreign.caPEM, twoReadyEndpoints(config))
	prober := &recordingProber{}

	result, err := mustNewTestRotator(t, client, config, now, prober).Run(context.Background())
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("Run() = %+v, %v; want the transition finished in one pass", result, err)
	}
	updated := mustGetSecret(t, client, config)
	if bytes.Equal(updated.Data[corev1.TLSCertKey], original.certPEM) {
		t.Fatal("the serving certificate was not replaced")
	}
	assertFinalBundles(t, client, config, updated.Data[CACertificateKey])
	for _, request := range prober.probeRequests() {
		if request.IdentityOnly {
			t.Fatal("the rotator proved a CA it could not have recovered")
		}
	}
}

func TestStillServingCountsOnlyExpiry(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	material := mustGenerateMaterial(t, now, config)
	// stillServing takes the leaf and bundle as already proved to belong
	// together, so an unrelated short-lived CA stands in for one that expires
	// before the leaf does.
	shortLived := config
	shortLived.ServingCertificateValidity = time.Hour
	shortLived.CACertificateValidity = 2 * time.Hour
	short := mustGenerateMaterial(t, now, shortLived)
	afterShort := now.Add(3 * time.Hour)
	tests := []struct {
		name   string
		leaf   *certificateMaterial
		bundle []byte
		at     time.Time
		want   bool
	}{
		{name: "valid leaf and CA", leaf: &material, bundle: material.caPEM, at: now, want: true},
		{name: "only CA expired", leaf: &material, bundle: short.caPEM, at: afterShort},
		{name: "one CA of several still valid", leaf: &material, bundle: mustCombine(t, short.caPEM, material.caPEM), at: afterShort, want: true},
		{name: "leaf expired", leaf: &material, bundle: material.caPEM, at: material.leaf.NotAfter},
		{name: "leaf one nanosecond from expiry", leaf: &material, bundle: material.caPEM, at: material.leaf.NotAfter.Add(-time.Nanosecond), want: true},
		// A leaf that looks not yet valid means this clock runs behind the one
		// that issued it; that must not shorten the delay.
		{name: "leaf not yet valid by this clock", leaf: &material, bundle: material.caPEM, at: material.leaf.NotBefore.Add(-time.Hour), want: true},
		{name: "no CA known", leaf: &material, at: now},
		{name: "no leaf", bundle: material.caPEM, at: now},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var leaf *x509.Certificate
			if test.leaf != nil {
				leaf = test.leaf.leaf
			}
			got, err := stillServing(leaf, test.bundle, test.at)
			if err != nil {
				t.Fatalf("stillServing() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("stillServing() = %v, want %v", got, test.want)
			}
		})
	}
}

// caTransitionFixture holds a generated Secret whose CA private key is gone:
// a state that starts a CA transition while the serving certificate still
// verifies, so the switch has to wait out the delay.
type caTransitionFixture struct {
	client     *fake.Clientset
	config     Config
	original   certificateMaterial
	sourceData map[string][]byte
	start      time.Time
	prober     *recordingProber
}

func newCATransitionFixture(t *testing.T) *caTransitionFixture {
	t.Helper()
	config := testConfig()
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	original := mustGenerateMaterial(t, start, config)
	keyless := secretForMaterial(config, original)
	delete(keyless.Data, CAPrivateKeyKey)
	return &caTransitionFixture{
		client:     newTestClient(config, keyless, original.caPEM, twoReadyEndpoints(config)),
		config:     config,
		original:   original,
		sourceData: cloneBytesMap(keyless.Data),
		start:      start,
		prober:     &recordingProber{},
	}
}

// pass runs one reconciliation at now with a fresh Rotator.
func (fixture *caTransitionFixture) pass(now time.Time, prober *recordingProber) (Result, error) {
	rotator, err := New(fixture.client, fixture.config)
	if err != nil {
		return Result{}, err
	}
	rotator.now = func() time.Time { return now }
	rotator.probe = prober
	return rotator.Run(context.Background())
}

// mustPass runs one successful pass at now and requires it to ask for the
// next one after exactly requeue (zero when nothing waits).
func (fixture *caTransitionFixture) mustPass(t *testing.T, now time.Time, requeue time.Duration) {
	t.Helper()
	result, err := fixture.pass(now, fixture.prober)
	if err != nil {
		t.Fatalf("pass at %s error = %v", now, err)
	}
	if result.RequeueAfter != requeue {
		t.Fatalf("pass at %s RequeueAfter = %s, want %s", now, result.RequeueAfter, requeue)
	}
}

func (fixture *caTransitionFixture) staged(t *testing.T) *pendingCandidate {
	t.Helper()
	pending, err := decodePendingCandidate(mustGetStagingSecret(t, fixture.client, fixture.config).Data, fixture.config)
	if err != nil {
		t.Fatalf("decode the staged transition: %v", err)
	}
	return pending
}

func (fixture *caTransitionFixture) assertSourceUnchanged(t *testing.T) {
	t.Helper()
	if !maps.EqualFunc(mustGetSecret(t, fixture.client, fixture.config).Data, fixture.sourceData, bytes.Equal) {
		t.Fatal("the generated Secret switched before the switch time")
	}
}

func (fixture *caTransitionFixture) assertEveryEntryTrusts(t *testing.T, certificates ...[]byte) {
	t.Helper()
	entries := managedEntryBundles(t, fixture.client, fixture.config)
	if len(entries) == 0 {
		t.Fatal("no managed webhook entry was read")
	}
	for index, bundle := range entries {
		for _, certificate := range certificates {
			if !caBundleContainsCertificate(bundle, certificate) {
				t.Fatalf("managed entry %d lacks a CA it must trust during the delay", index)
			}
		}
		assertBundleCertificateCount(t, bundle, len(certificates))
	}
}

// writesSince returns a function listing every write after the given action
// index, the Lease's own renewals aside.
func (fixture *caTransitionFixture) writesSince(index int) func() []string {
	return func() []string {
		var writes []string
		for _, action := range fixture.client.Actions()[index:] {
			verb := action.GetVerb()
			if (verb != "update" && verb != "create" && verb != "patch" && verb != "delete") ||
				action.GetResource().Resource == "leases" {
				continue
			}
			writes = append(writes, verb+" "+action.GetResource().Resource)
		}
		return writes
	}
}

// failOnce makes the first matching write fail as a lost request would.
func (fixture *caTransitionFixture) failOnce(verb, resource string, match func(*corev1.Secret) bool) {
	var once sync.Once
	fixture.client.PrependReactor(verb, resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if match != nil {
			secret, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			if !ok || !match(secret) {
				return false, nil, nil
			}
		}
		failed := false
		once.Do(func() { failed = true })
		if failed {
			return true, nil, fmt.Errorf("injected %s %s failure", verb, resource)
		}
		return false, nil, nil
	})
}

type writeTrace struct {
	mu     sync.Mutex
	writes []string
}

func (trace *writeTrace) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return slices.Clone(trace.writes)
}

// traceWrites records every later write to the Secrets and webhook
// configurations, in order, by resource and name.
func (fixture *caTransitionFixture) traceWrites() *writeTrace {
	trace := &writeTrace{}
	for _, resource := range []string{"secrets", "mutatingwebhookconfigurations", "validatingwebhookconfigurations"} {
		fixture.client.PrependReactor("*", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
			if verb := action.GetVerb(); verb != "update" && verb != "create" {
				return false, nil, nil
			}
			written, ok := action.(interface{ GetObject() runtime.Object })
			if !ok {
				return false, nil, nil
			}
			name := written.GetObject().(metav1.Object).GetName()
			trace.mu.Lock()
			trace.writes = append(trace.writes, action.GetVerb()+" "+resource+"/"+name)
			trace.mu.Unlock()
			return false, nil, nil
		})
	}
	return trace
}

func validatingEntryBundle(t *testing.T, fixture *caTransitionFixture, index int) []byte {
	t.Helper()
	configuration, err := fixture.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		context.Background(), fixture.config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	name := fixture.config.ValidatingWebhookNames[index]
	for _, webhook := range configuration.Webhooks {
		if webhook.Name == name {
			return webhook.ClientConfig.CABundle
		}
	}
	t.Fatalf("validating webhook %q not found", name)
	return nil
}

func mustCombine(t *testing.T, bundles ...[]byte) []byte {
	t.Helper()
	combined, err := combineCABundles(bundles...)
	if err != nil {
		t.Fatalf("combine CA bundles: %v", err)
	}
	return combined
}
