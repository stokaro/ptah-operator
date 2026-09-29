package e2e

import (
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func changedMigrationApprovalRefused(resource *ptahv1alpha1.PtahMigration, oldPlan types.UID, generation int64, disabled bool) bool {
	status := resource.Status
	if oldPlan == "" || generation <= 0 || resource.Generation != generation || status.ObservedGeneration != generation ||
		status.ActiveOperation != nil || status.UnresolvedRun != nil || status.Plan == nil ||
		status.Plan.Name == "" || status.Plan.UID == "" || status.Plan.UID == oldPlan {
		return false
	}
	if disabled {
		return conditionIs(status.Conditions, "Ready", "False", "ApplyDisabled") &&
			conditionIs(status.Conditions, "Progressing", "False", "ApplyDisabled")
	}
	return status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval &&
		conditionStatus(status.Conditions, "ApprovalRequired", "True")
}
