package controller

import (
	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// objectInventory is an observation's inventory as status keeps it: the
// counts the runner framed, kind by kind, and nothing when it framed none.
func objectInventory(inventory []runner.DriftInventorySummary) []operatorv1alpha1.ObjectInventoryStatus {
	if len(inventory) == 0 {
		return nil
	}
	kept := make([]operatorv1alpha1.ObjectInventoryStatus, 0, len(inventory))
	for _, entry := range inventory {
		kept = append(kept, operatorv1alpha1.ObjectInventoryStatus{
			Kind: entry.Kind, Desired: entry.Desired, Observed: entry.Observed,
		})
	}
	return kept
}
