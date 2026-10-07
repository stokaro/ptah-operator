package resultdelivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultstore"
)

func startTokenReceiver(t *testing.T, receiver *Receiver, certs certificates, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	server, err := receiver.Server(certs.server, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler
	if wrap != nil {
		handler = wrap(handler)
	}
	s := httptest.NewUnstartedServer(handler)
	s.Config.ReadHeaderTimeout, s.Config.ReadTimeout, s.Config.WriteTimeout = server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = server.TLSConfig
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func testTokenSender(t *testing.T, endpoint string, identity Identity, certs certificates, source TokenSource) *Sender {
	t.Helper()
	sender, err := NewTokenSender(endpoint, identity, &tls.Config{RootCAs: certs.roots},
		RetryPolicy{Attempts: 3, Interval: time.Millisecond, AttemptTimeout: time.Second, TotalTimeout: 3 * time.Second}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sender.Close)
	return sender
}

func TestTokenRotationAfterLostAcknowledgmentPreservesReceipt(t *testing.T) {
	identity := testIdentity()
	store, certs := testStore(t), testCertificates(t, identity)
	var reads, uploads, authentications, authorizations atomic.Int32
	var current atomic.Value
	current.Store("first.token")
	rotated := make(chan struct{})
	var firstReceipt resultstore.Receipt
	receiver, err := NewReceiver(ReceiverConfig{Store: store, MaxConcurrent: 1, Timeout: time.Second,
		AuthenticateToken: func(_ context.Context, token string, got Identity) error {
			authentications.Add(1)
			if token != current.Load().(string) || got != identity {
				return ErrAuthority
			}
			return nil
		}, Authorize: func(_ context.Context, got Identity) error {
			authorizations.Add(1)
			if got != identity {
				return ErrAuthority
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	payload := testPayload(t, identity)
	server := startTokenReceiver(t, receiver, certs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) != 0 {
				t.Error("token delivery still requires a client certificate")
			}
			if r.Method == http.MethodPut {
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, payload) {
					t.Error("retry changed result bytes")
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				if uploads.Add(1) == 1 {
					serveWithoutAcknowledgment(next, w, r)
					_, saved, err := store.Load(t.Context(), identity.Binding)
					if err != nil {
						t.Error("lost acknowledgment did not leave a complete result")
					}
					firstReceipt = saved
					current.Store("rotated.token")
					close(rotated)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	sender := testTokenSender(t, server.URL, identity, certs, func() (string, error) {
		if reads.Add(1) == 3 {
			<-rotated
		}
		return current.Load().(string), nil
	})
	if err := sender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	receipt, err := sender.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got, stored, err := store.Load(t.Context(), identity.Binding)
	if err != nil || !bytes.Equal(got, payload) || stored != receipt || uploads.Load() != 2 || reads.Load() != 3 || authentications.Load() != authorizations.Load() || authentications.Load() < 5 {
		t.Fatalf("rotation/lost acknowledgment changed publication: err=%v uploads=%d reads=%d checks=%d/%d", err, uploads.Load(), reads.Load(), authentications.Load(), authorizations.Load())
	}
	if receipt != firstReceipt {
		t.Fatal("redelivery created a second publication")
	}
}

func TestTokenRevocationBeforeCommitRefusesCompletion(t *testing.T) {
	for _, at := range []int32{1, 2, 3} {
		identity := testIdentity()
		store, certs := testStore(t), testCertificates(t, identity)
		var checks atomic.Int32
		r, err := NewReceiver(ReceiverConfig{Store: store, MaxConcurrent: 1, Timeout: time.Second,
			AuthenticateToken: func(context.Context, string, Identity) error {
				if checks.Add(1) >= at {
					return ErrAuthority
				}
				return nil
			}, Authorize: func(context.Context, Identity) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		server := startTokenReceiver(t, r, certs, nil)
		sender := testTokenSender(t, server.URL, identity, certs, func() (string, error) { return "bound.token", nil })
		if _, err := sender.Send(t.Context(), testPayload(t, identity)); err == nil || err.Error() != "result receiver returned HTTP 403" || checks.Load() != at {
			t.Fatalf("revoked token was accepted or retried: checks=%d err=%v", checks.Load(), err)
		}
		if _, _, err := store.Load(t.Context(), identity.Binding); !errors.Is(err, resultstore.ErrIncomplete) {
			t.Fatalf("revoked token completed publication: %v", err)
		}
	}
}

func TestTokenReviewRunsInsideBoundedReceiverSlot(t *testing.T) {
	identity := testIdentity()
	store, certs := testStore(t), testCertificates(t, identity)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r, err := NewReceiver(ReceiverConfig{Store: store, MaxConcurrent: 1, Timeout: time.Second,
		AuthenticateToken: func(ctx context.Context, _ string, _ Identity) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, Authorize: func(context.Context, Identity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := startTokenReceiver(t, r, certs, nil)
	source := func() (string, error) { return "bound.token", nil }
	first := testTokenSender(t, server.URL, identity, certs, source)
	done := make(chan error, 1)
	go func() { done <- first.Check(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("TokenReview did not start")
	}
	second := testTokenSender(t, server.URL, identity, certs, source)
	second.retry.Attempts = 1
	err = second.Check(t.Context())
	close(release)
	if err == nil || err.Error() != "result receiver preflight returned HTTP 503" || calls.Load() != 1 {
		t.Fatalf("saturated receiver started another TokenReview: calls=%d err=%v", calls.Load(), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatal("authentication preflight wrote result records")
	}
}

func TestTokenHeadersRefuseMalformedOrAmbiguousClaims(t *testing.T) {
	identity := testIdentity()
	encoded, _ := json.Marshal(identity)
	for name, mutate := range map[string]func(*http.Request){
		"valid":              func(*http.Request) {},
		"cleartext":          func(r *http.Request) { r.TLS = nil },
		"old TLS":            func(r *http.Request) { r.TLS.Version = tls.VersionTLS12 },
		"no token":           func(r *http.Request) { r.Header.Del("Authorization") },
		"duplicate token":    func(r *http.Request) { r.Header.Add("Authorization", "Bearer other") },
		"token whitespace":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer x y") },
		"oversized token":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 8193)) },
		"duplicate identity": func(r *http.Request) { r.Header.Add(identityHeader, r.Header.Get(identityHeader)) },
		"oversized identity": func(r *http.Request) { r.Header.Set(identityHeader, strings.Repeat("x", 4097)) },
		"unknown identity field": func(r *http.Request) {
			r.Header.Set(identityHeader, base64.RawURLEncoding.EncodeToString(append([]byte(`{"unknown":true,`), encoded[1:]...)))
		},
		"duplicate identity field": func(r *http.Request) {
			r.Header.Set(identityHeader, base64.RawURLEncoding.EncodeToString(append([]byte(`{"engine":"mysql",`), encoded[1:]...)))
		},
		"invalid identity": func(r *http.Request) {
			r.Header.Set(identityHeader, base64.RawURLEncoding.EncodeToString([]byte(`{}`)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodHead, "https://receiver.test", nil)
			r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
			r.Header.Set("Authorization", "Bearer bound.token")
			r.Header.Set(identityHeader, base64.RawURLEncoding.EncodeToString(encoded))
			mutate(r)
			got, token, err := tokenIdentity(r)
			if name == "valid" {
				if err != nil || got != identity || token != "bound.token" {
					t.Fatal("valid claim was not parsed")
				}
			} else if !errors.Is(err, ErrAuthority) {
				t.Fatalf("ambiguous or invalid claim accepted: %v", err)
			}
		})
	}
}

func TestTokenReceiverHasNoCertificateFallbackAndRechecksConnections(t *testing.T) {
	identity := testIdentity()
	store, certs := testStore(t), testCertificates(t, identity)
	var revoked atomic.Bool
	var checks atomic.Int32
	r, err := NewReceiver(ReceiverConfig{Store: store, MaxConcurrent: 1, Timeout: time.Second,
		AuthenticateToken: func(context.Context, string, Identity) error {
			checks.Add(1)
			if revoked.Load() {
				return ErrAuthority
			}
			return nil
		}, Authorize: func(context.Context, Identity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Server(certs.server, certs.roots); err == nil {
		t.Fatal("token receiver accepted a client CA fallback")
	}
	server := startTokenReceiver(t, r, certs, nil)
	if err := testSender(t, server.URL, identity, certs).Check(t.Context()); err == nil || err.Error() != "result receiver preflight returned HTTP 401" || checks.Load() != 0 {
		t.Fatalf("certificate-only caller reached token authentication: %v", err)
	}
	sender := testTokenSender(t, server.URL, identity, certs, func() (string, error) { return "bound.token", nil })
	if err := sender.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	if err := sender.Check(t.Context()); err == nil || err.Error() != "result receiver preflight returned HTTP 403" || checks.Load() != 2 {
		t.Fatalf("request reused an earlier authentication decision: checks=%d err=%v", checks.Load(), err)
	}
	if store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatal("authentication checks published a result")
	}
}
