package resultdelivery

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestSenderRetriesAfterStalledConnection(t *testing.T) {
	for _, operation := range []string{"preflight", "delivery"} {
		t.Run(operation, func(t *testing.T) {
			identity := testIdentity()
			certs := testCertificates(t, identity)
			store := testStore(t)
			var requests atomic.Int32
			receiver := testReceiver(t, store, func(context.Context, Identity) error { return nil }, time.Second)
			server := startReceiver(t, receiver, certs, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					next.ServeHTTP(w, r)
				})
			})
			policy := RetryPolicy{Attempts: 4, Interval: 10 * time.Millisecond, AttemptTimeout: 250 * time.Millisecond, TotalTimeout: 2 * time.Second}
			sender, err := NewSender(server.URL, identity, &tls.Config{RootCAs: certs.roots, Certificates: []tls.Certificate{certs.client}}, policy)
			if err != nil {
				t.Fatal(err)
			}

			// Stall the first connection before TCP establishment. Transport
			// detaches its dial from request cancellation, so request deadlines
			// alone cannot free the only per-host connection slot. DNS is an
			// in-process way to hold a real net.Dialer without a firewall or an
			// external address whose reachability could change.
			originalResolver := net.DefaultResolver
			release := make(chan struct{})
			var lookups atomic.Int32
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				lookups.Add(1)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return nil, errors.New("test lookup released")
				}
			}}
			dial := sender.transport.DialContext
			if dial == nil {
				// Match Transport's nil-DialContext behavior so the original
				// unbounded connection fails this recovery test.
				dial = (&net.Dialer{}).DialContext
			}
			var dials atomic.Int32
			firstDone := make(chan struct{})
			sender.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					defer close(firstDone)
					return dial(ctx, network, "stalled-result-receiver.invalid:443")
				}
				return dial(ctx, network, address)
			}
			t.Cleanup(func() {
				close(release)
				sender.Close()
				select {
				case <-firstDone:
				case <-time.After(5 * time.Second):
					t.Error("the diagnostic dial did not stop")
				}
				net.DefaultResolver = originalResolver
			})

			if operation == "preflight" {
				err = sender.Check(t.Context())
			} else {
				_, err = sender.Send(t.Context(), testPayload(t, identity))
			}
			if err != nil {
				t.Fatalf("stalled connection blocked recovery: %v (dials=%d, requests=%d)", err, dials.Load(), requests.Load())
			}
			if lookups.Load() == 0 || dials.Load() != 2 || requests.Load() != 1 {
				t.Fatalf("recovery did not replace the stalled connection exactly once: lookups=%d dials=%d requests=%d", lookups.Load(), dials.Load(), requests.Load())
			}
			if operation == "preflight" && store.Client.(*identifyingClient).count.Load() != 0 {
				t.Fatal("preflight published a result")
			}
		})
	}
}
