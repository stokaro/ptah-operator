package crdschemahistory

import "testing"

// TestHTTPSSourceBreakHoldsForEveryVersion18Object holds the declared break to
// its reason: every rule schema version 19 changes or adds holds for each
// shape a version 18 schema admitted, and each refuses an object it exists to
// refuse, so the evaluation is not vacuous.
func TestHTTPSSourceBreakHoldsForEveryVersion18Object(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "verification", "key": "policy.yaml"}
	configMap := map[string]any{"name": "orders-schema", "items": []any{map[string]any{"key": "a", "path": "a.sql"}}}
	git := map[string]any{"url": "https://git.example.com/app.git", "ref": map[string]any{"branch": "main"}, "path": "db/schema.hcl"}
	https := func(format, path string) map[string]any {
		return map[string]any{
			"url": "https://artifacts.example.com/orders.tar.gz", "digest": "sha256:1",
			"format": format, "path": path,
		}
	}
	sources := []map[string]any{
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy},
		{"configMap": configMap},
		{"git": git},
	}
	bindings := []map[string]any{
		{"resolvedReference": "oci://registry.example/schema@sha256:1", "digest": "sha256:1"},
		{"snapshot": map[string]any{"name": "ptah-src-0", "uid": "uid", "contentDigest": "sha256:1"}},
		{"git": map[string]any{"url": git["url"], "ref": git["ref"], "commit": "c", "path": "db", "contentDigest": "sha256:1"}},
	}
	for _, source := range sources {
		if !evaluateRule(t, ruleOneOfFourSources, source) {
			t.Errorf("%q refuses the version 18 source %v", ruleOneOfFourSources, source)
		}
		if !evaluateRule(t, ruleSchemaHTTPSFile, map[string]any{"desired": source}) {
			t.Errorf("%q refuses the schema spec desiring %v", ruleSchemaHTTPSFile, source)
		}
		if !evaluateRule(t, ruleMigrationHTTPSArchive, map[string]any{"artifact": source}) {
			t.Errorf("%q refuses the migration spec reading %v", ruleMigrationHTTPSArchive, source)
		}
	}
	for _, binding := range bindings {
		if !evaluateRule(t, ruleOneOfFourBindings, binding) {
			t.Errorf("%q refuses the version 18 binding %v", ruleOneOfFourBindings, binding)
		}
	}
	for rule, self := range map[string]map[string]any{
		ruleOneOfFourSources:      {"https": https("TarGzip", "db/migrations")},
		ruleOneOfFourBindings:     {"https": https("Zip", "db")},
		ruleSchemaHTTPSFile:       {"desired": map[string]any{"https": https("File", "schema.hcl")}},
		ruleMigrationHTTPSArchive: {"artifact": map[string]any{"https": https("Zip", ".")}},
	} {
		if !evaluateRule(t, rule, self) {
			t.Errorf("%q refuses the HTTPS source %v", rule, self)
		}
	}

	for _, refused := range []struct {
		rule string
		self map[string]any
	}{
		{ruleOneOfFourSources, map[string]any{}},
		{ruleOneOfFourSources, map[string]any{"git": git, "https": https("TarGzip", "db")}},
		{ruleOneOfFourSources, map[string]any{"ociRef": "oci://registry.example/schema:1", "https": https("Zip", "db")}},
		{ruleOneOfFourBindings, map[string]any{}},
		{ruleOneOfFourBindings, map[string]any{"snapshot": map[string]any{"name": "ptah-src-0"}, "https": https("Zip", "db")}},
		{ruleSchemaHTTPSFile, map[string]any{"desired": map[string]any{"https": https("TarGzip", ".")}}},
		{ruleMigrationHTTPSArchive, map[string]any{"artifact": map[string]any{"https": https("File", "migrations")}}},
	} {
		if evaluateRule(t, refused.rule, refused.self) {
			t.Errorf("%q admits %v", refused.rule, refused.self)
		}
	}
	if len(httpsSourceBreak.transitions) != 8 {
		t.Fatalf("the break declares %d transitions, the union and the two spec rules make 8", len(httpsSourceBreak.transitions))
	}
}
