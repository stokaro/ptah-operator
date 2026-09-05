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

func TestControllerWriteGuardNameIsReleaseDistinctAndVersioned(t *testing.T) {
	t.Parallel()

	name := ControllerWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1")
	if !strings.HasPrefix(name, controllerWriteGuardNamePrefix) || len(name) > 63 {
		t.Fatalf("controller write guard name %q is not a bounded versioned name", name)
	}
	if name != ControllerWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1") {
		t.Fatal("controller write guard name is not deterministic")
	}
	if name == ControllerWriteGuardPolicyName("other", "ptah", 1, "manager:v1") ||
		name == ControllerWriteGuardPolicyName("ptah-system", "other", 1, "manager:v1") {
		t.Fatal("controller write guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if ControllerWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage) ==
		ControllerWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName, other.ReleaseSequence, other.ManagerImage) {
		t.Fatal("controller write guard name did not change with candidate release identity")
	}
}

func TestControllerWriteGuardIsExactAndFailClosed(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	policy := guard.policy()
	native := stripAdmissionConvergenceDependencyProbe(t, policy)
	binding := guard.binding()
	if policy.Spec.ParamKind == nil || policy.Spec.ParamKind.APIVersion != "v1" || policy.Spec.ParamKind.Kind != "ConfigMap" ||
		binding.Spec.ParamRef == nil || binding.Spec.ParamRef.Name != ReleaseActivationName || binding.Spec.ParamRef.Namespace != guard.ReleaseNamespace {
		t.Fatal("controller write guard must fail closed on the release activation parameter")
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("controller write guard is not fail-closed")
	}
	assertExactControllerWriteMatch(t, native.Spec.MatchConstraints)
	assertControllerWriteMatchWithConvergenceProbe(t, binding.Spec.MatchResources)

	wantUsername := `request.userInfo.username in ["system:serviceaccount:ptah-system:ptah-controller"]`
	if !reflect.DeepEqual(native.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
		Name: "candidate-or-predecessor-controller-service-account", Expression: wantUsername,
	}}) {
		t.Fatalf("controller caller match is not exact: %#v", native.Spec.MatchConditions)
	}
	if binding.Spec.PolicyName != policy.Name ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("controller write guard binding is not exact deny-only enforcement: %#v", binding.Spec)
	}
}

func TestControllerWriteGuardCELContract(t *testing.T) {
	t.Parallel()

	policy := stripAdmissionConvergenceDependencyProbe(t, testControllerWriteGuard().policy())
	if len(policy.Spec.Variables) != 6 {
		t.Fatalf("controller write guard variables = %d, want six", len(policy.Spec.Variables))
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

	if len(policy.Spec.Validations) != 6 {
		t.Fatalf("controller write guard validations = %d, want six", len(policy.Spec.Validations))
	}
	for index, validation := range policy.Spec.Validations[2:] {
		if validation.Message != controllerWriteGuardDenialMessage() {
			t.Fatalf("structural validation %d lacks the unique denial message", index)
		}
	}
	if policy.Spec.Validations[0].Expression != testControllerWriteGuard().activationParameterExpression() ||
		policy.Spec.Validations[1].Message != controllerPrincipalGuardDenialMessage() {
		t.Fatal("activation shape and controller authority are not validated first")
	}
	if policy.Spec.Validations[2].Expression != `object.spec == oldObject.spec` ||
		!strings.Contains(policy.Spec.Validations[3].Expression, "object.status == oldObject.status") {
		t.Fatal("spec or status immutability is not enforced")
	}
	metadataExpression := policy.Spec.Validations[4].Expression
	for _, field := range []string{"labels", "annotations", "ownerReferences"} {
		if !strings.Contains(metadataExpression, "object.metadata."+field+" == oldObject.metadata."+field) {
			t.Fatalf("mutable metadata field %s is not preserved", field)
		}
	}
	finalizerExpression := policy.Spec.Validations[5].Expression
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
	if activationWeight >= policyWeight || policyWeight >= bindingWeight {
		t.Fatalf("release activation %d must precede controller write hooks %d/%d", activationWeight, policyWeight, bindingWeight)
	}
}

func TestControllerWriteGuardVerifyRejectsContractTampering(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
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
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
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
	guard.ManagerImage = renderedGuardManagerImage
	guard.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, "ptah-e2e-ptah-operator")
	name := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
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
		if policy == nil {
			t.Fatalf("rendered controller write policy: %v; compiled: %#v", err, guard.policy().Spec)
		}
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

func assertControllerWriteMatchWithConvergenceProbe(t *testing.T, match *admissionregistrationv1.MatchResources) {
	t.Helper()
	if match == nil || len(match.ResourceRules) != 2 {
		t.Fatalf("controller write binding rules = %#v, want native rule plus convergence marker rule", match)
	}
	if !reflect.DeepEqual(match.ResourceRules[1], admissionConvergenceProbeResourceRule()) {
		t.Fatalf("controller write binding convergence rule = %#v, want %#v", match.ResourceRules[1], admissionConvergenceProbeResourceRule())
	}
	native := match.DeepCopy()
	native.ResourceRules = native.ResourceRules[:1]
	assertExactControllerWriteMatch(t, native)
}

func testControllerWriteGuard() *ControllerWriteGuard {
	return &ControllerWriteGuard{
		Policies:                     &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                     &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
		ReleaseSequence:              1,
		ManagerImage:                 "registry.example/ptah@sha256:" + strings.Repeat("a", 64),
		PollEvery:                    time.Millisecond,
	}
}
