package e2e

import (
	"slices"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// pgExtensionChange is the category Ptah v0.14.0 counts a change to a
// PostgreSQL extension under: the change kind of its pgextension feature owner,
// after feature_changes. v0.13.0 counted a missing extension as
// extensions_added.
const pgExtensionChange = "feature_changes:ptah.run/pgextension/extension-change"

// droppedExtensionObserved is a schema of the digest whose last Observe found
// one difference: the declared extension, missing, in Ptah's own category,
// counted once and safe. The observation before the first Apply also found
// no extension, but it found no table either and counted tables_added beside
// it, so a schema that passes was read after it converged and lost the
// extension.
func droppedExtensionObserved(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Source.Digest == digest &&
		status.Target.HighestDriftSeverity == "safe" && status.Target.DriftFindingCount == 1 &&
		slices.Equal(status.Target.DriftFindings, []ptahv1alpha1.DriftFindingStatus{
			{Category: pgExtensionChange, Count: 1, Severity: "safe"},
		})
}
