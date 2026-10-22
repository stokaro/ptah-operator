package crdschemahistory

import "testing"

// TestGitSourceBreakHoldsForEveryVersion3Object holds the declared break to
// its reason: both rules schema version 4 changes hold for each shape a
// version 3 schema admitted, and each refuses an object it exists to refuse,
// so the evaluation is not vacuous.
func TestGitSourceBreakHoldsForEveryVersion3Object(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "verification", "key": "policy.yaml"}
	configMap := map[string]any{"name": "orders-schema", "items": []any{map[string]any{"key": "a", "path": "a.sql"}}}
	git := map[string]any{"url": "https://git.example.com/app.git", "ref": map[string]any{"branch": "main"}, "path": "db/schema.hcl"}
	sources := []map[string]any{
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy},
		{"configMap": configMap},
	}
	bindings := []map[string]any{
		{"resolvedReference": "oci://registry.example/schema@sha256:1", "digest": "sha256:1"},
		{"snapshot": map[string]any{"name": "ptah-src-0", "uid": "uid", "contentDigest": "sha256:1"}},
	}
	for _, source := range sources {
		if !evaluateRule(t, ruleOneOfThreeSources, source) {
			t.Errorf("%q refuses the version 3 source %v", ruleOneOfThreeSources, source)
		}
	}
	for _, binding := range bindings {
		if !evaluateRule(t, ruleOneOfThreeBindings, binding) {
			t.Errorf("%q refuses the version 3 binding %v", ruleOneOfThreeBindings, binding)
		}
	}
	if !evaluateRule(t, ruleOneOfThreeSources, map[string]any{"git": git}) {
		t.Errorf("%q refuses a Git source", ruleOneOfThreeSources)
	}

	for _, refused := range []struct {
		rule string
		self map[string]any
	}{
		{ruleOneOfThreeSources, map[string]any{}},
		{ruleOneOfThreeSources, map[string]any{"configMap": configMap, "git": git}},
		{ruleOneOfThreeSources, map[string]any{"ociRef": "oci://registry.example/schema:1", "git": git}},
		{ruleOneOfThreeBindings, map[string]any{}},
		{ruleOneOfThreeBindings, map[string]any{"snapshot": map[string]any{"name": "ptah-src-0"}, "git": git}},
	} {
		if evaluateRule(t, refused.rule, refused.self) {
			t.Errorf("%q admits %v", refused.rule, refused.self)
		}
	}
	if len(gitSourceBreak.transitions) != 6 {
		t.Fatalf("the break declares %d transitions, the union makes 6", len(gitSourceBreak.transitions))
	}
}
