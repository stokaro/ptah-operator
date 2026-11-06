package e2e

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

// The reachability row's fixtures: a schema and a migration, each on a database
// of its own on the data plane's server, whose URL first carries a password the
// server refuses.
const (
	reachabilitySchema            = "e2e-reachability-schema"
	reachabilityMigration         = "e2e-reachability-migration"
	reachabilitySchemaDatabase    = "ptah_e2e_reachability_schema"
	reachabilityMigrationDatabase = "ptah_e2e_reachability_migrations"
	reachabilitySchemaSource      = "e2e-reachability-schema-source"
	reachabilityMigrationSource   = "e2e-reachability-migration-source"
	reachabilitySchemaSecret      = "e2e-reachability-schema-database"
	reachabilityMigrationSecret   = "e2e-reachability-migration-database"
	reachabilityWrongPassword     = "e2e-not-the-password"
)

// reachabilityURLKey is the Secret key a resource of this row reads its URL
// from, which the sentence it reports has to name.
func reachabilityURLKey(secret string) corev1.SecretKeySelector {
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "url"}
}

// targetRefusedCredentials is a resource at generation whose last operation
// could not open its target because the target refused the credentials:
// DatabaseReachable False, with the authentication reason and the operator's
// sentence for the Secret key it read. The phase a failure passes through is not
// read, and neither are the conditions that say the operation failed.
func targetRefusedCredentials(conditions []metav1.Condition, generation int64, url corev1.SecretKeySelector) bool {
	reachable := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionDatabaseReachable)
	return reachable != nil && reachable.Status == metav1.ConditionFalse &&
		reachable.Reason == string(ptahv1alpha1.ReasonAuthenticationFailed) &&
		reachable.ObservedGeneration == generation &&
		reachable.Message == targetfailure.Message(dataplane.TargetAuthenticationFailed, url)
}

// targetAnsweredAgain is a resource at generation whose last operation opened
// its target: DatabaseReachable True with reason Observed, and no condition still
// saying the target refused it. A True beside that sentence is a condition that
// was never told about the failure.
func targetAnsweredAgain(conditions []metav1.Condition, generation int64) bool {
	reachable := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionDatabaseReachable)
	if reachable == nil || reachable.Status != metav1.ConditionTrue ||
		reachable.Reason != string(ptahv1alpha1.ReasonObserved) || reachable.ObservedGeneration != generation {
		return false
	}
	for _, condition := range conditions {
		if strings.HasPrefix(condition.Message, string(dataplane.TargetAuthenticationFailed)+":") {
			return false
		}
	}
	return true
}

// reachabilityReading is what a wait for this row says it saw.
func reachabilityReading(conditions []metav1.Condition) string {
	reachable := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionDatabaseReachable)
	if reachable == nil {
		return "no DatabaseReachable condition"
	}
	return fmt.Sprintf("DatabaseReachable=%s reason %s generation %d: %s",
		reachable.Status, reachable.Reason, reachable.ObservedGeneration, reachable.Message)
}
