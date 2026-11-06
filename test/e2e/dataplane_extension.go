package e2e

import (
	"slices"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	// pgExtensionKind is the kind Ptah gives a change to a PostgreSQL extension:
	// the change kind of its pgextension feature owner. Since v0.18.0 Ptah also
	// counts extensions under it in the drift report's inventory.
	pgExtensionKind = "ptah.run/pgextension/extension-change"
	// pgExtensionChange is the category Ptah v0.14.0 counts a change to a
	// PostgreSQL extension under: the kind, after feature_changes. v0.13.0
	// counted a missing extension as extensions_added.
	pgExtensionChange = "feature_changes:" + pgExtensionKind
)

// droppedExtensionObserved is a schema of the digest whose last Observe found
// one difference: the declared extension, missing, in Ptah's own category,
// counted once and safe, with the inventory saying the desired schema declares
// one extension and the database holds none. The observation before the first
// Apply also found no extension, but it found no table either and counted
// tables_added beside it, so a schema that passes was read after it converged
// and lost the extension.
func droppedExtensionObserved(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Source.Digest == digest &&
		status.Target.HighestDriftSeverity == "safe" && status.Target.DriftFindingCount == 1 &&
		slices.Equal(status.Target.DriftFindings, []ptahv1alpha1.DriftFindingStatus{
			{Category: pgExtensionChange, Count: 1, Severity: "safe"},
		}) &&
		slices.Contains(status.Target.Inventory, ptahv1alpha1.ObjectInventoryStatus{
			Kind: pgExtensionKind, Desired: 1, Observed: 0,
		})
}
