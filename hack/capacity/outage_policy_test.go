package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func registryAllowance(namespace, name string) *networkingv1.NetworkPolicy {
	port := intstr.FromInt32(5443)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "/" + name), ResourceVersion: "1"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "registry"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
			}},
		},
	}
}

func TestOutagePreflightRejectsAdditivePolicyBeforeCreatingFleet(t *testing.T) {
	t.Parallel()
	s := twoNamespaceScenarios()
	s.in.registryIP, s.load.Outage = "192.0.2.1", duration{time.Second}
	s.clientset = fake.NewClientset(registryAllowance("work-b", "registry"))
	if err := runScenarios(context.Background(), s); err == nil || !strings.Contains(err.Error(), "already has egress policy") {
		t.Fatal("an additive fault reached the workload", err)
	}
	for _, action := range s.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() != "list" {
			t.Fatal("preflight mutated the lab", action)
		}
	}
}

func TestOutageWithdrawsOnlyNamedAllowancesAndRestoresOnFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"no observation", "second namespace read", "ambiguous update"} {
		t.Run(failure, func(t *testing.T) {
			s := twoNamespaceScenarios()
			s.in.registryEgressPolicies, s.load.Outage = "schema-registry,migration-registry", duration{time.Nanosecond}
			var objects []runtime.Object
			for _, namespace := range s.in.namespaces {
				for _, name := range []string{"schema-registry", "migration-registry", "database", "results", "dns"} {
					objects = append(objects, registryAllowance(namespace, name))
				}
			}
			client := fake.NewClientset(objects...)
			s.clientset = client
			if failure == "ambiguous update" {
				failed := false
				client.PrependReactor("update", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
					obj := a.(clienttesting.UpdateAction).GetObject().(*networkingv1.NetworkPolicy)
					if obj.Namespace != "work-b" || failed {
						return false, nil, nil
					}
					failed = true
					if err := client.Tracker().Update(a.GetResource(), obj.DeepCopy(), obj.Namespace); err != nil {
						t.Fatal(err)
					}
					return true, nil, errors.New("lost update response")
				})
			}
			client.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				for _, object := range objects {
					original := object.(*networkingv1.NetworkPolicy)
					stored, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), original.Namespace, original.Name)
					if err != nil {
						t.Fatal(err)
					}
					current := stored.(*networkingv1.NetworkPolicy)
					want := original.Spec.DeepCopy()
					if strings.HasSuffix(original.Name, "-registry") {
						want.Egress = nil
					}
					if !equality.Semantic.DeepEqual(current.Spec, *want) {
						t.Fatalf("fault changed the wrong rules: %s/%s", original.Namespace, original.Name)
					}
				}
				if failure == "second namespace read" && a.GetNamespace() == "work-b" {
					return true, nil, errors.New("second namespace read failed")
				}
				return false, nil, nil
			})
			want := map[string]string{"no observation": "no fresh registry-read failure", "second namespace read": "second namespace read failed", "ambiguous update": "lost update response"}[failure]
			if err := s.outage(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("unexpected fault result", err)
			}
			for _, object := range objects {
				original := object.(*networkingv1.NetworkPolicy)
				current, err := client.NetworkingV1().NetworkPolicies(original.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
				if err != nil || !equality.Semantic.DeepEqual(current, original) {
					t.Fatalf("failed outage did not restore %s/%s: %v", original.Namespace, original.Name, err)
				}
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" || action.GetVerb() == "delete" {
					t.Fatal("isolated outage must not add an allow-all policy or delete existing policies")
				}
			}
		})
	}
}

func TestRegistryPolicyRecoveryRefusesReplacementAndChangedRules(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"UID", "selector", "allowance", "metadata"} {
		t.Run(change, func(t *testing.T) {
			original := registryAllowance("work", "registry")
			current := original.DeepCopy()
			current.Spec.Egress, current.ResourceVersion = nil, "2"
			switch change {
			case "UID":
				current.UID = "replacement"
			case "selector":
				current.Spec.PodSelector = metav1.LabelSelector{}
			case "allowance":
				current.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			case "metadata":
				current.Annotations = map[string]string{"concurrent-metadata": "preserve"}
			}
			client := fake.NewClientset(current)
			s := &scenarios{clientset: client}
			err := s.restoreRegistryPolicy(context.Background(), original)
			if change == "metadata" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := client.NetworkingV1().NetworkPolicies("work").Get(context.Background(), "registry", metav1.GetOptions{})
				if err != nil || got.Annotations["concurrent-metadata"] != "preserve" || got.ResourceVersion != "2" || !equality.Semantic.DeepEqual(got.Spec, original.Spec) {
					t.Fatal("recovery lost metadata or the current resource version", got, err)
				}
			} else {
				if err == nil {
					t.Fatal("recovery overwrote concurrent changes")
				}
				if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
					t.Fatal("recovery made a write after detecting a concurrent change")
				}
			}
		})
	}
}
