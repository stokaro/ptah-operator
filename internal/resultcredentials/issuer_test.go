package resultcredentials

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

type credentialAPI struct {
	client.Client
	creates atomic.Int64
	lostACK atomic.Bool
}

func (c *credentialAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	obj.SetUID(types.UID(fmt.Sprintf("credential-%d", c.creates.Add(1))))
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if c.lostACK.Swap(false) {
		return errors.New("API write response lost")
	}
	return nil
}

func testCA(t *testing.T, validFor time.Duration) (tls.Certificate, *x509.CertPool, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "result credentials test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(validFor), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testIssuer(t *testing.T, api client.Client) (*Issuer, tls.Certificate, *x509.CertPool, []byte) {
	t.Helper()
	ca, roots, trust := testCA(t, 7*24*time.Hour)
	issuer, err := New(api, api, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	return issuer, ca, roots, trust
}

func getCredential(t *testing.T, c client.Reader, f *resulttest.Fixture) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Identity.Binding.Namespace, Name: jobconfig.CredentialName(f.Identity.Binding.UID, f.Identity.Binding.OperationID, f.Identity.Binding.JobName)}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIssueAndReadBackAllOperationCredentials(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			api := &credentialAPI{Client: f.Client(t)}
			issuer, _, _, _ := testIssuer(t, api)
			first, err := issuer.Ensure(t.Context(), f.Identity)
			if err != nil {
				t.Fatal(err)
			}
			secret := getCredential(t, api, f)
			cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
			if err != nil {
				t.Fatal(err)
			}
			identity, err := resultdelivery.ClientIdentity(cert)
			if err != nil || identity != f.Identity {
				t.Fatalf("issued identity=%#v, %v", identity, err)
			}
			if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != f.Identity.Binding.UID || secret.OwnerReferences[0].Kind != f.Identity.Binding.Kind {
				t.Fatal("credential is not owned by the persistent operation resource")
			}
			repeated, err := issuer.Ensure(t.Context(), f.Identity)
			if err != nil || first != repeated || api.creates.Load() != 1 {
				t.Fatalf("retry changed credential: %#v %#v %v creates=%d", first, repeated, err, api.creates.Load())
			}
		})
	}
}

func TestLostCreateResponseAndConcurrentIssuers(t *testing.T) {
	f := resulttest.New(t, "migration-apply-admitted-scheduling")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, api)
	api.lostACK.Store(true)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err == nil {
		t.Fatal("lost API response returned a receipt")
	}
	secret := getCredential(t, api, f)
	recovered, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil || recovered.UID != secret.UID || api.creates.Load() != 1 {
		t.Fatalf("recovery replaced the first credential: %#v, %v", recovered, err)
	}
	t.Run("concurrent first publication", func(t *testing.T) {
		f := resulttest.New(t, "schema-observe")
		api := &credentialAPI{Client: f.Client(t)}
		issuer, _, _, _ := testIssuer(t, api)
		var wg sync.WaitGroup
		results := make(chan Credential, 8)
		failures := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				result, err := issuer.Ensure(t.Context(), f.Identity)
				if err != nil {
					failures <- err
				} else {
					results <- result
				}
			})
		}
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		var first Credential
		count := 0
		for result := range results {
			count++
			if first.UID == "" {
				first = result
			} else if first != result {
				t.Errorf("issuers returned different credentials: %#v, %#v", first, result)
			}
		}
		if count != 8 {
			t.Fatalf("only %d issuers succeeded", count)
		}
	})
}

func TestReplacementPodCannotRemintAttemptCredential(t *testing.T) {
	f := resulttest.New(t, "migration-apply-admitted-scheduling")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, api)
	first, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	original := getCredential(t, api, f)
	if err := api.Delete(t.Context(), f.Pod); err != nil {
		t.Fatal(err)
	}
	replacement := f.Pod.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-pod"
	replacement.Name = f.Job.Name + "-fghij"
	if err := api.Client.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	identity := f.Identity
	identity.Binding.PodUID = replacement.UID
	identity.Binding.PodName = replacement.Name
	if _, err := issuer.Ensure(t.Context(), identity); !errors.Is(err, ErrCredential) {
		t.Fatalf("replacement minted another credential: %v", err)
	}
	retained := getCredential(t, api, f)
	if retained.UID != first.UID || !bytes.Equal(original.Data["tls.crt"], retained.Data["tls.crt"]) || api.creates.Load() != 1 {
		t.Fatal("replacement changed the reserved credential")
	}
}

func TestOldAttemptCannotAcquireNewGeneration(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	f.Subject.SetGeneration(3)
	f.Identity.Binding.Generation = 3
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, _, _ := testIssuer(t, api)
	if _, err := issuer.Ensure(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) || api.creates.Load() != 0 {
		t.Fatalf("old template received current-generation authority: %v", err)
	}
}

func TestIssuerRejectsCorruptedOrForeignCredential(t *testing.T) {
	changes := map[string]func(*corev1.Secret){
		"mutable":              func(s *corev1.Secret) { s.Immutable = ptr.To(false) },
		"wrong owner":          func(s *corev1.Secret) { s.OwnerReferences[0].UID = "other" },
		"wrong type":           func(s *corev1.Secret) { s.Type = corev1.SecretTypeOpaque },
		"invalid key":          func(s *corev1.Secret) { s.Data["tls.key"] = []byte("invalid") },
		"changed server trust": func(s *corev1.Secret) { s.Data["ca.crt"] = []byte("invalid") },
		"unknown data":         func(s *corev1.Secret) { s.Data["extra"] = []byte("extra") },
		"changed metadata":     func(s *corev1.Secret) { s.Labels["app.kubernetes.io/component"] = "other" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, "schema-plan-dev-fence-scheduling")
			api := &credentialAPI{Client: f.Client(t)}
			issuer, _, _, _ := testIssuer(t, api)
			if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
				t.Fatal(err)
			}
			secret := getCredential(t, api, f)
			change(secret)
			// Fake-client corruption models a persisted bad object, not an API claim
			// that immutable Secret data can be updated on a real cluster.
			if err := api.Update(t.Context(), secret); err != nil {
				t.Fatal(err)
			}
			if _, err := issuer.Ensure(t.Context(), f.Identity); !errors.Is(err, ErrCredential) || api.creates.Load() != 1 {
				t.Fatalf("corrupt credential was repaired or accepted: %v", err)
			}
		})
	}
}

func TestSignerRotationReusesCredentialUnderOverlap(t *testing.T) {
	f := resulttest.New(t, "schema-resolve")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, _, roots, serverTrust := testIssuer(t, api)
	first, err := issuer.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	nextCA, nextRoots, _ := testCA(t, 7*24*time.Hour)
	nextParsed, err := x509.ParseCertificate(nextCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(nextParsed)
	rotated, err := New(api, api, nextCA, roots, serverTrust)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := rotated.Ensure(t.Context(), f.Identity)
	if err != nil || reused != first || api.creates.Load() != 1 {
		t.Fatalf("overlap replaced credential: %#v %v", reused, err)
	}
	removed, err := New(api, api, nextCA, nextRoots, serverTrust)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removed.Ensure(t.Context(), f.Identity); !errors.Is(err, ErrCredential) {
		t.Fatalf("removed client trust still accepted old credential: %v", err)
	}
}

func TestInsufficientSignerLifetimeRefusesBeforeCreation(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	api := &credentialAPI{Client: f.Client(t)}
	ca, roots, trust := testCA(t, time.Minute)
	issuer, err := New(api, api, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Ensure(t.Context(), f.Identity); !errors.Is(err, ErrCredential) || api.creates.Load() != 0 {
		t.Fatalf("short signer issued an unusable credential: %v", err)
	}
}

type secretReadFailure struct {
	client.Reader
	creates     *atomic.Int64
	unavailable error
}

func (r secretReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok && r.creates.Load() > 0 {
		return r.unavailable
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestCredentialRequiresDirectReadBack(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	api := &credentialAPI{Client: f.Client(t)}
	ca, roots, trust := testCA(t, 7*24*time.Hour)
	unavailable := errors.New("credential readback unavailable")
	issuer, err := New(api, secretReadFailure{Reader: api, creates: &api.creates, unavailable: unavailable}, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Ensure(t.Context(), f.Identity); !errors.Is(err, unavailable) {
		t.Fatalf("unreadable write returned success: %v", err)
	}
	secret := getCredential(t, api, f)
	recovered, err := New(api, api, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := recovered.Ensure(t.Context(), f.Identity)
	if err != nil || receipt.UID != secret.UID || api.creates.Load() != 1 {
		t.Fatalf("readback retry replaced credential: %#v %v", receipt, err)
	}
}

func TestIssuedCredentialDeliversThroughLiveAuthority(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	api := &credentialAPI{Client: f.Client(t)}
	issuer, ca, roots, _ := testIssuer(t, api)
	if _, err := issuer.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	secret := getCredential(t, api, f)
	clientCert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &serverKey.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	store := resultstore.Store{Client: api, Reader: api}
	receiver, err := resultdelivery.NewReceiver(resultdelivery.ReceiverConfig{Store: store, Authorize: resultauthority.Authorizer{Reader: api}.Check, MaxConcurrent: 1, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server, err := receiver.Server(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: serverKey}, roots)
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewUnstartedServer(server.Handler)
	local.TLS = server.TLSConfig
	local.StartTLS()
	defer local.Close()
	sender, err := resultdelivery.NewSender(local.URL, f.Identity, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}}, resultdelivery.RetryPolicy{Attempts: 1, Interval: time.Millisecond, AttemptTimeout: 3 * time.Second, TotalTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	payload, err := resultdelivery.Encode(f.Identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "execution refused"}})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	stored, receipt, err := store.Load(t.Context(), f.Identity.Binding)
	if err != nil || receipt != delivered || !bytes.Equal(stored, payload) {
		t.Fatalf("issued credential did not persist exact bytes: %#v %v", receipt, err)
	}
	f.Subject.SetGeneration(f.Subject.GetGeneration() + 1)
	if err := api.Update(t.Context(), f.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(t.Context(), payload); err == nil {
		t.Fatal("old credential remained authorized after generation change")
	}
}

type retiringWriter struct {
	client.Client
	retire func() error
}

func (w retiringWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := w.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	return w.retire()
}

func TestAuthorityChangeAtCredentialWriteReturnsNoReceipt(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	api := &credentialAPI{Client: f.Client(t)}
	ca, roots, trust := testCA(t, 7*24*time.Hour)
	writer := retiringWriter{Client: api, retire: func() error { f.Subject.SetGeneration(3); return api.Update(t.Context(), f.Subject) }}
	issuer, err := New(writer, api, ca, roots, trust)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := issuer.Ensure(t.Context(), f.Identity)
	if !errors.Is(err, resultdelivery.ErrAuthority) || receipt.UID != "" {
		t.Fatalf("retired authority returned a credential receipt: %#v %v", receipt, err)
	}
	// A cross-object change can race creation. The Secret is evidence of that
	// race, not authority: neither a successful issuance receipt nor receiver
	// authorization survives the changed generation.
	_ = getCredential(t, api, f)
	if err := (resultauthority.Authorizer{Reader: api}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("retired identity authorizes delivery: %v", err)
	}
}
