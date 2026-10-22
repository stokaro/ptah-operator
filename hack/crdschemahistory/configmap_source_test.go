package crdschemahistory

import (
	"testing"

	"github.com/google/cel-go/cel"
)

// evaluateRule runs one x-kubernetes-validations rule against self.
func evaluateRule(t *testing.T, rule string, self map[string]any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(rule)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile %q: %v", rule, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := program.Eval(map[string]any{"self": self})
	if err != nil {
		t.Fatalf("evaluate %q on %v: %v", rule, self, err)
	}
	held, ok := value.Value().(bool)
	if !ok {
		t.Fatalf("%q evaluates to %v", rule, value)
	}
	return held
}

// TestConfigMapSourceBreakHoldsForEveryVersion2Object holds the declared
// break to its reason: every rule schema version 3 adds holds for each shape
// a version 2 schema admitted, so no stored object stops validating. Each rule
// also refuses an object it exists to refuse, so the evaluation is not
// vacuous.
func TestConfigMapSourceBreakHoldsForEveryVersion2Object(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "verification", "key": "policy.yaml"}
	sources := []map[string]any{
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy},
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy, "registryAuthFrom": map[string]any{"name": "registry"}},
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy, "transport": map[string]any{}},
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy, "transport": map[string]any{"plainHTTP": true}},
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy, "transport": map[string]any{"caFrom": policy}},
	}
	bindings := []map[string]any{
		{"resolvedReference": "oci://registry.example/schema@sha256:1", "digest": "sha256:1"},
		{"resolvedReference": "oci://registry.example/schema@sha256:1", "digest": "sha256:1", "registryAuthFrom": map[string]any{"name": "registry"}, "transport": map[string]any{"plainHTTP": true}},
	}
	plan := map[string]any{"contractVersion": int64(1), "artifactDigest": "sha256:1", "verificationPolicyUID": "uid", "verificationPolicyDigest": "sha256:2"}

	for _, source := range sources {
		for _, rule := range []string{ruleOneSource, rulePolicyWithOCI, ruleNoRegistryAuth, ruleNoTransport} {
			if !evaluateRule(t, rule, source) {
				t.Errorf("%q refuses the version 2 source %v", rule, source)
			}
		}
		if !evaluateRule(t, ruleSchemaOneFile, map[string]any{"desired": source}) || !evaluateRule(t, ruleSchemaPolicy, map[string]any{"desired": source}) {
			t.Errorf("a schema rule refuses the version 2 source %v", source)
		}
		if !evaluateRule(t, ruleMigrationPolicy, map[string]any{"artifact": source}) {
			t.Errorf("the migration policy rule refuses the version 2 source %v", source)
		}
	}
	for _, binding := range bindings {
		if !evaluateRule(t, ruleOneBinding, binding) {
			t.Errorf("%q refuses the version 2 binding %v", ruleOneBinding, binding)
		}
	}
	if !evaluateRule(t, rulePlanContract, plan) {
		t.Errorf("%q refuses a version 2 plan", rulePlanContract)
	}

	configMap := map[string]any{"name": "orders-schema", "items": []any{map[string]any{"key": "a", "path": "a.sql"}}}
	for rule, refused := range map[string]map[string]any{
		ruleOneSource:      {"ociRef": "oci://registry.example/schema:1", "configMap": configMap, "verificationPolicyFrom": policy},
		rulePolicyWithOCI:  {"configMap": configMap, "verificationPolicyFrom": policy},
		ruleNoRegistryAuth: {"configMap": configMap, "registryAuthFrom": map[string]any{"name": "registry"}},
		ruleNoTransport:    {"configMap": configMap, "transport": map[string]any{"plainHTTP": true}},
		ruleOneBinding:     {"resolvedReference": "oci://x/y@sha256:1", "snapshot": map[string]any{"name": "ptah-src-0"}},
		rulePlanContract:   {"contractVersion": int64(2), "artifactDigest": "sha256:1"},
		ruleSchemaOneFile:  {"desired": map[string]any{"configMap": map[string]any{"items": []any{1, 2}}}},
		ruleSchemaPolicy:   {"desired": map[string]any{"verificationPolicyFrom": map[string]any{"name": "", "key": "k"}}},
	} {
		if evaluateRule(t, rule, refused) {
			t.Errorf("%q admits %v", rule, refused)
		}
	}
	if len(configMapSourceBreak.transitions) != 17 {
		t.Fatalf("the break declares %d transitions, the union makes 17", len(configMapSourceBreak.transitions))
	}
}
