package crdupgrade

import (
	"reflect"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const managerStateProofManager = "system:serviceaccount:ptah-system:ptah-controller"

func testManagerStateGuard() *ManagerStateGuard {
	return &ManagerStateGuard{
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
	}
}

// The guards spell the annotation the way the finalizers are spelled, so the
// spelling has to be held to the API's.
func TestTheGuardsNameTheAnnotationTheAPIDoes(t *testing.T) {
	t.Parallel()
	if unresolvedRunAnnotation != operatorv1alpha1.UnresolvedRunAnnotation {
		t.Fatalf("guards name %q, the API names %q", unresolvedRunAnnotation, operatorv1alpha1.UnresolvedRunAnnotation)
	}
}

// Both policies fail closed, deny, and are bound to exactly what they match.
func TestManagerStateGuardsAreFailClosedAndBoundAsMatched(t *testing.T) {
	t.Parallel()

	guards := testManagerStateGuard().Policies()
	if len(guards) != 2 {
		t.Fatalf("manager state guards = %d, want the status guard and the unresolved-run guard", len(guards))
	}
	for _, guard := range guards {
		policy, binding := guard.Policy, guard.Binding
		if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
			t.Fatalf("%s does not fail closed", policy.Name)
		}
		if binding.Spec.PolicyName != policy.Name ||
			!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) ||
			!reflect.DeepEqual(binding.Spec.MatchResources, policy.Spec.MatchConstraints) {
			t.Fatalf("%s binding is not exact deny-only enforcement of what the policy matches: %#v", policy.Name, binding.Spec)
		}
		if len(policy.Spec.MatchConditions) != 0 {
			t.Fatalf("%s exempts through a match condition, which no row can rewrite: %#v", policy.Name, policy.Spec.MatchConditions)
		}
	}
}

// The status guard selects the status subresource of every kind the operator
// ships that has one, by rule rather than by list, so a kind added later is
// already covered. It selects no main-resource write.
func TestStatusWriteGuardSelectsEveryStatusSubresource(t *testing.T) {
	t.Parallel()

	policy := testManagerStateGuard().Policies()[0].Policy
	candidates, err := Candidates()
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for _, crd := range candidates {
		for _, version := range crd.Spec.Versions {
			resource := schema.GroupVersionResource{Group: crd.Spec.Group, Version: version.Name, Resource: crd.Spec.Names.Plural}
			kind := schema.GroupVersionKind{Group: crd.Spec.Group, Version: version.Name, Kind: crd.Spec.Names.Kind}
			if version.Subresources != nil && version.Subresources.Status != nil {
				if !controllerWriteRulesMatch(policy.Spec.MatchConstraints.ResourceRules, statusUpdate(resource, kind, "status")) {
					t.Errorf("the status guard does not select an UPDATE of %s/status", resource.Resource)
				}
				covered++
			}
			if controllerWriteRulesMatch(policy.Spec.MatchConstraints.ResourceRules, statusUpdate(resource, kind, "")) {
				t.Errorf("the status guard selects a main-resource UPDATE of %s", resource.Resource)
			}
		}
	}
	if covered != 7 {
		t.Fatalf("the status guard was held to %d status subresources, want the seven the operator ships", covered)
	}
}

func statusUpdate(resource schema.GroupVersionResource, kind schema.GroupVersionKind, subresource string) admission.Attributes {
	return admission.NewAttributesRecord(nil, nil, kind, "team-a", "orders", resource, subresource,
		admission.Update, nil, false, &user.DefaultInfo{Name: "someone"})
}

// Only the manager writes status, whoever else asks.
func TestStatusWriteGuardAdmitsTheManagerAlone(t *testing.T) {
	t.Parallel()

	policy := testManagerStateGuard().Policies()[0].Policy
	for _, test := range []struct {
		username string
		admitted bool
	}{
		{username: managerStateProofManager, admitted: true},
		{username: "dba@example.test"},
		{username: "kubernetes-admin"},
		{username: "system:serviceaccount:ptah-system:ptah-crd-manager"},
		{username: "system:serviceaccount:team-a:ptah-controller"},
	} {
		request := managerStateRequest("ptahmigrations", "status", test.username)
		object := controllerWriteSubject("ptahmigrations", nil)
		results := evaluatePolicyValidations(t, policy, object, object, request)
		if results[0] != test.admitted {
			t.Errorf("a status write by %s admitted = %t, want %t", test.username, results[0], test.admitted)
		}
	}
}

// The copy of an unresolved run is the manager's once the resource exists. A
// person may not add it, change it or remove it; an ordinary edit that leaves
// it where it is goes through.
func TestUnresolvedRunGuardHoldsTheCopyToTheManager(t *testing.T) {
	t.Parallel()

	policy := testManagerStateGuard().Policies()[1].Policy
	withCopy := func(value string) func(map[string]any) {
		return func(object map[string]any) {
			object["metadata"].(map[string]any)["annotations"].(map[string]any)[unresolvedRunAnnotation] = value
		}
	}
	editSpec := func(object map[string]any) { object["spec"].(map[string]any)["suspend"] = true }
	for _, test := range []struct {
		name     string
		username string
		old, new []func(map[string]any)
		admitted bool
	}{
		{name: "a person adds the copy", username: "dba@example.test", new: []func(map[string]any){withCopy("{}")}},
		{name: "a person removes the copy", username: "dba@example.test", old: []func(map[string]any){withCopy("{}")}},
		{
			name: "a person rewrites the copy", username: "dba@example.test",
			old: []func(map[string]any){withCopy(`{"a":1}`)}, new: []func(map[string]any){withCopy(`{"a":2}`)},
		},
		{
			name: "a person edits spec and leaves the copy", username: "dba@example.test",
			old: []func(map[string]any){withCopy("{}")}, new: []func(map[string]any){withCopy("{}"), editSpec},
			admitted: true,
		},
		{
			name: "a person edits a migration with no copy", username: "dba@example.test",
			new: []func(map[string]any){editSpec}, admitted: true,
		},
		{name: "the manager writes the copy", username: managerStateProofManager, new: []func(map[string]any){withCopy("{}")}, admitted: true},
		{name: "the manager removes the copy", username: managerStateProofManager, old: []func(map[string]any){withCopy("{}")}, admitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := controllerWriteSubject("ptahmigrations", nil)
			object := controllerWriteSubject("ptahmigrations", nil)
			for _, shape := range test.old {
				shape(oldObject)
			}
			for _, shape := range test.new {
				shape(object)
			}
			results := evaluatePolicyValidations(t, policy, object, oldObject, managerStateRequest("ptahmigrations", "", test.username))
			if results[0] != test.admitted {
				t.Fatalf("admitted = %t, want %t", results[0], test.admitted)
			}
		})
	}
}

func managerStateRequest(resource, subresource, username string) map[string]any {
	request := controllerWriteRequest(resource, "ptah-controller")
	request["subResource"] = subresource
	request["userInfo"] = map[string]any{"username": username}
	return request
}
