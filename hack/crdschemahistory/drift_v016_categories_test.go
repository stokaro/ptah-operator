package crdschemahistory

import (
	"regexp"
	"strings"
	"testing"
)

// TestDriftCategoriesV016BreakWidensTheCategory holds the declared break to
// what its reason says: the version 9 pattern admits every category, bare or
// with a kind, that the version 2 pattern admitted, admits the 22 categories
// Ptah v0.16.0 added, and still refuses near misses.
func TestDriftCategoriesV016BreakWidensTheCategory(t *testing.T) {
	t.Parallel()

	before := regexp.MustCompile(driftCategoryPatternWithFeatures)
	after := regexp.MustCompile(driftCategoryPatternV016)
	if len(driftCategoriesAddedInV016) != 22 {
		t.Fatalf("Ptah v0.16.0 added 22 categories, this list holds %d", len(driftCategoriesAddedInV016))
	}
	admitted := append([]string{
		"undecided",
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
	}, driftCategoriesBeforeFeatures...)
	for _, category := range admitted {
		if !before.MatchString(category) {
			t.Fatalf("the version 2 pattern refuses %q, so it is not what this test compares against", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 9 pattern refuses %q, which version 2 admitted", category)
		}
	}
	for _, category := range driftCategoriesAddedInV016 {
		if before.MatchString(category) {
			t.Errorf("the version 2 pattern already admitted %q", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 9 pattern refuses %q", category)
		}
	}
	for _, category := range []string{
		"", "grants-added", "Grants_added", "grants_added:ptah.run/x", "grants", "views_renamed",
		"feature_changes:", "private_schema_name",
	} {
		if after.MatchString(category) {
			t.Errorf("the version 9 pattern admits %q", category)
		}
	}
	if strings.Count(driftCategoryPatternV016, "|")-strings.Count(driftCategoryPatternWithFeatures, "|") != 22 {
		t.Error("the version 9 pattern adds something other than the 22 categories")
	}
}
