package kubeapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestStabilityBarrierDoesNotCountOpeningOrClosingProbeTime(t *testing.T) {
	clock := newBarrierClock()
	providerCalls := 0
	probeCalls := 0
	storedCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			providerCalls++
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
		},
		Probe: func(_ context.Context, _ Endpoint) (bool, error) {
			probeCalls++
			clock.advance(4 * time.Second)
			return true, nil
		},
		StoredContract: func(context.Context) (string, bool, error) {
			storedCalls++
			return "contract-a", true, nil
		},
		StabilityDuration: 5 * time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Minute,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if probeCalls != 3 {
		t.Fatalf("endpoint probe calls = %d, want 3; a long probe satisfied the stability duration", probeCalls)
	}
	if providerCalls != 4 || storedCalls != 4 {
		t.Fatalf("provider/stored calls = %d/%d, want 4/4 including final closing checks", providerCalls, storedCalls)
	}
	if got, want := clock.elapsed(), 14*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s", got, want)
	}
}

func TestStabilityBarrierResetsOnClosingTopologyChurn(t *testing.T) {
	clock := newBarrierClock()
	snapshots := []Snapshot{
		barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"),
		barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"),
		barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"),
		// A selected EndpointSlice UID or resourceVersion change is reflected
		// by InventoryIdentity and invalidates the opening proof.
		barrierSnapshot("rv-b", "inventory-b", "10.0.0.1:6443"),
		barrierSnapshot("rv-b", "inventory-b", "10.0.0.1:6443"),
		barrierSnapshot("rv-b", "inventory-b", "10.0.0.1:6443"),
		barrierSnapshot("rv-b", "inventory-b", "10.0.0.1:6443"),
		barrierSnapshot("rv-b", "inventory-b", "10.0.0.1:6443"),
	}
	providerCalls := 0
	probeCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			if providerCalls >= len(snapshots) {
				t.Fatal("provider called more times than scripted")
			}
			snapshot := snapshots[providerCalls]
			providerCalls++
			return snapshot, nil
		},
		Probe: func(context.Context, Endpoint) (bool, error) {
			probeCalls++
			return true, nil
		},
		StabilityDuration: 2 * time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Minute,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 8 || probeCalls != 6 {
		t.Fatalf("provider/probe calls = %d/%d, want 8/6 after topology reset", providerCalls, probeCalls)
	}
	if got, want := clock.elapsed(), 5*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s", got, want)
	}
}

func TestStabilityBarrierIgnoresCollectionResourceVersionOnlyChurn(t *testing.T) {
	clock := newBarrierClock()
	snapshots := []Snapshot{
		barrierSnapshot("collection-rv-1", "selected-inventory-a", "10.0.0.1:6443"),
		barrierSnapshot("collection-rv-2", "selected-inventory-a", "10.0.0.1:6443"),
		barrierSnapshot("collection-rv-3", "selected-inventory-a", "10.0.0.1:6443"),
		barrierSnapshot("collection-rv-4", "selected-inventory-a", "10.0.0.1:6443"),
	}
	providerCalls := 0
	probeCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			snapshot := snapshots[providerCalls]
			providerCalls++
			return snapshot, nil
		},
		Probe: func(context.Context, Endpoint) (bool, error) {
			probeCalls++
			return true, nil
		},
		StabilityDuration: 2 * time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Minute,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 4 || probeCalls != 3 {
		t.Fatalf("provider/probe calls = %d/%d, want 4/3 without an unrelated-churn reset", providerCalls, probeCalls)
	}
	if got, want := clock.elapsed(), 2*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s", got, want)
	}
}

func TestStabilityBarrierRetriesEndpointDeadlineAndCoversSnapshot(t *testing.T) {
	clock := newBarrierClock()
	firstEndpointCalls := 0
	secondEndpointCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443", "10.0.0.2:6443"), nil
		},
		Probe: func(probeCtx context.Context, endpoint Endpoint) (bool, error) {
			switch endpoint.Address {
			case "10.0.0.1:6443":
				firstEndpointCalls++
				if firstEndpointCalls == 1 {
					<-probeCtx.Done()
					return false, fmt.Errorf("request: %w", probeCtx.Err())
				}
			case "10.0.0.2:6443":
				secondEndpointCalls++
			default:
				t.Fatalf("unexpected endpoint %q", endpoint.Address)
			}
			return true, nil
		},
		StabilityDuration: time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    5 * time.Millisecond,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if firstEndpointCalls != 3 || secondEndpointCalls != 3 {
		t.Fatalf("endpoint calls = %d/%d, want 3/3; every endpoint must be covered after a timeout", firstEndpointCalls, secondEndpointCalls)
	}
}

func TestStabilityBarrierReturnsOrdinaryProbeError(t *testing.T) {
	wantErr := errors.New("malformed response")
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
		},
		Probe:             func(context.Context, Endpoint) (bool, error) { return false, wantErr },
		StabilityDuration: time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Second,
	}

	err := barrier.Wait(context.Background())
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "10.0.0.1:6443") {
		t.Fatalf("Wait() error = %v, want wrapped fatal probe error", err)
	}
}

func TestStabilityBarrierResetsWhenStoredContractChangesAtClose(t *testing.T) {
	clock := newBarrierClock()
	identities := []string{"contract-a", "contract-a", "contract-b", "contract-b", "contract-b", "contract-b"}
	storedCalls := 0
	providerCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			providerCalls++
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
		},
		Probe: func(context.Context, Endpoint) (bool, error) { return true, nil },
		StoredContract: func(context.Context) (string, bool, error) {
			if storedCalls >= len(identities) {
				t.Fatal("stored-contract probe called more times than scripted")
			}
			identity := identities[storedCalls]
			storedCalls++
			return identity, true, nil
		},
		StabilityDuration: time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Minute,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if storedCalls != 6 || providerCalls != 5 {
		t.Fatalf("stored/provider calls = %d/%d, want 6/5 after closing contract reset", storedCalls, providerCalls)
	}
	if got, want := clock.elapsed(), 3*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s", got, want)
	}
}

func TestStabilityBarrierRetriesProviderErrorsAndInconclusiveResults(t *testing.T) {
	clock := newBarrierClock()
	providerCalls := 0
	probeCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			providerCalls++
			if providerCalls == 1 {
				return Snapshot{}, errors.New("discovery unavailable")
			}
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
		},
		Probe: func(context.Context, Endpoint) (bool, error) {
			probeCalls++
			return probeCalls != 1, nil
		},
		StabilityDuration: time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Minute,
	}

	if err := barrier.wait(context.Background(), clock.now, clock.sleep); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if providerCalls != 5 || probeCalls != 3 {
		t.Fatalf("provider/probe calls = %d/%d, want 5/3 after two stability resets", providerCalls, probeCalls)
	}
}

func TestStabilityBarrierReturnsOrdinaryStoredContractError(t *testing.T) {
	wantErr := errors.New("foreign object")
	providerCalls := 0
	barrier := &StabilityBarrier{
		Provider: func(context.Context) (Snapshot, error) {
			providerCalls++
			return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
		},
		Probe: func(context.Context, Endpoint) (bool, error) { return true, nil },
		StoredContract: func(context.Context) (string, bool, error) {
			return "", false, wantErr
		},
		StabilityDuration: time.Second,
		PollEvery:         time.Second,
		RequestTimeout:    time.Second,
	}

	err := barrier.Wait(context.Background())
	if !errors.Is(err, wantErr) || providerCalls != 0 {
		t.Fatalf("Wait() error/provider calls = %v/%d, want fatal stored error before the sweep", err, providerCalls)
	}
}

func TestStabilityBarrierRejectsInvalidConfigurationAndSnapshots(t *testing.T) {
	valid := func() *StabilityBarrier {
		return &StabilityBarrier{
			Provider: func(context.Context) (Snapshot, error) {
				return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443"), nil
			},
			Probe:             func(context.Context, Endpoint) (bool, error) { return true, nil },
			StabilityDuration: time.Second,
			PollEvery:         time.Second,
			RequestTimeout:    time.Second,
		}
	}
	tests := []struct {
		name    string
		context context.Context
		barrier func() *StabilityBarrier
		want    string
	}{
		{name: "nil context", barrier: valid, want: "context is nil"},
		{name: "nil barrier", context: context.Background(), barrier: func() *StabilityBarrier { return nil }, want: "barrier is nil"},
		{name: "nil provider", context: context.Background(), barrier: func() *StabilityBarrier { b := valid(); b.Provider = nil; return b }, want: "provider and probe"},
		{name: "nil probe", context: context.Background(), barrier: func() *StabilityBarrier { b := valid(); b.Probe = nil; return b }, want: "provider and probe"},
		{name: "zero stability", context: context.Background(), barrier: func() *StabilityBarrier { b := valid(); b.StabilityDuration = 0; return b }, want: "timing values"},
		{name: "empty resourceVersion", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.Provider = func(context.Context) (Snapshot, error) {
				return barrierSnapshot("", "inventory-a", "10.0.0.1:6443"), nil
			}
			return b
		}, want: "resourceVersion"},
		{name: "padded identity", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.Provider = func(context.Context) (Snapshot, error) {
				return barrierSnapshot("rv-a", "inventory-a ", "10.0.0.1:6443"), nil
			}
			return b
		}, want: "inventory identity"},
		{name: "empty endpoints", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.Provider = func(context.Context) (Snapshot, error) { return barrierSnapshot("rv-a", "inventory-a"), nil }
			return b
		}, want: "snapshot is empty"},
		{name: "duplicate endpoint", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.Provider = func(context.Context) (Snapshot, error) {
				return barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443", "10.0.0.1:6443"), nil
			}
			return b
		}, want: "duplicated"},
		{name: "nil endpoint config", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.Provider = func(context.Context) (Snapshot, error) {
				snapshot := barrierSnapshot("rv-a", "inventory-a", "10.0.0.1:6443")
				snapshot.Endpoints[0].RESTConfig = nil
				return snapshot, nil
			}
			return b
		}, want: "incomplete endpoint"},
		{name: "empty stored identity", context: context.Background(), barrier: func() *StabilityBarrier {
			b := valid()
			b.StoredContract = func(context.Context) (string, bool, error) { return "", true, nil }
			return b
		}, want: "contract identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := test.context
			err := test.barrier().Wait(ctx)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Wait() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestStabilityKeyIgnoresEndpointOrderAndCollectionResourceVersion(t *testing.T) {
	first, err := newStabilityKey(barrierSnapshot("rv-a", "inventory-a", "10.0.0.2:6443", "10.0.0.1:6443"), "contract-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newStabilityKey(barrierSnapshot("rv-b", "inventory-a", "10.0.0.1:6443", "10.0.0.2:6443"), "contract-a")
	if err != nil {
		t.Fatal(err)
	}
	changedInventory := second
	changedInventory.inventory = "inventory-b"
	changedAddress := second
	changedAddress.addresses = []string{"10.0.0.1:6443", "10.0.0.3:6443"}
	if !first.equal(second) || changedInventory.equal(second) || changedAddress.equal(second) {
		t.Fatalf(
			"stability key equality mishandled collection churn or selected topology: first=%#v second=%#v inventory=%#v address=%#v",
			first,
			second,
			changedInventory,
			changedAddress,
		)
	}
	if !reflect.DeepEqual(first.addresses, []string{"10.0.0.1:6443", "10.0.0.2:6443"}) {
		t.Fatalf("canonical endpoint addresses = %#v", first.addresses)
	}
}

type barrierClock struct {
	start   time.Time
	current time.Time
}

func newBarrierClock() *barrierClock {
	start := time.Unix(1_000, 0)
	return &barrierClock{start: start, current: start}
}

func (c *barrierClock) now() time.Time {
	return c.current
}

func (c *barrierClock) sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(duration)
	return nil
}

func (c *barrierClock) advance(duration time.Duration) {
	c.current = c.current.Add(duration)
}

func (c *barrierClock) elapsed() time.Duration {
	return c.current.Sub(c.start)
}

func barrierSnapshot(resourceVersion, identity string, addresses ...string) Snapshot {
	endpoints := make([]Endpoint, 0, len(addresses))
	for _, address := range addresses {
		endpoints = append(endpoints, Endpoint{Address: address, RESTConfig: &rest.Config{Host: "https://" + address}})
	}
	return Snapshot{
		InventoryResourceVersion: resourceVersion,
		InventoryIdentity:        identity,
		Endpoints:                endpoints,
	}
}
