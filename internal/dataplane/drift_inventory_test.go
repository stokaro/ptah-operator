package dataplane_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// driftWithInventory is a converged report, as Ptah prints one, that carries
// the given inventory.
func driftWithInventory(inventory string) string {
	return `{"contract_version":1,"command":"schema drift","drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres",` +
		`"diff":{"tables_added":[]},"inventory":` + inventory + `}`
}

// A drift report counts each kind of object on both sides, and the decoder
// reads the counts as Ptah wrote them. A report without an inventory, from a
// Ptah that does not count, decodes to none rather than to an empty one.
func TestDecodeDriftReadsTheInventory(t *testing.T) {
	t.Parallel()
	report, err := dataplane.DecodeDrift([]byte(driftWithInventory(
		`[{"kind":"columns","desired":87,"observed":85},{"kind":"tables","desired":12,"observed":11}]`)), 0)
	if err != nil {
		t.Fatalf("DecodeDrift() error = %v", err)
	}
	if got := fmt.Sprint(report.Inventory); got != "[{columns 87 85} {tables 12 11}]" {
		t.Fatalf("inventory = %s", got)
	}

	uncounted, err := dataplane.DecodeDrift([]byte(`{"contract_version":1,"command":"schema drift","drift":false,"failed":false,"failure_threshold":"all",`+
		`"highest_severity":"","dialect":"postgres","diff":{}}`), 0)
	if err != nil {
		t.Fatalf("DecodeDrift() error = %v", err)
	}
	if uncounted.Inventory != nil {
		t.Fatalf("a report with no inventory decoded to %v", uncounted.Inventory)
	}
}

func TestDecodeDriftRefusesAnInventoryItCannotKeep(t *testing.T) {
	t.Parallel()
	many := make([]string, 0, dataplane.MaxDriftInventoryEntries+1)
	for range dataplane.MaxDriftInventoryEntries + 1 {
		many = append(many, `{"kind":"tables","desired":1,"observed":1}`)
	}
	tests := []struct {
		name      string
		inventory string
		want      string
	}{
		{name: "a kind no category names", inventory: `[{"kind":"widgets","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "a category rather than a kind", inventory: `[{"kind":"tables_added","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "a stem with only one side", inventory: `[{"kind":"columns_modified","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "the same kind twice", inventory: `[{"kind":"tables","desired":1,"observed":1},{"kind":"tables","desired":2,"observed":2}]`,
			want: "drift report counts kind tables twice"},
		{name: "a negative count", inventory: `[{"kind":"tables","desired":-1,"observed":1}]`,
			want: "drift report counts kind tables below zero"},
		{name: "a count past an int32", inventory: `[{"kind":"tables","desired":2147483648,"observed":1}]`,
			want: "cannot unmarshal number 2147483648"},
		{name: "more kinds than the vocabulary pairs", inventory: "[" + strings.Join(many, ",") + "]",
			want: "drift report counts 129 kinds, more than the 128 any vocabulary pairs"},
		{name: "a feature kind twice", inventory: `[{"kind":"ptah.run/pgpolicy/policy-change","desired":1,"observed":1},` +
			`{"kind":"ptah.run/pgpolicy/policy-change","desired":1,"observed":0}]`,
			want: "drift report counts kind ptah.run/pgpolicy/policy-change twice"},
		{name: "a negative feature count", inventory: `[{"kind":"ptah.run/pgextension/extension-change","desired":1,"observed":-1}]`,
			want: "drift report counts kind ptah.run/pgextension/extension-change below zero"},
		{name: "a feature kind with no domain", inventory: `[{"kind":"pgextension/extension-change","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "a feature kind with no path", inventory: `[{"kind":"ptah.run","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "a feature kind with an object name", inventory: `[{"kind":"ptah.run/pgextension/extension change pg_trgm","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
		{name: "a feature kind past its bound", inventory: `[{"kind":"ptah.run/` + strings.Repeat("a", dataplane.MaxDriftFindingKindBytes) +
			`","desired":1,"observed":1}]`,
			want: "drift report counts a kind no finding category names"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := dataplane.DecodeDrift([]byte(driftWithInventory(test.inventory)), 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeDrift() error = %v, want %q", err, test.want)
			}
		})
	}
}

// An inventory kind is the stem of an addition and a removal in the
// vocabulary, or the kind a feature owner's categories carry, and nothing else
// is.
func TestIsKnownDriftInventoryKind(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string]bool{
		"tables": true, "columns": true, "rls_policies": true, "enum_values": true,
		"": false, "tables_added": false, "data_rows": false, "rls_force": true, "widgets": false,
		"ptah.run/pgextension/extension-change": true, "ptah.run/pgpolicy/table-state-change": true,
		"ptah.run": false, "public.users": false, "Ptah.run/pgextension/extension-change": false,
		"ptah.run/pgextension/extension change": false, "feature_changes:ptah.run/pgextension/extension-change": false,
		strings.Repeat("a", 60) + ".io/" + strings.Repeat("b", dataplane.MaxDriftFindingKindBytes-64): true,
		strings.Repeat("a", 60) + ".io/" + strings.Repeat("b", dataplane.MaxDriftFindingKindBytes-63): false,
	} {
		if got := dataplane.IsKnownDriftInventoryKind(kind); got != want {
			t.Errorf("IsKnownDriftInventoryKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

// Ptah counts the objects a feature owner compares under the kind its
// qualified categories carry. The decoder keeps those entries beside the stems,
// with their counts and in the order Ptah wrote them. The reports are the ones
// the Ptah v0.18.0 release image printed.
func TestDecodeDriftKeepsFeatureKindsInTheInventory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		report string
		want   string
	}{
		{"testdata/drift-feature-extension.json",
			"[{columns 2 2} {ptah.run/pgextension/extension-change 1 0} {tables 1 1}]"},
		{"testdata/drift-feature-policy.json",
			"[{columns 2 2} {ptah.run/pgpolicy/policy-change 1 0} {ptah.run/pgpolicy/table-state-change 1 1} {tables 1 1}]"},
	} {
		t.Run(test.report, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(test.report)
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeDrift(data, 1)
			if err != nil {
				t.Fatalf("DecodeDrift() error = %v", err)
			}
			if got := fmt.Sprint(report.Inventory); got != test.want {
				t.Fatalf("inventory = %s, want %s", got, test.want)
			}
		})
	}

	only, err := dataplane.DecodeDrift([]byte(driftWithInventory(
		`[{"kind":"ptah.run/pgextension/extension-change","desired":1,"observed":1}]`)), 0)
	if err != nil {
		t.Fatalf("DecodeDrift() error = %v", err)
	}
	if got := fmt.Sprint(only.Inventory); got != "[{ptah.run/pgextension/extension-change 1 1}]" {
		t.Fatalf("an inventory of a feature kind alone decoded to %s", got)
	}
}
