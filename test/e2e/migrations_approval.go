package e2e

import (
	"errors"
	"fmt"
	"strings"

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

// approveWaitWillNotReach is the status kubectl ptah exits with when a wait ends
// short of its target: exitWillNotReach in cmd/kubectl-ptah/operate.go.
const approveWaitWillNotReach = 8

// approveWaitStoppedOnAnotherRun holds what kubectl ptah approve --wait wrote to
// stderr when the plan it approved went stale and another plan ran and was read
// before the wait read again: exit 8, the stop that names another plan's run, and
// both plans. Exit 0 is what #882 fixed: the replacement's run read as the run of
// the approved plan.
func approveWaitStoppedOnAnotherRun(code int, stderr, approved, ran string) error {
	if approved == "" || ran == "" || approved == ran {
		return errors.New("the approved plan and the plan that ran are not two named plans")
	}
	if code != approveWaitWillNotReach {
		return fmt.Errorf("exit status %d, want %d", code, approveWaitWillNotReach)
	}
	if !strings.Contains(stderr, " will not reach applied: OtherPlanApplied.\n") {
		return errors.New("stderr does not say the wait stopped on another plan's run (OtherPlanApplied)")
	}
	if !strings.Contains(stderr, fmt.Sprintf(" plan %s ran, not %s: ", ran, approved)) {
		return fmt.Errorf("stderr does not name plan %s as the one that ran in place of plan %s", ran, approved)
	}
	return nil
}
