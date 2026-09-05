package main

// These tests are white-box because direct endpoint fan-out and the final
// deletion order are package-local orchestration boundaries of the manager
// binary, rather than reusable public APIs.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestTeardownRetirementEndpointProviderProbesEveryFenceOnEveryEndpoint(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	marker.UID = "marker-uid"
	marker.ResourceVersion = "7"

	snapshots := []kubernetesAPIServerEndpointSnapshot{
		{
			InventoryIdentity: "topology-a",
			Endpoints: []kubernetesAPIServerEndpoint{
				{Address: "10.0.0.1:6443", RESTConfig: &rest.Config{Host: "https://10.0.0.1:6443"}},
				{Address: "10.0.0.2:6443", RESTConfig: &rest.Config{Host: "https://10.0.0.2:6443"}},
			},
		},
		{
			InventoryIdentity: "topology-b",
			Endpoints: []kubernetesAPIServerEndpoint{
				{Address: "10.0.0.1:6443", RESTConfig: &rest.Config{Host: "https://10.0.0.1:6443"}},
				{Address: "10.0.0.3:6443", RESTConfig: &rest.Config{Host: "https://10.0.0.3:6443"}},
			},
		},
	}
	snapshotIndex := 0
	apiEndpoints := func(context.Context) (kubernetesAPIServerEndpointSnapshot, error) {
		return snapshots[snapshotIndex], nil
	}
	clients := map[string][]*teardownRetirementProbeClient{}
	factory := func(config *rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
		client := &teardownRetirementProbeClient{marker: marker.DeepCopy(), probes: probes}
		clients[config.Host] = append(clients[config.Host], client)
		return client, nil
	}
	provider := newTeardownRetirementEndpointProvider(apiEndpoints, guard, probes, factory)

	for expectedTopologyIndex := range snapshots {
		snapshotIndex = expectedTopologyIndex
		endpoints, err := provider(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(endpoints) != 2 {
			t.Fatalf("topology %d endpoints = %d, want 2", expectedTopologyIndex, len(endpoints))
		}
		for _, endpoint := range endpoints {
			proven, probeErr := endpoint.probe(context.Background())
			if probeErr != nil || !proven {
				t.Fatalf("endpoint %s probe = %v, %v", endpoint.name, proven, probeErr)
			}
		}
	}
	if got := len(clients["https://10.0.0.1:6443"]); got != 2 {
		t.Fatalf("client creations for retained address across topology change = %d, want 2", got)
	}
	for address, generations := range clients {
		for _, client := range generations {
			if client.updateCalls != len(probes) {
				t.Errorf("client %s probe updates = %d, want %d", address, client.updateCalls, len(probes))
			}
		}
	}
}

func TestTeardownRetirementEndpointProbeRequiresEveryExactDenial(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	marker.UID = "marker-uid"
	marker.ResourceVersion = "7"
	client := &teardownRetirementProbeClient{marker: marker, probes: probes, failManager: probes[1].FieldManager}
	provider := newTeardownRetirementEndpointProvider(
		func(context.Context) (kubernetesAPIServerEndpointSnapshot, error) {
			return kubernetesAPIServerEndpointSnapshot{
				InventoryIdentity: "topology-a",
				Endpoints:         []kubernetesAPIServerEndpoint{{Address: "10.0.0.1:6443", RESTConfig: &rest.Config{}}},
			}, nil
		},
		guard,
		probes,
		func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) { return client, nil },
	)
	endpoints, err := provider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	proven, err := endpoints[0].probe(context.Background())
	if err != nil || proven {
		t.Fatalf("joint probe = %v, %v, want unproven without a generic error", proven, err)
	}
	if client.updateCalls != 2 {
		t.Fatalf("probe stopped after %d updates, want exact failing second probe", client.updateCalls)
	}
}

func TestTeardownRetirementAdmissionBarrierDefersDynamicObservationsUntilWait(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	storedCalls := 0
	factoryCalls := 0
	barrier, err := newTeardownRetirementAdmissionBarrierWith(
		context.Background(),
		&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca")}},
		&sequencedEndpointSliceLister{},
		guard,
		probes,
		func(context.Context) error {
			storedCalls++
			return errors.New("not yet ready")
		},
		func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
			factoryCalls++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if barrier == nil {
		t.Fatal("constructor returned a nil barrier")
	}
	if storedCalls != 0 || factoryCalls != 0 {
		t.Fatalf("constructor performed dynamic observations: stored=%d factory=%d", storedCalls, factoryCalls)
	}
}

func TestTeardownRetirementCredentialObserverWaitsForEveryEndpoint(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	first := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{unauthorized: true},
	)
	second := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{},
		teardownRetirementCredentialState{unauthorized: true},
	)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, first, second)
	defer observer.Close()
	clock := &teardownRetirementTestClock{now: time.Unix(100, 0)}
	observer.pollEvery = time.Second
	observer.stabilityDuration = 2 * time.Second
	observer.retirementTimeout = 10 * time.Second
	observer.now = clock.Now
	observer.sleep = clock.Sleep

	if err := observer.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(first.updateManagers) != 0 {
		t.Fatalf("already Unauthorized endpoint received marker mutations: %v", first.updateManagers)
	}
	if len(second.updateManagers) != len(probes) {
		t.Fatalf("temporarily authenticated endpoint updates = %d, want %d", len(second.updateManagers), len(probes))
	}
}

func TestTeardownRetirementCredentialObserverRejectsAuthenticationRegressionWithoutMutation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{unauthorized: true},
		teardownRetirementCredentialState{},
	)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()
	clock := &teardownRetirementTestClock{now: time.Unix(100, 0)}
	observer.pollEvery = time.Second
	observer.stabilityDuration = 2 * time.Second
	observer.retirementTimeout = 10 * time.Second
	observer.now = clock.Now
	observer.sleep = clock.Sleep

	err := observer.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authenticated after an Unauthorized") {
		t.Fatalf("Wait() error = %v, want authentication regression", err)
	}
	if len(client.updateManagers) != 0 {
		t.Fatalf("regressed endpoint received marker mutations: %v", client.updateManagers)
	}
}

func TestTeardownRetirementCredentialObserverAllowsSixtySecondInvalidationBoundary(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	states := make([]teardownRetirementCredentialState, 0, 62)
	for range 61 {
		states = append(states, teardownRetirementCredentialState{})
	}
	states = append(states, teardownRetirementCredentialState{unauthorized: true})
	client := newTeardownRetirementCredentialClient(t, guard, probes, states...)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()
	clock := &teardownRetirementTestClock{now: time.Unix(100, 0)}
	observer.pollEvery = time.Second
	observer.stabilityDuration = 5 * time.Second
	observer.retirementTimeout = teardownRetirementCredentialTimeout
	observer.now = clock.Now
	observer.sleep = clock.Sleep

	if err := observer.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := clock.Now().Sub(time.Unix(100, 0)); elapsed != 66*time.Second {
		t.Fatalf("retirement proof elapsed = %s, want 66s", elapsed)
	}
}

func TestTeardownRetirementCredentialObserverRejectsAdmittedMarkerMutation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{admittedManager: probes[1].FieldManager},
	)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	err := observer.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "was admitted") {
		t.Fatalf("Wait() error = %v, want admitted marker rejection", err)
	}
}

func TestTeardownRetirementCredentialObserverRecognizesWrappedProbeUnauthorized(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{unauthorizedManager: probes[0].FieldManager},
		teardownRetirementCredentialState{unauthorized: true},
	)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()
	clock := &teardownRetirementTestClock{now: time.Unix(100, 0)}
	observer.pollEvery = time.Second
	observer.stabilityDuration = time.Second
	observer.retirementTimeout = 10 * time.Second
	observer.now = clock.Now
	observer.sleep = clock.Sleep

	if err := observer.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.updateManagers, []string{probes[0].FieldManager}) {
		t.Fatalf("marker mutations after wrapped Unauthorized = %v", client.updateManagers)
	}
}

func TestTeardownRetirementCredentialObserverRejectsForeignPhase(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes,
		teardownRetirementCredentialState{active: true},
	)
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	err := observer.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "foreign teardown retirement phase") {
		t.Fatalf("Wait() error = %v, want foreign phase rejection", err)
	}
	if len(client.updateManagers) != 0 {
		t.Fatalf("foreign phase received marker mutations: %v", client.updateManagers)
	}
}

func TestTeardownRetirementCredentialObserverValidatesFrozenSnapshot(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	valid := teardownRetirementCredentialSnapshot("10.0.0.1:6443")
	tests := []struct {
		name   string
		mutate func(*kubernetesAPIServerEndpointSnapshot)
		want   string
	}{
		{name: "empty resourceVersion", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.InventoryResourceVersion = "" }, want: "resourceVersion"},
		{name: "padded identity", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.InventoryIdentity += " " }, want: "identity"},
		{name: "empty endpoints", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.Endpoints = nil }, want: "empty"},
		{name: "padded address", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.Endpoints[0].Address += " " }, want: "incomplete"},
		{name: "duplicate endpoint", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) {
			snapshot.Endpoints = append(snapshot.Endpoints, snapshot.Endpoints[0])
		}, want: "duplicates"},
		{name: "nil REST config", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.Endpoints[0].RESTConfig = nil }, want: "incomplete"},
		{name: "unpinned host", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) {
			snapshot.Endpoints[0].RESTConfig.Host = "https://kubernetes.default.svc"
		}, want: "not pinned"},
		{name: "unverified TLS", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) { snapshot.Endpoints[0].RESTConfig.CAData = nil }, want: "verified CA"},
		{name: "missing frozen token", mutate: func(snapshot *kubernetesAPIServerEndpointSnapshot) {
			snapshot.Endpoints[0].RESTConfig.BearerToken = ""
		}, want: "frozen bearer token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := valid
			snapshot.Endpoints = append([]kubernetesAPIServerEndpoint(nil), valid.Endpoints...)
			for index := range snapshot.Endpoints {
				snapshot.Endpoints[index].RESTConfig = rest.CopyConfig(snapshot.Endpoints[index].RESTConfig)
			}
			test.mutate(&snapshot)
			factoryCalls := 0
			watchCalls := 0
			observer, err := newTeardownRetirementCredentialObserverForSnapshot(
				context.Background(),
				snapshot,
				guard,
				probes,
				func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
					factoryCalls++
					return nil, errors.New("unexpected factory call")
				},
				func(context.Context, metav1.ListOptions) (watch.Interface, error) {
					watchCalls++
					return nil, errors.New("unexpected watch call")
				},
			)
			if observer != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("constructor = %#v, %v, want error containing %q", observer, err, test.want)
			}
			if factoryCalls != 0 || watchCalls != 0 {
				t.Fatalf("invalid snapshot caused side effects: factory=%d watch=%d", factoryCalls, watchCalls)
			}
		})
	}
}

func TestTeardownRetirementCredentialObserverPinsWatchToSnapshot(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, options := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	if options.ResourceVersion != "100" || options.LabelSelector != discoveryv1.LabelServiceName+"="+kubernetesServiceName || !options.AllowWatchBookmarks {
		t.Fatalf("watch options = %#v", options)
	}
}

func TestTeardownRetirementCredentialObserverFreezesLoadedToken(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	snapshot := teardownRetirementCredentialSnapshot("10.0.0.1:6443")
	snapshot.Endpoints[0].RESTConfig.BearerTokenFile = "/var/run/secrets/token"
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	fakeWatch := watch.NewRaceFreeFake()
	var received *rest.Config
	observer, err := newTeardownRetirementCredentialObserverForSnapshot(
		context.Background(),
		snapshot,
		guard,
		probes,
		func(config *rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
			received = config
			return client, nil
		},
		func(context.Context, metav1.ListOptions) (watch.Interface, error) { return fakeWatch, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if received == nil || received.BearerToken != "frozen-token" || received.BearerTokenFile != "" {
		t.Fatalf("frozen client authentication = token present %t, token file %q", received != nil && received.BearerToken != "", received.BearerTokenFile)
	}
}

func TestTeardownRetirementCredentialObserverRejectsWatchStartupFailure(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, err := newTeardownRetirementCredentialObserverForSnapshot(
		context.Background(),
		teardownRetirementCredentialSnapshot("10.0.0.1:6443"),
		guard,
		probes,
		func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) { return client, nil },
		func(context.Context, metav1.ListOptions) (watch.Interface, error) {
			return nil, errors.New("watch unavailable")
		},
	)
	if observer != nil || err == nil || !strings.Contains(err.Error(), "watch unavailable") {
		t.Fatalf("constructor = %#v, %v, want watch startup failure", observer, err)
	}
}

func TestTeardownRetirementCredentialObserverFailsOnTopologyChange(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	observer.topologyWatch.(*watch.RaceFreeFakeWatcher).Modify(&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Name: "kubernetes", Namespace: kubernetesServiceNamespace,
		Labels: map[string]string{discoveryv1.LabelServiceName: kubernetesServiceName},
	}})
	<-observer.monitorDone
	if err := observer.verifyTopology(); err == nil || !strings.Contains(err.Error(), "inventory changed") {
		t.Fatalf("verifyTopology() error = %v, want topology change", err)
	}
}

func TestTeardownRetirementCredentialObserverAcceptsBookmarkButRejectsExpiredWatch(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()
	fakeWatch := observer.topologyWatch.(*watch.RaceFreeFakeWatcher)
	fakeWatch.Action(watch.Bookmark, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "101"}})
	fakeWatch.Error(&metav1.Status{
		Status:  metav1.StatusFailure,
		Message: "too old resource version",
		Reason:  metav1.StatusReasonExpired,
		Code:    410,
	})
	<-observer.monitorDone
	if err := observer.verifyTopology(); err == nil || !apierrors.IsResourceExpired(err) {
		t.Fatalf("verifyTopology() error = %v, want expired watch", err)
	}
}

func TestTeardownRetirementCredentialObserverRejectsMalformedWatchEvent(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	observer.topologyWatch.(*watch.RaceFreeFakeWatcher).Action(watch.Bookmark, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "101"}})
	<-observer.monitorDone
	if err := observer.verifyTopology(); err == nil || !strings.Contains(err.Error(), "malformed bookmark") {
		t.Fatalf("verifyTopology() error = %v, want malformed bookmark", err)
	}
}

func TestTeardownRetirementCredentialObserverFailsWhenWatchCloses(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{unauthorized: true})
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()

	observer.topologyWatch.Stop()
	<-observer.monitorDone
	if err := observer.verifyTopology(); err == nil || !strings.Contains(err.Error(), "watch closed") {
		t.Fatalf("verifyTopology() error = %v, want closed watch", err)
	}
}

func TestTeardownRetirementCredentialObserverHonorsCancellation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	probes := teardownRetirementManagerTestProbes(t, guard)
	client := newTeardownRetirementCredentialClient(t, guard, probes, teardownRetirementCredentialState{})
	observer, _ := newTestTeardownRetirementCredentialObserver(t, guard, probes, client)
	defer observer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	observer.sleep = func(sleepCtx context.Context, _ time.Duration) error {
		cancel()
		<-sleepCtx.Done()
		return sleepCtx.Err()
	}

	if err := observer.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context cancellation", err)
	}
}

func TestTeardownRetirementFinalizerDeletesMarkersBeforeActivation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	dedicated, err := guard.MarkerTarget()
	if err != nil {
		t.Fatal(err)
	}
	secondary := crdupgrade.TeardownRetirementMarkerTarget{
		Name: "ptah-admission-convergence-v1-1-deadbeefcafe",
		Verify: func(object *corev1.ConfigMap) error {
			if object.Data["contract"] != "exact" {
				return errors.New("secondary marker differs")
			}
			return nil
		},
	}
	dedicatedObject, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	dedicatedObject.UID = "dedicated-uid"
	dedicatedObject.ResourceVersion = "11"
	secondaryObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, Namespace: dedicatedObject.Namespace, UID: "secondary-uid", ResourceVersion: "12"},
		Data:       map[string]string{"contract": "exact"},
	}
	activation := teardownRetirementManagerTestActivation(t, guard)
	client := &teardownRetirementFinalizerClient{objects: map[string]*corev1.ConfigMap{
		dedicated.Name:                   dedicatedObject,
		secondary.Name:                   secondaryObject,
		crdupgrade.ReleaseActivationName: activation,
	}}
	finalizer, err := newTeardownRetirementFinalizer(client, guard, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{secondary.Name, crdupgrade.ReleaseActivationName}
	if !reflect.DeepEqual(client.deletes, wantOrder) {
		t.Fatalf("deletion order = %v, want %v", client.deletes, wantOrder)
	}
	for _, name := range wantOrder {
		if client.deleteOptions[name].Preconditions == nil || client.deleteOptions[name].Preconditions.UID == nil ||
			client.deleteOptions[name].Preconditions.ResourceVersion == nil {
			t.Errorf("ConfigMap/%s deletion lacks UID/resourceVersion preconditions", name)
		}
	}
	if err := finalizer.Finalize(context.Background()); err != nil {
		t.Fatalf("completed retry: %v", err)
	}
	if len(client.deletes) != len(wantOrder) {
		t.Fatalf("completed retry issued more mutations: %v", client.deletes)
	}
	if client.objects[dedicated.Name] == nil {
		t.Fatal("finalizer deleted the Helm-owned dedicated marker")
	}
	for _, name := range client.gets {
		if name == dedicated.Name {
			t.Fatal("finalizer inspected the Helm-owned dedicated marker")
		}
	}
}

func TestTeardownRetirementFinalizerAcceptsOnlyContiguousRetryPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		presentMarkers  []bool
		activation      bool
		wantDeleteCount int
		wantError       string
	}{
		{name: "fresh", presentMarkers: []bool{true, true}, activation: true, wantDeleteCount: 3},
		{name: "first marker already deleted", presentMarkers: []bool{false, true}, activation: true, wantDeleteCount: 2},
		{name: "all markers already deleted", presentMarkers: []bool{false, false}, activation: true, wantDeleteCount: 1},
		{name: "complete", presentMarkers: []bool{false, false}, activation: false},
		{name: "non-contiguous marker prefix", presentMarkers: []bool{true, false}, activation: true, wantError: "non-contiguous"},
		{name: "activation absent before retained secondary marker", presentMarkers: []bool{true, false}, activation: false, wantError: "retains"},
		{name: "activation absent before all retained markers", presentMarkers: []bool{true, true}, activation: false, wantError: "retains"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			guard := teardownRetirementManagerTestGuard()
			first := crdupgrade.TeardownRetirementMarkerTarget{Name: "first", Verify: func(*corev1.ConfigMap) error { return nil }}
			second := crdupgrade.TeardownRetirementMarkerTarget{Name: "second", Verify: func(*corev1.ConfigMap) error { return nil }}
			markers := []crdupgrade.TeardownRetirementMarkerTarget{first, second}
			objects := map[string]*corev1.ConfigMap{}
			for index, present := range test.presentMarkers {
				if present {
					objects[markers[index].Name] = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Name: markers[index].Name, UID: types.UID(strconv.Itoa(index + 1)), ResourceVersion: strconv.Itoa(index + 1),
					}}
				}
			}
			if test.activation {
				objects[crdupgrade.ReleaseActivationName] = teardownRetirementManagerTestActivation(t, guard)
			}
			client := &teardownRetirementFinalizerClient{objects: objects}
			finalizer, err := newTeardownRetirementFinalizer(client, guard, markers...)
			if err != nil {
				t.Fatal(err)
			}
			err = finalizer.Finalize(context.Background())
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Finalize() error = %v, want containing %q", err, test.wantError)
				}
				if len(client.deletes) != 0 {
					t.Fatalf("unsafe retry mutated %v", client.deletes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(client.deletes) != test.wantDeleteCount {
				t.Fatalf("delete count = %d, want %d", len(client.deletes), test.wantDeleteCount)
			}
		})
	}
}

func TestTeardownRetirementFinalizerRejectsForeignObjectWithoutMutation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	secondary := crdupgrade.TeardownRetirementMarkerTarget{
		Name: "secondary",
		Verify: func(object *corev1.ConfigMap) error {
			if object.Data["contract"] != "exact" {
				return errors.New("secondary marker differs")
			}
			return nil
		},
	}
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, UID: "marker-uid", ResourceVersion: "1"},
		Data:       map[string]string{"contract": "foreign"},
	}
	client := &teardownRetirementFinalizerClient{objects: map[string]*corev1.ConfigMap{
		secondary.Name:                   marker,
		crdupgrade.ReleaseActivationName: teardownRetirementManagerTestActivation(t, guard),
	}}
	finalizer, err := newTeardownRetirementFinalizer(client, guard, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "secondary marker differs") {
		t.Fatalf("Finalize() error = %v, want secondary marker rejection", err)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("foreign marker caused mutations: %v", client.deletes)
	}
}

func TestTeardownRetirementFinalizerResumesAfterEveryDelete(t *testing.T) {
	t.Parallel()

	for crashAfter := 1; crashAfter <= 2; crashAfter++ {
		t.Run(strconv.Itoa(crashAfter), func(t *testing.T) {
			t.Parallel()
			guard := teardownRetirementManagerTestGuard()
			dedicated, err := guard.MarkerTarget()
			if err != nil {
				t.Fatal(err)
			}
			secondary := crdupgrade.TeardownRetirementMarkerTarget{
				Name: "secondary",
				Verify: func(object *corev1.ConfigMap) error {
					if object.Data["contract"] != "exact" {
						return errors.New("secondary marker differs")
					}
					return nil
				},
			}
			dedicatedObject, markerErr := guard.Marker()
			if markerErr != nil {
				t.Fatal(markerErr)
			}
			dedicatedObject.UID, dedicatedObject.ResourceVersion = "dedicated-uid", "1"
			client := &teardownRetirementFinalizerClient{
				objects: map[string]*corev1.ConfigMap{
					secondary.Name: {
						ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, UID: "secondary-uid", ResourceVersion: "1"},
						Data:       map[string]string{"contract": "exact"},
					},
					dedicated.Name:                   dedicatedObject,
					crdupgrade.ReleaseActivationName: teardownRetirementManagerTestActivation(t, guard),
				},
				failAfterDelete: crashAfter,
			}
			finalizer, finalizerErr := newTeardownRetirementFinalizer(client, guard, secondary)
			if finalizerErr != nil {
				t.Fatal(finalizerErr)
			}
			if err := finalizer.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated crash") {
				t.Fatalf("first Finalize() error = %v, want simulated crash", err)
			}

			client.failAfterDelete = 0
			if err := finalizer.Finalize(context.Background()); err != nil {
				t.Fatalf("retry Finalize() error = %v", err)
			}
			if len(client.objects) != 1 || client.objects[dedicated.Name] == nil {
				t.Fatalf("retry retained %d ConfigMaps; dedicated marker present = %t", len(client.objects), client.objects[dedicated.Name] != nil)
			}
		})
	}
}

func TestConfiguredTeardownRetirementGuardAddsOnlyExactCertificateRecoveryPair(t *testing.T) {
	t.Parallel()

	baseRollout := teardownRetirementManagerTestRollout()
	if _, err := newConfiguredTeardownRetirementGuard(baseRollout, crdupgrade.RuntimeAdmissionContract{}); err != nil {
		t.Fatal(err)
	}

	rollout := teardownRetirementManagerTestRollout()
	rollout.CertificateArgs = append(rollout.CertificateArgs,
		"--recreate-missing-secret=true",
		"--secret-create-policy-name="+rollout.CertificateDeploymentName,
		"--secret-create-policy-binding-name="+rollout.CertificateDeploymentName,
		"--secret-create-service-account-name="+rollout.CertificateDeploymentName,
	)
	contract := crdupgrade.RuntimeAdmissionContract{
		CertificateRuntimeEnabled:     true,
		CertificateServiceAccountName: rollout.CertificateDeploymentName,
	}
	configured, err := newConfiguredTeardownRetirementGuard(rollout, contract)
	if err != nil {
		t.Fatal(err)
	}
	_, err = configured.WithOriginalPairs(crdupgrade.TeardownOriginalPairVerifier{
		Name: rollout.CertificateDeploymentName,
		VerifyPolicy: func(*admissionregistrationv1.ValidatingAdmissionPolicy) error {
			return nil
		},
		VerifyBinding: func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("configured certificate pair was not registered exactly once: %v", err)
	}

	disabledContract := contract
	disabledContract.CertificateRuntimeEnabled = false
	if _, err := newConfiguredTeardownRetirementGuard(rollout, disabledContract); err == nil || !strings.Contains(err.Error(), "runtime is disabled") {
		t.Fatalf("disabled certificate runtime error = %v", err)
	}
	rollout.CertificateArgs = []string{"--recreate-missing-secret=true"}
	if _, err := newConfiguredTeardownRetirementGuard(rollout, contract); err == nil || !strings.Contains(err.Error(), "policy-name") {
		t.Fatalf("incomplete certificate recovery error = %v", err)
	}
}

func TestCertificateRecoveryRetirementMetadataIsExact(t *testing.T) {
	t.Parallel()

	rollout := teardownRetirementManagerTestRollout()
	name := rollout.CertificateDeploymentName
	object := &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:            name,
		UID:             "policy-uid",
		ResourceVersion: "7",
		Annotations: map[string]string{
			"meta.helm.sh/release-name":      rollout.ReleaseName,
			"meta.helm.sh/release-namespace": rollout.ReleaseNamespace,
		},
		Labels: map[string]string{
			"helm.sh/chart":                "ptah-operator-0.1.0",
			"app.kubernetes.io/name":       "ptah-operator",
			"app.kubernetes.io/instance":   rollout.ReleaseName,
			"app.kubernetes.io/version":    "0.1.0",
			"app.kubernetes.io/managed-by": "Helm",
			"app.kubernetes.io/component":  "certificate-rotation",
		},
	}}
	if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", name, rollout, object); err != nil {
		t.Fatal(err)
	}
	object.Labels["foreign"] = "true"
	if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", name, rollout, object); err == nil {
		t.Fatal("foreign metadata was accepted")
	}
}

func teardownRetirementManagerTestGuard() *crdupgrade.TeardownRetirementGuard {
	return crdupgrade.NewTeardownRetirementGuard(teardownRetirementManagerTestRollout())
}

func teardownRetirementManagerTestRollout() *crdupgrade.RolloutGuard {
	releaseNamespace := "ptah-system"
	releaseName := "ptah"
	managerImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	identity := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName + "\n1\n" + managerImage))
	return &crdupgrade.RolloutGuard{
		Policies:                           emptyTeardownRetirementPolicyReader{},
		Bindings:                           emptyTeardownRetirementBindingReader{},
		ReleaseName:                        releaseName,
		ReleaseNamespace:                   releaseNamespace,
		CoordinationNamespace:              releaseNamespace,
		LeaderElectionID:                   "ptah-operator.operator.ptah.dev",
		WebhookServiceName:                 "ptah-webhook",
		WebhookTimeoutSeconds:              10,
		WebhookSecretName:                  "ptah-webhook-cert",
		WebhookPort:                        9443,
		CertificateHealthPort:              8081,
		HookServiceAccountName:             fmt.Sprintf("ptah-crd-v1-%x", identity)[:24],
		ControllerServiceAccountName:       "ptah-controller",
		ControllerDeploymentName:           "ptah-controller",
		ControllerReplicas:                 2,
		CertificateDeploymentName:          "ptah-cert-rotator",
		ControllerStateVersion:             1,
		AdmissionContractVersion:           1,
		ReleaseSequence:                    1,
		ManagerImage:                       managerImage,
		ControllerArgs:                     []string{"--webhook-port=9443"},
		CertificateArgs:                    []string{"--namespace=ptah-system"},
		RuntimeDeploymentConfigExpressions: []string{`object.spec.replicas == 2`},
		RuntimePodConfigExpressions:        []string{`object.spec.restartPolicy == "Always"`},
		RuntimeAdmissionContractB64:        "e30=",
		PollEvery:                          time.Millisecond,
	}
}

func teardownRetirementManagerTestProbes(t *testing.T, guard *crdupgrade.TeardownRetirementGuard) []crdupgrade.TeardownRetirementProbe {
	t.Helper()
	probes := make([]crdupgrade.TeardownRetirementProbe, 0, 2)
	for _, fence := range []crdupgrade.TeardownFence{crdupgrade.TeardownFenceA, crdupgrade.TeardownFenceB} {
		_, _, probe, err := guard.OriginalFencePair(fence)
		if err != nil {
			t.Fatal(err)
		}
		probes = append(probes, probe)
	}
	return probes
}

func teardownRetirementManagerTestActivation(t *testing.T, guard *crdupgrade.TeardownRetirementGuard) *corev1.ConfigMap {
	t.Helper()
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	attempt := marker.Data["release-attempt"]
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crdupgrade.ReleaseActivationName,
			Namespace:       marker.Namespace,
			UID:             "activation-uid",
			ResourceVersion: "13",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ptah-operator",
				"app.kubernetes.io/instance":   "ptah",
				"app.kubernetes.io/component":  "rollout-guard",
			},
			Annotations: map[string]string{
				"helm.sh/hook":                                "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                         "-166",
				"helm.sh/resource-policy":                     "keep",
				"operator.ptah.dev/rollout-guard-version":     "1",
				"operator.ptah.dev/release-name":              "ptah",
				"operator.ptah.dev/release-namespace":         "ptah-system",
				crdupgrade.ControllerStateVersionAnnotation:   "1",
				crdupgrade.AdmissionContractVersionAnnotation: "1",
				crdupgrade.ReleaseSequenceAnnotation:          "1",
				crdupgrade.ManagerImageAnnotation:             marker.Annotations[crdupgrade.ManagerImageAnnotation],
			},
		},
		Data: map[string]string{
			"active-release-sequence":                        "1",
			"controller-credentials":                         "draining",
			"controller-credentials-target-release-sequence": "1",
			"controller-credentials-attempt":                 attempt,
		},
	}
}

type emptyTeardownRetirementPolicyReader struct{}

func (emptyTeardownRetirementPolicyReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	return nil, errors.New("unexpected policy read")
}

type emptyTeardownRetirementBindingReader struct{}

func (emptyTeardownRetirementBindingReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	return nil, errors.New("unexpected binding read")
}

type teardownRetirementProbeClient struct {
	marker      *corev1.ConfigMap
	probes      []crdupgrade.TeardownRetirementProbe
	failManager string
	updateCalls int
}

func (c *teardownRetirementProbeClient) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	return c.marker.DeepCopy(), nil
}

func (c *teardownRetirementProbeClient) Update(_ context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	c.updateCalls++
	if options.FieldManager == c.failManager {
		return nil, directAdmissionPolicyDenialError("other", "other", "other")
	}
	for _, probe := range c.probes {
		if options.FieldManager == probe.FieldManager {
			return nil, directAdmissionPolicyDenialError(probe.PolicyName, probe.BindingName, probe.Message)
		}
	}
	return object, nil
}

type teardownRetirementCredentialState struct {
	unauthorized        bool
	active              bool
	admittedManager     string
	unauthorizedManager string
}

type teardownRetirementCredentialClient struct {
	marker         *corev1.ConfigMap
	activation     *corev1.ConfigMap
	probes         []crdupgrade.TeardownRetirementProbe
	states         []teardownRetirementCredentialState
	phaseCalls     int
	currentState   int
	updateManagers []string
}

func newTeardownRetirementCredentialClient(
	t *testing.T,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	states ...teardownRetirementCredentialState,
) *teardownRetirementCredentialClient {
	t.Helper()
	if len(states) == 0 {
		t.Fatal("credential endpoint states are required")
	}
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	marker.UID, marker.ResourceVersion = "marker-uid", "7"
	return &teardownRetirementCredentialClient{
		marker:     marker,
		activation: teardownRetirementManagerTestActivation(t, guard),
		probes:     append([]crdupgrade.TeardownRetirementProbe(nil), probes...),
		states:     append([]teardownRetirementCredentialState(nil), states...),
	}
}

func (c *teardownRetirementCredentialClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	if name == crdupgrade.ReleaseActivationName {
		c.currentState = min(c.phaseCalls, len(c.states)-1)
		c.phaseCalls++
		state := c.states[c.currentState]
		if state.unauthorized {
			return nil, fmt.Errorf("wrapped endpoint authorization: %w", apierrors.NewUnauthorized("expired cleanup credential"))
		}
		if state.active {
			return c.activation.DeepCopy(), nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	if name != c.marker.Name {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return c.marker.DeepCopy(), nil
}

func (c *teardownRetirementCredentialClient) Update(
	_ context.Context,
	object *corev1.ConfigMap,
	options metav1.UpdateOptions,
) (*corev1.ConfigMap, error) {
	c.updateManagers = append(c.updateManagers, options.FieldManager)
	state := c.states[c.currentState]
	if state.unauthorizedManager == options.FieldManager {
		return nil, fmt.Errorf("wrapped endpoint authorization: %w", apierrors.NewUnauthorized("expired cleanup credential"))
	}
	if state.admittedManager == options.FieldManager {
		return object.DeepCopy(), nil
	}
	for _, probe := range c.probes {
		if probe.FieldManager == options.FieldManager {
			return nil, directAdmissionPolicyDenialError(probe.PolicyName, probe.BindingName, probe.Message)
		}
	}
	return nil, errors.New("unexpected cleanup credential fence probe")
}

func teardownRetirementCredentialSnapshot(addresses ...string) kubernetesAPIServerEndpointSnapshot {
	endpoints := make([]kubernetesAPIServerEndpoint, 0, len(addresses))
	for _, address := range addresses {
		endpoints = append(endpoints, kubernetesAPIServerEndpoint{
			Address: address,
			RESTConfig: &rest.Config{
				Host:        "https://" + address,
				BearerToken: "frozen-token",
				TLSClientConfig: rest.TLSClientConfig{
					ServerName: kubernetesServiceTLSServerName,
					CAData:     []byte("ca"),
				},
			},
		})
	}
	return kubernetesAPIServerEndpointSnapshot{
		InventoryResourceVersion: "100",
		InventoryIdentity:        "sha256:frozen",
		Endpoints:                endpoints,
	}
}

func newTestTeardownRetirementCredentialObserver(
	t *testing.T,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	clients ...*teardownRetirementCredentialClient,
) (*teardownRetirementCredentialObserver, metav1.ListOptions) {
	t.Helper()
	addresses := make([]string, 0, len(clients))
	for index := range clients {
		addresses = append(addresses, fmt.Sprintf("10.0.0.%d:6443", index+1))
	}
	clientIndex := 0
	fakeWatch := watch.NewRaceFreeFake()
	var watchOptions metav1.ListOptions
	observer, err := newTeardownRetirementCredentialObserverForSnapshot(
		context.Background(),
		teardownRetirementCredentialSnapshot(addresses...),
		guard,
		probes,
		func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
			client := clients[clientIndex]
			clientIndex++
			return client, nil
		},
		func(_ context.Context, options metav1.ListOptions) (watch.Interface, error) {
			watchOptions = options
			return fakeWatch, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return observer, watchOptions
}

type teardownRetirementTestClock struct {
	now time.Time
}

func (c *teardownRetirementTestClock) Now() time.Time {
	return c.now
}

func (c *teardownRetirementTestClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(duration)
	return nil
}

type teardownRetirementFinalizerClient struct {
	objects         map[string]*corev1.ConfigMap
	gets            []string
	deletes         []string
	deleteOptions   map[string]metav1.DeleteOptions
	failAfterDelete int
}

func (c *teardownRetirementFinalizerClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	c.gets = append(c.gets, name)
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownRetirementFinalizerClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != object.UID || *options.Preconditions.ResourceVersion != object.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("precondition differs"))
	}
	delete(c.objects, name)
	c.deletes = append(c.deletes, name)
	if c.deleteOptions == nil {
		c.deleteOptions = map[string]metav1.DeleteOptions{}
	}
	c.deleteOptions[name] = options
	if c.failAfterDelete > 0 && len(c.deletes) == c.failAfterDelete {
		return errors.New("simulated crash after delete")
	}
	return nil
}

var (
	_ crdupgrade.AdmissionConvergenceMarkerClient       = (*teardownRetirementProbeClient)(nil)
	_ crdupgrade.AdmissionConvergenceMarkerClient       = (*teardownRetirementCredentialClient)(nil)
	_ teardownRetirementConfigMapClient                 = (*teardownRetirementFinalizerClient)(nil)
	_ crdupgrade.ValidatingAdmissionPolicyReader        = emptyTeardownRetirementPolicyReader{}
	_ crdupgrade.ValidatingAdmissionPolicyBindingReader = emptyTeardownRetirementBindingReader{}
)
