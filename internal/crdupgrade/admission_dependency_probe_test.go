package crdupgrade

// These helpers intentionally inspect private policy structure: the dependency
// probe is a security protocol layered around several otherwise independent
// admission contracts, and tests must assert both the wrapper and native CEL.

import (
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

func stripAdmissionConvergenceDependencyProbe(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	if policy == nil || policy.Spec.MatchConstraints == nil {
		t.Fatal("dependency policy or match constraints are nil")
	}
	if len(policy.Spec.MatchConstraints.ResourceRules) < 2 {
		t.Fatalf("dependency policy resource rules = %d, want native rules plus marker rule", len(policy.Spec.MatchConstraints.ResourceRules))
	}
	if len(policy.Spec.Variables) < 2 || policy.Spec.Variables[0].Name != "isAnyAdmissionConvergenceProbe" || policy.Spec.Variables[1].Name != "isAdmissionConvergenceProbe" {
		t.Fatalf("dependency policy variables do not start with the convergence probe union and exact selector: %#v", policy.Spec.Variables)
	}
	anyProbeExpression := policy.Spec.Variables[0].Expression
	probeExpression := policy.Spec.Variables[1].Expression
	if want := admissionConvergenceAnyProbeRequestExpressionFromExact(t, probeExpression); anyProbeExpression != want {
		t.Fatalf("dependency probe union = %q, want %q", anyProbeExpression, want)
	}
	if !strings.Contains(probeExpression, `request.options.fieldManager == "`+admissionConvergenceProbeFieldManagerPrefix) {
		t.Fatalf("dependency probe does not pin an exact content-versioned field manager: %q", probeExpression)
	}
	markerRule := policy.Spec.MatchConstraints.ResourceRules[len(policy.Spec.MatchConstraints.ResourceRules)-1]
	wantMarkerRule := admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
				Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
			},
		},
	}
	if !reflect.DeepEqual(markerRule, wantMarkerRule) {
		t.Fatalf("dependency marker rule = %#v, want %#v", markerRule, wantMarkerRule)
	}
	if len(policy.Spec.Validations) < 2 {
		t.Fatal("dependency policy lacks probe validations")
	}
	probeValidations := policy.Spec.Validations[len(policy.Spec.Validations)-2:]
	if got := probeValidations[0]; got.Expression != `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true` || got.Message != admissionConvergenceProbePersistenceMessage {
		t.Fatalf("dependency dry-run validation = %#v", got)
	}
	if got := probeValidations[1]; got.Expression != `!variables.isAdmissionConvergenceProbe` || !strings.HasPrefix(got.Message, "Ptah admission convergence confirmed exact workload guard "+admissionConvergenceProbeFieldManagerPrefix) {
		t.Fatalf("dependency proof validation = %#v", got)
	}

	native := policy.DeepCopy()
	native.Spec.MatchConstraints.ResourceRules = native.Spec.MatchConstraints.ResourceRules[:len(native.Spec.MatchConstraints.ResourceRules)-1]
	native.Spec.Variables = native.Spec.Variables[2:]
	native.Spec.Validations = native.Spec.Validations[:len(native.Spec.Validations)-2]
	matchPrefix := "(" + anyProbeExpression + ") || ("
	for index := range native.Spec.MatchConditions {
		expression := native.Spec.MatchConditions[index].Expression
		if !strings.HasPrefix(expression, matchPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("dependency match condition %d is not exactly probe-wrapped: %q", index, expression)
		}
		native.Spec.MatchConditions[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, matchPrefix), ")")
	}
	validationPrefix := "variables.isAnyAdmissionConvergenceProbe || ("
	for index := range native.Spec.Validations {
		expression := native.Spec.Validations[index].Expression
		if !strings.HasPrefix(expression, validationPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("dependency validation %d is not exactly probe-gated: %q", index, expression)
		}
		native.Spec.Validations[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, validationPrefix), ")")
	}
	return native
}

func admissionConvergenceAnyProbeRequestExpressionFromExact(t *testing.T, exact string) string {
	t.Helper()
	marker := ` && request.options.fieldManager == "`
	index := strings.LastIndex(exact, marker)
	if index < 0 || !strings.HasSuffix(exact, `"`) {
		t.Fatalf("exact dependency probe expression has no field-manager suffix: %q", exact)
	}
	return exact[:index] + ` && request.options.fieldManager.matches("^` + admissionConvergenceProbeFieldManagerPrefix + `[0-9a-f]{64}$")`
}

func TestAdmissionConvergenceDependencyProbeUsesFullAttemptIdentity(t *testing.T) {
	t.Parallel()

	policyName := "example-policy"
	left := strings.Repeat("a", 12) + strings.Repeat("b", 52)
	right := strings.Repeat("a", 12) + strings.Repeat("c", 52)
	leftProbe := newAdmissionConvergenceDependencyProbe(policyName, left)
	rightProbe := newAdmissionConvergenceDependencyProbe(policyName, right)
	if leftProbe.FieldManager == rightProbe.FieldManager || leftProbe.Message == rightProbe.Message {
		t.Fatal("dependency proof identity ignores the full release-attempt digest")
	}
	for _, probe := range []admissionConvergenceDependencyProbe{leftProbe, rightProbe} {
		digest := strings.TrimPrefix(probe.FieldManager, admissionConvergenceProbeFieldManagerPrefix)
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			t.Fatalf("probe field manager %q does not carry a full lowercase SHA-256 digest", probe.FieldManager)
		}
	}
}

func TestRemoveAdmissionConvergenceDependencyProbeRequiresExactWrapper(t *testing.T) {
	t.Parallel()

	wrapped := runtimePodGuardFixture().hookIdentityPolicy()
	want := stripAdmissionConvergenceDependencyProbe(t, wrapped)
	got := wrapped.DeepCopy()
	if err := removeAdmissionConvergenceDependencyProbe(got); err != nil {
		t.Fatalf("remove exact dependency probe: %v", err)
	}
	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatal("removing the exact dependency probe did not restore the native policy")
	}

	tests := []struct {
		name   string
		mutate func(*admissionregistrationv1.ValidatingAdmissionPolicy)
	}{
		{
			name: "foreign union variable",
			mutate: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Spec.Variables[0].Name = "foreign"
			},
		},
		{
			name: "foreign marker rule",
			mutate: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Spec.MatchConstraints.ResourceRules[len(policy.Spec.MatchConstraints.ResourceRules)-1].Operations = []admissionregistrationv1.OperationType{admissionregistrationv1.Create}
			},
		},
		{
			name: "foreign proof validation",
			mutate: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Spec.Validations[len(policy.Spec.Validations)-1].Message = "foreign"
			},
		},
		{
			name: "foreign native validation wrapper",
			mutate: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Spec.Validations[0].Expression = "true"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policy := wrapped.DeepCopy()
			test.mutate(policy)
			if err := removeAdmissionConvergenceDependencyProbe(policy); err == nil {
				t.Fatal("removeAdmissionConvergenceDependencyProbe() accepted a foreign wrapper")
			}
		})
	}
}
