package e2e

import (
	"errors"

	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The artifact row changes only the desired migration artifact. Its first
// decision selects [1 2], the replacement [1 2 3], on the same resource and
// database under the same policy. A different plan UID alone proves none of
// those bindings.
func changedMigrationArtifactPlan(old, current *ptahv1alpha1.PtahMigrationPlan, oldDigest, currentDigest string) error {
	if old == nil || current == nil || old.UID == "" || current.UID == "" || current.UID == old.UID {
		return errors.New("the plans have no distinct immutable identities")
	}
	if !sha256Pattern.MatchString(oldDigest) || !sha256Pattern.MatchString(currentDigest) || oldDigest == currentDigest ||
		old.Spec.ArtifactDigest != oldDigest || current.Spec.ArtifactDigest != currentDigest {
		return errors.New("the plans do not bind the original and replacement artifact digests")
	}
	if old.Spec.MigrationRef.Name == "" || old.Spec.MigrationRef.UID == "" || current.Spec.MigrationRef != old.Spec.MigrationRef ||
		!sha256Pattern.MatchString(old.Spec.TargetIdentityDigest) || current.Spec.TargetIdentityDigest != old.Spec.TargetIdentityDigest ||
		!sha256Pattern.MatchString(old.Spec.CoordinationDigest) || current.Spec.CoordinationDigest != old.Spec.CoordinationDigest ||
		old.Spec.PolicyFingerprint == "" || current.Spec.PolicyFingerprint != old.Spec.PolicyFingerprint {
		return errors.New("the artifact edit changed the resource, target, coordination or policy binding")
	}
	if old.Spec.Fingerprint == "" || current.Spec.Fingerprint == "" || current.Spec.Fingerprint == old.Spec.Fingerprint ||
		planVersionList(old) != "1 2" || planVersionList(current) != "1 2 3" {
		return errors.New("the new fingerprint does not carry the replacement sequence")
	}
	return nil
}

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
