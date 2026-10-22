package crdschemahistory

import "testing"

// TestSourcePolicyBreakHoldsForEveryVersion4Object holds the declared break
// to its reason: both rules hold for each source shape a version 4 schema
// admitted, and each refuses an object it exists to refuse.
func TestSourcePolicyBreakHoldsForEveryVersion4Object(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "verification", "key": "policy.yaml"}
	for _, source := range []map[string]any{
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy},
		{"configMap": map[string]any{"name": "orders-schema", "items": []any{map[string]any{"key": "a", "path": "a.sql"}}}},
		{"git": map[string]any{"url": "https://git.example.com/app.git", "ref": map[string]any{"branch": "main"}, "path": "db/schema.hcl"}},
	} {
		for _, rule := range []string{ruleSourcePolicyNotOCI, ruleSourcePolicyRequiredKey} {
			if !evaluateRule(t, rule, source) {
				t.Errorf("%q refuses the version 4 source %v", rule, source)
			}
		}
	}
	sourcePolicy := map[string]any{"name": "source-policy", "key": "policy.yaml"}
	for _, refused := range []struct {
		rule string
		self map[string]any
	}{
		{ruleSourcePolicyNotOCI, map[string]any{"ociRef": "oci://registry.example/schema:1", "sourcePolicyFrom": sourcePolicy}},
		{ruleSourcePolicyRequiredKey, map[string]any{"sourcePolicyFrom": map[string]any{"name": "source-policy", "key": ""}}},
		{ruleSourcePolicyRequiredKey, map[string]any{"sourcePolicyFrom": map[string]any{"name": "source-policy", "key": "p", "optional": true}}},
	} {
		if evaluateRule(t, refused.rule, refused.self) {
			t.Errorf("%q admits %v", refused.rule, refused.self)
		}
	}
	if len(sourcePolicyBreak.transitions) != 4 {
		t.Fatalf("the break declares %d transitions, two rules on two kinds make 4", len(sourcePolicyBreak.transitions))
	}
}
