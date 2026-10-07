package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type ackFixture struct {
	proxy    *resultACKProxy
	client   *http.Client
	url      string
	calls    atomic.Int32
	body     []byte
	status   int
	change   func(int32, *ackReceipt)
	token    string
	identity string
}

func TestResultACKProxyForwardsRotatedPodTokensWithoutRetainingThem(t *testing.T) {
	const originalToken, rotatedToken = "original.bound.token", "rotated.bound.token"
	identity := []byte(`{"binding":{"jobUID":"original-job","podUID":"original-pod"},"engine":"postgresql"}`)
	f := &ackFixture{body: []byte("private operation bytes"), token: originalToken,
		identity: base64.RawURLEncoding.EncodeToString(identity)}
	var expectedToken atomic.Value
	expectedToken.Store(originalToken)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedToken.Load().(string) ||
			r.Header.Get("X-Ptah-Result-Identity") != f.identity {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		f.calls.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(data, f.body) || r.Header.Get(resultDigestHeader) != digest(data) {
			t.Error("proxy changed the original bytes or digest")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ackReceipt{Name: "original", UID: "receipt", Digest: digest(data), Size: int64(len(data))})
	}))
	t.Cleanup(backend.Close)
	f.proxy = newResultACKTokenProxy(backend.Client(), backend.URL, "original-job")
	frontend := httptest.NewTLSServer(f.proxy)
	t.Cleanup(frontend.Close)
	f.url, f.client = frontend.URL, frontend.Client()
	// A plausible identity claim alone must not become successful evidence.
	head := func(token, claim string) int {
		t.Helper()
		r, _ := http.NewRequest(http.MethodHead, f.url+"/v1/results/original", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Ptah-Result-Identity", claim)
		res, err := f.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	untrustedStatus := head("untrusted.token", f.identity)
	var untrustedEvidence ackEvidence
	if err := json.Unmarshal(f.admin(http.MethodGet, "/evidence").Body.Bytes(), &untrustedEvidence); err != nil {
		t.Fatal(err)
	}
	if untrustedStatus != http.StatusUnauthorized || untrustedEvidence.Preflights != 0 || untrustedEvidence.IdentityDigest != "" {
		t.Fatal("an unauthenticated claim counted as a preflight")
	}
	if head(originalToken, f.identity) != http.StatusNoContent {
		t.Fatal("the original token did not reach the receiver")
	}
	foreign := base64.RawURLEncoding.EncodeToString(bytes.ReplaceAll(identity, []byte("original-pod"), []byte("replacement-pod")))
	if head(originalToken, foreign) != http.StatusForbidden {
		t.Fatal("the fixture accepted a changed Pod identity")
	}
	if res, err := f.put(f.body); err == nil || res != nil || f.calls.Load() != 1 {
		t.Fatal("the original successful receipt was not dropped")
	}
	f.token = rotatedToken
	expectedToken.Store(rotatedToken)
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { r, err := f.put(f.body); done <- result{r, err} }()
	var evidence ackEvidence
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err := json.Unmarshal(f.admin(http.MethodGet, "/evidence").Body.Bytes(), &evidence); err != nil {
			t.Fatal(err)
		}
		if len(evidence.Attempts) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !evidence.Dropped || evidence.Preflights != 1 || len(evidence.Attempts) != 2 ||
		evidence.Attempts[0].Receipt != evidence.Attempts[1].Receipt || evidence.Authentication != "pod-token" ||
		evidence.JobUID != "original-job" || evidence.PodUID != "original-pod" || evidence.IdentityDigest != digest(identity) ||
		evidence.ClientCertificateDigest != "" {
		t.Fatal("token rotation lost the original identity or durable receipt")
	}
	if f.admin(http.MethodPost, "/release").Code != http.StatusNoContent {
		t.Fatal("retry was not ready")
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.response.Body.Close()
	if got.response.StatusCode != http.StatusOK || f.calls.Load() != 2 {
		t.Fatal("retry did not complete")
	}
	encoded := f.admin(http.MethodGet, "/evidence").Body.String()
	for _, private := range []string{originalToken, rotatedToken, string(f.body)} {
		if strings.Contains(encoded, private) {
			t.Fatal("fixture evidence retained a token or payload")
		}
	}
}

func newACKFixture(t *testing.T) *ackFixture {
	t.Helper()
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	issue := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"results.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey}
	}
	serverCert := issue(2, x509.ExtKeyUsageServerAuth)
	clientCert := issue(3, x509.ExtKeyUsageClientAuth)
	newClient := func(cert tls.Certificate) *http.Client {
		tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}}, DisableKeepAlives: true}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 3 * time.Second}
	}
	serve := func(handler http.Handler) *httptest.Server {
		s := httptest.NewUnstartedServer(handler)
		s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{"http/1.1"}}
		s.StartTLS()
		t.Cleanup(s.Close)
		return s
	}
	f := &ackFixture{body: []byte("private operation bytes"), status: http.StatusOK}
	backend := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) != 1 || !bytes.Equal(r.TLS.PeerCertificates[0].Raw, clientCert.Certificate[0]) {
			t.Error("proxy did not preserve the original Pod's client identity")
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		count := f.calls.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(data, f.body) || r.Header.Get(resultDigestHeader) != digest(data) {
			t.Error("proxy changed the original result bytes or digest")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		receipt := ackReceipt{Name: "original-complete", UID: "immutable-completion-uid", Digest: digest(data), Size: int64(len(data))}
		if f.change != nil {
			f.change(count, &receipt)
		}
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	f.proxy = newResultACKProxy(newClient(clientCert), backend.URL, clientCert.Certificate[0])
	s := serve(f.proxy)
	f.url, f.client = s.URL, newClient(clientCert)
	// Another valid client under the same CA must not impersonate this Pod.
	wrong := newClient(issue(4, x509.ExtKeyUsageClientAuth))
	req, _ := http.NewRequest(http.MethodHead, s.URL+"/v1/results/original", nil)
	res, err := wrong.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || f.calls.Load() != 0 {
		t.Fatal("fixture did not refuse a different valid client certificate")
	}
	return f
}

func (f *ackFixture) put(body []byte) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPut, f.url+"/v1/results/original", bytes.NewReader(body))
	req.Header.Set(resultDigestHeader, digest(body))
	req.Header.Set("Content-Type", "application/vnd.ptah.result.v1+json")
	if f.identity != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("X-Ptah-Result-Identity", f.identity)
	}
	return f.client.Do(req)
}

func (f *ackFixture) admin(method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.proxy.admin(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestResultACKProxyDropsOnlyFirstDurableResponse(t *testing.T) {
	f := newACKFixture(t)
	if f.admin(http.MethodPost, "/release").Code != http.StatusConflict {
		t.Fatal("fixture released before any acknowledgment")
	}
	head, _ := http.NewRequest(http.MethodHead, f.url+"/v1/results/original", nil)
	response, err := f.client.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("fixture blocked runner preflight")
	}
	if response, err = f.put(f.body); err == nil || response != nil || f.calls.Load() != 1 {
		t.Fatal("first successful backend receipt reached the client")
	}
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := f.put(f.body); done <- result{response, err} }()
	deadline := time.Now().Add(time.Second)
	var evidence ackEvidence
	for time.Now().Before(deadline) {
		if err := json.Unmarshal(f.admin(http.MethodGet, "/evidence").Body.Bytes(), &evidence); err != nil {
			t.Fatal(err)
		}
		if len(evidence.Attempts) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !evidence.Dropped || len(evidence.Attempts) != 2 || evidence.Preflights != 1 || evidence.Released || evidence.Attempts[0].Receipt != evidence.Attempts[1].Receipt {
		t.Fatal("fixture did not observe two identical durable receipts")
	}
	select {
	case <-done:
		t.Fatal("retry response escaped before route restoration")
	default:
	}
	if r, err := f.put(f.body); err != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("fixture did not bound concurrent requests")
	} else {
		r.Body.Close()
	}
	for range 2 {
		if f.admin(http.MethodPost, "/release").Code != http.StatusNoContent {
			t.Fatal("fixture release was not idempotent")
		}
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.response.Body.Close()
	var receipt ackReceipt
	if got.response.StatusCode != http.StatusOK || json.NewDecoder(got.response.Body).Decode(&receipt) != nil || receipt != evidence.Attempts[0].Receipt || f.calls.Load() != 2 {
		t.Fatal("retry did not return the original receipt")
	}
	encoded := f.admin(http.MethodGet, "/evidence").Body.String()
	if strings.Contains(encoded, string(f.body)) || strings.Contains(encoded, "PRIVATE KEY") || strings.Contains(encoded, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.proxy.clientLeaf}))) {
		t.Fatal("fixture evidence leaked payload or credential bytes")
	}
}

func TestResultACKProxyRefusesFalseReceiptEvidence(t *testing.T) {
	for _, scenario := range []string{"refused", "unavailable", "wrong digest", "wrong size", "missing UID", "changed UID", "changed bytes"} {
		t.Run(scenario, func(t *testing.T) {
			f := newACKFixture(t)
			expected := http.StatusBadGateway
			switch scenario {
			case "refused":
				f.status, expected = http.StatusForbidden, http.StatusForbidden
			case "unavailable":
				f.status, expected = http.StatusServiceUnavailable, http.StatusServiceUnavailable
			case "wrong digest":
				f.change = func(_ int32, r *ackReceipt) { r.Digest = "sha256:wrong" }
			case "wrong size":
				f.change = func(_ int32, r *ackReceipt) { r.Size++ }
			case "missing UID":
				f.change = func(_ int32, r *ackReceipt) { r.UID = "" }
			case "changed UID":
				f.change = func(n int32, r *ackReceipt) {
					if n > 1 {
						r.UID = "different-completion"
					}
				}
			case "changed bytes":
				expected = http.StatusConflict
			}
			body := f.body
			prior := scenario == "changed UID" || scenario == "changed bytes"
			if prior {
				if _, err := f.put(body); err == nil {
					t.Fatal("first acknowledgment was not lost")
				}
				if scenario == "changed bytes" {
					body = []byte("other operation bytes")
				}
			}
			response, err := f.put(body)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != expected {
				t.Fatalf("status=%d, want %d", response.StatusCode, expected)
			}
			var evidence ackEvidence
			if err := json.Unmarshal(f.admin(http.MethodGet, "/evidence").Body.Bytes(), &evidence); err != nil {
				t.Fatal(err)
			}
			want := 0
			if prior {
				want = 1
			}
			if len(evidence.Attempts) != want || evidence.Dropped != prior || f.admin(http.MethodPost, "/release").Code != http.StatusConflict {
				t.Fatal("invalid or refused receipt counted as successful duplicate delivery")
			}
		})
	}
}

func TestResultACKProxyPausesRetryBeforeReachingReceiver(t *testing.T) {
	f := newACKFixture(t)
	f.proxy.mu.Lock()
	f.proxy.evidence.RetryGateEnabled = true
	f.proxy.mu.Unlock()
	if f.admin(http.MethodPost, "/resume-retry").Code != http.StatusConflict {
		t.Fatal("resumed before a retry reached the gate")
	}
	if response, err := f.put(f.body); err == nil || response != nil {
		t.Fatal("first acknowledgment was not lost")
	}
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { response, err := f.put(f.body); done <- result{response, err} }()
	read := func() ackEvidence {
		var e ackEvidence
		if err := json.Unmarshal(f.admin(http.MethodGet, "/evidence").Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	wait := func(ready func(ackEvidence) bool) ackEvidence {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			e := read()
			if ready(e) {
				return e
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("retry did not reach the required boundary")
		return ackEvidence{}
	}
	e := wait(func(e ackEvidence) bool { return e.RetryWaits == 1 })
	if len(e.Attempts) != 1 || f.calls.Load() != 1 || e.RetryResumedAt != nil {
		t.Fatal("paused retry reached the old receiver")
	}
	if f.admin(http.MethodPost, "/release").Code != http.StatusConflict {
		t.Fatal("released a retry before it reached the new receiver")
	}
	for range 2 {
		if f.admin(http.MethodPost, "/resume-retry").Code != http.StatusNoContent {
			t.Fatal("retry resume was not idempotent")
		}
	}
	e = wait(func(e ackEvidence) bool { return len(e.Attempts) == 2 })
	if e.RetryResumedAt == nil || !e.RetryResumedAt.After(e.Attempts[0].ReceivedAt) || !e.Attempts[1].ReceivedAt.After(*e.RetryResumedAt) {
		t.Fatal("receipt did not follow retry resumption")
	}
	select {
	case <-done:
		t.Fatal("response escaped before route restoration")
	default:
	}
	if f.admin(http.MethodPost, "/release").Code != http.StatusNoContent {
		t.Fatal("could not release the persisted retry")
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.response.Body.Close()
	if got.response.StatusCode != http.StatusOK || f.calls.Load() != 2 {
		t.Fatal("retry did not reach the receiver exactly once after resumption")
	}
}
