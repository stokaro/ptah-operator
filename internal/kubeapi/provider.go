// Package kubeapi discovers and directly addresses every API server advertised
// by the default Kubernetes Service.
package kubeapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	defaultServiceNamespace                     = metav1.NamespaceDefault
	defaultServiceName                          = "kubernetes"
	defaultServiceTLSPortName                   = "https"
	defaultServiceTLSServerName                 = "kubernetes.default.svc"
	defaultServiceEndpointSlicePageSize int64   = 200
	directQueriesPerSecond              float32 = 100
)

// EndpointSliceLister is the discovery client surface needed by the provider.
type EndpointSliceLister interface {
	List(context.Context, metav1.ListOptions) (*discoveryv1.EndpointSliceList, error)
}

// Endpoint is one directly addressable API server from a complete EndpointSlice
// inventory snapshot. RESTConfig retains the source credential and CA while
// pinning transport to Address and TLS SNI to the default Kubernetes Service.
type Endpoint struct {
	Address    string
	RESTConfig *rest.Config
}

// Snapshot is a complete, internally consistent view of API endpoints
// advertised by the default Kubernetes Service. InventoryIdentity fingerprints
// every selected EndpointSlice name, UID, and resourceVersion together with the
// canonical advertised address set. The LIST collection resourceVersion is
// retained separately because unrelated EndpointSlice churn may advance it
// without changing this selected inventory.
type Snapshot struct {
	InventoryResourceVersion string
	InventoryIdentity        string
	Endpoints                []Endpoint
}

// Provider returns the current direct API server endpoint snapshot.
type Provider func(context.Context) (Snapshot, error)

type topology struct {
	inventoryResourceVersion string
	inventoryIdentity        string
	addresses                []string
}

type sliceIdentity struct {
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

// NewDefaultServiceProvider constructs a dynamic provider for every ready API
// server address advertised by the default Kubernetes Service. The returned
// REST configs bypass proxies and retain verified TLS with the Service DNS name
// as SNI. burst must bound one complete consumer sweep.
func NewDefaultServiceProvider(base *rest.Config, slices EndpointSliceLister, burst int) (Provider, error) {
	if err := validateInputs(base, slices); err != nil {
		return nil, err
	}
	if burst <= 0 {
		return nil, fmt.Errorf("direct Kubernetes API endpoint burst must be positive")
	}
	baseConfig := deepCopyRESTConfig(base)

	return func(ctx context.Context) (Snapshot, error) {
		if ctx == nil {
			return Snapshot{}, fmt.Errorf("direct API endpoint discovery context is nil")
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		current, err := discoverTopology(ctx, slices)
		if err != nil {
			return Snapshot{}, err
		}
		endpoints := make([]Endpoint, 0, len(current.addresses))
		for _, address := range current.addresses {
			endpointConfig := deepCopyRESTConfig(baseConfig)
			endpointConfig.Host = "https://" + address
			endpointConfig.ServerName = defaultServiceTLSServerName
			// A process-level HTTPS proxy would collapse independently addressed
			// requests back onto shared infrastructure.
			endpointConfig.Proxy = directProxy
			// One convergence sweep is a bounded burst. Consumers supply their
			// operation-specific bound when constructing this provider.
			endpointConfig.RateLimiter = nil
			endpointConfig.QPS = directQueriesPerSecond
			endpointConfig.Burst = burst
			endpoints = append(endpoints, Endpoint{Address: address, RESTConfig: endpointConfig})
		}
		return Snapshot{
			InventoryResourceVersion: current.inventoryResourceVersion,
			InventoryIdentity:        current.inventoryIdentity,
			Endpoints:                endpoints,
		}, nil
	}, nil
}

// WaitForInitialSnapshot retries fail-closed discovery observations until a
// complete initial topology is available or ctx is canceled.
func WaitForInitialSnapshot(ctx context.Context, provider Provider, pollEvery time.Duration) (Snapshot, error) {
	return waitForInitialSnapshot(ctx, provider, pollEvery, sleep)
}

func waitForInitialSnapshot(
	ctx context.Context,
	provider Provider,
	pollEvery time.Duration,
	wait func(context.Context, time.Duration) error,
) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, fmt.Errorf("direct API endpoint discovery context is nil")
	}
	if provider == nil || pollEvery <= 0 || wait == nil {
		return Snapshot{}, fmt.Errorf("direct API endpoint discovery retry configuration is invalid")
	}
	for {
		snapshot, err := provider(ctx)
		if contextErr := ctx.Err(); contextErr != nil {
			return Snapshot{}, contextErr
		}
		if err == nil {
			return snapshot, nil
		}
		if waitErr := wait(ctx, pollEvery); waitErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return Snapshot{}, contextErr
			}
			return Snapshot{}, fmt.Errorf(
				"direct API endpoint discovery did not recover; last observation: %v: %w",
				err,
				waitErr,
			)
		}
	}
}

func validateInputs(config *rest.Config, slices EndpointSliceLister) error {
	if config == nil {
		return fmt.Errorf("in-cluster REST configuration is required for direct Kubernetes API endpoints")
	}
	if config.Insecure {
		return fmt.Errorf("in-cluster REST configuration must verify API server TLS certificates")
	}
	if config.CAFile == "" && len(config.CAData) == 0 {
		return fmt.Errorf("in-cluster REST configuration has no API server CA")
	}
	if config.Transport != nil {
		return fmt.Errorf("in-cluster REST configuration must not define a custom Transport for direct Kubernetes API endpoints")
	}
	if config.WrapTransport != nil {
		return fmt.Errorf("in-cluster REST configuration must not define WrapTransport for direct Kubernetes API endpoints")
	}
	if config.Dial != nil {
		return fmt.Errorf("in-cluster REST configuration must not define a custom Dial function for direct Kubernetes API endpoints")
	}
	if slices == nil {
		return fmt.Errorf("EndpointSlice client is required for direct Kubernetes API endpoints")
	}
	return nil
}

func deepCopyRESTConfig(config *rest.Config) *rest.Config {
	copy := rest.CopyConfig(config)
	copy.CertData = append([]byte(nil), config.CertData...)
	copy.KeyData = append([]byte(nil), config.KeyData...)
	copy.CAData = append([]byte(nil), config.CAData...)
	copy.NextProtos = append([]string(nil), config.NextProtos...)
	return copy
}

func directProxy(*http.Request) (*url.URL, error) {
	return nil, nil
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func discoverTopology(ctx context.Context, slices EndpointSliceLister) (*topology, error) {
	const selector = discoveryv1.LabelServiceName + "=" + defaultServiceName

	seenTokens := map[string]struct{}{"": {}}
	seenSlices := make(map[string]struct{})
	seenSliceUIDs := make(map[string]string)
	seenAddresses := make(map[string]struct{})
	var addresses []string
	var sliceIdentities []sliceIdentity
	continueToken := ""
	resourceVersion := ""
	firstPage := true

	for {
		page, err := slices.List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			Limit:         defaultServiceEndpointSlicePageSize,
			Continue:      continueToken,
		})
		if err != nil {
			return nil, fmt.Errorf("list default Kubernetes Service EndpointSlices: %w", err)
		}
		if page == nil {
			return nil, fmt.Errorf("list default Kubernetes Service EndpointSlices returned a nil page")
		}
		if page.ResourceVersion == "" {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory returned an empty resourceVersion")
		}
		if page.RemainingItemCount != nil && *page.RemainingItemCount < 0 {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory returned a negative remaining item count")
		}
		if firstPage {
			resourceVersion = page.ResourceVersion
			firstPage = false
		} else if page.ResourceVersion != resourceVersion {
			return nil, fmt.Errorf(
				"default Kubernetes Service EndpointSlice inventory changed resourceVersion across pages: %q then %q",
				resourceVersion,
				page.ResourceVersion,
			)
		}

		for index := range page.Items {
			slice := &page.Items[index]
			if slice.Namespace != defaultServiceNamespace {
				return nil, fmt.Errorf("EndpointSlice %q has namespace %q, want %q", slice.Name, slice.Namespace, defaultServiceNamespace)
			}
			if strings.TrimSpace(slice.Name) == "" || slice.Name != strings.TrimSpace(slice.Name) {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice has an empty or padded name")
			}
			if slice.UID == "" {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice %q has an empty UID", slice.Name)
			}
			if strings.TrimSpace(slice.ResourceVersion) == "" || slice.ResourceVersion != strings.TrimSpace(slice.ResourceVersion) {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice %q has an empty or padded resourceVersion", slice.Name)
			}
			if previous, duplicate := seenSliceUIDs[string(slice.UID)]; duplicate {
				return nil, fmt.Errorf(
					"default Kubernetes Service EndpointSlices %q and %q share UID %q",
					previous,
					slice.Name,
					slice.UID,
				)
			}
			seenSliceUIDs[string(slice.UID)] = slice.Name
			if slice.Labels[discoveryv1.LabelServiceName] != defaultServiceName {
				return nil, fmt.Errorf("EndpointSlice %q is not owned by the default Kubernetes Service", slice.Name)
			}
			if _, duplicate := seenSlices[slice.Name]; duplicate {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice %q appeared more than once", slice.Name)
			}
			seenSlices[slice.Name] = struct{}{}
			sliceIdentities = append(sliceIdentities, sliceIdentity{
				Name:            slice.Name,
				UID:             string(slice.UID),
				ResourceVersion: slice.ResourceVersion,
			})

			port, err := endpointSliceHTTPSPort(slice)
			if err != nil {
				return nil, err
			}
			for endpointIndex := range slice.Endpoints {
				endpoint := &slice.Endpoints[endpointIndex]
				if endpointTerminating(endpoint) {
					continue
				}
				canonicalAddresses, err := endpointIPAddresses(slice, endpoint, endpointIndex)
				if err != nil {
					return nil, err
				}
				if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
					return nil, fmt.Errorf("EndpointSlice %q endpoint %d is non-terminating but not ready", slice.Name, endpointIndex)
				}
				if endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving {
					return nil, fmt.Errorf("EndpointSlice %q endpoint %d is non-terminating but not serving", slice.Name, endpointIndex)
				}
				for _, address := range canonicalAddresses {
					hostPort := net.JoinHostPort(address, fmt.Sprintf("%d", port))
					if _, duplicate := seenAddresses[hostPort]; duplicate {
						continue
					}
					seenAddresses[hostPort] = struct{}{}
					addresses = append(addresses, hostPort)
				}
			}
		}

		next := page.Continue
		if next == "" {
			break
		}
		if _, duplicate := seenTokens[next]; duplicate {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory repeated continue token %q", next)
		}
		seenTokens[next] = struct{}{}
		continueToken = next
	}

	if len(seenSlices) == 0 {
		return nil, fmt.Errorf("default Kubernetes Service has no EndpointSlices")
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("default Kubernetes Service has no ready, serving, non-terminating API server endpoints")
	}
	sort.Strings(addresses)
	sort.Slice(sliceIdentities, func(i, j int) bool {
		return sliceIdentities[i].Name < sliceIdentities[j].Name
	})
	fingerprintInput, err := json.Marshal(struct {
		Slices    []sliceIdentity `json:"slices"`
		Addresses []string        `json:"addresses"`
	}{
		Slices:    sliceIdentities,
		Addresses: addresses,
	})
	if err != nil {
		return nil, fmt.Errorf("encode default Kubernetes Service EndpointSlice inventory identity: %w", err)
	}
	fingerprint := sha256.Sum256(fingerprintInput)
	return &topology{
		inventoryResourceVersion: resourceVersion,
		inventoryIdentity:        fmt.Sprintf("sha256:%x", fingerprint),
		addresses:                addresses,
	}, nil
}

func endpointSliceHTTPSPort(slice *discoveryv1.EndpointSlice) (int32, error) {
	var port int32
	found := false
	for index := range slice.Ports {
		candidate := &slice.Ports[index]
		if candidate.Name == nil || *candidate.Name != defaultServiceTLSPortName {
			continue
		}
		if found {
			return 0, fmt.Errorf("EndpointSlice %q has more than one %q port", slice.Name, defaultServiceTLSPortName)
		}
		if candidate.Port == nil || *candidate.Port < 1 || *candidate.Port > 65535 {
			return 0, fmt.Errorf("EndpointSlice %q has an invalid %q port number", slice.Name, defaultServiceTLSPortName)
		}
		if candidate.Protocol != nil && *candidate.Protocol != corev1.ProtocolTCP {
			return 0, fmt.Errorf("EndpointSlice %q has a non-TCP %q port", slice.Name, defaultServiceTLSPortName)
		}
		port = *candidate.Port
		found = true
	}
	if !found {
		return 0, fmt.Errorf("EndpointSlice %q has no named and numbered %q port", slice.Name, defaultServiceTLSPortName)
	}
	return port, nil
}

func endpointIPAddresses(
	slice *discoveryv1.EndpointSlice,
	endpoint *discoveryv1.Endpoint,
	endpointIndex int,
) ([]string, error) {
	if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
		return nil, fmt.Errorf("EndpointSlice %q has unsupported address type %q", slice.Name, slice.AddressType)
	}
	if len(endpoint.Addresses) == 0 {
		return nil, fmt.Errorf("EndpointSlice %q endpoint %d has no addresses", slice.Name, endpointIndex)
	}

	addresses := make([]string, 0, len(endpoint.Addresses))
	seen := make(map[string]struct{}, len(endpoint.Addresses))
	for _, raw := range endpoint.Addresses {
		parsed := net.ParseIP(raw)
		if parsed == nil || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d has invalid IP address %q", slice.Name, endpointIndex, raw)
		}
		isIPv4 := parsed.To4() != nil
		if (slice.AddressType == discoveryv1.AddressTypeIPv4) != isIPv4 {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d address %q does not match address type %q", slice.Name, endpointIndex, raw, slice.AddressType)
		}
		canonical := parsed.String()
		if _, duplicate := seen[canonical]; duplicate {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d repeats address %q", slice.Name, endpointIndex, canonical)
		}
		seen[canonical] = struct{}{}
		addresses = append(addresses, canonical)
	}
	return addresses, nil
}

func endpointTerminating(endpoint *discoveryv1.Endpoint) bool {
	return endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating
}
