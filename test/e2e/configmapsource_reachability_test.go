package e2e

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

const reachabilityGeneration = int64(3)

func reachabilityCondition(status metav1.ConditionStatus, reason ptahv1alpha1.ConditionReason, message string) metav1.Condition {
	return metav1.Condition{
		Type: ptahv1alpha1.ConditionDatabaseReachable, Status: status, Reason: string(reason), Message: message,
		ObservedGeneration: reachabilityGeneration,
	}
}

// failedWith is the sentence on the condition each family writes it on, which
// the row must not accept beside a True.
func failedWith(conditionType, message string) metav1.Condition {
	return metav1.Condition{
		Type: conditionType, Status: metav1.ConditionTrue, Reason: string(ptahv1alpha1.ReasonOperationFailed),
		Message: message, ObservedGeneration: reachabilityGeneration,
	}
}

func TestTargetRefusedCredentialsRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	url := reachabilityURLKey(reachabilitySchemaSecret)
	sentence := targetfailure.Message(dataplane.TargetAuthenticationFailed, url)
	refused := reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonAuthenticationFailed, sentence)
	if !targetRefusedCredentials([]metav1.Condition{refused, failedWith(ptahv1alpha1.ConditionReconciliationFailed, sentence)}, reachabilityGeneration, url) {
		t.Fatal("the reading the row waits for was refused")
	}
	for name, conditions := range map[string][]metav1.Condition{
		"DatabaseReachable=True, reason Observed, next to the authentication_failed sentence": {
			reachabilityCondition(metav1.ConditionTrue, ptahv1alpha1.ReasonObserved, "Database schema was observed"),
			failedWith(ptahv1alpha1.ConditionReconciliationFailed, sentence),
		},
		"a schema that never reached its database": {
			failedWith(ptahv1alpha1.ConditionReconciliationFailed, sentence),
		},
		"no conditions at all": nil,
		"another kind's reason": {
			reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonDatabaseUnreachable, sentence),
		},
		"the reason with another kind's sentence": {
			reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonAuthenticationFailed,
				targetfailure.Message(dataplane.TargetDatabaseUnreachable, url)),
		},
		"the sentence for another Secret": {
			reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonAuthenticationFailed,
				targetfailure.Message(dataplane.TargetAuthenticationFailed, reachabilityURLKey(reachabilityMigrationSecret))),
		},
		"the generic failure reason": {
			reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonOperationFailed, sentence),
		},
		"Unknown under a retired binding": {
			reachabilityCondition(metav1.ConditionUnknown, ptahv1alpha1.ReasonExecutionBindingChanged, "historical"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if targetRefusedCredentials(conditions, reachabilityGeneration, url) {
				t.Errorf("a reading of %s was accepted", name)
			}
		})
	}
	t.Run("a reading of an older generation", func(t *testing.T) {
		t.Parallel()
		if targetRefusedCredentials([]metav1.Condition{refused}, reachabilityGeneration+1, url) {
			t.Error("a reading of an older generation was accepted")
		}
	})
}

func TestTargetAnsweredAgainRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	sentence := targetfailure.Message(dataplane.TargetAuthenticationFailed, reachabilityURLKey(reachabilitySchemaSecret))
	reached := reachabilityCondition(metav1.ConditionTrue, ptahv1alpha1.ReasonObserved, "Database schema was observed")
	settled := metav1.Condition{
		Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionFalse,
		Reason: string(ptahv1alpha1.ReasonSucceeded), Message: "The latest operation completed",
	}
	if !targetAnsweredAgain([]metav1.Condition{reached, settled}, reachabilityGeneration) {
		t.Fatal("the reading the row waits for was refused")
	}
	for name, conditions := range map[string][]metav1.Condition{
		"DatabaseReachable=True, reason Observed, next to the authentication_failed sentence on a schema": {
			reached, failedWith(ptahv1alpha1.ConditionReconciliationFailed, sentence),
		},
		"DatabaseReachable=True, reason Observed, next to the authentication_failed sentence on a migration": {
			reached, failedWith(ptahv1alpha1.ConditionMigrationProgressing, sentence),
		},
		"the refusal the row recovers from": {
			reachabilityCondition(metav1.ConditionFalse, ptahv1alpha1.ReasonAuthenticationFailed, sentence),
		},
		"no DatabaseReachable": {settled},
		"Unknown": {
			reachabilityCondition(metav1.ConditionUnknown, ptahv1alpha1.ReasonExecutionBindingChanged, "historical"),
		},
		"True for another reason": {
			reachabilityCondition(metav1.ConditionTrue, ptahv1alpha1.ReasonSucceeded, "the latest operation completed"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if targetAnsweredAgain(conditions, reachabilityGeneration) {
				t.Errorf("a reading of %s was accepted", name)
			}
		})
	}
	t.Run("a reading of an older generation", func(t *testing.T) {
		t.Parallel()
		if targetAnsweredAgain([]metav1.Condition{reached}, reachabilityGeneration+1) {
			t.Error("a reading of an older generation was accepted")
		}
	})
}

// The row writes a password no other row may meet, so its databases and Secrets
// are nobody else's.
func TestReachabilityFixturesAreTheirOwn(t *testing.T) {
	t.Parallel()
	for _, database := range []string{reachabilitySchemaDatabase, reachabilityMigrationDatabase} {
		for _, other := range []string{pgDatabase, configMapSchemaDatabase, configMapMigrationDatabase} {
			if database == other {
				t.Errorf("the row shares database %s with another row", database)
			}
		}
	}
	for _, secret := range []string{reachabilitySchemaSecret, reachabilityMigrationSecret} {
		for _, other := range []string{pgSecret, configMapSchemaSecret, configMapMigrationSecret} {
			if secret == other {
				t.Errorf("the row shares Secret %s with another row", secret)
			}
		}
	}
}
