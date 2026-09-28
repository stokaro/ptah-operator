package crd_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A rule that refuses too much is as wrong as one that refuses too little, and
// the refusal rows cannot tell: each range rule is held here at both of its
// ends, and each cross-field rule at equality.
func TestRangeRulesAdmitTheirBounds(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "bounds")
	tests := []struct {
		name   string
		base   func() *unstructured.Unstructured
		mutate func(*testing.T, *unstructured.Unstructured)
	}{
		{name: "PtahSchema interval of 10s", base: schemaBase(namespace), mutate: setting("10s", "spec", "interval")},
		{name: "PtahSchema interval of 24h", base: schemaBase(namespace), mutate: setting("24h", "spec", "interval")},
		{name: "PtahMigration interval of 10s", base: migrationBase(namespace), mutate: setting("10s", "spec", "interval")},
		{name: "PtahSchema connectTimeout of 1s", base: schemaBase(namespace), mutate: setting("1s", "spec", "execution", "connectTimeout")},
		{name: "PtahSchema connectTimeout of 10m", base: schemaBase(namespace), mutate: setting("10m", "spec", "execution", "connectTimeout")},
		{name: "PtahMigration failureRetryInterval of 5s", base: migrationBase(namespace), mutate: setting("5s", "spec", "execution", "failureRetryInterval")},
		{name: "PtahMigration failureRetryInterval of 1h", base: migrationBase(namespace), mutate: setting("1h", "spec", "execution", "failureRetryInterval")},
		{name: "PtahSchema lockTimeout of 10m", base: schemaBase(namespace), mutate: setting("10m", "spec", "policy", "lockTimeout")},
		{
			// An hour also exceeds the default deadline of fifteen minutes, so
			// the upper bound is reachable only with a deadline to match.
			name: "PtahMigration lockTimeout of 1h",
			base: migrationBase(namespace),
			mutate: both(setting(int64(3600), "spec", "execution", "activeDeadlineSeconds"),
				setting("1h", "spec", "policy", "lockTimeout")),
		},
		{
			name: "PtahSchema connectTimeout equal to the deadline",
			base: schemaBase(namespace),
			mutate: both(setting(int64(30), "spec", "execution", "activeDeadlineSeconds"),
				setting("30s", "spec", "execution", "connectTimeout")),
		},
		{
			name: "PtahSchema lockTimeout equal to the deadline",
			base: schemaBase(namespace),
			mutate: both(setting(int64(30), "spec", "execution", "activeDeadlineSeconds"),
				setting("30s", "spec", "policy", "lockTimeout")),
		},
		{
			// The migration's lock timeout defaults to five minutes, so a
			// deadline of exactly that is the shortest one needing no override.
			name:   "PtahMigration deadline equal to the defaulted lockTimeout",
			base:   migrationBase(namespace),
			mutate: setting(int64(300), "spec", "execution", "activeDeadlineSeconds"),
		},
		{
			name:   "PtahSchema by realm rather than by key",
			base:   schemaBase(namespace),
			mutate: both(removing("spec", "target", "coordinationKey"), setting(map[string]any{"name": "production-application-primary"}, "spec", "target", "realmRef")),
		},
		{
			name:   "PtahMigration named at 63 bytes",
			base:   migrationBase(namespace),
			mutate: func(_ *testing.T, object *unstructured.Unstructured) { object.SetName(longName[:63]) },
		},
		{
			name:   "PtahSchema with 128 protected tables",
			base:   schemaBase(namespace),
			mutate: setting(numbered("ledger_", 128), "spec", "policy", "protectedTables"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := test.base()
			test.mutate(t, object)
			if err := dryRunCreate(object); err != nil {
				t.Fatalf("the API server refused %s %s at the bound: %v", object.GetKind(), object.GetName(), err)
			}
		})
	}
}

// Defaults are applied before the rules run. The plainHTTP rules read
// self.plainHTTP, which exists only because it defaults to false. A
// DockerConfigJSON source needs a key to mount, and dockerConfigJSONKey
// defaults to .dockerconfigjson, so the API server fills it in even when it is
// sent as null. No rule asks for the key, since none could refuse anything:
// these rows are what shows a stored DockerConfigJSON source always names one.
func TestDefaultsFillWhatTheRulesRead(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "defaults")
	tests := []struct {
		name   string
		source any
		field  []string
		want   any
	}{
		{
			name:   "DockerConfigJSON mode with no key",
			source: map[string]any{"name": "registry-credentials", "mode": "DockerConfigJSON"},
			field:  []string{"spec", "desired", "registryAuthFrom", "dockerConfigJSONKey"},
			want:   ".dockerconfigjson",
		},
		{
			name:   "DockerConfigJSON mode with a null key",
			source: map[string]any{"name": "registry-credentials", "mode": "DockerConfigJSON", "dockerConfigJSONKey": nil},
			field:  []string{"spec", "desired", "registryAuthFrom", "dockerConfigJSONKey"},
			want:   ".dockerconfigjson",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := schemaBase(namespace)()
			object.SetName("registry-" + string(rune('a'+index)))
			set(t, object, test.source, "spec", "desired", "registryAuthFrom")
			if err := api.Create(context.Background(), object); err != nil {
				t.Fatalf("the API server refused PtahSchema %s: %v", object.GetName(), err)
			}
			got, _, _ := unstructured.NestedFieldNoCopy(reread(t, object).Object, test.field...)
			if got != test.want {
				t.Fatalf("PtahSchema %s stored %v = %v, want %v", object.GetName(), test.field, got, test.want)
			}
		})
	}

	t.Run("caFrom with plainHTTP unset", func(t *testing.T) {
		t.Parallel()
		object := schemaBase(namespace)()
		object.SetName("registry-ca")
		set(t, object, map[string]any{"caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt"}}, "spec", "desired", "transport")
		if err := api.Create(context.Background(), object); err != nil {
			t.Fatalf("the API server refused PtahSchema %s: %v", object.GetName(), err)
		}
		plain, found, _ := unstructured.NestedBool(reread(t, object).Object, "spec", "desired", "transport", "plainHTTP")
		if !found || plain {
			t.Fatalf("PtahSchema %s stored plainHTTP %v (present %v), want the default false", object.GetName(), plain, found)
		}
	})
}
