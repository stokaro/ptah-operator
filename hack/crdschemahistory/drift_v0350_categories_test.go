package crdschemahistory

import (
	"regexp"
	"strings"
	"testing"
)

// TestDriftCategoriesV0350BreakWidensTheCategory holds the declared break to
// what its reason says: the version 26 pattern admits every category, bare or
// with a kind, that the version 23 pattern admitted, admits the category Ptah
// v0.35.0 adds, bare only, and still refuses near misses.
func TestDriftCategoriesV0350BreakWidensTheCategory(t *testing.T) {
	t.Parallel()

	before := regexp.MustCompile(driftCategoryPatternV0331)
	after := regexp.MustCompile(driftCategoryPatternV0350)
	admitted := append([]string{
		"undecided",
		"role_memberships_added",
		"role_memberships_removed",
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
	}, append(append(append(append([]string(nil), driftCategoriesBeforeFeatures...), driftCategoriesAddedInV016...),
		driftCategoriesAddedInV023...), driftCategoriesAddedInV0331...)...)
	for _, category := range admitted {
		if !before.MatchString(category) {
			t.Fatalf("the version 23 pattern refuses %q, so it is not what this test compares against", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 26 pattern refuses %q, which version 23 admitted", category)
		}
	}
	for _, category := range driftCategoriesAddedInV0350 {
		if before.MatchString(category) {
			t.Errorf("the version 23 pattern already admitted %q", category)
		}
		if !after.MatchString(category) {
			t.Errorf("the version 26 pattern refuses %q", category)
		}
	}
	for _, category := range []string{
		"role_membership_modified", "role_memberships_changed", "role_memberships_modified:ptah.run/postgres/role",
		"Role_memberships_modified", "role-memberships-modified", "role_memberships", "memberships_modified",
	} {
		if after.MatchString(category) {
			t.Errorf("the version 26 pattern admits %q", category)
		}
	}
	if strings.Count(driftCategoryPatternV0350, "|")-strings.Count(driftCategoryPatternV0331, "|") != len(driftCategoriesAddedInV0350) {
		t.Errorf("the version 26 pattern adds something other than the %d category", len(driftCategoriesAddedInV0350))
	}
	if without := strings.Replace(driftCategoryPatternV0350, "role_memberships_modified|", "", 1); without != driftCategoryPatternV0331 {
		t.Errorf("the version 26 pattern without the category is\n%s\nwant the version 23 pattern\n%s", without, driftCategoryPatternV0331)
	}
}
