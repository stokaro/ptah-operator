package crdschemahistory

import (
	"regexp"
	"strings"
	"testing"
)

// TestFeatureInventoryBreakWidensTheKind holds the declared break to what its
// reason says: the version 10 pattern admits every kind the version 9 pattern
// admitted, admits the feature kinds Ptah v0.18.0 counts, and still refuses a
// kind that names an object.
func TestFeatureInventoryBreakWidensTheKind(t *testing.T) {
	t.Parallel()

	before := regexp.MustCompile(inventoryKindPatternStems)
	after := regexp.MustCompile(inventoryKindPatternWithFeatures)
	for _, kind := range []string{
		"a", "ab", "tables", "columns", "rls_policies", "enum_values", "rls_enabled_tables",
		"a_b", strings.Repeat("k", 64),
	} {
		if !before.MatchString(kind) {
			t.Fatalf("the version 9 pattern refuses %q, so it is not what this test compares against", kind)
		}
		if !after.MatchString(kind) {
			t.Errorf("the version 10 pattern refuses %q, which version 9 admitted", kind)
		}
	}
	for _, kind := range []string{
		"ptah.run/pgextension/extension-change",
		"ptah.run/pgpolicy/policy-change",
		"ptah.run/pgpolicy/table-state-change",
		"ptah.run/clickhouse/view-storage/v1",
	} {
		if before.MatchString(kind) {
			t.Errorf("the version 9 pattern already admitted %q", kind)
		}
		if !after.MatchString(kind) {
			t.Errorf("the version 10 pattern refuses %q", kind)
		}
	}
	for _, kind := range []string{
		"", "public.users", "ptah.run", "Ptah.run/x", "ptah.run/a b", "tables-added", "Tables", "_tables", "tables_",
		"feature_changes:ptah.run/x", "ptahrun/x", "ptah.run/", "ptah.run/../x y",
	} {
		if after.MatchString(kind) {
			t.Errorf("the version 10 pattern admits %q", kind)
		}
	}
}
