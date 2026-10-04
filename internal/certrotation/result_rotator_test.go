package certrotation

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type resultRotationFixture struct {
	r      *ResultRotator
	api    *fake.Clientset
	clock  time.Time
	probes int
}

func newResultRotationFixture(t *testing.T) *resultRotationFixture {
	t.Helper()
	config := ResultConfig{Config: Config{Namespace: "system", ReleaseName: "ptah", SecretName: "results", StagingSecretName: "result-journal", LeaseName: "result-rotation", ServiceName: "results", ServiceNamespace: "system", EndpointPortName: "https", HolderIdentity: "rotator/pod-uid", RenewalThreshold: 10 * 24 * time.Hour, ServingCertificateValidity: 30 * 24 * time.Hour, CACertificateValidity: 90 * 24 * time.Hour, ProbeTimeout: time.Second, ProbeInterval: time.Millisecond, LeaseDuration: 30 * time.Second, AcquireTimeout: 50 * time.Millisecond}, PolicyName: "result-enrollment"}
	meta := func(name, role string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: config.Namespace, Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "1", Labels: map[string]string{ResultTrustLabel: role, HelmManagedByLabel: HelmManagedByLabelValue}, Annotations: helmOwnershipAnnotations(config.Config)}
	}
	api := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: meta(config.SecretName, "projection"), Type: corev1.SecretTypeOpaque}, &corev1.Secret{ObjectMeta: meta(config.StagingSecretName, "journal"), Type: corev1.SecretTypeOpaque}, &corev1.ConfigMap{ObjectMeta: meta(config.PolicyName, "enrollment")}, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: config.Namespace, Name: config.LeaseName, UID: "lease-uid", ResourceVersion: "1"}})
	r, err := NewResultRotator(api, config)
	if err != nil {
		t.Fatal(err)
	}
	f := &resultRotationFixture{r: r, api: api, clock: time.Now().UTC().Truncate(time.Second)}
	f.bind()
	return f
}
func (f *resultRotationFixture) bind() {
	f.r.now = func() time.Time { return f.clock }
	f.r.probe = func(ctx context.Context, p map[string][]byte, cas []certificateMaterial) error {
		f.probes++
		if len(p) != 6 || len(cas) == 0 {
			return errors.New("empty probe")
		}
		leaf, err := parseLeafAndKey(p["tls.crt"], p["tls.key"])
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(p["ca.crt"])
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: f.clock, DNSName: "results.system.svc", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return err
		}
		signer, _, err := parseSingleCertificate(p["client-ca.crt"])
		if err != nil || !certificateCurrentlyValid(signer, f.clock) {
			return errors.New("projected client signer is expired")
		}
		return ctx.Err()
	}
}
func (f *resultRotationFixture) restart(t *testing.T) {
	t.Helper()
	r, err := NewResultRotator(f.api, f.r.config)
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	f.bind()
}
func (f *resultRotationFixture) step(t *testing.T) Result {
	t.Helper()
	result, err := f.r.step(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *resultRotationFixture) state(t *testing.T) resultJournal {
	t.Helper()
	s, err := f.api.CoreV1().Secrets("system").Get(t.Context(), "result-journal", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.r.decodeJournal(s.Data)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func (f *resultRotationFixture) bootstrap(t *testing.T) {
	t.Helper()
	f.step(t)
	f.step(t)
	if f.state(t).Phase != "stable" {
		t.Fatal("bootstrap did not converge")
	}
}
func (f *resultRotationFixture) beginCA(t *testing.T) {
	t.Helper()
	f.clock = f.clock.Add(81 * 24 * time.Hour)
	f.step(t)
	if f.state(t).Phase == "leaf" {
		f.step(t)
		f.step(t)
	}
	if f.state(t).Phase != "prepare" {
		t.Fatal("CA rotation did not start")
	}
}

func TestResultRotationPersistsBothWaitsAndResumes(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	original := f.state(t).Current
	if f.step(t).RequeueAfter != 0 {
		t.Fatal("healthy trust rotated")
	}
	f.beginCA(t)
	candidate := f.state(t).Next
	f.step(t)
	first := f.state(t)
	if first.FencedAt == nil {
		t.Fatal("first fence was not persisted")
	}
	if !first.FencedAt.Equal(f.clock) {
		t.Fatal("fence timestamp is not the readback time")
	}
	f.restart(t)
	result := f.step(t)
	if result.RequeueAfter != resultcredentials.MaxCredentialLifetime+maximumCertificatePolicyClockSkew {
		t.Fatalf("short retirement wait: %s", result.RequeueAfter)
	}
	projection, _ := f.api.CoreV1().Secrets("system").Get(t.Context(), "results", metav1.GetOptions{})
	if !bytes.Equal(projection.Data["client-ca.crt"], original.ClientCA) || bytes.Equal(projection.Data["ca.crt"], original.ServerCA) {
		t.Fatal("prepare changed signer or failed to expand trust")
	}
	f.clock = f.clock.Add(result.RequeueAfter - time.Nanosecond)
	if got := f.step(t); got.RequeueAfter != time.Nanosecond || f.state(t).Phase != "prepare" {
		t.Fatal("old server trust retired early")
	}
	f.clock = f.clock.Add(time.Nanosecond)
	f.step(t)
	if f.state(t).Phase != "switch" || f.state(t).FencedAt != nil {
		t.Fatal("switch reused first wait")
	}
	f.step(t)
	second := f.state(t)
	if !second.FencedAt.After(*first.FencedAt) {
		t.Fatal("second wait did not start independently")
	}
	f.restart(t)
	result = f.step(t)
	if result.RequeueAfter != resultcredentials.MaxCredentialLifetime+maximumCertificatePolicyClockSkew {
		t.Fatal("second wait shortened")
	}
	projection, _ = f.api.CoreV1().Secrets("system").Get(t.Context(), "results", metav1.GetOptions{})
	if !bytes.Equal(projection.Data["client-ca.crt"], candidate.ClientCA) || !bytes.Contains(projection.Data["client-trust.crt"], original.ClientCA) {
		t.Fatal("switch lost old client trust or retained old signer")
	}
	f.clock = f.clock.Add(result.RequeueAfter)
	f.step(t)
	f.restart(t)
	f.step(t)
	final := f.state(t)
	if final.Phase != "stable" || final.Next != nil || final.FencedAt != nil || !equalBytes(final.Current.projection(), candidate.projection()) {
		t.Fatal("rotation changed candidate or retained private staging")
	}
	if f.probes < 5 {
		t.Fatal("transition skipped endpoint verification")
	}
}
func TestResultRotationFenceStartsAfterDelayedPolicyReadback(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	f.beginCA(t)
	before := f.clock
	f.api.PrependReactor("update", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		f.clock = f.clock.Add(3 * time.Hour)
		return false, nil, nil
	})
	f.step(t)
	st := f.state(t)
	if !st.FencedAt.Equal(before.Add(3 * time.Hour)) {
		t.Fatal("policy write latency shortened the fence")
	}
	if remaining := f.r.retirementDeadline(st).Sub(f.clock); remaining != resultcredentials.MaxCredentialLifetime+maximumCertificatePolicyClockSkew {
		t.Fatalf("wait is %s", remaining)
	}
}
func TestResultRotationResumesUncertainWrites(t *testing.T) {
	for _, target := range []string{"result-journal", "result-enrollment", "results"} {
		t.Run(target, func(t *testing.T) {
			f := newResultRotationFixture(t)
			f.bootstrap(t)
			f.beginCA(t)
			if target == "results" {
				f.step(t)
			}
			before := f.state(t)
			expected := before.Next.projection()
			lost, denyRead := false, false
			f.api.PrependReactor("update", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.UpdateAction).GetObject()
				named := obj.(metav1.Object)
				if named.GetName() != target || lost {
					return false, nil, nil
				}
				lost, denyRead = true, true
				if err := f.api.Tracker().Update(action.GetResource(), obj, action.GetNamespace()); err != nil {
					t.Fatal(err)
				}
				return true, nil, errors.New("lost write response")
			})
			f.api.PrependReactor("get", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				if denyRead && action.(ktesting.GetAction).GetName() == target {
					denyRead = false
					return true, nil, errors.New("readback unavailable")
				}
				return false, nil, nil
			})
			if _, err := f.r.step(t.Context()); err == nil || !lost {
				t.Fatal("uncertain write was reported complete")
			}
			f.clock = f.clock.Add(time.Minute)
			f.restart(t)
			f.step(t)
			after := f.state(t)
			if after.Next == nil || !equalBytes(after.Next.projection(), expected) {
				t.Fatal("restart regenerated pending trust")
			}
			if before.FencedAt != nil && !after.FencedAt.Equal(*before.FencedAt) {
				t.Fatal("restart rewrote a persisted wait")
			}
		})
	}
}
func TestResultRotationRefusesPolicyRollbackDuringWait(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	f.beginCA(t)
	f.step(t)
	st := f.state(t)
	policy, _ := f.api.CoreV1().ConfigMaps("system").Get(t.Context(), "result-enrollment", metav1.GetOptions{})
	policy.Data = enrollment(st.Current.projection())
	if _, err := f.api.CoreV1().ConfigMaps("system").Update(t.Context(), policy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.step(t.Context()); err == nil {
		t.Fatal("rolled-back policy reused the old wait")
	}
	got, _ := f.api.CoreV1().ConfigMaps("system").Get(t.Context(), "result-enrollment", metav1.GetOptions{})
	if !maps.Equal(got.Data, policy.Data) {
		t.Fatal("refusal overwrote foreign policy")
	}
}
func TestResultRotationRenewsLeafWithoutChangingAuthorities(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	before := f.state(t).Current
	f.clock = f.clock.Add(21 * 24 * time.Hour)
	f.step(t)
	if f.state(t).Phase != "leaf" {
		t.Fatal("leaf renewal started CA transition")
	}
	f.restart(t)
	f.step(t)
	after := f.state(t).Current
	if bytes.Equal(after.ServerCertificate, before.ServerCertificate) || !bytes.Equal(after.ServerCA, before.ServerCA) || !bytes.Equal(after.ClientCA, before.ClientCA) {
		t.Fatal("leaf rotation changed authorities or kept old leaf")
	}
}
func TestResultRotationRecoversExpiredAuthorities(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	f.clock = f.clock.Add(91 * 24 * time.Hour)
	for range 6 {
		f.step(t)
	}
	if f.state(t).Phase != "stable" {
		t.Fatalf("expired authorities prevented recovery: %s", f.state(t).Phase)
	}
	server, client, err := f.r.inspectKeys(f.state(t).Current)
	if err != nil || !certificateCurrentlyValid(server.ca, f.clock) || !certificateCurrentlyValid(client.ca, f.clock) {
		t.Fatal("recovery retained expired authorities")
	}
}
func TestResultRotationRefusesLostJournalOrReplacedObjects(t *testing.T) {
	for _, scenario := range []string{"lost journal", "replaced projection", "replaced policy", "foreign labels", "foreign projection", "malformed journal"} {
		t.Run(scenario, func(t *testing.T) {
			f := newResultRotationFixture(t)
			f.bootstrap(t)
			switch scenario {
			case "replaced policy":
				p, _ := f.api.CoreV1().ConfigMaps("system").Get(t.Context(), "result-enrollment", metav1.GetOptions{})
				p.UID = "replacement"
				f.api.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), p, "system")
			default:
				name := "results"
				if scenario == "lost journal" || scenario == "malformed journal" {
					name = "result-journal"
				}
				s, _ := f.api.CoreV1().Secrets("system").Get(t.Context(), name, metav1.GetOptions{})
				switch scenario {
				case "lost journal":
					s.Data = nil
				case "malformed journal":
					s.Data = map[string][]byte{resultJournalKey: []byte(`{"version":1}`)}
				case "foreign labels":
					s.Labels["foreign"] = "true"
				case "foreign projection":
					s.Data["tls.key"] = []byte("foreign")
				case "replaced projection":
					s.UID = "replacement"
				}
				f.api.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), s, "system")
			}
			if _, err := f.r.step(t.Context()); err == nil {
				t.Fatal("unsafe rotation state accepted")
			}
		})
	}
}
func TestResultRotatorRunReleasesLease(t *testing.T) {
	f := newResultRotationFixture(t)
	if _, err := f.r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease, err := f.api.CoordinationV1().Leases("system").Get(t.Context(), "result-rotation", metav1.GetOptions{})
	if err != nil || lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		t.Fatal("rotation leaked its Lease")
	}
	if f.state(t).Phase != "bootstrap" {
		t.Fatal("Run did not persist bootstrap material")
	}
}

// A valid leaf can expire inside the first overlap wait. Renew it under the
// old CA first; otherwise the endpoint probe can strand the CA transition.
func TestResultRotationRepairsLeafBeforeOverlapWait(t *testing.T) {
	f := newResultRotationFixture(t)
	f.bootstrap(t)
	f.beginCA(t)
	// Return to a stable record with a valid leaf that will expire in one hour.
	st := f.state(t)
	current, _, err := f.r.inspectKeys(st.Current)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := generateServingMaterialForService(f.r.random, f.clock, time.Hour, f.r.config.ServiceName, f.r.config.Namespace, current)
	if err != nil {
		t.Fatal(err)
	}
	st.Current.ServerCertificate, st.Current.ServerCertificateKey = leaf.certPEM, leaf.keyPEM
	st.Next, st.Phase = nil, "stable"
	journal, projection, policy, err := f.r.objects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.saveJournal(t.Context(), journal, st); err != nil {
		t.Fatal(err)
	}
	if err := f.r.writeProjection(t.Context(), projection, st.Current.projection()); err != nil {
		t.Fatal(err)
	}
	if err := f.r.writePolicy(t.Context(), policy, enrollment(st.Current.projection())); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	pending := f.state(t)
	if pending.Phase != "leaf" || !bytes.Equal(pending.Next.ServerCA, st.Current.ServerCA) {
		t.Fatal("CA transition started without extending the short-lived leaf")
	}
	repaired, _, err := f.r.inspectKeys(*pending.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.leaf.NotAfter.After(f.clock.Add(resultcredentials.MaxCredentialLifetime + maximumCertificatePolicyClockSkew)) {
		t.Fatal("replacement leaf cannot survive the first wait")
	}
}
