package crd_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// status.target.inventory holds counts by kind and nothing else: at most 128
// kinds, each once, named the way a finding category's stem is, with no
// negative count.
func TestStatusBoundsTheInventory(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "inventory")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store the schema: %v", err)
	}
	entry := func(kind string) map[string]any {
		return map[string]any{"kind": kind, "desired": int64(12), "observed": int64(11)}
	}
	kinds := func(count int) []any {
		listed := make([]any, 0, count)
		for index := range count {
			listed = append(listed, entry(fmt.Sprintf("kind_%c%c", 'a'+index/26, 'a'+index%26)))
		}
		return listed
	}
	changed := func(change func(map[string]any)) []any {
		first := entry("tables")
		change(first)
		return []any{first, entry("columns")}
	}

	stored := reread(t, schema)
	set(t, stored, kinds(128), "status", "target", "inventory")
	if err := api.Status().Update(context.Background(), stored, client.DryRunAll); err != nil {
		t.Fatalf("the status refused 128 kinds: %v", err)
	}
	zero := reread(t, schema)
	set(t, zero, changed(func(e map[string]any) { e["observed"] = int64(0) }), "status", "target", "inventory")
	if err := api.Status().Update(context.Background(), zero, client.DryRunAll); err != nil {
		t.Fatalf("the status refused a kind the database holds none of: %v", err)
	}

	for _, test := range []struct {
		name      string
		inventory []any
		want      cause
	}{
		{"129 kinds", kinds(129), cause{"status.target.inventory", "Too many"}},
		{"the same kind twice", []any{entry("tables"), entry("tables")}, cause{"status.target.inventory[1]", "Duplicate value"}},
		{"a kind that names an object", changed(func(e map[string]any) { e["kind"] = "public.users" }),
			cause{"status.target.inventory[0].kind", "should match"}},
		{"a kind past the bound", changed(func(e map[string]any) { e["kind"] = strings.Repeat("k", 65) }),
			cause{"status.target.inventory[0].kind", "Too long"}},
		{"a negative count", changed(func(e map[string]any) { e["desired"] = int64(-1) }),
			cause{"status.target.inventory[0].desired", "should be greater than or equal to 0"}},
		{"no count", changed(func(e map[string]any) { delete(e, "observed") }),
			cause{"status.target.inventory[0].observed", "Required value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := reread(t, schema)
			set(t, refused, test.inventory, "status", "target", "inventory")
			err := api.Status().Update(context.Background(), refused, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Error(mismatch)
			}
		})
	}
}
