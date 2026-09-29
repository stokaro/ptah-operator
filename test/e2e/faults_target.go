package e2e

import (
	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func schemaRetargetRefused(result runner.Result, previous string) bool {
	return sha256Pattern.MatchString(previous) && sha256Pattern.MatchString(result.TargetIdentityDigest) &&
		result.TargetIdentityDigest != previous && result.Error != nil && result.Error.Code == "target_binding_mismatch" &&
		result.ChildExitCode == -1 && !result.MutationStarted && !result.Uncertain && result.Truncation == nil
}

func freshApprovalConverged(resource *ptahv1alpha1.PtahSchema) bool {
	status := resource.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.ObservedGeneration == resource.Generation &&
		status.ActiveOperation == nil && status.PendingObservation == nil && conditionStatus(status.Conditions, "Ready", "True")
}
