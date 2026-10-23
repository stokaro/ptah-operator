package crd_test

import (
	"fmt"
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
	rows := []refusal{
		{
			name:   source + " is required",
			mutate: removing("spec", source),
			want:   []cause{{field, "Required value"}},
		},
		{
			name:   source + " with neither ociRef nor configMap",
			mutate: removing("spec", source, "ociRef"),
			want: []cause{
				{field, "set exactly one of ociRef, configMap and git"},
				{field, "verificationPolicyFrom is required with ociRef and refused with configMap and git"},
			},
		},
		{
			name:   source + ".configMap beside ociRef",
			mutate: setting(configMapSource(1), "spec", source, "configMap"),
			want:   []cause{{field, "set exactly one of ociRef, configMap and git"}},
		},
		{
			name:   source + ".configMap with a verification policy",
			mutate: both(removing("spec", source, "ociRef"), setting(configMapSource(1), "spec", source, "configMap")),
			want:   []cause{{field, "verificationPolicyFrom is required with ociRef and refused with configMap and git"}},
		},
		{
			name:   source + ".configMap with registry credentials",
			mutate: both(asConfigMapSource(source), setting(map[string]any{"name": "registry-credentials"}, "spec", source, "registryAuthFrom")),
			want:   []cause{{field, "registryAuthFrom is refused with configMap and git"}},
		},
		{
			name:   source + ".configMap over plain HTTP",
			mutate: both(asConfigMapSource(source), setting(map[string]any{"plainHTTP": true}, "spec", source, "transport")),
			want:   []cause{{field, "transport is refused with configMap and git"}},
		},
		{
			name:   source + ".configMap with a registry CA",
			mutate: both(asConfigMapSource(source), setting(map[string]any{"caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt"}}, "spec", source, "transport")),
			want:   []cause{{field, "transport is refused with configMap and git"}},
		},
		{
			name:   source + ".configMap.name outside its pattern",
			mutate: both(asConfigMapSource(source), setting("Schema", "spec", source, "configMap", "name")),
			want:   []cause{{field + ".configMap.name", "should match"}},
		},
		{
			name:   source + ".configMap selecting no file",
			mutate: both(asConfigMapSource(source), setting([]any{}, "spec", source, "configMap", "items")),
			want:   []cause{{field + ".configMap.items", "should have at least 1 items"}},
		},
		{
			name:   source + ".configMap.items naming one path twice",
			mutate: both(asConfigMapSource(source), setting([]any{item("a", "schema.sql"), item("b", "schema.sql")}, "spec", source, "configMap", "items")),
			want:   []cause{{field + ".configMap.items[1]", "Duplicate value"}},
		},
		{
			name:   source + ".configMap.items key outside its pattern",
			mutate: both(asConfigMapSource(source), setting([]any{item("a/b", "schema.sql")}, "spec", source, "configMap", "items")),
			want:   []cause{{field + ".configMap.items[0].key", "should match"}},
		},
	}
	for _, path := range []string{"../schema.sql", "/schema.sql", "db//schema.sql", "db/./schema.sql", "db/..", "db\\schema.sql", "schema.sql\n", "db/"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.configMap.items path %q", source, path),
			mutate: both(asConfigMapSource(source), setting([]any{item("schema", path)}, "spec", source, "configMap", "items")),
			want:   []cause{{field + ".configMap.items[0].path", "should match"}},
		})
	}
	rows = append(rows, gitRefusals(source)...)
	return append(rows, []refusal{
		{
			name:   source + ".ociRef without the oci scheme",
			mutate: setting("ghcr.io/example/schema:1.4.0", "spec", source, "ociRef"),
			want:   []cause{{field + ".ociRef", "should match"}},
		},
		{
			name:   source + ".verificationPolicyFrom is required with ociRef",
			mutate: removing("spec", source, "verificationPolicyFrom"),
			want:   []cause{{field, "verificationPolicyFrom is required with ociRef and refused with configMap and git"}},
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
	}...)
}

// configMapSource selects count files from a ConfigMap.
func configMapSource(count int) map[string]any {
	items := make([]any, count)
	for index := range items {
		items[index] = item(fmt.Sprintf("file%d.sql", index), fmt.Sprintf("file%d.sql", index))
	}
	return map[string]any{"name": "orders-schema", "items": items}
}

func item(key, path string) map[string]any { return map[string]any{"key": key, "path": path} }

// asConfigMapSource replaces the base resource's OCI source with a ConfigMap
// that selects one file, which every rule admits.
func asConfigMapSource(source string) func(*testing.T, *unstructured.Unstructured) {
	return func(t *testing.T, object *unstructured.Unstructured) {
		unstructured.RemoveNestedField(object.Object, "spec", source, "ociRef")
		unstructured.RemoveNestedField(object.Object, "spec", source, "verificationPolicyFrom")
		set(t, object, configMapSource(1), "spec", source, "configMap")
	}
}

// gitSource selects a path of a branch over https://.
func gitSource(path string) map[string]any {
	return map[string]any{"url": "https://git.example.com/acme/app.git", "ref": map[string]any{"branch": "main"}, "path": path}
}

// asGitSource replaces the base resource's OCI source with a Git branch over
// https://, which every rule admits.
func asGitSource(source string) func(*testing.T, *unstructured.Unstructured) {
	return func(t *testing.T, object *unstructured.Unstructured) {
		unstructured.RemoveNestedField(object.Object, "spec", source, "ociRef")
		unstructured.RemoveNestedField(object.Object, "spec", source, "verificationPolicyFrom")
		set(t, object, gitSource("db/schema.hcl"), "spec", source, "git")
	}
}

// gitRefusals are the rules of a Git source, which read the same under
// spec.desired and spec.artifact.
func gitRefusals(source string) []refusal {
	field := "spec." + source
	git := field + ".git"
	knownHosts := map[string]any{"knownHostsFrom": map[string]any{"name": "git-hosts", "key": "known_hosts"}}
	rows := []refusal{
		{
			name:   source + ".git beside ociRef",
			mutate: setting(gitSource("db/schema.hcl"), "spec", source, "git"),
			want:   []cause{{field, "set exactly one of ociRef, configMap and git"}},
		},
		{
			name:   source + ".git beside configMap",
			mutate: both(asGitSource(source), setting(configMapSource(1), "spec", source, "configMap")),
			want:   []cause{{field, "set exactly one of ociRef, configMap and git"}},
		},
		{
			name:   source + ".git with a verification policy",
			mutate: both(removing("spec", source, "ociRef"), setting(gitSource("db/schema.hcl"), "spec", source, "git")),
			want:   []cause{{field, "verificationPolicyFrom is required with ociRef and refused with configMap and git"}},
		},
		{
			name:   source + ".git with registry credentials",
			mutate: both(asGitSource(source), setting(map[string]any{"name": "registry-credentials"}, "spec", source, "registryAuthFrom")),
			want:   []cause{{field, "registryAuthFrom is refused with configMap and git"}},
		},
		{
			name:   source + ".git with a registry transport",
			mutate: both(asGitSource(source), setting(map[string]any{"plainHTTP": true}, "spec", source, "transport")),
			want:   []cause{{field, "transport is refused with configMap and git"}},
		},
		{
			name:   source + ".git.ref naming a branch and a tag",
			mutate: both(asGitSource(source), setting(map[string]any{"branch": "main", "tag": "v1"}, "spec", source, "git", "ref")),
			want:   []cause{{git + ".ref", "set exactly one of branch, tag and commit"}},
		},
		{
			name:   source + ".git.ref naming nothing",
			mutate: both(asGitSource(source), setting(map[string]any{}, "spec", source, "git", "ref")),
			want:   []cause{{git + ".ref", "set exactly one of branch, tag and commit"}},
		},
		{
			name:   source + ".git.ref.commit abbreviated",
			mutate: both(asGitSource(source), setting(map[string]any{"commit": "3f1c2b0"}, "spec", source, "git", "ref")),
			want:   []cause{{git + ".ref.commit", "should match"}},
		},
		{
			name:   source + ".git.ref.branch with a space",
			mutate: both(asGitSource(source), setting(map[string]any{"branch": "my branch"}, "spec", source, "git", "ref")),
			want:   []cause{{git + ".ref.branch", "should match"}},
		},
		{
			name:   source + ".git ssh:// without known hosts",
			mutate: both(asGitSource(source), setting("ssh://git@git.example.com/acme/app.git", "spec", source, "git", "url")),
			want:   []cause{{git, "an ssh:// repository needs transport.knownHostsFrom"}},
		},
		{
			name:   source + ".git https:// with known hosts",
			mutate: both(asGitSource(source), setting(knownHosts, "spec", source, "git", "transport")),
			want:   []cause{{git, "transport.knownHostsFrom is for an ssh:// repository"}},
		},
		{
			name: source + ".git ssh:// with a CA",
			mutate: both(asGitSource(source), both(setting("ssh://git@git.example.com/acme/app.git", "spec", source, "git", "url"),
				setting(map[string]any{"knownHostsFrom": knownHosts["knownHostsFrom"], "caFrom": map[string]any{"name": "git-ca", "key": "ca.pem"}},
					"spec", source, "git", "transport"))),
			want: []cause{{git, "transport.caFrom is for an https:// repository"}},
		},
		{
			name: source + ".git.transport.caFrom optional",
			mutate: both(asGitSource(source), setting(map[string]any{"caFrom": map[string]any{"name": "git-ca", "key": "ca.pem", "optional": true}},
				"spec", source, "git", "transport")),
			want: []cause{{git + ".transport", "caFrom must name a required ConfigMap key"}},
		},
		{
			name: source + ".git.transport.knownHostsFrom without a key",
			mutate: both(asGitSource(source), both(setting("ssh://git@git.example.com/acme/app.git", "spec", source, "git", "url"),
				setting(map[string]any{"knownHostsFrom": map[string]any{"name": "git-hosts", "key": ""}}, "spec", source, "git", "transport"))),
			want: []cause{{git + ".transport", "knownHostsFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".git.authFrom.name empty",
			mutate: both(asGitSource(source), setting(map[string]any{"name": ""}, "spec", source, "git", "authFrom")),
			want:   []cause{{git + ".authFrom.name", "should be at least 1 chars long"}},
		},
	}
	for _, url := range []string{
		"git+https://git.example.com/acme/app.git", "http://git.example.com/acme/app.git", "https://bot:token@git.example.com/acme/app.git",
		"https://git.example.com/acme/app.git?ref=main", "https://git.example.com", "file:///srv/app.git",
	} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.git.url %q", source, url),
			mutate: both(asGitSource(source), setting(url, "spec", source, "git", "url")),
			want:   []cause{{git + ".url", "should match"}},
		})
	}
	for _, path := range []string{"../schema.hcl", "/schema.hcl", "db//schema.hcl", "db/./schema.hcl", "db\\schema.hcl", "db/"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.git.path %q", source, path),
			mutate: both(asGitSource(source), setting(path, "spec", source, "git", "path")),
			want:   []cause{{git + ".path", "should match"}},
		})
	}
	return rows
}

func TestPtahSchemaRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-refusals")
	rows := append(targetRefusals(), artifactRefusals("desired")...)
	rows = append(rows,
		refusal{
			name:   "desired.git selecting the repository root",
			mutate: both(asGitSource("desired"), setting(".", "spec", "desired", "git", "path")),
			want:   []cause{{"spec", "desired.git.path selects one file for a schema"}},
		},
		refusal{
			name:   "desired.configMap selecting two files",
			mutate: both(asConfigMapSource("desired"), setting(configMapSource(2), "spec", "desired", "configMap")),
			want:   []cause{{"spec", "desired.configMap selects exactly one file for a schema"}},
		},
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
