package kubeapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

func TestNewDefaultServiceProviderReturnsStableDirectConfigsAndDeepCopiesTLSMaterial(t *testing.T) {
	ready := true
	terminating := true
	lister := &scriptedLister{pages: map[string]*discoveryv1.EndpointSliceList{
		"": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "next"},
			Items: []discoveryv1.EndpointSlice{
				validSlice(
					"kubernetes-v6",
					discoveryv1.AddressTypeIPv6,
					7443,
					discoveryv1.Endpoint{Addresses: []string{"2001:db8:0:0::20"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
				),
			},
		},
		"next": {
			ListMeta: metav1.ListMeta{ResourceVersion: "42"},
			Items: []discoveryv1.EndpointSlice{
				validSlice(
					"kubernetes-v4",
					discoveryv1.AddressTypeIPv4,
					6443,
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.12"}},
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.99"}, Conditions: discoveryv1.EndpointConditions{Terminating: &terminating}},
				),
			},
		},
	}}
	base := &rest.Config{
		Host:        "https://kubernetes.default.svc",
		APIPath:     "/api",
		BearerToken: "projected-token",
		TLSClientConfig: rest.TLSClientConfig{
			ServerName: "source-name",
			CertData:   []byte("client-certificate"),
			KeyData:    []byte("client-key"),
			CAData:     []byte("cluster-ca"),
			NextProtos: []string{"h2", "http/1.1"},
		},
	}
	base.Proxy = http.ProxyFromEnvironment

	provider, err := NewDefaultServiceProvider(base, lister, 7)
	if err != nil {
		t.Fatalf("NewDefaultServiceProvider() error = %v", err)
	}
	base.Host = "https://mutated.invalid"
	base.CertData[0] = 'X'
	base.KeyData[0] = 'X'
	base.CAData[0] = 'X'
	base.NextProtos[0] = "mutated"

	first, err := provider(context.Background())
	if err != nil {
		t.Fatalf("provider() error = %v", err)
	}
	if first.InventoryResourceVersion != "42" || !strings.HasPrefix(first.InventoryIdentity, "sha256:") {
		t.Fatalf("snapshot identity = %q/%q", first.InventoryResourceVersion, first.InventoryIdentity)
	}
	if got, want := endpointAddresses(first), []string{"10.0.0.12:6443", "[2001:db8::20]:7443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoint addresses = %#v, want %#v", got, want)
	}
	for _, endpoint := range first.Endpoints {
		config := endpoint.RESTConfig
		if config == nil || config.Host != "https://"+endpoint.Address || config.ServerName != defaultServiceTLSServerName {
			t.Fatalf("direct endpoint config = %#v", config)
		}
		if config.APIPath != "/api" || config.BearerToken != "projected-token" || config.QPS != directQueriesPerSecond || config.Burst != 7 || config.RateLimiter != nil {
			t.Fatalf("direct endpoint config did not preserve source settings and direct rate limit: %#v", config)
		}
		if string(config.CertData) != "client-certificate" || string(config.KeyData) != "client-key" || string(config.CAData) != "cluster-ca" {
			t.Fatalf("direct endpoint TLS material aliased the source config: %s/%s/%s", config.CertData, config.KeyData, config.CAData)
		}
		if got, want := config.NextProtos, []string{"h2", "http/1.1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("direct endpoint protocols = %#v, want %#v", got, want)
		}
		if config.Proxy == nil {
			t.Fatal("direct endpoint config has no explicit proxy bypass")
		}
		proxy, proxyErr := config.Proxy(&http.Request{})
		if proxyErr != nil || proxy != nil {
			t.Fatalf("direct endpoint proxy = %v, %v; want nil, nil", proxy, proxyErr)
		}
	}

	first.Endpoints[0].RESTConfig.CertData[0] = 'Y'
	first.Endpoints[0].RESTConfig.KeyData[0] = 'Y'
	first.Endpoints[0].RESTConfig.CAData[0] = 'Y'
	first.Endpoints[0].RESTConfig.NextProtos[0] = "changed"
	if string(first.Endpoints[1].RESTConfig.CertData) != "client-certificate" ||
		string(first.Endpoints[1].RESTConfig.KeyData) != "client-key" ||
		string(first.Endpoints[1].RESTConfig.CAData) != "cluster-ca" ||
		first.Endpoints[1].RESTConfig.NextProtos[0] != "h2" {
		t.Fatal("direct endpoint configs share mutable TLS material")
	}
	second, err := provider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.InventoryIdentity != first.InventoryIdentity || string(second.Endpoints[0].RESTConfig.CAData) != "cluster-ca" {
		t.Fatalf("refreshed snapshot reused mutable state: %#v", second)
	}
	if second.Endpoints[0].RESTConfig == first.Endpoints[0].RESTConfig {
		t.Fatal("refreshed snapshot reused a REST config pointer")
	}

	wantOptions := []metav1.ListOptions{
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: defaultServiceEndpointSlicePageSize},
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: defaultServiceEndpointSlicePageSize, Continue: "next"},
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: defaultServiceEndpointSlicePageSize},
		{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes", Limit: defaultServiceEndpointSlicePageSize, Continue: "next"},
	}
	if !reflect.DeepEqual(lister.options, wantOptions) {
		t.Fatalf("EndpointSlice LIST options = %#v, want %#v", lister.options, wantOptions)
	}
}

func TestNewDefaultServiceProviderRejectsTransportOverrides(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*rest.Config)
		want   string
	}{
		{
			name: "custom Transport",
			mutate: func(config *rest.Config) {
				config.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
			},
			want: "custom Transport",
		},
		{
			name: "WrapTransport",
			mutate: func(config *rest.Config) {
				config.WrapTransport = transport.WrapperFunc(func(rt http.RoundTripper) http.RoundTripper { return rt })
			},
			want: "WrapTransport",
		},
		{
			name: "custom Dial",
			mutate: func(config *rest.Config) {
				config.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") }
			},
			want: "custom Dial",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(config)
			_, err := NewDefaultServiceProvider(config, &scriptedLister{}, 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewDefaultServiceProvider() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestNewDefaultServiceProviderRejectsInvalidInputs(t *testing.T) {
	validLister := &scriptedLister{}
	tests := []struct {
		name   string
		config *rest.Config
		lister EndpointSliceLister
		burst  int
		want   string
	}{
		{name: "nil config", lister: validLister, burst: 1, want: "REST configuration"},
		{name: "insecure TLS", config: &rest.Config{TLSClientConfig: rest.TLSClientConfig{Insecure: true, CAData: []byte("ca")}}, lister: validLister, burst: 1, want: "verify API server TLS"},
		{name: "missing CA", config: &rest.Config{}, lister: validLister, burst: 1, want: "no API server CA"},
		{name: "nil lister", config: validConfig(), burst: 1, want: "EndpointSlice client"},
		{name: "zero burst", config: validConfig(), lister: validLister, want: "burst must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewDefaultServiceProvider(test.config, test.lister, test.burst)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewDefaultServiceProvider() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestWaitForInitialSnapshotRetriesAndCancellationWins(t *testing.T) {
	t.Run("recovers", func(t *testing.T) {
		calls := 0
		waits := 0
		want := Snapshot{InventoryResourceVersion: "42", InventoryIdentity: "sha256:test"}
		got, err := waitForInitialSnapshot(
			context.Background(),
			func(context.Context) (Snapshot, error) {
				calls++
				if calls == 1 {
					return Snapshot{}, errors.New("discovery unavailable")
				}
				return want, nil
			},
			time.Second,
			func(context.Context, time.Duration) error {
				waits++
				return nil
			},
		)
		if err != nil || !reflect.DeepEqual(got, want) || calls != 2 || waits != 1 {
			t.Fatalf("waitForInitialSnapshot() = %#v, %v after %d calls/%d waits", got, err, calls, waits)
		}
	})

	t.Run("cancellation after provider wins", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		waits := 0
		_, err := waitForInitialSnapshot(
			ctx,
			func(context.Context) (Snapshot, error) {
				cancel()
				return Snapshot{}, errors.New("stale provider error")
			},
			time.Second,
			func(context.Context, time.Duration) error {
				waits++
				return nil
			},
		)
		if !errors.Is(err, context.Canceled) || waits != 0 {
			t.Fatalf("waitForInitialSnapshot() = %v after %d waits, want immediate cancellation", err, waits)
		}
	})

	for _, test := range []struct {
		name      string
		ctx       context.Context
		provider  Provider
		pollEvery time.Duration
		wait      func(context.Context, time.Duration) error
	}{
		{name: "nil context", provider: func(context.Context) (Snapshot, error) { return Snapshot{}, nil }, pollEvery: time.Second, wait: func(context.Context, time.Duration) error { return nil }},
		{name: "nil provider", ctx: context.Background(), pollEvery: time.Second, wait: func(context.Context, time.Duration) error { return nil }},
		{name: "zero poll", ctx: context.Background(), provider: func(context.Context) (Snapshot, error) { return Snapshot{}, nil }, wait: func(context.Context, time.Duration) error { return nil }},
		{name: "nil wait", ctx: context.Background(), provider: func(context.Context) (Snapshot, error) { return Snapshot{}, nil }, pollEvery: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := waitForInitialSnapshot(test.ctx, test.provider, test.pollEvery, test.wait)
			if err == nil {
				t.Fatal("waitForInitialSnapshot() accepted invalid retry configuration")
			}
		})
	}
}

type scriptedLister struct {
	pages   map[string]*discoveryv1.EndpointSliceList
	options []metav1.ListOptions
}

func (l *scriptedLister) List(_ context.Context, options metav1.ListOptions) (*discoveryv1.EndpointSliceList, error) {
	l.options = append(l.options, options)
	page := l.pages[options.Continue]
	if page == nil {
		return nil, errors.New("unexpected EndpointSlice page")
	}
	return page.DeepCopy(), nil
}

func validSlice(name string, addressType discoveryv1.AddressType, port int32, endpoints ...discoveryv1.Endpoint) discoveryv1.EndpointSlice {
	portName := defaultServiceTLSPortName
	protocol := corev1.ProtocolTCP
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       defaultServiceNamespace,
			Labels:          map[string]string{discoveryv1.LabelServiceName: defaultServiceName},
			UID:             types.UID("uid-" + name),
			ResourceVersion: "rv-" + name,
		},
		AddressType: addressType,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port}},
		Endpoints:   endpoints,
	}
}

func validConfig() *rest.Config {
	return &rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: []byte("cluster-ca")}}
}

func endpointAddresses(snapshot Snapshot) []string {
	addresses := make([]string, 0, len(snapshot.Endpoints))
	for _, endpoint := range snapshot.Endpoints {
		addresses = append(addresses, endpoint.Address)
	}
	return addresses
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
