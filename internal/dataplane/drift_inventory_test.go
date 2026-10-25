package dataplane_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// driftWithInventory is a converged report, as Ptah prints one, that carries
// the given inventory.
func driftWithInventory(inventory string) string {
	return `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres",` +
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

	uncounted, err := dataplane.DecodeDrift([]byte(`{"drift":false,"failed":false,"failure_threshold":"all",`+
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
// vocabulary, and nothing else is.
func TestIsKnownDriftInventoryKind(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string]bool{
		"tables": true, "columns": true, "rls_policies": true, "enum_values": true,
		"": false, "tables_added": false, "data_rows": false, "rls_force": true, "widgets": false,
	} {
		if got := dataplane.IsKnownDriftInventoryKind(kind); got != want {
			t.Errorf("IsKnownDriftInventoryKind(%q) = %v, want %v", kind, got, want)
		}
	}
}
