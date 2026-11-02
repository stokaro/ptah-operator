package desiredspec_test

import (
	"encoding/json"
	"reflect"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/desiredspec"
)

// The rows admission's spec-writer rule is held to, as the API server sends
// a spec: suspend defaults to false, so a stored object carries
// "suspend": false where a marshaled Go object omits it.
func TestWithoutSuspendSetsAsideOnlyTheTopLevelKey(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		old, new string
		same     bool
	}{
		{"an explicit false turned true", `{"suspend": false, "desired": {"ociRef": "oci://registry.test/acme/app@sha256:aa"}}`,
			`{"suspend": true, "desired": {"ociRef": "oci://registry.test/acme/app@sha256:aa"}}`, true},
		{"a missing key and an explicit false", `{"desired": {"ociRef": "oci://registry.test/acme/app@sha256:aa"}}`,
			`{"suspend": false, "desired": {"ociRef": "oci://registry.test/acme/app@sha256:aa"}}`, true},
		{"a nested key named suspend", `{"suspend": false, "execution": {"nodeSelector": {"suspend": "a"}}}`,
			`{"suspend": false, "execution": {"nodeSelector": {"suspend": "b"}}}`, false},
		{"another field", `{"suspend": true, "policy": {"allowDestructive": false}}`,
			`{"suspend": true, "policy": {"allowDestructive": true}}`, false},
		{"a value that is not an object", `"suspend"`, `"suspend"`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var old, new any
			if err := json.Unmarshal([]byte(row.old), &old); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(row.new), &new); err != nil {
				t.Fatal(err)
			}
			if same := reflect.DeepEqual(desiredspec.WithoutSuspend(old), desiredspec.WithoutSuspend(new)); same != row.same {
				t.Fatalf("WithoutSuspend judges %s and %s the same: %t, want %t", row.old, row.new, same, row.same)
			}
			oldOf, err := desiredspec.Of(old)
			if err != nil {
				t.Fatal(err)
			}
			newOf, err := desiredspec.Of(new)
			if err != nil {
				t.Fatal(err)
			}
			if same := oldOf == newOf; same != row.same {
				t.Fatalf("Of gives %s and %s, the same: %t, want %t", oldOf, newOf, same, row.same)
			}
		})
	}
}

// Of reads a typed spec the same way: the plugin holds a review to the spec
// its typed client decoded.
func TestOfReadsATypedSpec(t *testing.T) {
	t.Parallel()
	spec := operatorv1alpha1.PtahSchemaSpec{Desired: operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.test/acme/app@sha256:aa"}}
	reviewed, err := desiredspec.Of(spec)
	if err != nil {
		t.Fatal(err)
	}
	suspended := spec
	suspended.Suspend = true
	edited := spec
	edited.Policy.AllowDestructive = true
	nested := spec
	nested.Execution.NodeSelector = map[string]string{"suspend": "true"}
	for _, row := range []struct {
		name string
		spec operatorv1alpha1.PtahSchemaSpec
		same bool
	}{
		{"suspended", suspended, true},
		{"edited", edited, false},
		{"a nested key named suspend", nested, false},
	} {
		got, err := desiredspec.Of(row.spec)
		if err != nil {
			t.Fatal(err)
		}
		if same := got == reviewed; same != row.same {
			t.Fatalf("%s: Of gives %s, against %s the same: %t, want %t", row.name, got, reviewed, same, row.same)
		}
	}
}
