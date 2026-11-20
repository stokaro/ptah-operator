package crdschemahistory

import (
	"regexp"
	"strings"
	"testing"
)

// TestDriftCategoriesV0331BreakWidensTheCategory holds the declared break to
// what its reason says: the version 23 pattern admits every category, bare or
// with a kind, that the version 17 pattern admitted, admits the two categories
// Ptah v0.33.1 adds, bare only, and still refuses near misses.
func TestDriftCategoriesV0331BreakWidensTheCategory(t *testing.T) {
	t.Parallel()

	before := regexp.MustCompile(driftCategoryPatternV023)
	after := regexp.MustCompile(driftCategoryPatternV0331)
	admitted := append([]string{
		"undecided",
		"columns_modified",
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
	}, append(append(append([]string(nil), driftCategoriesBeforeFeatures...), driftCategoriesAddedInV016...), driftCategoriesAddedInV023...)...)
	for _, category := range admitted {
		if !before.MatchString(category) {
			t.Fatalf("the version 17 pattern refuses %q, so it is not what this test compares against", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 23 pattern refuses %q, which version 17 admitted", category)
		}
	}
	for _, category := range driftCategoriesAddedInV0331 {
		if before.MatchString(category) {
			t.Errorf("the version 17 pattern already admitted %q", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 23 pattern refuses %q", category)
		}
	}
	for _, category := range []string{
		"column_charset_converted", "column_charsets_changed", "table_collation_changed", "table_collations_converted",
		"table_collations_changed:ptah.run/mysql/table", "column_charsets_converted:ptah.run/x",
		"Table_collations_changed", "column-charsets-converted", "table_collations", "column_charsets",
	} {
		if after.MatchString(category) {
			t.Errorf("the version 23 pattern admits %q", category)
		}
	}
	if strings.Count(driftCategoryPatternV0331, "|")-strings.Count(driftCategoryPatternV023, "|") != len(driftCategoriesAddedInV0331) {
		t.Errorf("the version 23 pattern adds something other than the %d categories", len(driftCategoriesAddedInV0331))
	}
	without := strings.Replace(strings.Replace(driftCategoryPatternV0331, "column_charsets_converted|", "", 1), "table_collations_changed|", "", 1)
	if without != driftCategoryPatternV023 {
		t.Errorf("the version 23 pattern without the two categories is\n%s\nwant the version 17 pattern\n%s", without, driftCategoryPatternV023)
	}
}
