package crd_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	oneURLSource    = "set exactly one of urlFrom and urlFromSecretProviderClass"
	classAccount    = "a URL from a SecretProviderClass needs execution.serviceAccountName to name an account other than default"
	shadowNotTarget = "policy.driftCheck.shadow.urlFromSecretProviderClass must name another object than target.urlFromSecretProviderClass"
)

// fromClass rewrites a base so its target reads its URL from a
// SecretProviderClass object, under the named account the store trusts.
func fromClass(base func() *unstructured.Unstructured) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		object := base()
		remove(object, "spec", "target", "urlFrom")
		if err := unstructured.SetNestedMap(object.Object, map[string]any{"name": "application-store", "objectName": "url"},
			"spec", "target", "urlFromSecretProviderClass"); err != nil {
			panic(err)
		}
		if err := unstructured.SetNestedField(object.Object, "application-operations", "spec", "execution", "serviceAccountName"); err != nil {
			panic(err)
		}
		return object
	}
}

// classTargetRefusals are the refusals a target reading its URL from a
// SecretProviderClass object owes, the same for both kinds. Each row is
// shaped so that one rule refuses it.
func classTargetRefusals() []refusal {
	class := []string{"spec", "target", "urlFromSecretProviderClass"}
	at := func(field string) []string { return append(append([]string{}, class...), field) }
	return []refusal{
		{
			name:   "a target naming a Secret key and a class",
			mutate: setting(map[string]any{"name": "application-database", "key": "url"}, "spec", "target", "urlFrom"),
			want:   []cause{{"spec.target", oneURLSource}},
		},
		{
			name:   "a class without its name",
			mutate: removing(at("name")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.name", "Required value"}},
		},
		{
			name:   "a class without its object",
			mutate: removing(at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "Required value"}},
		},
		{
			name:   "a class name that is no object name",
			mutate: setting("Application_Store", at("name")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.name", "should match"}},
		},
		{
			name:   "a class name past 253 characters",
			mutate: setting(strings.Repeat("a", 254), at("name")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.name", "Too long"}},
		},
		{
			name:   "an empty object name",
			mutate: setting("", at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "at least 1 chars long"}},
		},
		{
			name:   "an object name past 253 characters",
			mutate: setting(strings.Repeat("u", 254), at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "Too long"}},
		},
		{
			name:   "an object in a directory",
			mutate: setting("database/url", at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "should match"}},
		},
		{
			name:   "the parent directory as the object",
			mutate: setting("..", at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "should match"}},
		},
		{
			name:   "an object name the driver keeps for itself",
			mutate: setting("..data", at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "should match"}},
		},
		{
			name:   "the volume itself as the object",
			mutate: setting(".", at("objectName")...),
			want:   []cause{{"spec.target.urlFromSecretProviderClass.objectName", "should match"}},
		},
		{
			name:   "a class under the namespace's default account",
			mutate: setting("default", "spec", "execution", "serviceAccountName"),
			want:   []cause{{"spec", classAccount}},
		},
		{
			name:   "a class under no named account",
			mutate: removing("spec", "execution", "serviceAccountName"),
			want:   []cause{{"spec", classAccount}},
		},
	}
}

func TestAURLFromASecretProviderClassRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	t.Run("PtahSchema", func(t *testing.T) {
		t.Parallel()
		namespace := newNamespace(t, "class-schema-refusals")
		rows := append(classTargetRefusals(),
			refusal{
				name:   "a development database naming no URL",
				mutate: setting(map[string]any{}, "spec", "dev"),
				want:   []cause{{"spec.dev", oneURLSource}},
			},
			refusal{
				name: "a development database naming a Secret key and a class",
				mutate: setting(map[string]any{
					"urlFrom":                    map[string]any{"name": "dev-database", "key": "url"},
					"urlFromSecretProviderClass": map[string]any{"name": "dev-store", "objectName": "url"},
				}, "spec", "dev"),
				want: []cause{{"spec.dev", oneURLSource}},
			},
		)
		assertRefusals(t, fromClass(schemaBase(namespace)), rows)
		// The account rule reads the development database too: a Secret
		// target beside a class development database needs the account.
		assertRefusals(t, schemaBase(namespace), []refusal{{
			name:   "a class development database under the default account",
			mutate: setting(map[string]any{"urlFromSecretProviderClass": map[string]any{"name": "dev-store", "objectName": "url"}}, "spec", "dev"),
			want:   []cause{{"spec", classAccount}},
		}})
	})

	t.Run("PtahMigration", func(t *testing.T) {
		t.Parallel()
		namespace := newNamespace(t, "class-migration-refusals")
		shadow := []string{"spec", "policy", "driftCheck", "shadow"}
		rows := append(classTargetRefusals(),
			refusal{
				name:   "a shadow naming the target's class object",
				mutate: setting(map[string]any{"urlFromSecretProviderClass": map[string]any{"name": "application-store", "objectName": "url"}}, shadow...),
				want:   []cause{{"spec", shadowNotTarget}},
			},
			refusal{
				name: "a shadow naming a Secret key and a class",
				mutate: setting(map[string]any{
					"urlFrom":                    map[string]any{"name": "application-shadow", "key": "url"},
					"urlFromSecretProviderClass": map[string]any{"name": "application-store", "objectName": "shadow-url"},
				}, shadow...),
				want: []cause{{"spec.policy.driftCheck.shadow", oneURLSource}},
			},
		)
		assertRefusals(t, fromClass(driftCheckedMigrationBase(namespace)), rows)
		assertRefusals(t, driftCheckedMigrationBase(namespace), []refusal{{
			name:   "a class shadow under the default account",
			mutate: setting(map[string]any{"urlFromSecretProviderClass": map[string]any{"name": "application-store", "objectName": "shadow-url"}}, shadow...),
			want:   []cause{{"spec", classAccount}},
		}})
	})
}

// Each place a URL comes from takes a class object as well as a Secret key,
// in any mix, and an object name the driver writes a file under: one that
// starts with a dot, or holds dots inside.
func TestAURLFromASecretProviderClassIsStored(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	class := func(name, object string) map[string]any { return map[string]any{"name": name, "objectName": object} }
	namespace := newNamespace(t, "class-admitted")
	for _, test := range []struct {
		name  string
		base  func() *unstructured.Unstructured
		value any
		path  []string
	}{
		{name: "a schema target", base: fromClass(schemaBase(namespace))},
		{name: "a hidden object file", base: fromClass(schemaBase(namespace)), value: ".url",
			path: []string{"spec", "target", "urlFromSecretProviderClass", "objectName"}},
		{name: "an object name with dots inside", base: fromClass(schemaBase(namespace)), value: "orders..primary.url",
			path: []string{"spec", "target", "urlFromSecretProviderClass", "objectName"}},
		{name: "a class development database beside a class target", base: fromClass(schemaBase(namespace)),
			value: map[string]any{"urlFromSecretProviderClass": class("application-dev-store", "url")}, path: []string{"spec", "dev"}},
		{name: "a Secret development database beside a class target", base: fromClass(schemaBase(namespace)),
			value: map[string]any{"urlFrom": map[string]any{"name": "dev-database", "key": "url"}}, path: []string{"spec", "dev"}},
		{name: "a migration target", base: fromClass(migrationBase(namespace))},
		{name: "another object of the target's class as the shadow", base: fromClass(driftCheckedMigrationBase(namespace)),
			value: map[string]any{"urlFromSecretProviderClass": class("application-store", "shadow-url")},
			path:  []string{"spec", "policy", "driftCheck", "shadow"}},
		{name: "the target's object name in another class as the shadow", base: fromClass(driftCheckedMigrationBase(namespace)),
			value: map[string]any{"urlFromSecretProviderClass": class("application-shadow-store", "url")},
			path:  []string{"spec", "policy", "driftCheck", "shadow"}},
		{name: "a Secret shadow beside a class target", base: fromClass(driftCheckedMigrationBase(namespace))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := test.base()
			if test.path != nil {
				set(t, object, test.value, test.path...)
			}
			if err := dryRunCreate(object); err != nil {
				t.Fatalf("the API server refused %s %s: %v", object.GetKind(), object.GetName(), err)
			}
		})
	}
}

// A claim records the class it bound, and a schema's claim the development
// database it read, under the same rule as the spec.
func TestAClaimRecordsTheClassItBound(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "class-claim")
	schema := fromClass(schemaBase(namespace))()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store PtahSchema %s: %v", schema.GetName(), err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	claim := func(dev map[string]any) map[string]any {
		return map[string]any{
			"type": "Plan", "id": "operation-01", "inputFingerprint": digest, "jobName": "ptah-plan-application-0123456789abcdef",
			"startedAt": "2026-11-23T10:00:00Z", "attempt": int64(1), "executionBindingID": "v1-" + strings.Repeat("a", 32),
			"target": map[string]any{
				"engine": "PostgreSQL", "urlFromSecretProviderClass": map[string]any{"name": "application-store", "objectName": "url"},
			},
			"observationDev": dev,
		}
	}

	stored := reread(t, schema)
	set(t, stored, claim(map[string]any{"urlFromSecretProviderClass": map[string]any{"name": "application-dev-store", "objectName": "url"}}),
		"status", "activeOperation")
	if err := api.Status().Update(context.Background(), stored, client.DryRunAll); err != nil {
		t.Fatalf("the status refused a claim bound to SecretProviderClass objects: %v", err)
	}
	refused := reread(t, schema)
	set(t, refused, claim(map[string]any{}), "status", "activeOperation")
	if mismatch := refusalMismatch(api.Status().Update(context.Background(), refused, client.DryRunAll),
		cause{"status.activeOperation.observationDev", oneURLSource}); mismatch != nil {
		t.Error(mismatch)
	}
}
