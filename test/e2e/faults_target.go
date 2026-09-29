package e2e

import (
	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func schemaRetargetRefused(result runner.Result, previous string) bool {
	return sha256Pattern.MatchString(previous) && sha256Pattern.MatchString(result.TargetIdentityDigest) &&
		result.TargetIdentityDigest != previous && result.Error != nil && result.Error.Code == "target_binding_mismatch" &&
		result.ChildExitCode == -1 && !result.MutationStarted && !result.Uncertain && result.Truncation == nil
}

// A read of the substituted database cannot settle the dispatched Apply's
// outcome. The controller must keep the original proof binding and name that
// refusal, even while it retries the read-only observation.
func retargetProofRefused(resource *ptahv1alpha1.PtahSchema, operationID, jobUID, target string) bool {
	pending := resource.Status.PendingObservation
	if operationID == "" || jobUID == "" || !sha256Pattern.MatchString(target) || pending == nil ||
		pending.Outcome != ptahv1alpha1.PendingObservationOutcomeUnknown ||
		pending.ApplyOperationID != operationID || string(pending.ApplyJobUID) != jobUID ||
		pending.Plan.TargetIdentityDigest != target {
		return false
	}
	for _, condition := range resource.Status.Conditions {
		if condition.Type == ptahv1alpha1.ConditionReconciliationFailed && condition.Status == metav1.ConditionTrue &&
			condition.Reason == string(ptahv1alpha1.ReasonOperationFailed) &&
			condition.Message == "post-apply observation target does not match the applied plan" {
			return true
		}
	}
	return false
}

func targetPlanAwaitingApproval(resource *ptahv1alpha1.PtahSchema, target string) bool {
	return sha256Pattern.MatchString(target) && planAwaitingApproval(resource) &&
		resource.Status.Plan.TargetIdentityDigest == target &&
		resource.Status.PendingObservation == nil && resource.Status.ActiveOperation == nil
}

func freshApprovalConverged(resource *ptahv1alpha1.PtahSchema) bool {
	status := resource.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.ObservedGeneration == resource.Generation &&
		status.ActiveOperation == nil && status.PendingObservation == nil && conditionStatus(status.Conditions, "Ready", "True")
}
