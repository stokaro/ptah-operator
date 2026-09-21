package examples_test

import (
	"os"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The guard is an example, and an example of a refusal has to be shown
// refusing. Reading its expression again catches a typo and misses the case it
// was written for: a policy that reads correctly and permits the thing it
// names is the failure this file exists to prevent.
//
// So the expressions are evaluated rather than inspected, against the requests
// an author and an administrator actually make.
const (
	adminGroup  = "<apply-policy-administrator-group>"
	authorGroup = "<desired-state-author-group>"
)

func readApplyPolicyGuard(t *testing.T) (
	*admissionregistrationv1.ValidatingAdmissionPolicy,
	*admissionregistrationv1.ValidatingAdmissionPolicyBinding,
) {
	t.Helper()

	contents, err := os.Open("approval-policy-guard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	decoder := utilyaml.NewYAMLOrJSONDecoder(contents, 4096)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := decoder.Decode(policy); err != nil {
		t.Fatalf("decode the policy: %v", err)
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := decoder.Decode(binding); err != nil {
		t.Fatalf("decode the binding: %v", err)
	}
	return policy, binding
}

// admits reports what the guard does with one request: whether the identity is
// matched at all, and whether every validation passed if it was.
func admits(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	object map[string]any,
	groups []string,
) (matched bool, allowed bool) {
	t.Helper()

	request := map[string]any{
		"userInfo": map[string]any{"username": "someone", "groups": groups},
	}
	for _, condition := range policy.Spec.MatchConditions {
		if !evaluateGuardCEL(t, condition.Expression, object, request) {
			return false, true
		}
	}
	for _, validation := range policy.Spec.Validations {
		if !evaluateGuardCEL(t, validation.Expression, object, request) {
			return true, false
		}
	}
	return true, true
}

func evaluateGuardCEL(t *testing.T, expression string, object, request map[string]any) bool {
	t.Helper()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	compiled, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile %q: %v", expression, issues.Err())
	}
	program, err := environment.Program(compiled)
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := program.Eval(map[string]any{"object": object, "request": request})
	if err != nil {
		t.Fatalf("evaluate %q: %v", expression, err)
	}
	result, ok := value.Value().(bool)
	if !ok {
		t.Fatalf("%q produced %T, want a bool", expression, value.Value())
	}
	return result
}

func schemaWithApplyPolicy(apply string) map[string]any {
	spec := map[string]any{
		"target":  map[string]any{"engine": "PostgreSQL", "coordinationKey": "team-a/orders"},
		"desired": map[string]any{"ociRef": "oci://registry.example/team/schema:stable"},
	}
	if apply != "" {
		spec["policy"] = map[string]any{"apply": apply}
	}
	return map[string]any{"spec": spec}
}

func TestTheApplyPolicyGuardRefusesOnlyTheBypass(t *testing.T) {
	policy, _ := readApplyPolicyGuard(t)

	tests := []struct {
		name        string
		object      map[string]any
		groups      []string
		wantMatched bool
		wantAllowed bool
	}{
		{
			// The bypass. An author with full rights over the resource still
			// cannot make approvals unnecessary.
			name:        "an author selecting Always",
			object:      schemaWithApplyPolicy("Always"),
			groups:      []string{authorGroup},
			wantMatched: true, wantAllowed: false,
		},
		{
			name:        "an author asking for approvals",
			object:      schemaWithApplyPolicy("OnApproval"),
			groups:      []string{authorGroup},
			wantMatched: true, wantAllowed: true,
		},
		{
			// The ordinary edit the separation is supposed to leave alone. An
			// author who never mentions policy is not affected by this guard,
			// and the API defaults the field to OnApproval.
			name:        "an author editing desired state and no policy",
			object:      schemaWithApplyPolicy(""),
			groups:      []string{authorGroup},
			wantMatched: true, wantAllowed: true,
		},
		{
			// The choice is an administrator's rather than nobody's.
			name:        "an apply-policy administrator selecting Always",
			object:      schemaWithApplyPolicy("Always"),
			groups:      []string{adminGroup},
			wantMatched: false, wantAllowed: true,
		},
		{
			// Memberships are not exclusive, and the exemption is a membership
			// rather than a sole identity.
			name:        "an administrator who is also an author",
			object:      schemaWithApplyPolicy("Always"),
			groups:      []string{authorGroup, adminGroup},
			wantMatched: false, wantAllowed: true,
		},
		{
			// A service account is nobody's administrator unless it is put in
			// the group, which is what keeps a controller or a CI identity
			// from being an accidental exemption.
			name:        "a service account selecting Always",
			object:      schemaWithApplyPolicy("Always"),
			groups:      []string{"system:serviceaccounts", "system:authenticated"},
			wantMatched: true, wantAllowed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matched, allowed := admits(t, policy, test.object, test.groups)
			if matched != test.wantMatched {
				t.Fatalf("matched = %t, want %t", matched, test.wantMatched)
			}
			if allowed != test.wantAllowed {
				t.Fatalf("allowed = %t, want %t", allowed, test.wantAllowed)
			}
		})
	}
}

// Both kinds expose the same field, so a guard that covered one of them would
// leave the other as the way around it.
func TestTheApplyPolicyGuardCoversBothFamiliesOnEveryWrite(t *testing.T) {
	policy, binding := readApplyPolicyGuard(t)

	if len(policy.Spec.MatchConstraints.ResourceRules) != 1 {
		t.Fatalf("resource rules = %d, want one", len(policy.Spec.MatchConstraints.ResourceRules))
	}
	rule := policy.Spec.MatchConstraints.ResourceRules[0]
	for _, resource := range []string{"ptahschemas", "ptahmigrations"} {
		if !containsString(rule.Resources, resource) {
			t.Fatalf("the guard does not cover %s, which exposes the same field", resource)
		}
	}
	// An UPDATE-only guard is bypassed by creating the resource with Always,
	// and a CREATE-only one by editing it afterwards.
	for _, operation := range []string{"CREATE", "UPDATE"} {
		if !containsOperation(rule.Operations, operation) {
			t.Fatalf("the guard does not cover %s, so the field can be set another way", operation)
		}
	}
	if policy.Spec.FailurePolicy == nil ||
		*policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("the guard does not fail closed, so an evaluation error permits the bypass")
	}

	// A policy nothing binds is inert, and reads exactly like one in force.
	if binding.Spec.PolicyName != policy.Name {
		t.Fatalf("the binding names %q, and the policy is %q", binding.Spec.PolicyName, policy.Name)
	}
	if len(binding.Spec.ValidationActions) != 1 ||
		binding.Spec.ValidationActions[0] != admissionregistrationv1.Deny {
		t.Fatalf("validation actions = %v, want Deny alone; Warn and Audit record the bypass rather than refusing it",
			binding.Spec.ValidationActions)
	}
}

// The message a person meets has to say what to do about it, because the
// author who meets it is not the one who can change the answer.
func TestTheApplyPolicyGuardSaysWhoCanChangeTheAnswer(t *testing.T) {
	policy, _ := readApplyPolicyGuard(t)

	if len(policy.Spec.Validations) != 1 {
		t.Fatalf("validations = %d, want the one rule", len(policy.Spec.Validations))
	}
	message := policy.Spec.Validations[0].Message
	for _, expected := range []string{"OnApproval", "apply-policy administrator"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("the refusal does not mention %q: %s", expected, message)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsOperation(values []admissionregistrationv1.OperationType, want string) bool {
	for _, value := range values {
		if string(value) == want {
			return true
		}
	}
	return false
}
