package crdupgrade

import (
	"reflect"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
)

func testOrphanDeletionGuard() *OrphanDeletionGuard {
	return &OrphanDeletionGuard{ReleaseName: "ptah", ReleaseNamespace: "ptah-system"}
}

// The guard fails closed, denies, is bound to exactly what it matches, and
// exempts nobody.
func TestOrphanDeletionGuardIsFailClosedAndBoundAsMatched(t *testing.T) {
	t.Parallel()

	guard := testOrphanDeletionGuard()
	policy, binding := guard.Policy(), guard.Binding()
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("%s does not fail closed", policy.Name)
	}
	if binding.Spec.PolicyName != policy.Name ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) ||
		!reflect.DeepEqual(binding.Spec.MatchResources, policy.Spec.MatchConstraints) {
		t.Fatalf("%s binding is not exact deny-only enforcement of what the policy matches: %#v", policy.Name, binding.Spec)
	}
	if len(policy.Spec.MatchConditions) != 0 {
		t.Fatalf("%s exempts through a match condition: %#v", policy.Name, policy.Spec.MatchConditions)
	}
	if want := OrphanDeletionGuardPolicyName("ptah-system", "ptah"); policy.Name != want {
		t.Fatalf("policy name = %q, want %q", policy.Name, want)
	}
}

// The guard selects a deletion of each kind that owns result records, in every
// version the operator serves, and no other operation and no other kind.
func TestOrphanDeletionGuardSelectsTheDeletionOfTheResultOwners(t *testing.T) {
	t.Parallel()

	rules := testOrphanDeletionGuard().Policy().Spec.MatchConstraints.ResourceRules
	candidates, err := Candidates()
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]bool{schemaResource: true, migrationResource: true}
	selected := 0
	for _, crd := range candidates {
		for _, version := range crd.Spec.Versions {
			resource := schema.GroupVersionResource{Group: crd.Spec.Group, Version: version.Name, Resource: crd.Spec.Names.Plural}
			kind := schema.GroupVersionKind{Group: crd.Spec.Group, Version: version.Name, Kind: crd.Spec.Names.Kind}
			for _, operation := range []admission.Operation{admission.Create, admission.Update, admission.Delete, admission.Connect} {
				attributes := admission.NewAttributesRecord(nil, nil, kind, "team-a", "orders", resource, "",
					operation, nil, false, &user.DefaultInfo{Name: "someone"})
				want := operation == admission.Delete && owners[resource.Resource]
				if got := controllerWriteRulesMatch(rules, attributes); got != want {
					t.Errorf("the guard selects %s of %s = %t, want %t", operation, resource.Resource, got, want)
				}
				if want {
					selected++
				}
			}
			status := admission.NewAttributesRecord(nil, nil, kind, "team-a", "orders", resource, "status",
				admission.Delete, nil, false, &user.DefaultInfo{Name: "someone"})
			if controllerWriteRulesMatch(rules, status) {
				t.Errorf("the guard selects a DELETE of %s/status", resource.Resource)
			}
		}
	}
	if selected != 2 {
		t.Fatalf("the guard was held to %d deletions it selects, want one for each of the two kinds", selected)
	}
}

// The verdict follows the registry's own decision whether to orphan: the
// legacy field, then the propagation policy, then the finalizer the resource
// carries. Only an orphaning deletion is refused, and the validation that
// refuses it says why.
func TestOrphanDeletionGuardRefusesExactlyTheOrphaningDeletions(t *testing.T) {
	t.Parallel()

	const (
		admitted = iota
		refusedForOptions
		refusedForFinalizer
	)
	policy := testOrphanDeletionGuard().Policy()
	if len(policy.Spec.Validations) != 2 ||
		policy.Spec.Validations[0].Message != orphanDeletionDenialMessage() ||
		policy.Spec.Validations[1].Message != orphanFinalizerDenialMessage() {
		t.Fatalf("validations = %#v, want the options rule and then the finalizer rule", policy.Spec.Validations)
	}
	for _, test := range []struct {
		name       string
		options    map[string]any
		finalizers []string
		want       int
	}{
		{name: "propagationPolicy Orphan", options: map[string]any{"propagationPolicy": "Orphan"}, want: refusedForOptions},
		{name: "orphanDependents true", options: map[string]any{"orphanDependents": true}, want: refusedForOptions},
		{
			name: "propagationPolicy Orphan on a resource that carries other finalizers", options: map[string]any{"propagationPolicy": "Orphan"},
			finalizers: []string{migrationOperationFinalizer}, want: refusedForOptions,
		},
		{name: "propagationPolicy Background", options: map[string]any{"propagationPolicy": "Background"}, want: admitted},
		{name: "propagationPolicy Foreground", options: map[string]any{"propagationPolicy": "Foreground"}, want: admitted},
		{name: "orphanDependents false", options: map[string]any{"orphanDependents": false}, want: admitted},
		{name: "no options", options: map[string]any{}, want: admitted},
		{name: "only a dry run", options: map[string]any{"dryRun": []any{"All"}}, want: admitted},
		{
			name: "no options on a resource that carries the operator's finalizer", options: map[string]any{},
			finalizers: []string{migrationOperationFinalizer}, want: admitted,
		},
		{
			name: "no options on a resource that carries the foreground finalizer", options: map[string]any{},
			finalizers: []string{metav1.FinalizerDeleteDependents}, want: admitted,
		},
		{
			name: "no options on a resource that carries the orphan finalizer", options: map[string]any{},
			finalizers: []string{migrationOperationFinalizer, metav1.FinalizerOrphanDependents}, want: refusedForFinalizer,
		},
		{
			name: "propagationPolicy Background on a resource that carries the orphan finalizer", options: map[string]any{"propagationPolicy": "Background"},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: admitted,
		},
		{
			name: "propagationPolicy Foreground on a resource that carries the orphan finalizer", options: map[string]any{"propagationPolicy": "Foreground"},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: admitted,
		},
		{
			name: "orphanDependents false on a resource that carries the orphan finalizer", options: map[string]any{"orphanDependents": false},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: admitted,
		},
		{
			name: "propagationPolicy Orphan on a resource that carries the orphan finalizer", options: map[string]any{"propagationPolicy": "Orphan"},
			finalizers: []string{metav1.FinalizerOrphanDependents}, want: refusedForOptions,
		},
	} {
		for _, resource := range []string{schemaResource, migrationResource} {
			t.Run(resource+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				request := map[string]any{
					"operation": "DELETE",
					"namespace": controllerWriteProofNamespace,
					"name":      controllerWriteProofName,
					"resource":  map[string]any{"group": operatorAPIGroup, "version": "v1alpha1", "resource": resource},
					"userInfo":  map[string]any{"username": "dba@example.test"},
					"options":   test.options,
				}
				results := evaluatePolicyValidations(t, policy, nil, controllerWriteSubject(resource, test.finalizers), request)
				if !results[0] && !results[1] {
					t.Fatalf("both validations refuse, so the refusal would give two reasons for one request: %v", results)
				}
				got := admitted
				switch {
				case !results[0]:
					got = refusedForOptions
				case !results[1]:
					got = refusedForFinalizer
				}
				if got != test.want {
					t.Fatalf("verdict = %d, want %d (0 admitted, 1 refused for the options, 2 refused for the finalizer): %v", got, test.want, results)
				}
			})
		}
	}
}
