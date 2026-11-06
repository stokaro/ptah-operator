package e2e

import (
	"slices"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// unreadableTableObserved is a schema of the digest whose last Observe, under
// the login support/databases.md documents, found the table another role
// created and that table's primary key, each once and destructive, and nothing
// else, and counted both in the inventory beside the one the schema declares.
// That is what a superuser's report holds. Ptah v0.18.0 read tables through
// information_schema and reported neither under this login, and the same login
// with pg_read_all_data reported the table without its constraint.
func unreadableTableObserved(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Source.Digest == digest &&
		status.Target.HighestDriftSeverity == "destructive" && status.Target.DriftFindingCount == 2 &&
		slices.Equal(status.Target.DriftFindings, []ptahv1alpha1.DriftFindingStatus{
			{Category: "constraints_removed", Count: 1, Severity: "destructive"},
			{Category: "tables_removed", Count: 1, Severity: "destructive"},
		}) &&
		slices.Contains(status.Target.Inventory, ptahv1alpha1.ObjectInventoryStatus{Kind: "tables", Desired: 1, Observed: 2}) &&
		slices.Contains(status.Target.Inventory, ptahv1alpha1.ObjectInventoryStatus{Kind: "constraints", Desired: 1, Observed: 2})
}
