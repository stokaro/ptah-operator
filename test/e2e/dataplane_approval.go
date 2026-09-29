package e2e

import ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"

func changedSchemaApprovalRefused(resource *ptahv1alpha1.PtahSchema, oldPlan string, generation int64) bool {
	status := resource.Status
	return oldPlan != "" && generation > 0 && resource.Generation == generation && status.ObservedGeneration == generation &&
		status.ActiveOperation == nil && status.Plan != nil && status.Plan.Name != "" && status.Plan.UID != "" &&
		string(status.Plan.UID) != oldPlan && status.Phase == ptahv1alpha1.PhaseAwaitingApproval &&
		conditionIs(status.Conditions, "ApprovalRequired", "True", "PlanReady")
}
