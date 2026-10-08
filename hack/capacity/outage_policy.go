package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// NetworkPolicies are additive. An allow-except policy cannot withdraw an
// existing registry allowance and would open unrelated destinations. In an
// isolated namespace, withdraw only the explicitly named registry policies.
// The original IP-based fault is suitable only for the unrestricted HTTP lab.
func (s *scenarios) registryOutagePolicies(ctx context.Context) ([]*networkingv1.NetworkPolicy, error) {
	if s.load.Outage.Duration == 0 {
		return nil, nil
	}
	var names []string
	if s.in.registryEgressPolicies != "" {
		seen := map[string]bool{}
		for _, part := range strings.Split(s.in.registryEgressPolicies, ",") {
			name := strings.TrimSpace(part)
			if len(validation.IsDNS1123Subdomain(name)) != 0 || seen[name] {
				return nil, fmt.Errorf("invalid or repeated registry egress policy %q", name)
			}
			names, seen[name] = append(names, name), true
		}
	} else {
		ip := net.ParseIP(s.in.registryIP)
		if ip == nil || ip.To4() == nil || s.in.registryCA != "" {
			return nil, fmt.Errorf("the unrestricted HTTP lab needs an IPv4 registry address; use -registry-egress-policies for the isolated TLS profile")
		}
	}
	var originals []*networkingv1.NetworkPolicy
	for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		policies := s.clientset.NetworkingV1().NetworkPolicies(namespace)
		if len(names) == 0 {
			current, err := policies.List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			for _, policy := range current.Items {
				if egressPolicy(policy.Spec) {
					return nil, fmt.Errorf("namespace %s already has egress policy %s; use -registry-egress-policies instead of adding an allow-except policy", namespace, policy.Name)
				}
			}
			continue
		}
		for _, name := range names {
			policy, err := policies.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("read registry policy %s/%s: %w", namespace, name, err)
			}
			if policy.UID == "" || policy.ResourceVersion == "" || policy.DeletionTimestamp != nil ||
				!egressPolicy(policy.Spec) || len(policy.Spec.Egress) == 0 {
				return nil, fmt.Errorf("registry policy %s/%s must have a UID, resource version, and active egress allowance", namespace, name)
			}
			originals = append(originals, policy)
		}
	}
	return originals, nil
}

func egressPolicy(spec networkingv1.NetworkPolicySpec) bool {
	for _, kind := range spec.PolicyTypes {
		if kind == networkingv1.PolicyTypeEgress {
			return true
		}
	}
	return len(spec.Egress) > 0
}

// Read before restoring so metadata changes survive. UID and spec checks refuse
// replacement objects or concurrent policy edits; Update preserves the API's
// resource-version precondition. Already restored policies need no write.
func (s *scenarios) restoreRegistryPolicy(ctx context.Context, original *networkingv1.NetworkPolicy) error {
	policies := s.clientset.NetworkingV1().NetworkPolicies(original.Namespace)
	current, err := policies.Get(ctx, original.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read registry policy for recovery: %w", err)
	}
	if current.UID != original.UID || current.DeletionTimestamp != nil {
		return fmt.Errorf("registry policy %s/%s was replaced or is deleting; refusing recovery write", original.Namespace, original.Name)
	}
	if equality.Semantic.DeepEqual(current.Spec, original.Spec) {
		return nil
	}
	disabled := original.Spec.DeepCopy()
	disabled.Egress = nil
	if !equality.Semantic.DeepEqual(current.Spec, *disabled) {
		return fmt.Errorf("registry policy %s/%s changed during the outage; refusing recovery write", original.Namespace, original.Name)
	}
	current.Spec = *original.Spec.DeepCopy()
	_, err = policies.Update(ctx, current, metav1.UpdateOptions{})
	return err
}
