package crdschemahistory

import (
	"regexp"
	"strings"
	"testing"
)

// TestDriftCategoriesV023BreakWidensTheCategory holds the declared break to
// what its reason says: the version 17 pattern admits every category, bare or
// with a kind, that the version 9 pattern admitted, admits the two categories
// Ptah v0.23.0 adds, and still refuses near misses.
func TestDriftCategoriesV023BreakWidensTheCategory(t *testing.T) {
	t.Parallel()

	before := regexp.MustCompile(driftCategoryPatternV016)
	after := regexp.MustCompile(driftCategoryPatternV023)
	admitted := append([]string{
		"undecided",
		"columns_modified",
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
	}, append(append([]string(nil), driftCategoriesBeforeFeatures...), driftCategoriesAddedInV016...)...)
	for _, category := range admitted {
		if !before.MatchString(category) {
			t.Fatalf("the version 9 pattern refuses %q, so it is not what this test compares against", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 17 pattern refuses %q, which version 9 admitted", category)
		}
	}
	for _, category := range driftCategoriesAddedInV023 {
		if before.MatchString(category) {
			t.Errorf("the version 9 pattern already admitted %q", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 17 pattern refuses %q", category)
		}
	}
	for _, category := range []string{
		"column_types_narrowing", "column_type_narrowed", "column_identities_added", "column_identity_removed",
		"column_types_narrowed:ptah.run/x", "Column_types_narrowed", "column-types-narrowed", "column_types",
	} {
		if after.MatchString(category) {
			t.Errorf("the version 17 pattern admits %q", category)
		}
	}
	if strings.Count(driftCategoryPatternV023, "|")-strings.Count(driftCategoryPatternV016, "|") != len(driftCategoriesAddedInV023) {
		t.Errorf("the version 17 pattern adds something other than the %d categories", len(driftCategoriesAddedInV023))
	}
	if got, want := strings.Replace(driftCategoryPatternV023, "column_identities_removed|column_types_narrowed|", "", 1), driftCategoryPatternV016; got != want {
		t.Errorf("the version 17 pattern without the two categories is\n%s\nwant the version 9 pattern\n%s", got, want)
	}
}
