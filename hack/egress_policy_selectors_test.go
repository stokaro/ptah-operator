package main

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The published egress example is a set of selectors, and a selector that
// matches nothing looks exactly like one that matches everything it should.
// That is not hypothetical here: every policy in this file once selected
// schema-operation, so migration Pods were covered by nothing, and the render
// was valid YAML throughout.
//
// So the example is held to the labels the builders actually put on a Pod,
// taken from the workload package rather than retyped beside it.

const egressExample = "examples/networkpolicy-egress.yaml"

// podLabels is what a dispatched Pod of one family and operation carries. The
// values that vary per resource are not part of any selector in the example
// and are given placeholders; the keys are what the selectors match on.
func podLabels(component, resourceKey, resourceName, operation string) labels.Set {
	set := labels.Set{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   component,
		workload.LabelOperation:   operation,
		resourceKey:               resourceName,
		workload.LabelOperationID: "0123456789abcdef",
	}
	return set
}

// Every operation each family dispatches a Pod for. A policy set that covers
// four of five operations refuses nothing for the fifth.
func dispatchedPods() map[string]labels.Set {
	pods := map[string]labels.Set{}
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
		operatorv1alpha1.OperationApply,
	} {
		name := "PtahSchema/" + string(operation)
		pods[name] = podLabels(workload.ComponentSchemaOperation,
			workload.LabelSchema, "orders", strings.ToLower(string(operation)))
	}
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve,
		operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory,
		operatorv1alpha1.MigrationOperationApply,
	} {
		name := "PtahMigration/" + string(operation)
		pods[name] = podLabels(workload.ComponentMigrationOperation,
			workload.LabelMigration, "orders", strings.ToLower(string(operation)))
	}
	return pods
}

func egressPolicies(t *testing.T) []networkingv1.NetworkPolicy {
	t.Helper()

	var policies []networkingv1.NetworkPolicy
	for _, document := range strings.Split(string(readRepositoryFile(t, egressExample)), "\n---") {
		if strings.TrimSpace(stripComments(document)) == "" {
			continue
		}
		policy := networkingv1.NetworkPolicy{}
		if err := yaml.Unmarshal([]byte(document), &policy); err != nil {
			t.Fatalf("the example does not parse as Kubernetes objects: %v", err)
		}
		if policy.Kind != "NetworkPolicy" {
			continue
		}
		policies = append(policies, policy)
	}
	if len(policies) == 0 {
		t.Fatal("the example carries no NetworkPolicy, so nothing below was measured")
	}
	return policies
}

func stripComments(document string) string {
	var kept []string
	for _, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// A policy named for one family must not select the other, and every Pod
// either family dispatches must be selected by something.
//
// Those are two assertions because one alone passes through the defect this
// file once had. Checking only coverage does not catch it: the DNS policy
// selects both families through a matchExpression, so it keeps coverage true
// while every family-specific policy points at the wrong component. Checking
// only the family does not catch an operation nobody wrote a rule for.
//
// What is deliberately not asserted is that a family policy covers all of its
// family's operations. It should not: `ptah-schema-operations-database` skips
// Resolve and Verify because they open no database connection, and that is the
// least privilege the example is for.
func TestNoEgressPolicySelectsTheOtherFamily(t *testing.T) {
	t.Parallel()

	pods := dispatchedPods()
	for _, policy := range egressPolicies(t) {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatalf("policy %q has an unusable selector: %v", policy.Name, err)
		}
		forbidden := ""
		switch {
		case strings.HasPrefix(policy.Name, "ptah-schema-"):
			forbidden = "PtahMigration"
		case strings.HasPrefix(policy.Name, "ptah-migration-"):
			forbidden = "PtahSchema"
		default:
			continue
		}
		for name, set := range pods {
			if strings.HasPrefix(name, forbidden+"/") && selector.Matches(set) {
				t.Errorf("policy %q is named for the other family and selects a %s Pod",
					policy.Name, name)
			}
		}
	}
}

func TestEveryDispatchedPodIsSelectedBySomething(t *testing.T) {
	t.Parallel()

	policies := egressPolicies(t)
	for name, set := range dispatchedPods() {
		matched := false
		for _, policy := range policies {
			selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
			if err != nil {
				t.Fatalf("policy %q has an unusable selector: %v", policy.Name, err)
			}
			if selector.Matches(set) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("no policy selects a %s Pod, so the example governs nothing for it", name)
		}
	}
}

// The negative case the criterion asks for: a Pod this operator did not
// dispatch is selected by nothing.
func TestTheEgressExampleSelectsNothingElse(t *testing.T) {
	t.Parallel()

	foreign := []struct {
		name string
		set  labels.Set
	}{
		{"another operator's Pod", labels.Set{
			workload.LabelManagedBy: "some-other-operator",
			workload.LabelComponent: workload.ComponentSchemaOperation,
		}},
		{"a component this operator does not run", labels.Set{
			workload.LabelManagedBy: "ptah-operator",
			workload.LabelComponent: "controller",
		}},
		{"a Pod with no component at all", labels.Set{
			workload.LabelManagedBy: "ptah-operator",
		}},
	}

	for _, policy := range egressPolicies(t) {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatalf("policy %q has an unusable selector: %v", policy.Name, err)
		}
		for _, other := range foreign {
			if selector.Matches(other.set) {
				t.Errorf("policy %q selects %s", policy.Name, other.name)
			}
		}
	}
}
