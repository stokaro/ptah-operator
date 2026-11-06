package crd_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// longestFeatureKind is a kind of the 128 bytes the status bound admits.
var longestFeatureKind = strings.Repeat("a", 60) + ".io/" + strings.Repeat("b", 64)

// status.target.inventory holds counts by kind and nothing else: at most 128
// kinds, each once, named the way a finding category's stem or a feature
// category's kind is, with no negative count.
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
	for _, kind := range []string{
		"ptah.run/pgextension/extension-change",
		"ptah.run/pgpolicy/table-state-change",
		longestFeatureKind,
	} {
		feature := reread(t, schema)
		set(t, feature, []any{entry("columns"), entry(kind), entry("tables")}, "status", "target", "inventory")
		if err := api.Status().Update(context.Background(), feature, client.DryRunAll); err != nil {
			t.Fatalf("the status refused the feature kind %q: %v", kind, err)
		}
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
		{"a kind past the bound", changed(func(e map[string]any) { e["kind"] = strings.Repeat("k", 129) }),
			cause{"status.target.inventory[0].kind", "Too long"}},
		{"a feature kind past the bound", changed(func(e map[string]any) { e["kind"] = longestFeatureKind + "b" }),
			cause{"status.target.inventory[0].kind", "Too long"}},
		{"a feature kind with an uppercase domain", changed(func(e map[string]any) { e["kind"] = "Ptah.run/pgextension/extension-change" }),
			cause{"status.target.inventory[0].kind", "should match"}},
		{"a feature kind with a space", changed(func(e map[string]any) { e["kind"] = "ptah.run/pgextension/extension change" }),
			cause{"status.target.inventory[0].kind", "should match"}},
		{"a feature kind with no path", changed(func(e map[string]any) { e["kind"] = "ptah.run" }),
			cause{"status.target.inventory[0].kind", "should match"}},
		{"a finding category rather than its kind", changed(func(e map[string]any) { e["kind"] = "feature_changes:ptah.run/pgextension/extension-change" }),
			cause{"status.target.inventory[0].kind", "should match"}},
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

const (
	schemaCRD = "ptahschemas.operator.ptah.run"
	// earlierKindPattern and earlierKindBound are what schema version 9, the one
	// v0.2.8 and v0.2.9 install, holds an inventory kind to. These are the only
	// two values of that CRD's spec schema version 10 changed.
	earlierKindPattern = "^[a-z]([a-z_]*[a-z])?$"
	earlierKindBound   = int64(64)
)

// A rollback that puts the earlier CRD back meets entries that CRD would refuse.
// The chart does not do that: the earlier release's hook refuses to replace a
// newer schema version. So this measures the case that remains, a CRD someone
// put back by hand, and it is what the controller-state decision on the release
// page rests on: the stored entry stops nothing, every write that leaves it as
// it was or removes it is admitted, and only a new one is refused.
//
// It edits a CRD every other test reads, which is why it is not parallel: Go runs
// it to completion before any parallel test resumes.
func TestAStoredFeatureKindUnderTheEarlierCRD(t *testing.T) {
	plane.Require(t)

	const extension = "ptah.run/pgextension/extension-change"
	inventory := func(entries ...any) []any { return entries }
	entry := func(kind string, desired, observed int64) map[string]any {
		return map[string]any{"kind": kind, "desired": desired, "observed": observed}
	}
	columns, tables := entry("columns", 2, 2), entry("tables", 1, 1)
	// One entry only the pattern refuses, one both the pattern and the bound do.
	missing, longest := entry(extension, 1, 0), entry(longestFeatureKind, 1, 1)

	namespace := newNamespace(t, "inventory-rollback")
	schema := schemaBase(namespace)()
	if err := api.Create(context.Background(), schema); err != nil {
		t.Fatalf("store the schema: %v", err)
	}
	stored := reread(t, schema)
	set(t, stored, inventory(longest, columns, missing, tables), "status", "target", "inventory")
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("store the feature kinds under the current CRD: %v", err)
	}

	// A status with one kind the stored object does not hold, which only the
	// earlier CRD refuses. Dry run, so the object stays as the rows expect it.
	newEntryRefusal := func() error {
		probe := reread(t, schema)
		set(t, probe, inventory(longest, columns, missing, entry("ptah.run/pgpolicy/policy-change", 1, 0), tables),
			"status", "target", "inventory")
		return refusalMismatch(api.Status().Update(context.Background(), probe, client.DryRunAll),
			cause{"status.target.inventory[3].kind", "should match"})
	}
	if newEntryRefusal() == nil {
		t.Fatal("the current CRD refuses a feature kind, so the rows below prove nothing about the earlier one")
	}

	original := readCRD(t, schemaCRD)
	restored := false
	restore := func() error {
		current := readCRD(t, schemaCRD)
		current.Object["spec"] = original.DeepCopy().Object["spec"]
		return api.Update(context.Background(), current)
	}
	t.Cleanup(func() {
		if !restored {
			if err := restore(); err != nil {
				t.Errorf("put the current kind pattern back on %s: %v", schemaCRD, err)
			}
		}
	})
	earlier := readCRD(t, schemaCRD)
	if err := narrowInventoryKind(earlier); err != nil {
		t.Fatalf("%s: %v", schemaCRD, err)
	}
	if err := api.Update(context.Background(), earlier); err != nil {
		t.Fatalf("put the earlier kind pattern on %s: %v", schemaCRD, err)
	}
	if err := poll(func(context.Context) (bool, error) { return newEntryRefusal() == nil, nil }); err != nil {
		t.Fatalf("%s still admits a new feature kind after the kind was narrowed: %v; last answer: %v",
			schemaCRD, err, newEntryRefusal())
	}

	readInventory := func() []any {
		listed, _, err := unstructured.NestedSlice(reread(t, schema).Object, "status", "target", "inventory")
		if err != nil {
			t.Fatalf("read the stored inventory: %v", err)
		}
		return listed
	}
	holds := func(want []any) {
		t.Helper()
		if got := readInventory(); !reflect.DeepEqual(got, want) {
			t.Fatalf("the stored inventory is %v, want %v", got, want)
		}
	}
	// The manager's own status write: a merge patch of what changed, locked to
	// the version it read.
	patchStatus := func(change func(*unstructured.Unstructured)) error {
		before := reread(t, schema)
		after := before.DeepCopy()
		change(after)
		return api.Status().Patch(context.Background(), after,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}

	holds(inventory(longest, columns, missing, tables))

	// A new stem past the earlier bound fits the earlier pattern, so only the
	// bound refuses it.
	pastBound := reread(t, schema)
	set(t, pastBound, inventory(longest, columns, missing, tables, entry(strings.Repeat("k", int(earlierKindBound)+1), 1, 1)),
		"status", "target", "inventory")
	if mismatch := refusalMismatch(api.Status().Update(context.Background(), pastBound, client.DryRunAll),
		cause{"status.target.inventory[4].kind", "Too long"}); mismatch != nil {
		t.Errorf("a new stem past the earlier bound: %v", mismatch)
	}

	if err := patchStatus(func(object *unstructured.Unstructured) {
		set(t, object, int64(7), "status", "observedGeneration")
	}); err != nil {
		t.Errorf("a status patch that leaves the entries as they were: %v", err)
	}
	holds(inventory(longest, columns, missing, tables))

	whole := reread(t, schema)
	set(t, whole, int64(1), "status", "target", "driftFindingCount")
	if err := api.Status().Update(context.Background(), whole); err != nil {
		t.Errorf("a status update that sends the entries back unchanged: %v", err)
	}
	holds(inventory(longest, columns, missing, tables))

	spec := reread(t, schema)
	set(t, spec, "20m", "spec", "interval")
	if err := api.Update(context.Background(), spec); err != nil {
		t.Errorf("a write to the main resource: %v", err)
	}
	holds(inventory(longest, columns, missing, tables))

	recounted := entry(extension, 1, 1)
	if err := patchStatus(func(object *unstructured.Unstructured) {
		set(t, object, inventory(longest, columns, recounted, tables), "status", "target", "inventory")
	}); err != nil {
		t.Errorf("a status patch that changes a stored entry's counts and not its kind: %v", err)
	}
	holds(inventory(longest, columns, recounted, tables))

	if err := patchStatus(func(object *unstructured.Unstructured) {
		set(t, object, inventory(longest, recounted, tables), "status", "target", "inventory")
	}); err != nil {
		t.Errorf("a status patch that removes a stem and keeps the stored entries: %v", err)
	}
	holds(inventory(longest, recounted, tables))

	// What that manager's next Observe does: replace the list with stems.
	if err := patchStatus(func(object *unstructured.Unstructured) {
		set(t, object, inventory(columns, tables), "status", "target", "inventory")
	}); err != nil {
		t.Errorf("a status patch that replaces the entries with stems: %v", err)
	}
	holds(inventory(columns, tables))

	// With nothing stored to compare against, the same entry is refused.
	refused := patchStatus(func(object *unstructured.Unstructured) {
		set(t, object, inventory(columns, missing, tables), "status", "target", "inventory")
	})
	if mismatch := refusalMismatch(refused, cause{"status.target.inventory[1].kind", "should match"}); mismatch != nil {
		t.Errorf("a status patch that brings a feature kind back: %v", mismatch)
	}
	t.Logf("the earlier CRD refuses a feature kind the object does not hold: %v", refused)
	holds(inventory(columns, tables))

	if err := restore(); err != nil {
		t.Fatalf("put the current kind pattern back on %s: %v", schemaCRD, err)
	}
	restored = true
	if err := poll(func(context.Context) (bool, error) { return newEntryRefusal() != nil, nil }); err != nil {
		t.Fatalf("%s still refuses a feature kind after the kind was restored: %v", schemaCRD, err)
	}
}

// narrowInventoryKind holds status.target.inventory[].kind to what schema
// version 9 held it to in every served version, and refuses to report success if
// the kind is not the current one: a row that narrowed nothing proves nothing.
func narrowInventoryKind(crd *unstructured.Unstructured) error {
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil {
		return err
	}
	path := []string{
		"schema", "openAPIV3Schema", "properties", "status", "properties", "target", "properties",
		"inventory", "items", "properties", "kind",
	}
	narrowed := 0
	for index, entry := range versions {
		version, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("version %d is not an object", index)
		}
		kind, found, err := unstructured.NestedMap(version, path...)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("the CRD declares no inventory kind")
		}
		if pattern, _ := kind["pattern"].(string); pattern == earlierKindPattern {
			return errors.New("the inventory kind already holds the earlier pattern")
		}
		kind["pattern"], kind["maxLength"] = earlierKindPattern, earlierKindBound
		if err := unstructured.SetNestedMap(version, kind, path...); err != nil {
			return err
		}
		versions[index] = version
		narrowed++
	}
	if narrowed == 0 {
		return errors.New("the CRD serves no version")
	}
	return unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
}
