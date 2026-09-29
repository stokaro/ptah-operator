package e2e

import (
	"errors"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Hold the substitution to a new UID alone. An unrelated policy, artifact,
// target or history change would not prove the same-name identity boundary.
func replacedMigrationDecision(old, current *ptahv1alpha1.PtahMigration, oldPlan, currentPlan *ptahv1alpha1.PtahMigrationPlan) error {
	if old == nil || current == nil || old.Name == "" || old.Namespace == "" || old.UID == "" || current.UID == "" ||
		old.UID == current.UID || old.Name != current.Name || old.Namespace != current.Namespace || !reflect.DeepEqual(old.Spec, current.Spec) {
		return errors.New("migration replacement did not preserve its name, namespace and spec with a new UID")
	}
	if oldPlan == nil || currentPlan == nil || oldPlan.UID == "" || currentPlan.UID == "" || oldPlan.UID == currentPlan.UID ||
		oldPlan.Namespace != old.Namespace || currentPlan.Namespace != current.Namespace ||
		oldPlan.Spec.MigrationRef != (ptahv1alpha1.ImmutableObjectReference{Name: old.Name, UID: old.UID}) ||
		currentPlan.Spec.MigrationRef != (ptahv1alpha1.ImmutableObjectReference{Name: current.Name, UID: current.UID}) ||
		old.Status.Plan == nil || old.Status.Plan.Name != oldPlan.Name || old.Status.Plan.UID != oldPlan.UID ||
		!changedMigrationApprovalRefused(current, oldPlan.UID, current.Generation, false) || current.Status.Plan.Name != currentPlan.Name || current.Status.Plan.UID != currentPlan.UID {
		return errors.New("replacement did not reach its own exact approval gate")
	}
	if oldPlan.Spec.Fingerprint == "" || currentPlan.Spec.Fingerprint == "" || oldPlan.Spec.Fingerprint == currentPlan.Spec.Fingerprint ||
		oldPlan.Spec.ArtifactDigest == "" || oldPlan.Spec.ArtifactDigest != currentPlan.Spec.ArtifactDigest ||
		oldPlan.Spec.TargetIdentityDigest == "" || oldPlan.Spec.TargetIdentityDigest != currentPlan.Spec.TargetIdentityDigest ||
		oldPlan.Spec.CoordinationDigest == "" || oldPlan.Spec.CoordinationDigest != currentPlan.Spec.CoordinationDigest ||
		oldPlan.Spec.PolicyFingerprint == "" || oldPlan.Spec.PolicyFingerprint != currentPlan.Spec.PolicyFingerprint ||
		oldPlan.Spec.HistoryFingerprint == "" || oldPlan.Spec.HistoryFingerprint != currentPlan.Spec.HistoryFingerprint ||
		len(oldPlan.Spec.Migrations) == 0 || !reflect.DeepEqual(oldPlan.Spec.Migrations, currentPlan.Spec.Migrations) {
		return errors.New("replacement changed the planned work or reused the old fingerprint")
	}
	return nil
}

func migrationIdentityApprovalRefusal(err error, reason string) bool {
	return reason != "" && apierrors.IsForbidden(err) &&
		strings.Contains(err.Error(), `admission webhook "mmigrationapproval.operator.ptah.run" denied the request: `+reason)
}

// Both resource lifetimes must produce observed History SQL. This prevents
// an empty replacement inventory from making the old resource its control.
func migrationReplacementSQLControls(clients map[string]migrationSQLClient, counts map[string]int, oldUID, currentUID string) error {
	if oldUID == "" || currentUID == "" || oldUID == currentUID {
		return errors.New("SQL controls have no distinct migration identities")
	}
	seen := map[string]bool{}
	for host, actor := range clients {
		if actor.migrationUID != oldUID && actor.migrationUID != currentUID {
			return errors.New("SQL control belongs to an undeclared migration")
		}
		if counts[host] > 0 && actor.operation == "history" && actor.jobUID != "" && actor.podUID != "" {
			seen[actor.migrationUID] = true
		}
	}
	if !seen[oldUID] || !seen[currentUID] {
		return errors.New("SQL audit did not observe History controls from both migration identities")
	}
	return nil
}
