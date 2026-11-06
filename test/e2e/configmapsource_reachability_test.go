package e2e

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

// reachabilityFamily is one resource of the row: the readings it printed, its
// Secret, and the condition that says its operation failed.
type reachabilityFamily struct {
	name               string
	refused, recovered string
	secret, otherKey   string
	failure            string
}

func reachabilityFamilies() []reachabilityFamily {
	return []reachabilityFamily{
		{
			name: "a schema", refused: "configmap-refused-schema.json", recovered: "configmap-recovered-schema.json",
			secret: reachabilitySchemaSecret, otherKey: reachabilityMigrationSecret,
			failure: ptahv1alpha1.ConditionReconciliationFailed,
		},
		{
			name: "a migration", refused: "configmap-refused-migration.json", recovered: "configmap-recovered-migration.json",
			secret: reachabilityMigrationSecret, otherKey: reachabilitySchemaSecret,
			failure: ptahv1alpha1.ConditionMigrationProgressing,
		},
	}
}

// readReachabilityReading is a reading the row printed on a real cluster.
func readReachabilityReading(t *testing.T, file string) ([]metav1.Condition, int64) {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var reading struct {
		Status struct {
			ObservedGeneration int64              `json:"observedGeneration"`
			Conditions         []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &reading); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if len(reading.Status.Conditions) == 0 || reading.Status.ObservedGeneration == 0 {
		t.Fatalf("%s holds no conditions or no generation, so nothing here reads it", file)
	}
	return reading.Status.Conditions, reading.Status.ObservedGeneration
}

// rewriteReachable changes DatabaseReachable the way a mistake would.
func rewriteReachable(t *testing.T, conditions []metav1.Condition, change func(*metav1.Condition)) {
	t.Helper()
	reachable := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionDatabaseReachable)
	if reachable == nil {
		t.Fatal("the reading has no DatabaseReachable to rewrite")
	}
	change(reachable)
}

// The accepted reading is what the row matched on Kubernetes 1.35.8. Each refused
// reading is that status with the one field the mistake would change, or the
// other reading the same resource printed.
func TestTargetRefusedCredentialsRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	for _, family := range reachabilityFamilies() {
		url := reachabilityURLKey(family.secret)
		sentence := targetfailure.Message(dataplane.TargetAuthenticationFailed, url)
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			conditions, generation := readReachabilityReading(t, family.refused)
			if !targetRefusedCredentials(conditions, generation, url) {
				t.Fatal("the reading of the refused credentials was refused")
			}
			failure := meta.FindStatusCondition(conditions, family.failure)
			if failure == nil || failure.Message != sentence || failure.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
				t.Fatalf("%s = %#v, and the mistakes below rest on it carrying the sentence with OperationFailed", family.failure, failure)
			}
			recovered, recoveredGeneration := readReachabilityReading(t, family.recovered)
			if targetRefusedCredentials(recovered, recoveredGeneration, url) {
				t.Error("the reading taken after the Secret was corrected was accepted")
			}
			for name, mistake := range map[string]func(*metav1.Condition){
				"DatabaseReachable=True, reason Observed, next to the authentication_failed sentence": func(reachable *metav1.Condition) {
					reachable.Status, reachable.Reason, reachable.Message = metav1.ConditionTrue, string(ptahv1alpha1.ReasonObserved), "Database schema was observed"
				},
				"another kind's reason": func(reachable *metav1.Condition) {
					reachable.Reason = string(ptahv1alpha1.ReasonDatabaseUnreachable)
				},
				"the reason with another kind's sentence": func(reachable *metav1.Condition) {
					reachable.Message = targetfailure.Message(dataplane.TargetDatabaseUnreachable, url)
				},
				"the sentence for another Secret": func(reachable *metav1.Condition) {
					reachable.Message = targetfailure.Message(dataplane.TargetAuthenticationFailed, reachabilityURLKey(family.otherKey))
				},
				"the generic failure reason": func(reachable *metav1.Condition) {
					reachable.Reason = string(ptahv1alpha1.ReasonOperationFailed)
				},
				"Unknown under a retired binding": func(reachable *metav1.Condition) {
					reachable.Status, reachable.Reason = metav1.ConditionUnknown, string(ptahv1alpha1.ReasonExecutionBindingChanged)
				},
				"a reading of an older generation": func(reachable *metav1.Condition) {
					reachable.ObservedGeneration--
				},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					changed, generation := readReachabilityReading(t, family.refused)
					rewriteReachable(t, changed, mistake)
					if targetRefusedCredentials(changed, generation, url) {
						t.Errorf("a reading of %s was accepted", name)
					}
				})
			}
			t.Run("a resource that never wrote the condition", func(t *testing.T) {
				t.Parallel()
				changed, generation := readReachabilityReading(t, family.refused)
				meta.RemoveStatusCondition(&changed, ptahv1alpha1.ConditionDatabaseReachable)
				if targetRefusedCredentials(changed, generation, url) {
					t.Error("a reading with no DatabaseReachable was accepted")
				}
			})
		})
	}
}

func TestTargetAnsweredAgainRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	for _, family := range reachabilityFamilies() {
		url := reachabilityURLKey(family.secret)
		sentence := targetfailure.Message(dataplane.TargetAuthenticationFailed, url)
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			conditions, generation := readReachabilityReading(t, family.recovered)
			if !targetAnsweredAgain(conditions, generation) {
				t.Fatal("the reading after the Secret was corrected was refused")
			}
			refused, refusedGeneration := readReachabilityReading(t, family.refused)
			if targetAnsweredAgain(refused, refusedGeneration) {
				t.Error("the refusal the row recovers from was accepted")
			}
			was := meta.FindStatusCondition(refused, ptahv1alpha1.ConditionDatabaseReachable)
			now := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionDatabaseReachable)
			if !was.LastTransitionTime.Before(&now.LastTransitionTime) {
				t.Errorf("DatabaseReachable moved at %v and read False at %v, so the True was not written after the False",
					now.LastTransitionTime, was.LastTransitionTime)
			}
			t.Run("DatabaseReachable=True, reason Observed, next to the authentication_failed sentence", func(t *testing.T) {
				t.Parallel()
				left, generation := readReachabilityReading(t, family.refused)
				rewriteReachable(t, left, func(reachable *metav1.Condition) {
					reachable.Status, reachable.Reason, reachable.Message = metav1.ConditionTrue, string(ptahv1alpha1.ReasonObserved), "Database schema was observed"
				})
				if failure := meta.FindStatusCondition(left, family.failure); failure == nil || failure.Message != sentence {
					t.Fatalf("%s = %#v, want the sentence this row refuses a True beside", family.failure, failure)
				}
				if targetAnsweredAgain(left, generation) {
					t.Error("a True beside the sentence was accepted")
				}
			})
			for name, mistake := range map[string]func(*metav1.Condition){
				"Unknown under a retired binding": func(reachable *metav1.Condition) {
					reachable.Status, reachable.Reason = metav1.ConditionUnknown, string(ptahv1alpha1.ReasonExecutionBindingChanged)
				},
				"True for another reason": func(reachable *metav1.Condition) {
					reachable.Reason = string(ptahv1alpha1.ReasonSucceeded)
				},
				"Observed on a status that is not True": func(reachable *metav1.Condition) {
					reachable.Status = metav1.ConditionFalse
				},
				"a reading of an older generation": func(reachable *metav1.Condition) {
					reachable.ObservedGeneration--
				},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					changed, generation := readReachabilityReading(t, family.recovered)
					rewriteReachable(t, changed, mistake)
					if targetAnsweredAgain(changed, generation) {
						t.Errorf("a reading of %s was accepted", name)
					}
				})
			}
			t.Run("a resource that never wrote the condition", func(t *testing.T) {
				t.Parallel()
				changed, generation := readReachabilityReading(t, family.recovered)
				meta.RemoveStatusCondition(&changed, ptahv1alpha1.ConditionDatabaseReachable)
				if targetAnsweredAgain(changed, generation) {
					t.Error("a reading with no DatabaseReachable was accepted")
				}
			})
		})
	}
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
