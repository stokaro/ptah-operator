package resultdelivery

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreflightRetriesOccupiedUploadSlotWithoutPublishing(t *testing.T) {
	identity := testIdentity()
	store := testStore(t)
	certs := testCertificates(t, identity)
	var checks, requests atomic.Int32
	receiver := testReceiver(t, store, func(context.Context, Identity) error {
		checks.Add(1)
		return nil
	}, time.Second)
	server := startReceiver(t, receiver, certs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead {
				t.Error("preflight sent a publication request")
			}
			if requests.Add(1) == 1 {
				// Hold the real receiver's only upload slot for the first HEAD.
				receiver.slots <- struct{}{}
				next.ServeHTTP(w, r)
				<-receiver.slots
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	if err := testSender(t, server.URL, identity, certs).Check(t.Context()); err != nil {
		t.Fatalf("temporary upload contention aborted preflight: %v", err)
	}
	if requests.Load() != 2 || checks.Load() != 1 || store.Client.(*identifyingClient).count.Load() != 0 {
		t.Fatalf("requests=%d live checks=%d; preflight must retry authentication without publishing", requests.Load(), checks.Load())
	}
}

func TestPreflightRetryHonorsTotalDeadlineAndCancellation(t *testing.T) {
	identity := testIdentity()
	certs := testCertificates(t, identity)
	var calls atomic.Int32
	receiver := testReceiver(t, testStore(t), func(context.Context, Identity) error {
		calls.Add(1)
		return errors.New("API unavailable")
	}, time.Second)
	server := startReceiver(t, receiver, certs, nil)
	sender := testSender(t, server.URL, identity, certs)
	sender.retry = RetryPolicy{Attempts: 8, Interval: time.Second, AttemptTimeout: 50 * time.Millisecond, TotalTimeout: 100 * time.Millisecond}
	if err := sender.Check(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preflight did not retain its total deadline: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("preflight made %d authority checks across its deadline", calls.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sender.Check(ctx); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("canceled preflight reached the receiver or lost its cancellation: %v", err)
	}
}
