package crd_test

import (
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// schemaBase is the smallest PtahSchema the reference page offers, which every
// refusal row below departs from by one change.
func schemaBase(namespace string) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		return resource("PtahSchema", namespace, "application", map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/application-primary",
				"urlFrom":         map[string]any{"name": "application-database", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://ghcr.io/example/application-schema:1.4.0",
				"verificationPolicyFrom": map[string]any{"name": "ptah-verification-policy", "key": "policy.yaml"},
			},
		})
	}
}

func migrationBase(namespace string) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		return resource("PtahMigration", namespace, "orders", map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/application-primary",
				"urlFrom":         map[string]any{"name": "application-database", "key": "url"},
			},
			"artifact": map[string]any{
				"ociRef":                 "oci://ghcr.io/example/orders-migrations:1.4.0",
				"verificationPolicyFrom": map[string]any{"name": "ptah-migration-verification-policy", "key": "policy.yaml"},
			},
		})
	}
}

// setting returns a mutation that writes value at path.
func setting(value any, path ...string) func(*testing.T, *unstructured.Unstructured) {
	return func(t *testing.T, object *unstructured.Unstructured) {
		t.Helper()
		set(t, object, value, path...)
	}
}

// removing returns a mutation that deletes the field at path.
func removing(path ...string) func(*testing.T, *unstructured.Unstructured) {
	return func(_ *testing.T, object *unstructured.Unstructured) {
		remove(object, path...)
	}
}

// both applies two mutations in order.
func both(first, second func(*testing.T, *unstructured.Unstructured)) func(*testing.T, *unstructured.Unstructured) {
	return func(t *testing.T, object *unstructured.Unstructured) {
		t.Helper()
		first(t, object)
		second(t, object)
	}
}

// longName is one byte past what a Job label can carry.
var longName = strings.Repeat("a", 64)

// The name rule sits on the root of the schema, and the API server reports a
// root-level rule against a nil field path, which it prints as "<nil>".
var nameLengthCause = cause{"<nil>", "metadata.name must be at most 63 bytes"}

// targetRefusals hold for both families: the target, its coordination and the
// timeouts are one shape in PtahSchema and PtahMigration.
func targetRefusals() []refusal {
	return []refusal{
		{
			name:   "target is required",
			mutate: removing("spec", "target"),
			want:   []cause{{"spec.target", "Required value"}},
		},
		{
			name:   "target.engine is required",
			mutate: removing("spec", "target", "engine"),
			want:   []cause{{"spec.target.engine", "Required value"}},
		},
		{
			name:   "target.urlFrom is required",
			mutate: removing("spec", "target", "urlFrom"),
			want:   []cause{{"spec.target.urlFrom", "Required value"}},
		},
		{
			name:   "target.urlFrom.key is required",
			mutate: removing("spec", "target", "urlFrom", "key"),
			want:   []cause{{"spec.target.urlFrom.key", "Required value"}},
		},
		{
			name:   "target.engine outside its pattern",
			mutate: setting("9postgres", "spec", "target", "engine"),
			want:   []cause{{"spec.target.engine", "should match"}},
		},
		{
			name:   "target.engine past 63 bytes",
			mutate: setting("P"+strings.Repeat("g", 63), "spec", "target", "engine"),
			want:   []cause{{"spec.target.engine", "Too long"}},
		},
		{
			name:   "coordinationKey outside its pattern",
			mutate: setting("Production/Primary", "spec", "target", "coordinationKey"),
			want:   []cause{{"spec.target.coordinationKey", "should match"}},
		},
		{
			name:   "coordinationKey past 253 bytes",
			mutate: setting(strings.Repeat("k", 254), "spec", "target", "coordinationKey"),
			want:   []cause{{"spec.target.coordinationKey", "Too long"}},
		},
		{
			name:   "realmRef.name outside its pattern",
			mutate: both(removing("spec", "target", "coordinationKey"), setting(map[string]any{"name": "Primary_Realm"}, "spec", "target", "realmRef")),
			want:   []cause{{"spec.target.realmRef.name", "should match"}},
		},
		{
			name:   "both coordinationKey and realmRef",
			mutate: setting(map[string]any{"name": "production-application-primary"}, "spec", "target", "realmRef"),
			want:   []cause{{"spec.target", "set exactly one of coordinationKey and realmRef"}},
		},
		{
			name:   "neither coordinationKey nor realmRef",
			mutate: removing("spec", "target", "coordinationKey"),
			want:   []cause{{"spec.target", "set exactly one of coordinationKey and realmRef"}},
		},
		{
			name:   "target.urlFrom optional",
			mutate: setting(true, "spec", "target", "urlFrom", "optional"),
			want:   []cause{{"spec", "target.urlFrom must name a required Secret key"}},
		},
		{
			name:   "target.urlFrom with an empty name",
			mutate: setting("", "spec", "target", "urlFrom", "name"),
			want:   []cause{{"spec", "target.urlFrom must name a required Secret key"}},
		},
		{
			name:   "target.urlFrom with an empty key",
			mutate: setting("", "spec", "target", "urlFrom", "key"),
			want:   []cause{{"spec", "target.urlFrom must name a required Secret key"}},
		},
		{
			name:   "interval under 10s",
			mutate: setting("9s", "spec", "interval"),
			want:   []cause{{"spec.interval", "interval must be between 10s and 24h"}},
		},
		{
			name:   "interval over 24h",
			mutate: setting("24h1s", "spec", "interval"),
			want:   []cause{{"spec.interval", "interval must be between 10s and 24h"}},
		},
		{
			name:   "execution.connectTimeout under 1s",
			mutate: setting("999ms", "spec", "execution", "connectTimeout"),
			want:   []cause{{"spec.execution.connectTimeout", "connectTimeout must be between 1s and 10m"}},
		},
		{
			name:   "execution.connectTimeout over 10m",
			mutate: setting("10m1s", "spec", "execution", "connectTimeout"),
			want:   []cause{{"spec.execution.connectTimeout", "connectTimeout must be between 1s and 10m"}},
		},
		{
			name:   "execution.failureRetryInterval under 5s",
			mutate: setting("4s", "spec", "execution", "failureRetryInterval"),
			want:   []cause{{"spec.execution.failureRetryInterval", "failureRetryInterval must be between 5s and 1h"}},
		},
		{
			name:   "execution.failureRetryInterval over 1h",
			mutate: setting("1h1s", "spec", "execution", "failureRetryInterval"),
			want:   []cause{{"spec.execution.failureRetryInterval", "failureRetryInterval must be between 5s and 1h"}},
		},
		{
			name:   "execution.activeDeadlineSeconds under 30",
			mutate: setting(int64(29), "spec", "execution", "activeDeadlineSeconds"),
			want:   []cause{{"spec.execution.activeDeadlineSeconds", "greater than or equal to 30"}},
		},
		{
			name:   "execution.activeDeadlineSeconds over a day",
			mutate: setting(int64(86401), "spec", "execution", "activeDeadlineSeconds"),
			want:   []cause{{"spec.execution.activeDeadlineSeconds", "less than or equal to 86400"}},
		},
		{
			name: "execution.connectTimeout past the deadline",
			mutate: both(
				setting(int64(30), "spec", "execution", "activeDeadlineSeconds"),
				setting("31s", "spec", "execution", "connectTimeout")),
			want: []cause{{"spec", "execution.connectTimeout must not exceed execution.activeDeadlineSeconds"}},
		},
		{
			name: "policy.lockTimeout past the deadline",
			mutate: both(
				setting(int64(30), "spec", "execution", "activeDeadlineSeconds"),
				setting("31s", "spec", "policy", "lockTimeout")),
			want: []cause{{"spec", "policy.lockTimeout must not exceed execution.activeDeadlineSeconds"}},
		},
		{
			name:   "execution.resources.claims with a repeated name",
			mutate: setting([]any{map[string]any{"name": "gpu"}, map[string]any{"name": "gpu"}}, "spec", "execution", "resources", "claims"),
			want:   []cause{{"spec.execution.resources.claims[1]", "Duplicate value"}},
		},
		{
			name:   "execution.podMetadata.labels past 16 entries",
			mutate: setting(numberedMap("declared.example/label-", 17), "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "Too many"}},
		},
		{
			name:   "execution.podMetadata.labels with a key that is not a label key",
			mutate: setting(map[string]any{"-mesh": "true"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "must be valid Kubernetes label keys"}},
		},
		{
			name:   "execution.podMetadata.labels with a name past 63 bytes",
			mutate: setting(map[string]any{"acme.example/" + strings.Repeat("k", 64): "true"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "must be valid Kubernetes label keys"}},
		},
		{
			name:   "execution.podMetadata.labels under the operator's prefix",
			mutate: setting(map[string]any{"operator.ptah.run/schema": "other"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "reserved"}},
		},
		{
			name:   "execution.podMetadata.labels under app.kubernetes.io",
			mutate: setting(map[string]any{"app.kubernetes.io/name": "ptah-operator"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "reserved"}},
		},
		{
			name:   "execution.podMetadata.labels naming job-name",
			mutate: setting(map[string]any{"job-name": "other"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels", "reserved"}},
		},
		// The API server names a map value by its key after a dot, whatever
		// the key holds, so the field below reads as one more path segment.
		{
			name:   "execution.podMetadata.labels with a value outside its pattern",
			mutate: setting(map[string]any{"acme.example/team": "plat form"}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels.acme.example/team", "should match"}},
		},
		{
			name:   "execution.podMetadata.labels with a value past 63 bytes",
			mutate: setting(map[string]any{"acme.example/team": strings.Repeat("v", 64)}, "spec", "execution", "podMetadata", "labels"),
			want:   []cause{{"spec.execution.podMetadata.labels.acme.example/team", "Too long"}},
		},
		{
			name:   "execution.podMetadata.annotations past 16 entries",
			mutate: setting(numberedMap("declared.example/note-", 17), "spec", "execution", "podMetadata", "annotations"),
			want:   []cause{{"spec.execution.podMetadata.annotations", "Too many"}},
		},
		{
			name:   "execution.podMetadata.annotations with a key that is not an annotation key",
			mutate: setting(map[string]any{"a/b/c": "true"}, "spec", "execution", "podMetadata", "annotations"),
			want:   []cause{{"spec.execution.podMetadata.annotations", "must be valid Kubernetes annotation keys"}},
		},
		{
			name:   "execution.podMetadata.annotations naming the LimitRanger annotation",
			mutate: setting(map[string]any{"kubernetes.io/limit-ranger": "x"}, "spec", "execution", "podMetadata", "annotations"),
			want:   []cause{{"spec.execution.podMetadata.annotations", "reserved"}},
		},
		{
			name:   "execution.podMetadata.annotations under a ptah.run subdomain",
			mutate: setting(map[string]any{"mesh.ptah.run/inject": "false"}, "spec", "execution", "podMetadata", "annotations"),
			want:   []cause{{"spec.execution.podMetadata.annotations", "reserved"}},
		},
		{
			name:   "execution.podMetadata.annotations with a value past 1024 bytes",
			mutate: setting(map[string]any{"acme.example/note": strings.Repeat("n", 1025)}, "spec", "execution", "podMetadata", "annotations"),
			want:   []cause{{"spec.execution.podMetadata.annotations.acme.example/note", "Too long"}},
		},
		{
			name:   "policy.apply outside its enum",
			mutate: setting("Sometimes", "spec", "policy", "apply"),
			want:   []cause{{"spec.policy.apply", "Unsupported value"}},
		},
		{
			name:   "a 64-byte name on create",
			mutate: func(_ *testing.T, object *unstructured.Unstructured) { object.SetName(longName) },
			want:   []cause{nameLengthCause},
		},
	}
}

// artifactRefusals are the source rules, which read the same under
// spec.desired in a PtahSchema and spec.artifact in a PtahMigration.
func artifactRefusals(source string) []refusal {
	field := "spec." + source
	return []refusal{
		{
			name:   source + " is required",
			mutate: removing("spec", source),
			want:   []cause{{field, "Required value"}},
		},
		{
			name:   source + ".ociRef is required",
			mutate: removing("spec", source, "ociRef"),
			want:   []cause{{field + ".ociRef", "Required value"}},
		},
		{
			name:   source + ".ociRef without the oci scheme",
			mutate: setting("ghcr.io/example/schema:1.4.0", "spec", source, "ociRef"),
			want:   []cause{{field + ".ociRef", "should match"}},
		},
		{
			name:   source + ".verificationPolicyFrom is required",
			mutate: removing("spec", source, "verificationPolicyFrom"),
			want:   []cause{{field + ".verificationPolicyFrom", "Required value"}},
		},
		{
			name:   source + ".verificationPolicyFrom optional",
			mutate: setting(true, "spec", source, "verificationPolicyFrom", "optional"),
			want:   []cause{{"spec", source + ".verificationPolicyFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".verificationPolicyFrom with an empty name",
			mutate: setting("", "spec", source, "verificationPolicyFrom", "name"),
			want:   []cause{{"spec", source + ".verificationPolicyFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".transport.caFrom optional",
			mutate: setting(map[string]any{"caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt", "optional": true}}, "spec", source, "transport"),
			want:   []cause{{"spec", source + ".transport.caFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".transport.caFrom over plain HTTP",
			mutate: setting(map[string]any{"plainHTTP": true, "caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt"}}, "spec", source, "transport"),
			want:   []cause{{field + ".transport", "caFrom cannot be used with plainHTTP"}},
		},
		{
			name:   source + ".registryAuthFrom.mode outside its enum",
			mutate: setting(map[string]any{"name": "registry-credentials", "mode": "Basic"}, "spec", source, "registryAuthFrom"),
			want:   []cause{{field + ".registryAuthFrom.mode", "Unsupported value"}},
		},
		{
			name:   source + ".registryAuthFrom.name is required",
			mutate: setting(map[string]any{"mode": "Environment"}, "spec", source, "registryAuthFrom"),
			want:   []cause{{field + ".registryAuthFrom.name", "Required value"}},
		},
	}
}

func TestPtahSchemaRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-refusals")
	rows := append(targetRefusals(), artifactRefusals("desired")...)
	rows = append(rows,
		refusal{
			name:   "policy.driftSeverity outside its enum",
			mutate: setting("warning", "spec", "policy", "driftSeverity"),
			want:   []cause{{"spec.policy.driftSeverity", "Unsupported value"}},
		},
		refusal{
			name:   "policy.transactionMode outside its enum",
			mutate: setting("statement", "spec", "policy", "transactionMode"),
			want:   []cause{{"spec.policy.transactionMode", "Unsupported value"}},
		},
		refusal{
			name:   "policy.lockTimeout over 10m",
			mutate: setting("10m1s", "spec", "policy", "lockTimeout"),
			want:   []cause{{"spec.policy.lockTimeout", "lockTimeout must be between 1s and 10m"}},
		},
		refusal{
			name:   "policy.exclude with a repeated table",
			mutate: setting([]any{"schema_migrations", "schema_migrations"}, "spec", "policy", "exclude"),
			want:   []cause{{"spec.policy.exclude[1]", "Duplicate value"}},
		},
		refusal{
			name:   "policy.exclude with a blank entry",
			mutate: setting([]any{" padded"}, "spec", "policy", "exclude"),
			want:   []cause{{"spec.policy.exclude[0]", "should match"}},
		},
		refusal{
			name:   "policy.exclude past 128 entries",
			mutate: setting(numbered("table_", 129), "spec", "policy", "exclude"),
			want:   []cause{{"spec.policy.exclude", "Too many"}},
		},
		refusal{
			name:   "policy.protectedTables with a repeated table",
			mutate: setting([]any{"billing_ledger", "billing_ledger"}, "spec", "policy", "protectedTables"),
			want:   []cause{{"spec.policy.protectedTables[1]", "Duplicate value"}},
		},
		refusal{
			name:   "policy.protectedTables outside its pattern",
			mutate: setting([]any{"1ledger"}, "spec", "policy", "protectedTables"),
			want:   []cause{{"spec.policy.protectedTables[0]", "should match"}},
		},
		refusal{
			name:   "policy.protectedTables past 128 entries",
			mutate: setting(numbered("ledger_", 129), "spec", "policy", "protectedTables"),
			want:   []cause{{"spec.policy.protectedTables", "Too many"}},
		},
		refusal{
			name:   "dev.urlFrom is required",
			mutate: setting(map[string]any{}, "spec", "dev"),
			want:   []cause{{"spec.dev.urlFrom", "Required value"}},
		},
		refusal{
			name:   "dev.urlFrom optional",
			mutate: setting(map[string]any{"urlFrom": map[string]any{"name": "dev-database", "key": "url", "optional": true}}, "spec", "dev"),
			want:   []cause{{"spec", "dev.urlFrom must name a required Secret key"}},
		},
	)
	assertRefusals(t, schemaBase(namespace), rows)
}

func TestPtahMigrationRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-refusals")
	rows := append(targetRefusals(), artifactRefusals("artifact")...)
	rows = append(rows,
		refusal{
			// A PtahSchema accepts "all"; a migration runs files, so the
			// migration's enum stops at file and none.
			name:   "policy.transactionMode all, which only a PtahSchema takes",
			mutate: setting("all", "spec", "policy", "transactionMode"),
			want:   []cause{{"spec.policy.transactionMode", "Unsupported value"}},
		},
		refusal{
			name:   "policy.lockTimeout over 1h",
			mutate: setting("1h1s", "spec", "policy", "lockTimeout"),
			want:   []cause{{"spec.policy.lockTimeout", "policy.lockTimeout must be between 1s and 1h"}},
		},
		refusal{
			// The default lock timeout is five minutes, and the API server
			// applies it before the rule runs: a short deadline alone is enough.
			name:   "a deadline under the defaulted lockTimeout",
			mutate: setting(int64(60), "spec", "execution", "activeDeadlineSeconds"),
			want:   []cause{{"spec", "policy.lockTimeout must not exceed execution.activeDeadlineSeconds"}},
		},
	)
	assertRefusals(t, migrationBase(namespace), rows)
}

// numbered returns count distinct names, each valid on its own, so the only
// thing wrong with the list is its length.
func numbered(prefix string, count int) []any {
	names := make([]any, count)
	for index := range names {
		names[index] = prefix + strconv.Itoa(index)
	}
	return names
}

// numberedMap is count keys under prefix, each with one value: more than a
// bounded map takes.
func numberedMap(prefix string, count int) map[string]any {
	entries := make(map[string]any, count)
	for index := 0; index < count; index++ {
		entries[prefix+strconv.Itoa(index)] = "v"
	}
	return entries
}
