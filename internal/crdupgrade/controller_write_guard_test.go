package crdupgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The guard is an ordinary release object that an upgrade updates in place, so
// its name follows the release and nothing that changes between versions of
// it.
func TestControllerWriteGuardNameIsReleaseDistinctAndVersioned(t *testing.T) {
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
}

func TestControllerWriteGuardIsExactAndFailClosed(t *testing.T) {
	t.Parallel()

	guard := testControllerWriteGuard()
	policy := guard.policy()
	binding := guard.binding()
	if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
		t.Fatal("controller write guard reads a parameter")
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("controller write guard is not fail-closed")
	}
	assertExactControllerWriteMatch(t, policy.Spec.MatchConstraints)
	assertExactControllerWriteMatch(t, binding.Spec.MatchResources)

	wantUsername := `request.userInfo.username == "system:serviceaccount:ptah-system:ptah-controller"`
	if !reflect.DeepEqual(policy.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
		Name: "controller-service-account", Expression: wantUsername,
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
		variables["activeFinalizer"] != `request.resource.resource == "ptahmigrations" ? "operator.ptah.run/migration-operation" : "operator.ptah.run/active-operation"` ||
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
	if policy.Spec.Validations[0].Expression != `dyn(object).spec == dyn(oldObject).spec` ||
		!strings.Contains(policy.Spec.Validations[1].Expression, "dyn(object).status == dyn(oldObject).status") {
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

func TestRenderedControllerWriteGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_CONTROLLER_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_CONTROLLER_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testControllerWriteGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, "ptah-e2e-ptah-operator")
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
	if policy == nil || binding == nil {
		t.Fatalf("the chart does not render the controller write guard policy and binding %s", name)
	}
	if !reflect.DeepEqual(policy.Spec, guard.policy().Spec) {
		t.Fatalf("rendered controller write policy differs from the compiled contract\nrendered: %#v\ncompiled: %#v", policy.Spec, guard.policy().Spec)
	}
	if !reflect.DeepEqual(binding.Spec, guard.binding().Spec) {
		t.Fatalf("rendered controller write binding differs from the compiled contract: %#v", binding.Spec)
	}
	for _, metadata := range []metav1.ObjectMeta{policy.ObjectMeta, binding.ObjectMeta} {
		if metadata.Annotations["helm.sh/hook"] != "" || metadata.Annotations["helm.sh/resource-policy"] != "" ||
			metadata.Labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Fatalf("rendered controller write guard %s is not an ordinary release object: annotations %v, labels %v", metadata.Name, metadata.Annotations, metadata.Labels)
		}
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
		!reflect.DeepEqual(rule.APIGroups, []string{"operator.ptah.run"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"v1alpha1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"ptahschemas", "ptahmigrations"}) ||
		len(rule.ResourceNames) != 0 || rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
		t.Fatalf("controller write rule is not exact: %#v", rule)
	}
}

func testControllerWriteGuard() *ControllerWriteGuard {
	return &ControllerWriteGuard{
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
	}
}
