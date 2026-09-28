package crdupgrade

import (
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
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
	policy := guard.Policy()
	binding := guard.Binding()
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

	policy := testControllerWriteGuard().Policy()
	if len(policy.Spec.Variables) != 8 {
		t.Fatalf("controller write guard variables = %d, want eight", len(policy.Spec.Variables))
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
	if variables["managedAnnotation"] != `request.resource.resource == "ptahmigrations" ? "operator.ptah.run/unresolved-run" : ""` ||
		!strings.Contains(variables["oldAnnotations"], "oldObject.metadata.annotations") ||
		!strings.Contains(variables["newAnnotations"], "object.metadata.annotations") {
		t.Fatalf("controller annotation variables are incomplete: %#v", variables)
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
	for _, field := range []string{"labels", "ownerReferences"} {
		if !strings.Contains(metadataExpression, "object.metadata."+field+" == oldObject.metadata."+field) {
			t.Fatalf("mutable metadata field %s is not preserved", field)
		}
	}
	for _, contract := range []string{
		"variables.newAnnotations.all(key, key == variables.managedAnnotation || (key in variables.oldAnnotations && variables.oldAnnotations[key] == variables.newAnnotations[key]))",
		"variables.oldAnnotations.all(key, key == variables.managedAnnotation || key in variables.newAnnotations)",
	} {
		if !strings.Contains(metadataExpression, contract) {
			t.Fatalf("annotation contract lacks %q", contract)
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
