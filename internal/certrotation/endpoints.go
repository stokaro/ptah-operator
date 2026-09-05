package certrotation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type endpointIdentity struct {
	podUID  string
	address string
	port    int32
}

func (r *Rotator) endpointSnapshot(ctx context.Context) ([]endpointIdentity, error) {
	selector := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: r.config.ServiceName}).String()
	endpointSlices, err := r.client.DiscoveryV1().EndpointSlices(r.config.ServiceNamespace).List(
		ctx, metav1.ListOptions{LabelSelector: selector},
	)
	if err != nil {
		return nil, fmt.Errorf("list webhook Service EndpointSlices: %w", err)
	}

	seenAddresses := make(map[string]struct{})
	var snapshot []endpointIdentity
	for i := range endpointSlices.Items {
		endpointSlice := &endpointSlices.Items[i]
		port, err := endpointSlicePort(endpointSlice, r.config.EndpointPortName)
		if err != nil {
			return nil, err
		}
		for endpointIndex := range endpointSlice.Endpoints {
			endpoint := &endpointSlice.Endpoints[endpointIndex]
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
				return nil, fmt.Errorf("EndpointSlice %q contains an endpoint that is not explicitly ready", endpointSlice.Name)
			}
			if endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
				return nil, fmt.Errorf("EndpointSlice %q contains a terminating endpoint", endpointSlice.Name)
			}
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.UID == "" {
				return nil, fmt.Errorf("EndpointSlice %q contains an endpoint without a Pod UID", endpointSlice.Name)
			}
			if endpoint.TargetRef.Namespace != "" && endpoint.TargetRef.Namespace != r.config.ServiceNamespace {
				return nil, fmt.Errorf("EndpointSlice %q contains a cross-namespace Pod endpoint", endpointSlice.Name)
			}
			if len(endpoint.Addresses) == 0 {
				return nil, fmt.Errorf("EndpointSlice %q contains an endpoint without an address", endpointSlice.Name)
			}
			for _, address := range endpoint.Addresses {
				if net.ParseIP(address) == nil {
					return nil, fmt.Errorf("EndpointSlice %q contains a non-IP address", endpointSlice.Name)
				}
				if _, duplicate := seenAddresses[address]; duplicate {
					return nil, fmt.Errorf("webhook endpoint address %q appears more than once", address)
				}
				seenAddresses[address] = struct{}{}
				snapshot = append(snapshot, endpointIdentity{
					podUID:  string(endpoint.TargetRef.UID),
					address: address,
					port:    port,
				})
			}
		}
	}
	if len(snapshot) == 0 {
		return nil, errors.New("webhook Service has no ready Pod endpoints")
	}
	slices.SortFunc(snapshot, func(left, right endpointIdentity) int {
		if compared := strings.Compare(left.podUID, right.podUID); compared != 0 {
			return compared
		}
		if compared := strings.Compare(left.address, right.address); compared != 0 {
			return compared
		}
		return int(left.port - right.port)
	})
	return snapshot, nil
}

func endpointSlicePort(endpointSlice *discoveryv1.EndpointSlice, name string) (int32, error) {
	var found *int32
	for i := range endpointSlice.Ports {
		port := &endpointSlice.Ports[i]
		if port.Name == nil || *port.Name != name {
			continue
		}
		if port.Port == nil || *port.Port < 1 || *port.Port > 65535 {
			return 0, fmt.Errorf("EndpointSlice %q has an invalid %q port", endpointSlice.Name, name)
		}
		if found != nil && *found != *port.Port {
			return 0, fmt.Errorf("EndpointSlice %q has conflicting %q ports", endpointSlice.Name, name)
		}
		found = port.Port
	}
	if found == nil {
		return 0, fmt.Errorf("EndpointSlice %q has no %q port", endpointSlice.Name, name)
	}
	return *found, nil
}
