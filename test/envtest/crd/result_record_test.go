package crd_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResultRecordSchema(t *testing.T) {
	plane.Require(t)
	namespace := newNamespace(t, "result-record")
	base := func() *unstructured.Unstructured {
		return resource("PtahResultRecord", namespace, "record", map[string]any{"type": "chunk", "data": "cGF5bG9hZA=="})
	}
	assertRefusals(t, base, []refusal{
		{name: "missing spec", mutate: func(_ *testing.T, o *unstructured.Unstructured) { remove(o, "spec") }, want: []cause{{"spec", "Required value"}}},
		{name: "missing type", mutate: func(_ *testing.T, o *unstructured.Unstructured) { remove(o, "spec", "type") }, want: []cause{{"spec.type", "Required value"}}},
		{name: "unknown role", mutate: setting("unknown", "spec", "type"), want: []cause{{"spec.type", "Unsupported value"}}},
		{name: "missing data", mutate: func(_ *testing.T, o *unstructured.Unstructured) { remove(o, "spec", "data") }, want: []cause{{"spec.data", "Required value"}}},
		{name: "empty data", mutate: setting("", "spec", "data"), want: []cause{{"spec.data", "at least 1"}}},
		{name: "oversized encoded data", mutate: setting(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 524291))), "spec", "data"), want: []cause{{"spec.data", "699052"}}},
	})
	for _, role := range []string{"credential", "intent", "chunk", "complete"} {
		t.Run(role, func(t *testing.T) {
			object := base()
			object.SetName(role)
			set(t, object, role, "spec", "type")
			if err := api.Create(t.Context(), object); err != nil {
				t.Fatal(err)
			}
			for field, value := range map[string]string{"data": "Y2hhbmdlZA==", "type": "intent"} {
				if field == "type" && role == "intent" {
					value = "chunk"
				}
				changed := object.DeepCopy()
				set(t, changed, value, "spec", field)
				if mismatch := refusalMismatch(api.Update(t.Context(), changed), cause{"spec", "a result record is immutable"}); mismatch != nil {
					t.Fatal(mismatch)
				}
			}
		})
	}
}
