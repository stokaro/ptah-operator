package crdschemahistory

import "testing"

// TestS3SourceBreakHoldsForEveryVersion26Object holds the declared break to
// its reason: every rule schema version 27 changes or adds holds for each
// shape a version 26 schema admitted, and each refuses an object it exists to
// refuse, so the evaluation is not vacuous.
func TestS3SourceBreakHoldsForEveryVersion26Object(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "verification", "key": "policy.yaml"}
	configMap := map[string]any{"name": "orders-schema", "items": []any{map[string]any{"key": "a", "path": "a.sql"}}}
	git := map[string]any{"url": "https://git.example.com/app.git", "ref": map[string]any{"branch": "main"}, "path": "db/schema.hcl"}
	https := map[string]any{
		"url": "https://artifacts.example.com/orders.tar.gz", "digest": "sha256:1", "format": "TarGzip", "path": "db",
	}
	s3 := func(format, path string) map[string]any {
		return map[string]any{
			"endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "orders", "key": "orders.tar.gz",
			"digest": "sha256:1", "format": format, "path": path,
		}
	}
	sources := []map[string]any{
		{"ociRef": "oci://registry.example/schema:1", "verificationPolicyFrom": policy},
		{"configMap": configMap},
		{"git": git},
		{"https": https},
	}
	bindings := []map[string]any{
		{"resolvedReference": "oci://registry.example/schema@sha256:1", "digest": "sha256:1"},
		{"snapshot": map[string]any{"name": "ptah-src-0", "uid": "uid", "contentDigest": "sha256:1"}},
		{"git": map[string]any{"url": git["url"], "ref": git["ref"], "commit": "c", "path": "db", "contentDigest": "sha256:1"}},
		{"https": map[string]any{"url": https["url"], "digest": "sha256:1", "format": "TarGzip", "path": "db", "contentDigest": "sha256:1"}},
	}
	for _, source := range sources {
		if !evaluateRule(t, ruleOneOfFiveSources, source) {
			t.Errorf("%q refuses the version 26 source %v", ruleOneOfFiveSources, source)
		}
		if !evaluateRule(t, ruleSchemaS3File, map[string]any{"desired": source}) {
			t.Errorf("%q refuses the schema spec desiring %v", ruleSchemaS3File, source)
		}
		if !evaluateRule(t, ruleMigrationS3Archive, map[string]any{"artifact": source}) {
			t.Errorf("%q refuses the migration spec reading %v", ruleMigrationS3Archive, source)
		}
	}
	for _, binding := range bindings {
		if !evaluateRule(t, ruleOneOfFiveBindings, binding) {
			t.Errorf("%q refuses the version 26 binding %v", ruleOneOfFiveBindings, binding)
		}
	}
	for rule, self := range map[string]map[string]any{
		ruleOneOfFiveSources:   {"s3": s3("TarGzip", "db/migrations")},
		ruleOneOfFiveBindings:  {"s3": s3("Zip", "db")},
		ruleSchemaS3File:       {"desired": map[string]any{"s3": s3("File", "schema.hcl")}},
		ruleMigrationS3Archive: {"artifact": map[string]any{"s3": s3("Zip", ".")}},
	} {
		if !evaluateRule(t, rule, self) {
			t.Errorf("%q refuses the S3 source %v", rule, self)
		}
	}

	for _, refused := range []struct {
		rule string
		self map[string]any
	}{
		{ruleOneOfFiveSources, map[string]any{}},
		{ruleOneOfFiveSources, map[string]any{"https": https, "s3": s3("TarGzip", "db")}},
		{ruleOneOfFiveSources, map[string]any{"ociRef": "oci://registry.example/schema:1", "s3": s3("Zip", "db")}},
		{ruleOneOfFiveBindings, map[string]any{}},
		{ruleOneOfFiveBindings, map[string]any{"snapshot": map[string]any{"name": "ptah-src-0"}, "s3": s3("Zip", "db")}},
		{ruleSchemaS3File, map[string]any{"desired": map[string]any{"s3": s3("TarGzip", ".")}}},
		{ruleMigrationS3Archive, map[string]any{"artifact": map[string]any{"s3": s3("File", "migrations")}}},
	} {
		if evaluateRule(t, refused.rule, refused.self) {
			t.Errorf("%q admits %v", refused.rule, refused.self)
		}
	}
	if len(s3SourceBreak.transitions) != 8 {
		t.Fatalf("the break declares %d transitions, the union and the two spec rules make 8", len(s3SourceBreak.transitions))
	}
}
