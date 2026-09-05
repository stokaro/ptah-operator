package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestAdmissionConvergenceBarrierRequiresStableAllEndpointWindow(t *testing.T) {
	t.Parallel()

	clock := newAdmissionBarrierClock()
	first := &scriptedAdmissionProbe{results: []bool{true}}
	second := &scriptedAdmissionProbe{results: []bool{false, false, true}}
	endpoints := admissionBarrierEndpoints("topology-a", map[string]*scriptedAdmissionProbe{
		"https://10.0.0.1:6443": first,
		"https://10.0.0.2:6443": second,
	})
	barrier := testAdmissionBarrier(clock, endpoints, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
		return endpoints, nil
	})
	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.now().Sub(clock.start), 7*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s (full window after stale endpoint recovered)", got, want)
	}
	if first.calls != 8 || second.calls != 8 {
		t.Fatalf("endpoint probe calls = %d/%d, want 8/8", first.calls, second.calls)
	}
}

func TestAdmissionConvergenceBarrierResetsOnTopologyChurn(t *testing.T) {
	t.Parallel()

	clock := newAdmissionBarrierClock()
	probe := &scriptedAdmissionProbe{results: []bool{true}}
	a := admissionBarrierEndpoints("topology-a", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
	b := admissionBarrierEndpoints("topology-b", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
	providerCalls := 0
	barrier := testAdmissionBarrier(clock, a, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
		providerCalls++
		if providerCalls < 5 {
			return a, nil
		}
		return b, nil
	})
	if err := barrier.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.now().Sub(clock.start), 7*time.Second; got != want {
		t.Fatalf("barrier elapsed = %s, want %s after topology reset", got, want)
	}
}

func TestContinuousCredentialWindowResetsAtSecond64OnEndpointState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		barrier     func(*admissionBarrierClock) *admissionConvergenceBarrier
		wantElapsed time.Duration
	}{
		{
			name:        "stale endpoint response",
			wantElapsed: 130 * time.Second,
			barrier: func(clock *admissionBarrierClock) *admissionConvergenceBarrier {
				calls := 0
				endpoint := namedAdmissionConvergenceProbe{
					name: "https://10.0.0.1:6443", topologyIdentity: "topology-a",
					probe: func(context.Context) (bool, error) {
						calls++
						return calls != 65, nil
					},
				}
				return testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{endpoint}, nil)
			},
		},
		{
			name:        "topology change",
			wantElapsed: 129 * time.Second,
			barrier: func(clock *admissionBarrierClock) *admissionConvergenceBarrier {
				probe := &scriptedAdmissionProbe{results: []bool{true}}
				a := admissionBarrierEndpoints("topology-a", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
				b := admissionBarrierEndpoints("topology-b", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
				calls := 0
				return testAdmissionBarrier(clock, a, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
					calls++
					if calls < 129 {
						return a, nil
					}
					return b, nil
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := newAdmissionBarrierClock()
			observer := &constantAdmissionStabilityObserver{identity: "protected-pods:1:10", proven: true}
			if err := test.barrier(clock).WaitWithStabilityObserver(
				context.Background(),
				retiredCredentialRevocationDelay,
				observer,
			); err != nil {
				t.Fatal(err)
			}
			if got, want := clock.now().Sub(clock.start), test.wantElapsed; got != want {
				t.Fatalf("joint credential window elapsed = %s, want %s after t=64s reset", got, want)
			}
			if observer.closeCalls != 1 {
				t.Fatalf("observer Close calls = %d, want 1", observer.closeCalls)
			}
		})
	}
}

func TestAdmissionConvergenceBarrierRetriesDiscoveryAndRequestTimeouts(t *testing.T) {
	t.Parallel()

	t.Run("discovery", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probe := &scriptedAdmissionProbe{results: []bool{true}}
		endpoints := admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
		calls := 0
		barrier := testAdmissionBarrier(clock, nil, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("temporary discovery outage")
			}
			return endpoints, nil
		})
		if err := barrier.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := clock.now().Sub(clock.start), 6*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want %s after discovery retry", got, want)
		}
	})

	t.Run("request deadline", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		ctx, cancel := context.WithCancel(context.Background())
		probeCalls := 0
		probe := namedAdmissionConvergenceProbe{
			name: "https://10.0.0.1:6443", topologyIdentity: "topology",
			probe: func(probeCtx context.Context) (bool, error) {
				probeCalls++
				<-probeCtx.Done()
				return false, probeCtx.Err()
			},
		}
		barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{probe}, nil)
		barrier.requestTimeout = time.Millisecond
		barrier.sleep = func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}
		err := barrier.Wait(ctx)
		if !errors.Is(err, context.Canceled) || probeCalls != 1 {
			t.Fatalf("Wait() = %v after %d probes, want one retryable deadline then cancellation", err, probeCalls)
		}
	})
}

func TestAdmissionConvergenceBarrierRetriesInitialEndpointSliceTransitions(t *testing.T) {
	t.Parallel()

	ready := validAPIEndpointSlice(
		"kubernetes",
		discoveryv1.AddressTypeIPv4,
		6443,
		discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}},
	)
	notReady := false
	notReadySlice := validAPIEndpointSlice(
		"kubernetes",
		discoveryv1.AddressTypeIPv4,
		6443,
		discoveryv1.Endpoint{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: &notReady},
		},
	)
	validList := oneEndpointSlicePage(ready)[""]
	tests := []struct {
		name    string
		initial endpointSliceListResult
	}{
		{
			name: "timeout",
			initial: endpointSliceListResult{err: apierrors.NewServerTimeout(
				schema.GroupResource{Resource: "endpointslices"},
				"list",
				1,
			)},
		},
		{
			name:    "no ready endpoint",
			initial: endpointSliceListResult{list: oneEndpointSlicePage(notReadySlice)[""]},
		},
		{
			name:    "empty inventory",
			initial: endpointSliceListResult{list: &discoveryv1.EndpointSliceList{ListMeta: metav1.ListMeta{ResourceVersion: "41"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lister := &sequencedEndpointSliceLister{results: []endpointSliceListResult{
				test.initial,
				{list: validList},
			}}
			barrier, err := newMarkerAdmissionConvergenceBarrier(
				context.Background(),
				validRBACRESTConfig(),
				lister,
				&crdupgrade.AdmissionConvergenceGuard{},
				nil,
				func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
					return inertAdmissionMarkerClient{}, nil
				},
				func(context.Context, crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) { return true, nil },
			)
			if err != nil {
				t.Fatalf("construct barrier: %v", err)
			}
			if lister.calls != 0 {
				t.Fatalf("constructor performed %d eager EndpointSlice LISTs, want 0", lister.calls)
			}
			clock := newAdmissionBarrierClock()
			barrier.pollEvery = time.Second
			barrier.stabilityDuration = 5 * time.Second
			barrier.requestTimeout = time.Second
			barrier.now = clock.now
			barrier.sleep = clock.sleep
			if err := barrier.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, want := clock.now().Sub(clock.start), 6*time.Second; got != want {
				t.Fatalf("barrier elapsed = %s, want %s after initial EndpointSlice transition", got, want)
			}
			if lister.calls != 13 {
				t.Fatalf("EndpointSlice LIST calls = %d, want 13", lister.calls)
			}
		})
	}
}

func TestAdmissionConvergenceBuildersDeferEndpointDiscovery(t *testing.T) {
	t.Parallel()

	lister := &sequencedEndpointSliceLister{results: []endpointSliceListResult{{err: errors.New("EndpointSlice API is starting")}}}
	barrier, err := newMarkerAdmissionConvergenceBarrier(
		context.Background(),
		validRBACRESTConfig(),
		lister,
		&crdupgrade.AdmissionConvergenceGuard{},
		nil,
		func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
			return inertAdmissionMarkerClient{}, nil
		},
		func(context.Context, crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) { return true, nil },
	)
	if err != nil || barrier == nil {
		t.Fatalf("newMarkerAdmissionConvergenceBarrier() = %#v, %v, want deferred barrier", barrier, err)
	}
	certificateBarrier, err := newCertificateRecoveryConvergenceBarrier(
		context.Background(),
		validRBACRESTConfig(),
		lister,
		"ptah-system",
		"certificate-recovery",
		"certificate-recovery",
		"webhook-certificate",
	)
	if err != nil || certificateBarrier == nil {
		t.Fatalf("newCertificateRecoveryConvergenceBarrier() = %#v, %v, want deferred barrier", certificateBarrier, err)
	}
	if lister.calls != 0 {
		t.Fatalf("admission builders performed %d eager EndpointSlice LISTs, want 0", lister.calls)
	}
}

func TestWaitForStoredAdmissionConvergenceClassifiesInitialState(t *testing.T) {
	t.Parallel()

	t.Run("transient GET then exact", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		calls := 0
		err := waitForStoredAdmissionConvergence(context.Background(), time.Second, time.Second, clock.sleep, func(context.Context) error {
			calls++
			if calls == 1 {
				return apierrors.NewServerTimeout(schema.GroupResource{Resource: "configmaps"}, "get", 1)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || clock.now().Sub(clock.start) != time.Second {
			t.Fatalf("verify calls/elapsed = %d/%s, want 2/1s", calls, clock.now().Sub(clock.start))
		}
	})

	t.Run("foreign stored object is fatal", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		want := errors.New("foreign stored policy")
		err := waitForStoredAdmissionConvergence(context.Background(), time.Second, time.Second, clock.sleep, func(context.Context) error {
			return want
		})
		if !errors.Is(err, want) || clock.now() != clock.start {
			t.Fatalf("wait error/time = %v/%s, want immediate foreign error", err, clock.now())
		}
	})

	t.Run("cancellation wins during initial retry", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		ctx, cancel := context.WithCancel(context.Background())
		err := waitForStoredAdmissionConvergence(ctx, time.Second, time.Second, clock.sleep, func(context.Context) error {
			cancel()
			return apierrors.NewServerTimeout(schema.GroupResource{Resource: "configmaps"}, "get", 1)
		})
		if !errors.Is(err, context.Canceled) || clock.now() != clock.start {
			t.Fatalf("wait error/time = %v/%s, want immediate context cancellation", err, clock.now())
		}
	})
}

func TestAdmissionConvergenceBarrierCancellationWinsDuringInitialDiscovery(t *testing.T) {
	t.Parallel()

	clock := newAdmissionBarrierClock()
	ctx, cancel := context.WithCancel(context.Background())
	barrier := testAdmissionBarrier(clock, nil, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
		cancel()
		return nil, errors.New("foreign discovery response")
	})
	err := barrier.Wait(ctx)
	if !errors.Is(err, context.Canceled) || clock.now() != clock.start {
		t.Fatalf("Wait() error/time = %v/%s, want immediate context cancellation", err, clock.now())
	}
}

func TestAdmissionConvergenceBarrierJoinsRecurringStoredContractVerification(t *testing.T) {
	t.Parallel()

	t.Run("transient storage miss resets full window", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probe := &scriptedAdmissionProbe{results: []bool{true}}
		endpoints := admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{
			"https://10.0.0.1:6443": probe,
		})
		barrier := testAdmissionBarrier(clock, endpoints, nil)
		storedCalls := 0
		barrier.verifyStored = func(context.Context) error {
			storedCalls++
			if storedCalls == 10 {
				return apierrors.NewServerTimeout(schema.GroupResource{Resource: "validatingadmissionpolicies"}, "get", 1)
			}
			return nil
		}
		if err := barrier.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := clock.now().Sub(clock.start), 10*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want %s after stored-contract reset at t=4s", got, want)
		}
		if storedCalls != 22 || probe.calls != 11 {
			t.Fatalf("stored/endpoint calls = %d/%d, want 22/11", storedCalls, probe.calls)
		}
	})

	t.Run("foreign replacement fails while cached denial remains exact", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probe := &scriptedAdmissionProbe{results: []bool{true}}
		endpoints := admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{
			"https://10.0.0.1:6443": probe,
		})
		barrier := testAdmissionBarrier(clock, endpoints, nil)
		storedCalls := 0
		barrier.verifyStored = func(context.Context) error {
			storedCalls++
			if storedCalls == 9 {
				return errors.New("workload dependency policy has foreign stored spec")
			}
			return nil
		}
		err := barrier.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "foreign stored spec") {
			t.Fatalf("Wait() error = %v, want foreign stored replacement failure", err)
		}
		if got, want := clock.now().Sub(clock.start), 4*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want stop at t=4s", got)
		}
		if probe.calls != 4 {
			t.Fatalf("cached exact endpoint denial probes = %d, want 4 before stored replacement", probe.calls)
		}
	})
}

func TestAdmissionConvergenceBarrierClosesEverySuccessfulSweep(t *testing.T) {
	t.Parallel()

	t.Run("topology changes on final eligible closing discovery", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probe := &scriptedAdmissionProbe{results: []bool{true}}
		a := admissionBarrierEndpoints("topology-a", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
		b := admissionBarrierEndpoints("topology-b", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
		providerCalls := 0
		barrier := testAdmissionBarrier(clock, nil, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
			providerCalls++
			if providerCalls < 12 {
				return a, nil
			}
			return b, nil
		})
		if err := barrier.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := clock.now().Sub(clock.start), 11*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want %s after eligible closing topology change", got, want)
		}
		if providerCalls != 24 || probe.calls != 12 {
			t.Fatalf("provider/probe calls = %d/%d, want 24/12", providerCalls, probe.calls)
		}
	})

	t.Run("stored contract changes while endpoint probe runs", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		replaced := false
		barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{{
			name: "https://10.0.0.1:6443", topologyIdentity: "topology",
			probe: func(context.Context) (bool, error) {
				replaced = true
				return true, nil
			},
		}}, nil)
		storedCalls := 0
		barrier.verifyStored = func(context.Context) error {
			storedCalls++
			if replaced {
				return errors.New("stored policy was replaced during endpoint probe")
			}
			return nil
		}
		err := barrier.Wait(context.Background())
		if err == nil || !strings.Contains(err.Error(), "replaced during endpoint probe") {
			t.Fatalf("Wait() error = %v, want closing stored replacement failure", err)
		}
		if storedCalls != 2 || clock.now() != clock.start {
			t.Fatalf("stored calls/time = %d/%s, want 2/%s", storedCalls, clock.now(), clock.start)
		}
	})

	t.Run("first long sweep cannot consume stability window", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probeCalls := 0
		barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{{
			name: "https://10.0.0.1:6443", topologyIdentity: "topology",
			probe: func(context.Context) (bool, error) {
				probeCalls++
				if probeCalls == 1 {
					clock.current = clock.current.Add(10 * time.Second)
				}
				return true, nil
			},
		}}, nil)
		if err := barrier.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := clock.now().Sub(clock.start), 15*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want %s from first sweep close", got, want)
		}
		if probeCalls != 6 {
			t.Fatalf("endpoint probe calls = %d, want 6", probeCalls)
		}
	})

	t.Run("closing observer change resets full window", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name     string
			identity string
			proven   bool
		}{
			{name: "identity", identity: "protected-pods:b", proven: true},
			{name: "event", identity: "protected-pods:a"},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				clock := newAdmissionBarrierClock()
				observer := &scriptedAdmissionStabilityObserver{
					identities: []string{"protected-pods:a"},
					proven:     []bool{true},
					atCall: map[int]admissionStabilityObservation{
						12: {identity: test.identity, proven: test.proven},
					},
				}
				barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{{
					name: "https://10.0.0.1:6443", topologyIdentity: "topology", probe: alwaysAdmissionProbe(true),
				}}, nil)
				if err := barrier.WaitWithStabilityObserver(context.Background(), 5*time.Second, observer); err != nil {
					t.Fatal(err)
				}
				if got, want := clock.now().Sub(clock.start), 11*time.Second; got != want {
					t.Fatalf("barrier elapsed = %s, want %s after closing observer reset", got, want)
				}
				if observer.calls != 24 || observer.closeCalls != 1 {
					t.Fatalf("observer calls/close calls = %d/%d, want 24/1", observer.calls, observer.closeCalls)
				}
			})
		}
	})

	t.Run("transient closing discovery resets full window", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		probe := &scriptedAdmissionProbe{results: []bool{true}}
		endpoints := admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{"https://10.0.0.1:6443": probe})
		providerCalls := 0
		barrier := testAdmissionBarrier(clock, nil, func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
			providerCalls++
			if providerCalls == 2 {
				return nil, apierrors.NewServerTimeout(schema.GroupResource{Resource: "endpointslices"}, "list", 1)
			}
			return endpoints, nil
		})
		if err := barrier.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := clock.now().Sub(clock.start), 6*time.Second; got != want {
			t.Fatalf("barrier elapsed = %s, want %s after closing discovery retry", got, want)
		}
		if providerCalls != 14 || probe.calls != 7 {
			t.Fatalf("provider/probe calls = %d/%d, want 14/7", providerCalls, probe.calls)
		}
	})
}

func TestAdmissionConvergenceBarrierCancellationWinsDuringSweepClose(t *testing.T) {
	t.Parallel()

	t.Run("reset sleep", func(t *testing.T) {
		t.Parallel()
		clock := newAdmissionBarrierClock()
		ctx, cancel := context.WithCancel(context.Background())
		barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{{
			name: "https://10.0.0.1:6443", topologyIdentity: "topology", probe: alwaysAdmissionProbe(false),
		}}, nil)
		barrier.sleep = func(context.Context, time.Duration) error {
			cancel()
			return errors.New("sentinel sleep error")
		}
		if err := barrier.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() error = %v, want context cancellation before sleep error", err)
		}
	})

	for _, test := range []struct {
		name      string
		configure func(*admissionConvergenceBarrier, context.CancelFunc) admissionConvergenceStabilityObserver
	}{
		{
			name: "stored verification",
			configure: func(barrier *admissionConvergenceBarrier, cancel context.CancelFunc) admissionConvergenceStabilityObserver {
				calls := 0
				barrier.verifyStored = func(context.Context) error {
					calls++
					if calls == 2 {
						cancel()
						return errors.New("foreign stored object")
					}
					return nil
				}
				return nil
			},
		},
		{
			name: "endpoint discovery",
			configure: func(barrier *admissionConvergenceBarrier, cancel context.CancelFunc) admissionConvergenceStabilityObserver {
				calls := 0
				endpoints := barrier.endpoints
				barrier.endpointProvider = func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
					calls++
					if calls == 2 {
						cancel()
						return nil, errors.New("foreign discovery response")
					}
					return endpoints, nil
				}
				return nil
			},
		},
		{
			name: "stability observer",
			configure: func(_ *admissionConvergenceBarrier, cancel context.CancelFunc) admissionConvergenceStabilityObserver {
				return &scriptedAdmissionStabilityObserver{
					identities: []string{"protected-pods:a"}, proven: []bool{true}, cancelAt: 2, cancel: cancel,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := newAdmissionBarrierClock()
			ctx, cancel := context.WithCancel(context.Background())
			barrier := testAdmissionBarrier(clock, []namedAdmissionConvergenceProbe{{
				name: "https://10.0.0.1:6443", topologyIdentity: "topology", probe: alwaysAdmissionProbe(true),
			}}, nil)
			observer := test.configure(barrier, cancel)
			var err error
			if observer == nil {
				err = barrier.Wait(ctx)
			} else {
				err = barrier.WaitWithStabilityObserver(ctx, 5*time.Second, observer)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait() error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestAdmissionConvergenceBarrierRejectsInvalidTopologyAndProbeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		endpoints []namedAdmissionConvergenceProbe
		provider  admissionConvergenceEndpointProvider
		want      string
	}{
		{name: "missing", want: "endpoints are empty"},
		{
			name: "duplicate",
			endpoints: []namedAdmissionConvergenceProbe{
				{name: "https://10.0.0.1:6443", topologyIdentity: "same", probe: alwaysAdmissionProbe(true)},
				{name: "https://10.0.0.1:6443", topologyIdentity: "same", probe: alwaysAdmissionProbe(true)},
			},
			want: "duplicated",
		},
		{
			name: "mixed topology",
			endpoints: []namedAdmissionConvergenceProbe{
				{name: "https://10.0.0.1:6443", topologyIdentity: "a", probe: alwaysAdmissionProbe(true)},
				{name: "https://10.0.0.2:6443", topologyIdentity: "b", probe: alwaysAdmissionProbe(true)},
			},
			want: "do not share",
		},
		{
			name: "refreshed duplicate",
			endpoints: []namedAdmissionConvergenceProbe{
				{name: "https://10.0.0.1:6443", topologyIdentity: "a", probe: alwaysAdmissionProbe(true)},
			},
			provider: func(context.Context) ([]namedAdmissionConvergenceProbe, error) {
				return []namedAdmissionConvergenceProbe{
					{name: "https://10.0.0.2:6443", topologyIdentity: "b", probe: alwaysAdmissionProbe(true)},
					{name: "https://10.0.0.2:6443", topologyIdentity: "b", probe: alwaysAdmissionProbe(true)},
				}, nil
			},
			want: "duplicated",
		},
		{
			name: "unexpected denial",
			endpoints: []namedAdmissionConvergenceProbe{{
				name: "https://10.0.0.9:6443", topologyIdentity: "a",
				probe: func(context.Context) (bool, error) { return false, errors.New("wrong denial") },
			}},
			want: "https://10.0.0.9:6443",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := newAdmissionBarrierClock()
			barrier := testAdmissionBarrier(clock, test.endpoints, test.provider)
			err := barrier.Wait(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Wait() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestMarkerAdmissionEndpointProviderUsesCanonicalIPv6AndTopologyIdentity(t *testing.T) {
	t.Parallel()

	snapshots := []kubernetesAPIServerEndpointSnapshot{
		{
			InventoryResourceVersion: "collection-rv-1", InventoryIdentity: "selected-items-a",
			Endpoints: []kubernetesAPIServerEndpoint{{Address: "https://[2001:db8::1]:6443", RESTConfig: &rest.Config{Host: "https://[2001:db8::1]:6443"}}},
		},
		{
			InventoryResourceVersion: "collection-rv-2", InventoryIdentity: "selected-items-a",
			Endpoints: []kubernetesAPIServerEndpoint{{Address: "https://[2001:db8::1]:6443", RESTConfig: &rest.Config{Host: "https://[2001:db8::1]:6443"}}},
		},
		{
			InventoryResourceVersion: "collection-rv-3", InventoryIdentity: "selected-items-b",
			Endpoints: []kubernetesAPIServerEndpoint{{Address: "https://[2001:db8::1]:6443", RESTConfig: &rest.Config{Host: "https://[2001:db8::1]:6443"}}},
		},
	}
	index := 0
	apiProvider := func(context.Context) (kubernetesAPIServerEndpointSnapshot, error) {
		result := snapshots[min(index, len(snapshots)-1)]
		index++
		return result, nil
	}
	factoryHosts := []string{}
	provider := newMarkerAdmissionEndpointProvider(
		apiProvider,
		&crdupgrade.AdmissionConvergenceGuard{},
		func(config *rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
			factoryHosts = append(factoryHosts, config.Host)
			return inertAdmissionMarkerClient{}, nil
		},
		func(context.Context, crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) { return true, nil },
	)
	for call := range 3 {
		endpoints, err := provider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(endpoints) != 1 || endpoints[0].name != "https://[2001:db8::1]:6443" || endpoints[0].topologyIdentity != snapshots[call].InventoryIdentity {
			t.Fatalf("provider call %d = %#v", call, endpoints)
		}
	}
	wantHosts := []string{"https://[2001:db8::1]:6443", "https://[2001:db8::1]:6443"}
	if !reflect.DeepEqual(factoryHosts, wantHosts) {
		t.Fatalf("direct client hosts = %v, want %v; collection RV alone must not recreate clients", factoryHosts, wantHosts)
	}
}

func TestDirectCertificateRecoveryProbeRequiresExactAttributedDenial(t *testing.T) {
	t.Parallel()

	const (
		policyName  = "certificate-recovery-policy"
		bindingName = "certificate-recovery-binding"
		probeName   = "webhook-certificate-admission-probe-"
	)
	exactDenial := directAdmissionPolicyDenialError(policyName, bindingName, certificateRecoveryGuardDenialMessage)
	tests := []struct {
		name      string
		ctx       func() context.Context
		createErr error
		want      bool
		wantError string
	}{
		{name: "exact denial", createErr: exactDenial, want: true},
		{name: "admitted is inconclusive"},
		{
			name:      "wrong denial is fatal",
			createErr: directAdmissionPolicyDenialError(policyName, bindingName, "foreign denial"),
			wantError: "unexpected response",
		},
		{
			name:      "server timeout retries",
			createErr: apierrors.NewServerTimeout(schema.GroupResource{Resource: "secrets"}, "create", 1),
		},
		{
			name: "canceled context wins",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			createErr: context.Canceled,
			wantError: context.Canceled.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			secrets := &recordingSecretCreateClient{createErr: test.createErr}
			client := &directSecretCreateProbeClient{secrets: secrets}
			ctx := context.Background()
			if test.ctx != nil {
				ctx = test.ctx()
			}
			got, err := client.probe(ctx, policyName, bindingName, probeName)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("probe() error = %v, want containing %q", err, test.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("probe() = %t, want %t", got, test.want)
			}
			if secrets.object == nil || secrets.object.Name != "" || secrets.object.GenerateName != probeName || secrets.object.Type != corev1.SecretTypeOpaque {
				t.Fatalf("probe Secret = %#v", secrets.object)
			}
			if !reflect.DeepEqual(secrets.options.DryRun, []string{metav1.DryRunAll}) {
				t.Fatalf("probe DryRun = %v, want All", secrets.options.DryRun)
			}
		})
	}
}

type recordingSecretCreateClient struct {
	object    *corev1.Secret
	options   metav1.CreateOptions
	createErr error
}

type endpointSliceListResult struct {
	list *discoveryv1.EndpointSliceList
	err  error
}

type sequencedEndpointSliceLister struct {
	results []endpointSliceListResult
	calls   int
}

func (l *sequencedEndpointSliceLister) List(_ context.Context, options metav1.ListOptions) (*discoveryv1.EndpointSliceList, error) {
	if options.Continue != "" {
		return nil, fmt.Errorf("unexpected continuation token %q", options.Continue)
	}
	index := min(l.calls, len(l.results)-1)
	l.calls++
	result := l.results[index]
	if result.err != nil {
		return nil, result.err
	}
	if result.list == nil {
		return nil, nil
	}
	return result.list.DeepCopy(), nil
}

func (c *recordingSecretCreateClient) Create(
	_ context.Context,
	object *corev1.Secret,
	options metav1.CreateOptions,
) (*corev1.Secret, error) {
	c.object = object.DeepCopy()
	c.options = options
	if c.createErr != nil {
		return nil, c.createErr
	}
	return object.DeepCopy(), nil
}

func directAdmissionPolicyDenialError(policyName, bindingName, denialMessage string) error {
	cause := fmt.Sprintf(
		"ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s",
		policyName,
		bindingName,
		denialMessage,
	)
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure,
		Reason: metav1.StatusReasonInvalid,
		Code:   422,
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
			Message: cause,
		}}},
	}}
}

type admissionBarrierClock struct {
	start   time.Time
	current time.Time
}

func newAdmissionBarrierClock() *admissionBarrierClock {
	start := time.Date(2026, time.September, 5, 0, 0, 0, 0, time.UTC)
	return &admissionBarrierClock{start: start, current: start}
}

func (c *admissionBarrierClock) now() time.Time { return c.current }

func (c *admissionBarrierClock) sleep(_ context.Context, duration time.Duration) error {
	c.current = c.current.Add(duration)
	return nil
}

type scriptedAdmissionProbe struct {
	results []bool
	errors  []error
	calls   int
}

type constantAdmissionStabilityObserver struct {
	identity   string
	proven     bool
	closeCalls int
}

type admissionStabilityObservation struct {
	identity string
	proven   bool
	err      error
}

type scriptedAdmissionStabilityObserver struct {
	identities []string
	proven     []bool
	errors     []error
	atCall     map[int]admissionStabilityObservation
	cancelAt   int
	cancel     context.CancelFunc
	calls      int
	closeCalls int
}

func (o *constantAdmissionStabilityObserver) Observe(context.Context, string) (string, bool, error) {
	return o.identity, o.proven, nil
}

func (o *constantAdmissionStabilityObserver) Close() {
	o.closeCalls++
}

func (o *scriptedAdmissionStabilityObserver) Observe(context.Context, string) (string, bool, error) {
	o.calls++
	if o.cancelAt == o.calls && o.cancel != nil {
		o.cancel()
	}
	if observation, ok := o.atCall[o.calls]; ok {
		return observation.identity, observation.proven, observation.err
	}
	index := o.calls - 1
	identity := ""
	if len(o.identities) != 0 {
		identity = o.identities[min(index, len(o.identities)-1)]
	}
	proven := false
	if len(o.proven) != 0 {
		proven = o.proven[min(index, len(o.proven)-1)]
	}
	var err error
	if len(o.errors) != 0 {
		err = o.errors[min(index, len(o.errors)-1)]
	}
	return identity, proven, err
}

func (o *scriptedAdmissionStabilityObserver) Close() {
	o.closeCalls++
}

func (p *scriptedAdmissionProbe) probe(context.Context) (bool, error) {
	index := min(p.calls, max(len(p.results), len(p.errors))-1)
	p.calls++
	var result bool
	if len(p.results) != 0 {
		result = p.results[min(index, len(p.results)-1)]
	}
	if len(p.errors) != 0 {
		return result, p.errors[min(index, len(p.errors)-1)]
	}
	return result, nil
}

func admissionBarrierEndpoints(topology string, probes map[string]*scriptedAdmissionProbe) []namedAdmissionConvergenceProbe {
	result := make([]namedAdmissionConvergenceProbe, 0, len(probes))
	for name, probe := range probes {
		result = append(result, namedAdmissionConvergenceProbe{name: name, topologyIdentity: topology, probe: probe.probe})
	}
	return result
}

func alwaysAdmissionProbe(result bool) admissionConvergenceProbe {
	return func(context.Context) (bool, error) { return result, nil }
}

func testAdmissionBarrier(
	clock *admissionBarrierClock,
	endpoints []namedAdmissionConvergenceProbe,
	provider admissionConvergenceEndpointProvider,
) *admissionConvergenceBarrier {
	return &admissionConvergenceBarrier{
		endpoints: endpoints, endpointProvider: provider,
		pollEvery: time.Second, stabilityDuration: 5 * time.Second, requestTimeout: time.Second,
		now: clock.now, sleep: clock.sleep,
	}
}

type inertAdmissionMarkerClient struct{}

func (inertAdmissionMarkerClient) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	return nil, errors.New("unexpected Get")
}

func (inertAdmissionMarkerClient) Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	return nil, errors.New("unexpected Update")
}

var _ crdupgrade.AdmissionConvergenceMarkerClient = inertAdmissionMarkerClient{}

func Example_admissionConvergenceEndpointSetKey() {
	endpoints := []namedAdmissionConvergenceProbe{
		{name: "https://10.0.0.2:6443", topologyIdentity: "items"},
		{name: "https://10.0.0.1:6443", topologyIdentity: "items"},
	}
	fmt.Println(admissionConvergenceEndpointSetKey(endpoints) == "items\x00https://10.0.0.1:6443\x00https://10.0.0.2:6443")
	// Output: true
}
