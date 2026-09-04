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

func TestControllerWriteGuardNameIsStableAndVersioned(t *testing.T) {
	t.Parallel()

	name := ControllerWriteGuardPolicyName("ptah-system", "ptah")
	if !strings.HasPrefix(name, controllerWriteGuardNamePrefix) || len(name) > 63 {
		t.Fatalf("controller write guard name %q is not a bounded versioned name", name)
	}
	if name != ControllerWriteGuardPolicyName("ptah-system", "ptah") {
		t.Fatal("controller write guard name is not deterministic")
	}
	if name == ControllerWriteGuardPolicyName("other", "ptah") ||
		name == ControllerWriteGuardPolicyName("ptah-system", "other") {
		t.Fatal("controller write guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if ControllerWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName) !=
		ControllerWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName) {
		t.Fatal("stable controller write guard name changed with candidate release identity")
	}
}

func TestControllerWriteGuardIsExactAndFailClosed(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	policy := guard.policy()
	binding := guard.binding()
	if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
		t.Fatal("controller write guard must not depend on an admission parameter")
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("controller write guard is not fail-closed")
	}
	assertExactControllerWriteMatch(t, policy.Spec.MatchConstraints)
	assertExactControllerWriteMatch(t, binding.Spec.MatchResources)

	wantUsername := `request.userInfo.username == "system:serviceaccount:ptah-system:ptah-controller"`
	if !reflect.DeepEqual(policy.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
		Name: "exact-controller-service-account", Expression: wantUsername,
	}}) {
		t.Fatalf("controller caller match is not exact: %#v", policy.Spec.MatchConditions)
	}
	if binding.Spec.PolicyName != policy.Name ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("controller write guard binding is not exact deny-only enforcement: %#v", binding.Spec)
	}
}

func TestControllerWriteGuardCELContract(t *testing.T) {
	t.Parallel()

	policy := testControllerWriteGuard().policy()
	if len(policy.Spec.Variables) != 5 {
		t.Fatalf("controller write guard variables = %d, want five", len(policy.Spec.Variables))
	}
	variables := map[string]string{}
	for _, variable := range policy.Spec.Variables {
		variables[variable.Name] = variable.Expression
	}
	if !strings.Contains(variables["oldFinalizers"], "oldObject.metadata.finalizers") ||
		!strings.Contains(variables["newFinalizers"], "object.metadata.finalizers") ||
		variables["activeFinalizer"] != `"operator.ptah.dev/active-operation"` ||
		!strings.Contains(variables["oldActiveCount"], "filter") ||
		!strings.Contains(variables["newActiveCount"], "filter") {
		t.Fatalf("controller finalizer variables are incomplete: %#v", variables)
	}

	if len(policy.Spec.Validations) != 4 {
		t.Fatalf("controller write guard validations = %d, want four", len(policy.Spec.Validations))
	}
	for index, validation := range policy.Spec.Validations {
		if validation.Message != controllerWriteGuardDenialMessage() {
			t.Fatalf("validation %d lacks the unique denial message", index)
		}
	}
	if policy.Spec.Validations[0].Expression != `object.spec == oldObject.spec` ||
		!strings.Contains(policy.Spec.Validations[1].Expression, "object.status == oldObject.status") {
		t.Fatal("spec or status immutability is not enforced")
	}
	metadataExpression := policy.Spec.Validations[2].Expression
	for _, field := range []string{"labels", "annotations", "ownerReferences"} {
		if !strings.Contains(metadataExpression, "object.metadata."+field+" == oldObject.metadata."+field) {
			t.Fatalf("mutable metadata field %s is not preserved", field)
		}
	}
	finalizerExpression := policy.Spec.Validations[3].Expression
	for _, contract := range []string{
		"variables.oldActiveCount <= 1",
		"variables.newActiveCount <= 1",
		"variables.oldFinalizers == variables.newFinalizers",
		"variables.newFinalizers.filter(value, value != variables.activeFinalizer) == variables.oldFinalizers",
		"variables.newFinalizers == variables.oldFinalizers.filter(value, value != variables.activeFinalizer)",
	} {
		if !strings.Contains(finalizerExpression, contract) {
			t.Fatalf("finalizer contract lacks %q", contract)
		}
	}
}

func TestControllerWriteGuardPrecedesManagerPrivileges(t *testing.T) {
	t.Parallel()

	policyWeight, err := strconv.Atoi(controllerWritePolicyWeight)
	if err != nil {
		t.Fatal(err)
	}
	bindingWeight, err := strconv.Atoi(controllerWriteBindingWeight)
	if err != nil {
		t.Fatal(err)
	}
	activationWeight, err := strconv.Atoi(releaseActivationHookWeight)
	if err != nil {
		t.Fatal(err)
	}
	if policyWeight >= bindingWeight || bindingWeight >= activationWeight {
		t.Fatalf("controller write hook order %d/%d must precede activation %d", policyWeight, bindingWeight, activationWeight)
	}
}

func TestControllerWriteGuardVerifyRejectsContractTampering(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policy := guard.policy()
	binding := guard.binding()
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: policy}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: binding}}
	if err := guard.Verify(context.Background()); err != nil {
		t.Fatalf("verify exact controller write guard: %v", err)
	}

	policy.Spec.Validations[0].Expression = "true"
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered controller write policy error = %v", err)
	}
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: guard.policy()}}
	binding.Spec.MatchResources.ResourceRules[0].Resources = []string{"ptahschemas/status"}
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered controller write binding error = %v", err)
	}
}

func TestControllerWriteGuardWaitReady(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: readyPolicy(guard.policy())}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: guard.binding()}}
	if err := guard.WaitReady(context.Background()); err != nil {
		t.Fatalf("wait for ready controller write guard: %v", err)
	}
}

func TestRenderedControllerWriteGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testControllerWriteGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.ControllerServiceAccountName = "ptah-e2e-ptah-operator"
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
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
		t.Fatalf("rendered controller write policy: %v\nrendered: %#v\ncompiled: %#v", err, policy.Spec, guard.policy().Spec)
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered controller write binding: %v", err)
	}
	if policy.Annotations["helm.sh/hook-weight"] != controllerWritePolicyWeight ||
		binding.Annotations["helm.sh/hook-weight"] != controllerWriteBindingWeight {
		t.Fatal("controller write guard must be installed before manager privileges")
	}
}

func assertExactControllerWriteMatch(t *testing.T, match *admissionregistrationv1.MatchResources) {
	t.Helper()
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("controller write guard matching is not Exact")
	}
	if match.NamespaceSelector == nil || len(match.NamespaceSelector.MatchLabels) != 0 ||
		len(match.NamespaceSelector.MatchExpressions) != 0 || match.ObjectSelector == nil ||
		len(match.ObjectSelector.MatchLabels) != 0 || len(match.ObjectSelector.MatchExpressions) != 0 ||
		len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("controller write guard must declare exact match-all selectors without exclusions: %#v", match)
	}
	if len(match.ResourceRules) != 1 {
		t.Fatalf("controller write guard rules = %d, want one", len(match.ResourceRules))
	}
	rule := match.ResourceRules[0]
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Update}) ||
		!reflect.DeepEqual(rule.APIGroups, []string{"operator.ptah.dev"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"v1alpha1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"ptahschemas"}) ||
		len(rule.ResourceNames) != 0 || rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
		t.Fatalf("controller write rule is not exact: %#v", rule)
	}
}

func testControllerWriteGuard() *ControllerWriteGuard {
	return &ControllerWriteGuard{
		Policies:                     &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                     &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
		PollEvery:                    time.Millisecond,
	}
}
