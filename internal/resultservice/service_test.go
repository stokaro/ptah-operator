package resultservice

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/runner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type authority struct {
	cert            *x509.Certificate
	key             *ecdsa.PrivateKey
	certPEM, keyPEM []byte
}

func makeAuthority(t *testing.T, name string) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(7 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return authority{cert: parsed, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})}
}
func mountedTrust(t *testing.T) Config {
	t.Helper()
	directory := t.TempDir()
	serverCA := makeAuthority(t, "server CA")
	clientCA := makeAuthority(t, "client CA")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, serverCA.cert, &key.PublicKey, serverCA.key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), "ca.crt": serverCA.certPEM, "client-ca.crt": clientCA.certPEM, "client-ca.key": clientCA.keyPEM, "client-trust.crt": clientCA.certPEM} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Config{Endpoint: "https://127.0.0.1", Address: "127.0.0.1:0", CertificateDirectory: directory, Uploads: 1, UploadTimeout: time.Second, Consumer: resultconsumer.Options{Workers: 1, Entries: 2, Timeout: time.Second, Retention: time.Minute}}
}

type identifyingAPI struct {
	client.Client
	sequence atomic.Int64
}

func (c *identifyingAPI) Create(ctx context.Context, obj client.Object, options ...client.CreateOption) error {
	obj.SetUID(types.UID(fmt.Sprintf("created-%d", c.sequence.Add(1))))
	return c.Client.Create(ctx, obj, options...)
}

type noSecrets struct{ client.Reader }

func (r noSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return errors.New("Secret GET forbidden")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func running(t *testing.T, s *Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for s.Ready(nil) != nil {
		select {
		case err := <-done:
			t.Fatalf("service stopped before readiness: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("service not ready")
		}
		time.Sleep(time.Millisecond)
	}
	var stopped atomic.Bool
	stop := func() {
		if !stopped.CompareAndSwap(false, true) {
			return
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("service did not stop")
		}
		if s.Ready(nil) == nil {
			t.Error("stopped service is ready")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestServiceIssuesDeliversAndConsumesAcrossRestart(t *testing.T) {
	config := mountedTrust(t)
	f := resulttest.New(t, "schema-observe")
	api := &identifyingAPI{Client: f.Client(t)}
	s, err := New(config, api, noSecrets{Reader: api})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ready(nil) == nil {
		t.Fatal("unstarted service is ready")
	}
	if s.NeedLeaderElection() {
		t.Fatal("service cannot follow leader changes")
	}
	stop := running(t, s)
	issued, err := s.Issuer.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := api.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: issued.Name}, secret); err != nil {
		t.Fatal(err)
	}
	credential, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
		t.Fatal("missing server trust")
	}
	sender, err := resultdelivery.NewSender("https://"+s.address, f.Identity, &tls.Config{Certificates: []tls.Certificate{credential}, RootCAs: roots}, resultdelivery.RetryPolicy{Attempts: 2, Interval: time.Millisecond, AttemptTimeout: time.Second, TotalTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if err := sender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	value := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: f.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}}
	payload, err := resultdelivery.Encode(f.Identity, value)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256(payload)) {
		t.Fatal("receipt digest changed")
	}
	stop()
	next, err := New(config, api, noSecrets{Reader: api})
	if err != nil {
		t.Fatal(err)
	}
	running(t, next)
	if repeated, err := next.Issuer.Ensure(t.Context(), f.Identity); err != nil || repeated != issued {
		t.Fatalf("replica changed canonical credential: %v", err)
	}
	if err := api.Delete(t.Context(), f.Pod); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(t.Context(), f.Job); err != nil {
		t.Fatal(err)
	}
	b := f.Identity.Binding
	request := resultconsumer.Request{Namespace: b.Namespace, Kind: b.Kind, Name: b.Name, UID: b.UID, Generation: b.Generation, ExecutionBindingID: b.ExecutionBindingID, InputFingerprint: b.InputFingerprint, Operation: b.Operation, OperationID: b.OperationID, JobName: b.JobName, JobUID: b.JobUID, Engine: f.Identity.Engine}
	deadline := time.Now().Add(3 * time.Second)
	for {
		loaded, err := next.Consumer.Poll(t.Context(), request)
		if errors.Is(err, resultconsumer.ErrPending) || errors.Is(err, resultconsumer.ErrStopped) {
			if time.Now().After(deadline) {
				t.Fatal("consumer not ready")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil || loaded.Receipt != receipt || loaded.Value.Error == nil || loaded.Value.Error.Code != "refused" {
			t.Fatalf("restarted service lost result: %v", err)
		}
		break
	}
}

func TestServiceRefusesIncompleteOrForeignTrust(t *testing.T) {
	for _, scenario := range []string{"missing server key", "oversized signer", "foreign client trust", "wrong server name", "server CA as leaf", "partial options"} {
		t.Run(scenario, func(t *testing.T) {
			config := mountedTrust(t)
			f := resulttest.New(t, "schema-observe")
			api := f.Client(t)
			switch scenario {
			case "missing server key":
				if err := os.Remove(filepath.Join(config.CertificateDirectory, "tls.key")); err != nil {
					t.Fatal(err)
				}
			case "oversized signer":
				if err := os.WriteFile(filepath.Join(config.CertificateDirectory, "client-ca.key"), make([]byte, 64<<10+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign client trust":
				other := makeAuthority(t, "foreign")
				if err := os.WriteFile(filepath.Join(config.CertificateDirectory, "client-trust.crt"), other.certPEM, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong server name":
				config.Endpoint = "https://foreign.example"
			case "server CA as leaf":
				ca := makeAuthority(t, "misused CA")
				for name, data := range map[string][]byte{"tls.crt": ca.certPEM, "tls.key": ca.keyPEM, "ca.crt": ca.certPEM} {
					if err := os.WriteFile(filepath.Join(config.CertificateDirectory, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "partial options":
				config.CertificateDirectory = ""
			}
			if s, err := New(config, api, api); err == nil || s != nil {
				t.Fatal("invalid trust/configuration started a service")
			}
		})
	}
}

func TestServiceRefusesOccupiedListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := mountedTrust(t)
	config.Address = listener.Addr().String()
	f := resulttest.New(t, "schema-observe")
	s, err := New(config, f.Client(t), f.Client(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(t.Context()); err == nil || s.Ready(nil) == nil {
		t.Fatal("occupied listener was accepted")
	}
}
