package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings the realm, partial-run, older-artifact, modified-file and
// out-of-order rows hold a migration and its rival to. Where a jq filter read
// a condition list with any(.status.conditions[]; ...) it could not iterate an
// absent list, and a clause that negated such an any() made the whole reading
// fail on one; the functions below refuse a status without conditions there.

// partialRunRecorded is what
// a run that committed half of itself left in the record. The outcome, and
// that the run claimed no version as applied. Both are facts about a run that
// is over, so they hold whatever the resource is doing when it is read. The
// phase and the active operation are deliberately absent: a resource that has
// stopped goes on resolving and reading at its interval, so Blocked and no
// active operation are true only between cycles.
func partialRunRecorded(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.LastRun != nil && status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomePartial &&
		len(status.LastRun.AppliedVersions) == 0
}

// dirtyReading is the reading a
// partially applied migration leaves behind. The history says a revision is
// dirty, it says which version the run stopped at, and the refusal names that
// reading. The phase is absent, since the status that carries this reading is
// as likely to say Resolving as Blocked, and so is the pending count: Ptah
// counts a revision not recorded applied as pending, so the number answers its
// bookkeeping rather than anything the proof claims.
func dirtyReading(status ptahv1alpha1.PtahMigrationStatus, stoppedAt int64) bool {
	history := status.History
	return history != nil && history.Dirty && history.CurrentVersion == stoppedAt &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryDirty")
}

// historyAhead is a database that has
// run more than the artifact carries. Nothing is pending here, and that is the
// trap: a revision the artifact does not carry is in no state at all, so a
// proof that counted only pending work would read this as success. Both
// numbers that disagree are asserted, because either one alone is satisfied
// by an ordinary settled database.
func historyAhead(status ptahv1alpha1.PtahMigrationStatus, digest string, databaseAt int64, artifactCovers int32) bool {
	history := status.History
	return status.Artifact != nil && status.Artifact.Digest == digest && history != nil &&
		history.CurrentVersion == databaseAt && history.AppliedCount == artifactCovers && history.PendingCount == 0 &&
		!history.Dirty && len(history.ModifiedVersions) == 0 && status.Plan == nil && status.ActiveOperation == nil &&
		status.Conditions != nil &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue)
}

// realmNotAuthorized is a schema refused for naming a realm that does not
// list its namespace.
func realmNotAuthorized(status ptahv1alpha1.PtahSchemaStatus) bool {
	return conditionIs(status.Conditions, ptahv1alpha1.ConditionReady, metav1.ConditionFalse, "RealmNotAuthorized")
}

// conditionMessagesAvoid is a condition list none of whose messages names any
// of the fragments: a refusal that told one claimant where the other lives
// would be the census leaking what it exists to count.
func conditionMessagesAvoid(conditions []metav1.Condition, fragments ...string) bool {
	for _, condition := range conditions {
		for _, fragment := range fragments {
			if strings.Contains(condition.Message, fragment) {
				return false
			}
		}
	}
	return true
}

// rivalRefusedWhole is the rival refused as a whole -- nothing claimed,
// nothing approvable -- and told nothing about the claimant the realm does
// admit.
func rivalRefusedWhole(status ptahv1alpha1.PtahSchemaStatus, namespace, name string) bool {
	return status.Phase == ptahv1alpha1.PhaseBlocked && status.ActiveOperation == nil &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "RealmNotAuthorized") &&
		conditionMessagesAvoid(status.Conditions, namespace, name)
}

// readyTransition is the transition time of the first Ready condition, which
// dates a refusal by the object's own clock rather than by the poll that
// noticed it.
func readyTransition(conditions []metav1.Condition) (time.Time, bool) {
	for _, condition := range conditions {
		if condition.Type == ptahv1alpha1.ConditionReady {
			return condition.LastTransitionTime.Time, !condition.LastTransitionTime.IsZero()
		}
	}
	return time.Time{}, false
}

// migrationKeptRunning is the migration after a new generation it finished
// while the rival stood refused: a reading of its database dated no earlier
// than the refusal, on the three applied migrations, Ready and not blocked.
func migrationKeptRunning(status ptahv1alpha1.PtahMigrationStatus, refusedAt time.Time) bool {
	history := status.History
	return history != nil && !history.ObservedAt.Time.Before(refusedAt) && history.CurrentVersion == 3 &&
		history.PendingCount == 0 &&
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue) &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue)
}

// rivalStillRefused is the rival, after the migration ran, still refused for
// the realm and idle.
func rivalStillRefused(status ptahv1alpha1.PtahSchemaStatus) bool {
	return status.ActiveOperation == nil && slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionReady && condition.Reason == "RealmNotAuthorized"
	})
}

// migrationRealmConflict is the migration blocked for the realm's conflict.
func migrationRealmConflict(status ptahv1alpha1.PtahMigrationStatus) bool {
	return conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "RealmConflict")
}

// rivalRealmConflict is the rival not ready for the realm's conflict.
func rivalRealmConflict(status ptahv1alpha1.PtahSchemaStatus) bool {
	return slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionReady && condition.Reason == "RealmConflict"
	})
}

// migrationConflictHeld is the migration stopped as a whole by the conflict:
// blocked and idle, and not Ready.
func migrationConflictHeld(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Phase == ptahv1alpha1.MigrationPhaseBlocked && status.ActiveOperation == nil &&
		migrationRealmConflict(status) && status.Conditions != nil &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue)
}

// rivalConflictHeld is the rival refused for the conflict, idle, and told
// nothing about the claimant it conflicts with.
func rivalConflictHeld(status ptahv1alpha1.PtahSchemaStatus, namespace, name string) bool {
	return slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
		return condition.Reason == "RealmConflict"
	}) && status.ActiveOperation == nil && conditionMessagesAvoid(status.Conditions, namespace, name)
}

// partialPlanned is the migration planning the fourth migration alone, on the
// three it applied.
func partialPlanned(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.History != nil && status.History.PendingCount == 1 && status.History.CurrentVersion == 3
}

// blockedMessageMatches is a Blocked condition whose message says what match
// wants.
func blockedMessageMatches(conditions []metav1.Condition, match func(string) bool) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionMigrationBlocked && match(condition.Message)
	})
}

var anyDigit = regexp.MustCompile(`[0-9]`)

// namesARevision is a refusal message that names the revision a person has to
// decide about.
func namesARevision(message string) bool { return anyDigit.MatchString(message) }

// namesBothVersions is a refusal message that names the database's version
// and the artifact's, because only one of them is a field.
func namesBothVersions(message string) bool {
	return strings.Contains(message, "3") && strings.Contains(message, "2")
}

// recoveredAfterPartial is the migration after a person undid the half and
// put the sequence back: InSync on the three migrations, nothing dirty,
// pending or planned, the partial run still its last, Ready by HistoryMatched
// and not blocked.
func recoveredAfterPartial(status ptahv1alpha1.PtahMigrationStatus) bool {
	history := status.History
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && history != nil && history.CurrentVersion == 3 &&
		history.AppliedCount == 3 && history.PendingCount == 0 && !history.Dirty && status.Plan == nil &&
		status.LastRun != nil && status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomePartial &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched") &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue)
}

// historyAheadBlocked is the older-artifact refusal the wait looks for.
func historyAheadBlocked(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Phase == ptahv1alpha1.MigrationPhaseBlocked &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryAhead")
}

// settledOnTheThree is the migration settled again once its artifact matches
// its database.
func settledOnTheThree(status ptahv1alpha1.PtahMigrationStatus) bool {
	history := status.History
	return history != nil && history.CurrentVersion == 3 && history.AppliedCount == 3 && history.PendingCount == 0 &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched")
}

// modifiedRefusal is the refusal of an artifact whose applied first migration
// was edited: the exact modified version named, nothing planned or running,
// blocked for it and not Ready.
func modifiedRefusal(status ptahv1alpha1.PtahMigrationStatus, digest string) bool {
	history := status.History
	return status.Artifact != nil && status.Artifact.Digest == digest && history != nil &&
		slices.Equal(history.ModifiedVersions, []int64{1}) && history.CurrentVersion == 3 &&
		status.Plan == nil && status.ActiveOperation == nil &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryModified") &&
		status.Conditions != nil &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue)
}

// outOfOrderBlocked is the out-of-order refusal the wait looks for.
func outOfOrderBlocked(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Phase == ptahv1alpha1.MigrationPhaseBlocked &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryOutOfOrder")
}

// outOfOrderRefusal is the refusal of a migration numbered below the applied
// version, made before planning: blocked and idle, the late version named, the
// database at 30, not Ready for it, and no plan key in the stored status.
func outOfOrderRefusal(status ptahv1alpha1.PtahMigrationStatus, storesPlan bool) bool {
	history := status.History
	return outOfOrderBlocked(status) && status.ActiveOperation == nil && history != nil &&
		slices.Equal(history.OutOfOrderVersions, []int64{20}) && history.CurrentVersion == 30 &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "HistoryOutOfOrder") &&
		!storesPlan
}

// rivalAuthorGrant is examples/desired-state-author-role.yaml as the realm row
// installs it: its Role and RoleBinding, in the rival's namespace, bound to
// the rival's author group. It is the example's rather than a Role written for
// the test, so the row measures the starting point a reader copies.
func rivalAuthorGrant(example []byte, namespace, group string) ([]map[string]any, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(example), 4096)
	var objects []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the desired-state author example could not be read: %w", err)
		}
		if document == nil {
			continue
		}
		if document["kind"] == "List" {
			items, _ := document["items"].([]any)
			for _, item := range items {
				object, ok := item.(map[string]any)
				if !ok {
					return nil, errors.New("the desired-state author example lists a non-object")
				}
				objects = append(objects, object)
			}
			continue
		}
		objects = append(objects, document)
	}
	var kinds []string
	for _, object := range objects {
		kind, _ := object["kind"].(string)
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"Role", "RoleBinding"}) {
		return nil, errors.New("the author example is not a Role and a RoleBinding")
	}
	for index, object := range objects {
		object = runtime.DeepCopyJSON(object)
		metadata, _ := object["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			object["metadata"] = metadata
		}
		metadata["namespace"] = namespace
		if object["kind"] == "RoleBinding" {
			object["subjects"] = []any{map[string]any{
				"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": group,
			}}
		}
		objects[index] = object
	}
	return objects, nil
}
