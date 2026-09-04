package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestNamespaceDeletionGuardNameIsStableAndVersioned(t *testing.T) {
	name := NamespaceDeletionGuardPolicyName("ptah-system", "ptah")
	if !strings.HasPrefix(name, namespaceDeletionGuardNamePrefix) || len(name) > 63 {
		t.Fatalf("namespace deletion guard name %q is not a bounded versioned name", name)
	}
	if name != NamespaceDeletionGuardPolicyName("ptah-system", "ptah") {
		t.Fatal("namespace deletion guard name is not deterministic")
	}
	if name == NamespaceDeletionGuardPolicyName("other", "ptah") ||
		name == NamespaceDeletionGuardPolicyName("ptah-system", "other") {
		t.Fatal("namespace deletion guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	guard := NewNamespaceDeletionGuard(rollout)
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName) !=
		NamespaceDeletionGuardPolicyName(other.ReleaseNamespace, other.ReleaseName) {
		t.Fatal("stable namespace deletion guard name changed with candidate release identity")
	}
}

func TestNamespaceDeletionGuardMatchesOnlyExactReleaseNamespaceDelete(t *testing.T) {
	guard := testNamespaceDeletionGuard()
	policy := guard.policy()
	binding := guard.binding()

	if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
		t.Fatal("namespace deletion guard must not depend on an admission parameter")
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("namespace deletion guard is not fail-closed")
	}
	assertExactNamespaceDeletionMatch(t, policy.Spec.MatchConstraints, guard.ReleaseNamespace)
	assertExactNamespaceDeletionMatch(t, binding.Spec.MatchResources, guard.ReleaseNamespace)
	if len(policy.Spec.MatchConditions) != 0 || len(policy.Spec.Variables) != 0 {
		t.Fatal("namespace deletion guard must not widen its exact-name rule with conditions or variables")
	}
	if len(policy.Spec.Validations) != 1 || policy.Spec.Validations[0].Expression != "false" ||
		policy.Spec.Validations[0].Message != namespaceDeletionGuardDenialMessage() {
		t.Fatalf("namespace deletion guard must deny every matched request: %#v", policy.Spec.Validations)
	}
	if strings.Contains(policy.Spec.Validations[0].Expression, "userInfo") {
		t.Fatal("namespace deletion guard unexpectedly grants a caller exception")
	}
	if binding.Spec.PolicyName != policy.Name ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("namespace deletion guard binding is not exact deny-only enforcement: %#v", binding.Spec)
	}
}

func TestNamespaceDeletionGuardPrecedesParameterizedAdmissionResources(t *testing.T) {
	policyWeight, err := strconv.Atoi(namespaceDeletionPolicyWeight)
	if err != nil {
		t.Fatal(err)
	}
	bindingWeight, err := strconv.Atoi(namespaceDeletionBindingWeight)
	if err != nil {
		t.Fatal(err)
	}
	activationWeight, err := strconv.Atoi(releaseActivationHookWeight)
	if err != nil {
		t.Fatal(err)
	}
	if policyWeight >= bindingWeight || bindingWeight >= activationWeight {
		t.Fatalf("namespace deletion hook order %d/%d must precede activation %d", policyWeight, bindingWeight, activationWeight)
	}
}

func TestNamespaceDeletionGuardVerifyRejectsContractTampering(t *testing.T) {
	guard := testNamespaceDeletionGuard()
	name := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policy := guard.policy()
	binding := guard.binding()
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: policy}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: binding}}
	if err := guard.Verify(context.Background()); err != nil {
		t.Fatalf("verify exact namespace deletion guard: %v", err)
	}

	policy.Spec.Validations[0].Expression = `request.userInfo.username == "allowed"`
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered namespace deletion policy error = %v", err)
	}
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: guard.policy()}}
	binding.Spec.MatchResources.ResourceRules[0].ResourceNames = []string{"other"}
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered namespace deletion binding error = %v", err)
	}
}

func TestNamespaceDeletionGuardWaitReady(t *testing.T) {
	guard := testNamespaceDeletionGuard()
	name := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: readyPolicy(guard.policy())}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: guard.binding()}}
	if err := guard.WaitReady(context.Background()); err != nil {
		t.Fatalf("wait for ready namespace deletion guard: %v", err)
	}
}

func TestRenderedNamespaceDeletionGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testNamespaceDeletionGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	name := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				binding = &object
			}
		}
	}
	if err := guard.verifyPolicy(policy); err != nil {
		t.Fatalf("rendered namespace deletion policy: %v", err)
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered namespace deletion binding: %v", err)
	}
	if policy.Annotations["helm.sh/hook-weight"] != namespaceDeletionPolicyWeight ||
		binding.Annotations["helm.sh/hook-weight"] != namespaceDeletionBindingWeight {
		t.Fatal("namespace deletion guard must be installed before parameterized admission resources")
	}
}

func assertExactNamespaceDeletionMatch(t *testing.T, match *admissionregistrationv1.MatchResources, namespace string) {
	t.Helper()
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("namespace deletion guard matching is not Exact")
	}
	if match.NamespaceSelector == nil || len(match.NamespaceSelector.MatchLabels) != 0 ||
		len(match.NamespaceSelector.MatchExpressions) != 0 || match.ObjectSelector == nil ||
		len(match.ObjectSelector.MatchLabels) != 0 || len(match.ObjectSelector.MatchExpressions) != 0 ||
		len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("namespace deletion guard must declare exact match-all selectors without exclusions: %#v", match)
	}
	if len(match.ResourceRules) != 1 {
		t.Fatalf("namespace deletion guard rules = %d, want one", len(match.ResourceRules))
	}
	rule := match.ResourceRules[0]
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Delete}) ||
		!reflect.DeepEqual(rule.APIGroups, []string{""}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"v1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"namespaces"}) ||
		!reflect.DeepEqual(rule.ResourceNames, []string{namespace}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.ClusterScope {
		t.Fatalf("namespace deletion rule is not exact: %#v", rule)
	}
}

func testNamespaceDeletionGuard() *NamespaceDeletionGuard {
	return &NamespaceDeletionGuard{
		Policies:         &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:         &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:      "ptah",
		ReleaseNamespace: "ptah-system",
		PollEvery:        time.Millisecond,
	}
}
