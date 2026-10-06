package resultdelivery

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func testIdentity() Identity {
	return Identity{Binding: resultstore.Binding{Namespace: "tenant", Kind: "PtahSchema", Name: "schema", UID: "schema-uid", Generation: 2,
		ExecutionBindingID: "v1-" + strings.Repeat("a", 32), InputFingerprint: "sha256:" + strings.Repeat("b", 64), Operation: "apply", OperationID: "apply-1",
		JobName: "apply-1-attempt-1", JobUID: "job-uid", PodName: "apply-pod", PodUID: "pod-uid"}, Engine: "postgresql"}
}

func testPayload(t *testing.T, identity Identity) []byte {
	t.Helper()
	payload, err := Encode(identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(identity.Binding.Operation),
		OperationID: identity.Binding.OperationID, ChildExitCode: -1, Error: &runner.ResultError{Code: "refused", Message: "refused before dispatch"}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

type identifyingClient struct {
	client.Client
	count atomic.Int64
}

func (c *identifyingClient) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	o.SetUID(types.UID(fmt.Sprintf("stored-%d", c.count.Add(1))))
	return c.Client.Create(ctx, o, opts...)
}

func testStore(t *testing.T) resultstore.Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := recordapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := &identifyingClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	return resultstore.Store{Client: c, Reader: c}
}

type certificates struct {
	server, client tls.Certificate
	roots          *x509.CertPool
}

func testCertificates(t *testing.T, identity Identity) certificates {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "result test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	issue := func(serial int64, usage x509.ExtKeyUsage, uris []*url.URL) tls.Certificate {
		private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: root.NotBefore, NotAfter: root.NotAfter,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, URIs: uris,
			DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		encoded, err := x509.CreateCertificate(rand.Reader, leaf, ca, &private.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private, Leaf: parsed}
	}
	uri, err := CertificateURI(identity)
	if err != nil {
		t.Fatal(err)
	}
	return certificates{issue(2, x509.ExtKeyUsageServerAuth, nil), issue(3, x509.ExtKeyUsageClientAuth, []*url.URL{uri}), roots}
}

func startReceiver(t *testing.T, receiver *Receiver, certs certificates, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	server, err := receiver.Server(certs.server, certs.roots)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler
	if wrap != nil {
		handler = wrap(handler)
	}
	local := httptest.NewUnstartedServer(handler)
	local.Config.ReadHeaderTimeout = server.ReadHeaderTimeout
	local.Config.ReadTimeout = server.ReadTimeout
	local.Config.WriteTimeout = server.WriteTimeout
	local.Config.ErrorLog = log.New(io.Discard, "", 0)
	local.TLS = server.TLSConfig
	local.StartTLS()
	t.Cleanup(local.Close)
	return local
}

func testReceiver(t *testing.T, store Publisher, authorize func(context.Context, Identity) error, timeout time.Duration) *Receiver {
	t.Helper()
	r, err := NewReceiver(ReceiverConfig{Store: store, Authorize: authorize, MaxConcurrent: 1, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testSender(t *testing.T, endpoint string, identity Identity, certs certificates) *Sender {
	t.Helper()
	s, err := NewSender(endpoint, identity, &tls.Config{RootCAs: certs.roots, Certificates: []tls.Certificate{certs.client}},
		RetryPolicy{Attempts: 3, Interval: time.Millisecond, AttemptTimeout: 2 * time.Second, TotalTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestLostAcknowledgmentOnlyRedeliversSavedBytes(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	var requests, authorizations atomic.Int32
	receiver := testReceiver(t, store, func(_ context.Context, got Identity) error {
		authorizations.Add(1)
		if got != identity {
			return ErrAuthority
		}
		return nil
	}, time.Second)
	server := startReceiver(t, receiver, certs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				next.ServeHTTP(&lostACK{ResponseWriter: w}, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	sender := testSender(t, server.URL, identity, certs)
	payload, err := Encode(identity, runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply,
		OperationID: identity.Binding.OperationID, MutationStarted: true, CoordinationDigest: "sha256:" + strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || authorizations.Load() != 6 {
		t.Fatalf("requests=%d authority checks=%d", requests.Load(), authorizations.Load())
	}
	got, stored, err := store.Load(t.Context(), identity.Binding)
	if err != nil || stored != receipt || !bytes.Equal(got, payload) {
		t.Fatalf("readback lost result: %v", err)
	}
	list := &recordapi.PtahResultRecordList{}
	if err := store.Client.List(t.Context(), list); err != nil || len(list.Items) != 3 {
		t.Fatalf("duplicate delivery created another publication: %v", err)
	}
	// Another receiver and client have no memory of the first delivery.
	restarted := testReceiver(t, resultstore.Store{Client: store.Client, Reader: store.Reader}, func(_ context.Context, got Identity) error {
		if got != identity {
			return ErrAuthority
		}
		return nil
	}, time.Second)
	second := startReceiver(t, restarted, certs, nil)
	replay, err := testSender(t, second.URL, identity, certs).Send(t.Context(), payload)
	if err != nil || replay != receipt {
		t.Fatalf("receiver replacement changed receipt: %v", err)
	}
}

type lostACK struct {
	http.ResponseWriter
	closed bool
}

func (w *lostACK) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *lostACK) WriteHeader(status int) {
	if status != http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	connection, _, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		_ = connection.Close()
		w.closed = true
	}
}
func (w *lostACK) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(p)
}

func TestConflictingPayloadGetsOneDefinitiveRefusal(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	var requests atomic.Int32
	r := testReceiver(t, store, func(context.Context, Identity) error { return nil }, time.Second)
	server := startReceiver(t, r, certs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); next.ServeHTTP(w, r) })
	})
	sender := testSender(t, server.URL, identity, certs)
	payload := testPayload(t, identity)
	if _, err := sender.Send(t.Context(), payload); err != nil {
		t.Fatal(err)
	}
	other := bytes.Replace(payload, []byte("refused before dispatch"), []byte("another valid refusal"), 1)
	if _, err := sender.Send(t.Context(), other); err == nil || err.Error() != "result receiver returned HTTP 409" {
		t.Fatalf("conflict accepted: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("definitive refusal was retried: %d", requests.Load())
	}
}

func TestAuthorityRecheckedAtPublicationBoundary(t *testing.T) {
	for _, refuseAt := range []int32{1, 2, 3} {
		t.Run(fmt.Sprint(refuseAt), func(t *testing.T) {
			identity := testIdentity()
			store := testStore(t)
			certs := testCertificates(t, identity)
			var calls atomic.Int32
			r := testReceiver(t, store, func(context.Context, Identity) error {
				if calls.Add(1) >= refuseAt {
					return errors.Join(ErrAuthority, errors.New("private authority details"))
				}
				return nil
			}, time.Second)
			server := startReceiver(t, r, certs, nil)
			if _, err := testSender(t, server.URL, identity, certs).Send(t.Context(), testPayload(t, identity)); err == nil || err.Error() != "result receiver returned HTTP 403" {
				t.Fatalf("authority change acknowledged: %v", err)
			}
			if _, _, err := store.Load(t.Context(), identity.Binding); !errors.Is(err, resultstore.ErrIncomplete) {
				t.Fatalf("retired authority committed a result: %v", err)
			}
		})
	}
}

func TestCurrentClientTrustRecheckedAtPublicationBoundary(t *testing.T) {
	for _, refuseAt := range []int32{1, 2, 3} {
		t.Run(fmt.Sprint(refuseAt), func(t *testing.T) {
			identity := testIdentity()
			store := testStore(t)
			certs := testCertificates(t, identity)
			var calls atomic.Int32
			r := testReceiver(t, store, func(context.Context, Identity) error { return nil }, time.Second)
			r.config.VerifyClient = func(*tls.ConnectionState) error {
				if calls.Add(1) >= refuseAt {
					return errors.New("private trust refusal details")
				}
				return nil
			}
			server := startReceiver(t, r, certs, nil)
			if _, err := testSender(t, server.URL, identity, certs).Send(t.Context(), testPayload(t, identity)); err == nil || err.Error() != "result receiver returned HTTP 403" {
				t.Fatalf("retired client trust acknowledged: %v", err)
			}
			if _, _, err := store.Load(t.Context(), identity.Binding); !errors.Is(err, resultstore.ErrIncomplete) {
				t.Fatalf("retired client trust committed a result: %v", err)
			}
		})
	}
}

func TestTLSRefusesUntrustedOrMissingClientAndServerCertificates(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	other := testCertificates(t, identity)
	store := testStore(t)
	var calls atomic.Int32
	r := testReceiver(t, store, func(context.Context, Identity) error { calls.Add(1); return nil }, time.Second)
	server := startReceiver(t, r, certs, nil)
	for name, config := range map[string]*tls.Config{
		"untrusted client": {RootCAs: certs.roots, Certificates: []tls.Certificate{other.client}},
		"missing client":   {RootCAs: certs.roots},
		"untrusted server": {RootCAs: other.roots, Certificates: []tls.Certificate{certs.client}},
	} {
		t.Run(name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: config}
			defer transport.CloseIdleConnections()
			c := &http.Client{Transport: transport, Timeout: time.Second}
			response, err := c.Get(server.URL)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("TLS accepted invalid trust")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid TLS reached authority checks")
	}
}

func TestExpiredIdentityOnExistingConnectionAndMalformedSAN(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	leaf := certs.client.Leaf
	state := &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	if got, err := authenticatedIdentity(state, time.Now()); err != nil || got != identity {
		t.Fatalf("valid identity refused: %v", err)
	}
	if _, err := authenticatedIdentity(state, leaf.NotAfter); !errors.Is(err, ErrAuthority) {
		t.Fatal("expired connection identity accepted")
	}
	uri, _ := CertificateURI(identity)
	for _, raw := range []string{uri.String() + "?x=1", uri.String() + "#fragment", "https://operator.ptah.run/v1/identity", strings.Replace(uri.String(), "operator.ptah.run", "other.example", 1)} {
		parsed, _ := url.Parse(raw)
		if _, err := certificateIdentity(parsed); !errors.Is(err, ErrAuthority) {
			t.Fatalf("malformed identity accepted: %s", raw)
		}
	}
}

func TestInvalidBodyAndRouteNeverReachStorage(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	server := startReceiver(t, testReceiver(t, store, func(context.Context, Identity) error { return nil }, time.Second), certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	payload := testPayload(t, identity)
	for _, row := range []struct {
		name   string
		mutate func(*http.Request)
		body   []byte
		want   int
	}{
		{"wrong digest", func(r *http.Request) { r.Header.Set(DigestHeader, "sha256:wrong") }, payload, 400},
		{"wrong route", func(r *http.Request) { r.URL.Path += "-other" }, payload, 403},
		{"query", func(r *http.Request) { r.URL.RawQuery = "token=do-not-echo" }, payload, 403},
		{"unknown length", func(r *http.Request) { r.ContentLength = -1 }, payload, 411},
		{"oversize", func(r *http.Request) { r.ContentLength = resultstore.MaxPayloadBytes + 1 }, payload, 413},
		{"unknown field", func(*http.Request) {}, append(payload[:len(payload)-1:len(payload)-1], []byte(",\"private\":\"do-not-echo\"}")...), 422},
		{"foreign operation", func(*http.Request) {}, bytes.Replace(payload, []byte("apply-1"), []byte("apply-2"), 1), 422},
		{"wrong type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, payload, 415},
	} {
		t.Run(row.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, sender.endpoint, bytes.NewReader(row.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", ContentType)
			request.Header.Set(DigestHeader, payloadDigest(row.body))
			row.mutate(request)
			// An intentionally inconsistent oversized body makes Transport fail
			// while sending. Use Expect to let the server refuse its headers first.
			request.Header.Set("Expect", "100-continue")
			transport := sender.transport.Clone()
			transport.ExpectContinueTimeout = time.Second
			defer transport.CloseIdleConnections()
			response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != row.want || bytes.Contains(body, []byte("do-not-echo")) {
				t.Fatalf("response status=%d body=%q", response.StatusCode, body)
			}
		})
	}
	list := &recordapi.PtahResultRecordList{}
	if err := store.Client.List(t.Context(), list); err != nil || len(list.Items) != 0 {
		t.Fatalf("invalid body persisted: %v", err)
	}
}

func TestSlowUploadReleasesSlotAndSaturationDoesNotQueue(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	entered := make(chan struct{}, 1)
	receiver := testReceiver(t, store, func(context.Context, Identity) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		return nil
	}, 500*time.Millisecond)
	server := startReceiver(t, receiver, certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	payload := testPayload(t, identity)
	reader, writer := io.Pipe()
	defer writer.Close()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, sender.endpoint, reader)
	request.ContentLength = int64(len(payload))
	request.Header.Set("Content-Type", ContentType)
	request.Header.Set(DigestHeader, payloadDigest(payload))
	request.Header.Set("Expect", "100-continue")
	transport := sender.transport.Clone()
	transport.ExpectContinueTimeout = time.Second
	defer transport.CloseIdleConnections()
	type responseOutcome struct {
		status int
		err    error
	}
	done := make(chan responseOutcome, 1)
	go func() {
		response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body.Close()
		}
		done <- responseOutcome{status, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow upload never entered receiver")
	}
	// A separate connection reaches the receiver while the first upload owns
	// its only slot. It gets an immediate 503 and creates no second upload.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, retry, err := sender.send(ctx, payload, payloadDigest(payload))
	if err == nil || !retry || err.Error() != "result receiver returned HTTP 503" {
		t.Fatalf("saturation did not refuse: %v", err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.status != http.StatusRequestTimeout {
			t.Fatalf("receiver did not time out the open body: status=%d err=%v", result.status, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled upload did not return")
	}
	_ = writer.Close()
	// The original stream must give up its slot without a reconcile worker.
	if _, err := sender.Send(t.Context(), payload); err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
}

func TestPlanDocumentIsDurableAndBoundToItsEngine(t *testing.T) {
	identity := testIdentity()
	identity.Binding.Operation = "plan"
	plan := `{"format_version":1,"name":"plan","dialect":"postgresql","from_fingerprint":"before","to_fingerprint":"after","statements":[{"sql":"CREATE TABLE public.example (id integer)","severity":"safe"}]}`
	result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: identity.Binding.OperationID,
		Stdout: plan, PlanContentDigest: payloadDigest([]byte(plan)), PlanOutcome: runner.PlanOutcomeChanges,
		CoordinationDigest: "sha256:" + strings.Repeat("c", 64)}
	payload, err := Encode(identity, result)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(identity, payload)
	if err != nil || decoded.Stdout != plan {
		t.Fatalf("plan bytes changed: %v", err)
	}
	identity.Engine = "mysql"
	if _, err := Decode(identity, payload); !errors.Is(err, ErrPayload) {
		t.Fatal("foreign plan engine accepted")
	}
	identity.Engine = "postgresql"
	result.Stdout = "opaque-process-sealed-plan"
	result.PlanContentDigest = payloadDigest([]byte(result.Stdout))
	if _, err := Encode(identity, result); !errors.Is(err, ErrPayload) {
		t.Fatal("unreadable process-sealed plan accepted")
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"protocolVersion":0,`), payload[1:]...)
	if _, err := Decode(identity, duplicate); !errors.Is(err, ErrPayload) {
		t.Fatal("ambiguous duplicate field accepted")
	}
}

func TestMaximumEscapingPlanCrossesTLSAndPersists(t *testing.T) {
	identity := testIdentity()
	identity.Binding.Operation = "plan"
	before := `{"format_version":1,"name":"plan","dialect":"postgresql","from_fingerprint":"before","to_fingerprint":"after","statements":[{"sql":"CREATE TABLE public.example (id integer)","severity":"safe","reason":"`
	after := `"}]}`
	plan := before + strings.Repeat("<", int(plancontract.MaxExecutableBytes)-len(before)-len(after)) + after
	result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: identity.Binding.OperationID,
		Stdout: plan, PlanContentDigest: payloadDigest([]byte(plan)), PlanOutcome: runner.PlanOutcomeChanges, CoordinationDigest: "sha256:" + strings.Repeat("c", 64)}
	payload, err := Encode(identity, result)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != int(plancontract.MaxExecutableBytes) || len(payload) <= 10<<20 {
		t.Fatal("test did not exercise the supported plan limit beyond kubelet log capacity")
	}
	store := testStore(t)
	certs := testCertificates(t, identity)
	r := testReceiver(t, store, func(_ context.Context, got Identity) error {
		if got != identity {
			return ErrAuthority
		}
		return nil
	}, time.Minute)
	server := startReceiver(t, r, certs, nil)
	sender, err := NewSender(server.URL, identity, &tls.Config{RootCAs: certs.roots, Certificates: []tls.Certificate{certs.client}},
		RetryPolicy{Attempts: 1, Interval: time.Millisecond, AttemptTimeout: time.Minute, TotalTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receipt, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got, loaded, err := store.Load(t.Context(), identity.Binding)
	if err != nil || loaded != receipt || !bytes.Equal(got, payload) {
		t.Fatalf("maximum result lost in transport: %v", err)
	}
	decoded, err := Decode(identity, got)
	if err != nil || decoded.Stdout != plan {
		t.Fatalf("maximum plan bytes changed: %v", err)
	}
	result.Stdout = before + "<" + strings.TrimPrefix(plan, before)
	result.PlanContentDigest = payloadDigest([]byte(result.Stdout))
	if _, err := Encode(identity, result); !errors.Is(err, ErrPayload) {
		t.Fatal("maximum-plus-one plan accepted")
	}
}

func TestTransientAuthorityFailureRetriesAndDefinitiveDenialDoesNot(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	store := testStore(t)
	var calls atomic.Int32
	r := testReceiver(t, store, func(context.Context, Identity) error {
		if calls.Add(1) == 1 {
			return errors.New("API temporarily unavailable: private details")
		}
		return nil
	}, time.Second)
	server := startReceiver(t, r, certs, nil)
	if _, err := testSender(t, server.URL, identity, certs).Send(t.Context(), testPayload(t, identity)); err != nil {
		t.Fatalf("temporary API outage stopped delivery: %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("authority calls=%d, want one transient failure and three successful checks", calls.Load())
	}
}

func TestSenderRefusesRedirectsAndMismatchedReceipts(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	store := testStore(t)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			r := testReceiver(t, store, func(context.Context, Identity) error { return nil }, time.Second)
			server := startReceiver(t, r, certs, func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Location", "https://unrelated.invalid/private")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(resultstore.Receipt{Name: "another-result", UID: "another-uid", Digest: "private-data", Size: 1})
				})
			})
			if _, err := testSender(t, server.URL, identity, certs).Send(t.Context(), testPayload(t, identity)); err == nil || strings.Contains(err.Error(), "private-data") {
				t.Fatalf("invalid receipt accepted or disclosed: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("definitive response retried or redirected")
			}
		})
	}
}

func TestDeliveryAttemptAndDeadlineBounds(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	store := testStore(t)
	var attempts atomic.Int32
	r := testReceiver(t, store, func(context.Context, Identity) error { attempts.Add(1); return errors.New("API unavailable") }, time.Second)
	server := startReceiver(t, r, certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	if _, err := sender.Send(t.Context(), testPayload(t, identity)); err == nil || err.Error() != "result delivery attempts exhausted: result receiver returned HTTP 503" {
		t.Fatalf("retry bound failed: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("made %d requests instead of three", attempts.Load())
	}
	attempts.Store(0)
	sender.retry = RetryPolicy{Attempts: 8, Interval: time.Second, AttemptTimeout: 50 * time.Millisecond, TotalTimeout: 100 * time.Millisecond}
	if _, err := sender.Send(t.Context(), testPayload(t, identity)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overall delivery deadline failed: %v", err)
	}
	if attempts.Load() > 1 {
		t.Fatal("delivery retried after its overall deadline")
	}
	attempts.Store(0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sender.Send(ctx, testPayload(t, identity)); !errors.Is(err, context.Canceled) || attempts.Load() != 0 {
		t.Fatalf("canceled delivery reached receiver: %v", err)
	}
}

func TestPreflightChecksLiveAuthorityWithoutPublishing(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	var calls atomic.Int64
	var retired atomic.Bool
	r := testReceiver(t, store, func(_ context.Context, got Identity) error {
		calls.Add(1)
		if got != identity || retired.Load() {
			return ErrAuthority
		}
		return nil
	}, time.Second)
	server := startReceiver(t, r, certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	if err := sender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatal("preflight skipped authority or published bytes")
	}
	retired.Store(true)
	if err := sender.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("retired authority passed preflight: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("reused connection did not recheck authority")
	}
	r.slots <- struct{}{}
	if err := sender.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("saturation passed preflight: %v", err)
	}
	<-r.slots
	if calls.Load() != 2 {
		t.Fatal("saturated preflight performed live API work")
	}
	if store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatal("preflight wrote a result")
	}
}

func TestPreflightBoundsAuthorityAndRefusesBodies(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	r := testReceiver(t, store, func(ctx context.Context, _ Identity) error {
		<-ctx.Done()
		return ctx.Err()
	}, 30*time.Millisecond)
	server := startReceiver(t, r, certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	start := time.Now()
	if err := sender.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("stalled authority passed preflight: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("preflight exceeded its server-side deadline")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodHead, sender.endpoint, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := sender.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("HEAD with a body returned %d", response.StatusCode)
	}
	if store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatal("preflight wrote a result")
	}
}
