package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stored-contract verification that runs out of its own request budget
// proved nothing about the contract. The client rate limiter reports that
// deadline in its own words rather than as context.DeadlineExceeded, and the
// sweep it interrupted is the barrier's to retry, not a drift to refuse.
func TestAdmissionConvergenceBarrierRetriesAVerificationThatOutranItsBudget(t *testing.T) {
	t.Parallel()
	clock := newAdmissionBarrierClock()
	probe := &scriptedAdmissionProbe{results: []bool{true}}
	endpoints := admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{
		"https://10.0.0.1:6443": probe,
	})
	barrier := testAdmissionBarrier(clock, endpoints, nil)
	barrier.requestTimeout = time.Millisecond
	storedCalls := 0
	barrier.verifyStored = func(ctx context.Context) error {
		storedCalls++
		if storedCalls == 3 {
			<-ctx.Done()
			return errors.New("get admission convergence dependency binding x: client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
		}
		return nil
	}
	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() refused a sweep the client rate limiter cut short: %v", err)
	}
	if storedCalls <= 3 {
		t.Fatalf("stored verification calls = %d, want the interrupted attempt to be retried", storedCalls)
	}
}
