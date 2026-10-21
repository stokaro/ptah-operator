package crdschemahistory

import (
	"regexp"
	"testing"
)

// TestDriftFeatureCategoriesBreakWidensTheCategory holds the declared break to
// what its reason says: the pattern admits every category the enum it replaces
// admitted, so a summary an earlier manager stored still validates. It also
// refuses near misses, so the widening is the feature categories and not any
// string.
func TestDriftFeatureCategoriesBreakWidensTheCategory(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(driftCategoryPatternWithFeatures)
	if len(driftCategoriesBeforeFeatures) != 67 {
		t.Fatalf("the version 1 enum held 67 categories, this list holds %d", len(driftCategoriesBeforeFeatures))
	}
	for _, category := range driftCategoriesBeforeFeatures {
		if !pattern.MatchString(category) {
			t.Errorf("the pattern refuses %q, which the version 1 enum admitted", category)
		}
	}
	for _, category := range []string{
		"undecided",
		"feature_changes",
		"feature_access_unknown",
		"feature_changes:ptah.run/pgextension/extension-change",
		"feature_access_widened:ptah.run/pgdefaultprivilege/default-privilege",
		"feature_changes:ptah.run/clickhouse/view-storage/v1",
	} {
		if !pattern.MatchString(category) {
			t.Errorf("the pattern refuses %q", category)
		}
	}
	for _, category := range []string{
		"",
		"tables-added",
		"private_schema_name",
		"Tables_added",
		"feature_changes:",
		"feature_changes:ptah.run",
		"feature_changes:ptahrun/extension",
		"feature_changes:Ptah.run/extension",
		"feature_changes:ptah.run/../secret",
		"feature_changes:ptah.run/ext ension",
		"feature_other:ptah.run/extension",
		"tables_added:ptah.run/extension",
		"feature_changes:ptah.run/extension:again",
	} {
		if pattern.MatchString(category) {
			t.Errorf("the pattern admits %q", category)
		}
	}
}
