package resultservice

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"maps"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func trustMaterial(t *testing.T, config Config) map[string][]byte {
	t.Helper()
	material := map[string][]byte{}
	for _, name := range trustFiles {
		data, err := os.ReadFile(filepath.Join(config.CertificateDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		material[name] = data
	}
	return material
}
func projectTrust(t *testing.T, directory string, material map[string][]byte) string {
	t.Helper()
	generation, err := os.MkdirTemp(directory, "..generation-")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range material {
		if err := os.WriteFile(filepath.Join(generation, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pending := filepath.Join(directory, "..data-next")
	if err := os.Symlink(filepath.Base(generation), pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	return generation
}
func resultSender(t *testing.T, s *Service, f *resulttest.Fixture, api client.Client) (*resultdelivery.Sender, *corev1.Secret) {
	t.Helper()
	issued, err := s.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := api.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: issued.Name}, secret); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
		t.Fatal("invalid projected server trust")
	}
	sender, err := resultdelivery.NewSender("https://"+s.address, f.Identity, &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots}, resultdelivery.RetryPolicy{Attempts: 1, Interval: time.Millisecond, AttemptTimeout: time.Second, TotalTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sender.Close)
	return sender, secret
}
func requireReload(t *testing.T, s *Service, material map[string][]byte) {
	t.Helper()
	projectTrust(t, s.config.CertificateDirectory, material)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.Ready(nil) != nil {
		t.Fatal("validated reload lost readiness")
	}
}

func TestProjectedTrustRotationPreservesReceiptsAndRejectsRetiredConnections(t *testing.T) {
	config := mountedTrust(t)
	old := trustMaterial(t, config)
	next := trustMaterial(t, mountedTrust(t))
	config.CertificateDirectory = t.TempDir()
	projectTrust(t, config.CertificateDirectory, old)
	first := resulttest.New(t, "schema-observe")
	second := resulttest.New(t, "schema-observe")
	// A separate namespace gives the second operation independent records and
	// Pod selection while retaining the production Job's admission fixture.
	second.Subject.SetNamespace("second-tenant")
	second.Job.Namespace, second.Pod.Namespace = "second-tenant", "second-tenant"
	second.Identity.Binding.Namespace = "second-tenant"
	apiClient := &identifyingAPI{Client: first.Client(t, second.Subject, second.Job, second.Pod)}
	s, err := New(config, apiClient, noSecrets{Reader: apiClient})
	if err != nil {
		t.Fatal(err)
	}
	running(t, s)
	firstSender, firstSecret := resultSender(t, s, first, apiClient)
	if err := firstSender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	payload, err := resultdelivery.Encode(first.Identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: first.Identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused"}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := firstSender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}

	// New Jobs learn both server roots before the server changes. Client signer
	// rotation has an overlap in which the original credential remains canonical.
	overlap := maps.Clone(old)
	overlap["ca.crt"] = append(bytes.Clone(old["ca.crt"]), next["ca.crt"]...)
	overlap["client-ca.crt"], overlap["client-ca.key"] = next["client-ca.crt"], next["client-ca.key"]
	overlap["client-trust.crt"] = append(bytes.Clone(old["client-trust.crt"]), next["client-trust.crt"]...)
	requireReload(t, s, overlap)
	if _, err := s.Ensure(t.Context(), first.Identity); err != nil {
		t.Fatalf("old canonical credential lost during overlap: %v", err)
	}
	if err := s.ValidateCreate(t.Context(), firstSecret); err != nil {
		t.Fatalf("old projection refused during overlap: %v", err)
	}
	if _, err := s.AuthorizePublication(t.Context(), first.Identity.Binding); err != nil {
		t.Fatalf("old publication refused during overlap: %v", err)
	}
	if err := firstSender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	secondSender, secondSecret := resultSender(t, s, second, apiClient)
	if !bytes.Equal(secondSecret.Data["ca.crt"], overlap["ca.crt"]) {
		t.Fatal("new credential did not receive expanded server trust")
	}
	record := &api.PtahResultRecord{}
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(secondSecret), record); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRecordCreate(t.Context(), record); err != nil {
		t.Fatalf("current issuance refused: %v", err)
	}
	forgedData := maps.Clone(secondSecret.Data)
	forgedData["ca.crt"] = old["ca.crt"]
	forged := record.DeepCopy()
	forged.Spec.Data, err = json.Marshal(forgedData)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRecordCreate(t.Context(), forged); err == nil {
		t.Fatal("new credential accepted a non-current server bundle")
	}
	forgedProjection := secondSecret.DeepCopy()
	forgedProjection.Data = forgedData
	if err := s.ValidateCreate(t.Context(), forgedProjection); err == nil {
		t.Fatal("projection was allowed to change canonical server trust")
	}
	cert, err := tls.X509KeyPair(secondSecret.Data["tls.crt"], secondSecret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(next["client-trust.crt"])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("new credential still uses retired signer: %v", err)
	}
	if err := secondSender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Deliberate retirement tests refusal, not the production overlap scheduler.
	// The first sender retains a real keep-alive connection to the old server.
	switched := maps.Clone(overlap)
	switched["tls.crt"], switched["tls.key"] = next["tls.crt"], next["tls.key"]
	switched["client-trust.crt"] = next["client-trust.crt"]
	requireReload(t, s, switched)
	secondSender.Close() // Require a new handshake against the new serving leaf.
	if err := secondSender.Check(t.Context()); err != nil {
		t.Fatalf("expanded server trust did not survive serving rotation: %v", err)
	}
	var reused atomic.Bool
	traced := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }})
	if err := firstSender.Check(traced); err == nil || !strings.Contains(err.Error(), "HTTP 403") || !reused.Load() {
		t.Fatalf("retired CA retained authority on a preexisting connection: reused=%v err=%v", reused.Load(), err)
	}
	if err := s.ValidateCreate(t.Context(), firstSecret); err == nil {
		t.Fatal("admission retained the retired client CA")
	}
	if _, err := s.AuthorizePublication(t.Context(), first.Identity.Binding); err == nil {
		t.Fatal("publication retained the retired client CA")
	}
	if _, err := s.Ensure(t.Context(), first.Identity); err == nil {
		t.Fatal("issuer retained the retired client CA")
	}
	stored, got, err := (resultstore.Store{Reader: apiClient}).Load(t.Context(), first.Identity.Binding)
	if err != nil || got != receipt || !bytes.Equal(stored, payload) {
		t.Fatalf("CA retirement invalidated acknowledged evidence: %v", err)
	}
}

func TestInvalidProjectedTrustRetainsSnapshotAndRecoversAutomatically(t *testing.T) {
	config := mountedTrust(t)
	material := trustMaterial(t, config)
	config.CertificateDirectory = t.TempDir()
	config.ReloadInterval = 5 * time.Millisecond
	projectTrust(t, config.CertificateDirectory, material)
	f := resulttest.New(t, "schema-observe")
	apiClient := &identifyingAPI{Client: f.Client(t)}
	s, err := New(config, apiClient, apiClient)
	if err != nil {
		t.Fatal(err)
	}
	running(t, s)
	initial := s.trust.Load()
	invalid := maps.Clone(material)
	invalid["tls.key"] = []byte("not a private key")
	projectTrust(t, config.CertificateDirectory, invalid)
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal("trust reload did not reach the expected state")
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(func() bool { return s.reloadFailed.Load() })
	if s.Ready(nil) == nil || s.trust.Load() != initial {
		t.Fatal("invalid projection replaced validated trust or remained ready")
	}
	if _, err := s.Ensure(t.Context(), f.Identity); err == nil {
		t.Fatal("new issuance continued with an invalid mount")
	}
	records := &api.PtahResultRecordList{}
	if err := apiClient.List(t.Context(), records); err != nil || len(records.Items) != 0 {
		t.Fatalf("invalid reload issued records: %v", err)
	}
	projectTrust(t, config.CertificateDirectory, material)
	wait(func() bool { return s.Ready(nil) == nil })
	if s.trust.Load() != initial {
		t.Fatal("restored identical material replaced its validated snapshot")
	}
	if _, err := s.Ensure(t.Context(), f.Identity); err != nil {
		t.Fatalf("issuance did not recover: %v", err)
	}
}

func TestTrustProjectionReadPinsOneGeneration(t *testing.T) {
	material := trustMaterial(t, mountedTrust(t))
	replacement := trustMaterial(t, mountedTrust(t))
	root := t.TempDir()
	oldDirectory := projectTrust(t, root, material)
	oldDirectory, err := filepath.EvalSymlinks(oldDirectory)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := trustDirectory(root)
	if err != nil || pinned != oldDirectory {
		t.Fatalf("projection was not pinned: %v", err)
	}
	projectTrust(t, root, replacement)
	observed := map[string][]byte{}
	for _, name := range trustFiles {
		observed[name], err = readTrustFile(pinned, name)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(observed) != 6 || trustDigest(observed) != trustDigest(material) || trustDigest(observed) == trustDigest(replacement) {
		t.Fatal("one load mixed projected generations")
	}
}
