package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings the uncertain-Apply, late-dispatch and restored-history rows
// hold a migration to. Each is the jq the shell phase ran, on typed objects,
// with jq's null handling kept: an absent field equals no string, and a
// condition list that is absent cannot be iterated, so a claim over it fails.

// uncertainApplyClaimed is a migration whose active operation is an Apply
// bound to its own Job, by name and by UID, and the two it names.
func uncertainApplyClaimed(migration *ptahv1alpha1.PtahMigration) (jobName, jobUID string, ok bool) {
	active := migration.Status.ActiveOperation
	if active == nil || active.Type != ptahv1alpha1.MigrationOperationApply || active.JobName == "" || active.JobUID == "" {
		return "", "", false
	}
	return active.JobName, string(active.JobUID), true
}

// uncertainRunRefused is the refusal a run whose evidence nobody could read
// leaves: Blocked on an Unknown last run, with no operation and no plan, the
// Blocked condition naming ApplyOutcomeUnknown, and nothing Ready.
func uncertainRunRefused(status ptahv1alpha1.PtahMigrationStatus) error {
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseBlocked:
		return fmt.Errorf("it is %s, not Blocked", status.Phase)
	case status.LastRun == nil || status.LastRun.Outcome != ptahv1alpha1.MigrationRunOutcomeUnknown:
		return errors.New("its last run is not Unknown")
	case status.ActiveOperation != nil:
		return errors.New("it still carries an operation")
	case status.Plan != nil:
		return errors.New("it published a plan")
	case status.Conditions == nil:
		return errors.New("it carries no conditions")
	case !conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "ApplyOutcomeUnknown"):
		return fmt.Errorf("it is not Blocked by ApplyOutcomeUnknown: %v", conditionSummary(status.Conditions))
	case conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue):
		return errors.New("it is Ready")
	}
	return nil
}

// unresolvedRunRecorded is the record of the run nobody established the
// effect of: Unknown, naming the Job by name and UID, the target it ran
// against and when it was recorded. The UID is what separates a record of
// this run from a record of some run.
func unresolvedRunRecorded(status ptahv1alpha1.PtahMigrationStatus, jobName, jobUID string) error {
	run := status.UnresolvedRun
	switch {
	case run == nil:
		return errors.New("it records no unresolved run")
	case run.Outcome != ptahv1alpha1.MigrationRunOutcomeUnknown:
		return fmt.Errorf("its unresolved run is %s, not Unknown", run.Outcome)
	case !presentIs(run.JobName, jobName) || !presentIs(string(run.JobUID), jobUID):
		return fmt.Errorf("its unresolved run names Job %q UID %q, not %s UID %s", run.JobName, run.JobUID, jobName, jobUID)
	case !sha256Pattern.MatchString(run.TargetIdentityDigest):
		return errors.New("its unresolved run names no target identity")
	case run.RecordedAt.IsZero():
		return errors.New("its unresolved run carries no time it was recorded")
	}
	return nil
}

// realmConflictBlocked is a migration whose Blocked condition is now the
// realm conflict's rather than the run's.
func realmConflictBlocked(status ptahv1alpha1.PtahMigrationStatus) bool {
	return conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "RealmConflict")
}

// unresolvedRunCopied is the copy of the record a restore that drops status
// keeps, on the resource's metadata, naming the same run the same way: the
// operation, the outcome and the target identity. A copy that is not JSON
// reads as an empty object, as jq's fromjson? // {} made it, and one that is
// JSON but not an object cannot be read at all.
func unresolvedRunCopied(migration *ptahv1alpha1.PtahMigration) bool {
	copied := map[string]any{}
	if value := migration.Annotations[ptahv1alpha1.UnresolvedRunAnnotation]; value != "" {
		var parsed any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			switch document := parsed.(type) {
			case map[string]any:
				copied = document
			case nil, bool:
				if document == true {
					return false
				}
			default:
				return false
			}
		}
	}
	record := map[string]any{}
	if migration.Status.UnresolvedRun != nil {
		value, err := jsonValue(migration.Status.UnresolvedRun)
		if err != nil {
			return false
		}
		record, _ = value.(map[string]any)
	}
	for _, field := range []string{"operationID", "outcome", "targetIdentityDigest"} {
		if !sameJSON(copied[field], record[field]) {
			return false
		}
	}
	return true
}

// unresolvedRunStands is the record of the operation still in status, and a
// copy of it still on the metadata.
func unresolvedRunStands(migration *ptahv1alpha1.PtahMigration, operationID string) bool {
	run := migration.Status.UnresolvedRun
	return run != nil && run.OperationID == operationID && migration.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] != ""
}

// runAcknowledged is a run
// nobody accounted for, settled in the name of the person whose
// acknowledgment named it. The record is gone from status and from the copy a
// restore would keep; the resolution names the run the record named, says a
// person settled it, and names the acknowledgment and the identity admission
// stamped on it. A reading of the database that settled the run names no
// person, and is not this.
func runAcknowledged(migration *ptahv1alpha1.PtahMigration, operationID, acknowledgment, person string) bool {
	if migration.Status.UnresolvedRun != nil {
		return false
	}
	if _, copied := migration.Annotations[ptahv1alpha1.UnresolvedRunAnnotation]; copied {
		return false
	}
	resolved := migration.Status.ResolvedRun
	return resolved != nil && resolved.OperationID == operationID &&
		resolved.Resolution == ptahv1alpha1.MigrationRunResolvedByAcknowledgment &&
		resolved.AcknowledgmentRef != nil && resolved.AcknowledgmentRef.Name == acknowledgment &&
		resolved.AcknowledgedBy != nil && resolved.AcknowledgedBy.Username == person
}

// acknowledgmentNamesResolution is an acknowledgment stamped from the request
// that created it, and named by UID in the resolution: the resolution is this
// acknowledgment's, not one like it.
func acknowledgmentNamesResolution(acknowledgment *ptahv1alpha1.PtahMigrationRunAcknowledgment,
	migration *ptahv1alpha1.PtahMigration, person, group string,
) bool {
	stamped := acknowledgment.Spec.AcknowledgedBy
	resolved := migration.Status.ResolvedRun
	return stamped.Username == person && slices.Contains(stamped.Groups, group) &&
		resolved != nil && resolved.AcknowledgmentRef != nil && acknowledgment.UID != "" &&
		resolved.AcknowledgmentRef.UID == acknowledgment.UID &&
		resolved.AcknowledgedBy != nil && resolved.AcknowledgedBy.Username == stamped.Username
}

// acknowledgmentConsumed is an acknowledgment the controller answered as
// consumed.
func acknowledgmentConsumed(acknowledgment *ptahv1alpha1.PtahMigrationRunAcknowledgment) bool {
	return conditionStatus(acknowledgment.Status.Conditions, ptahv1alpha1.ConditionAcknowledgmentConsumed, metav1.ConditionTrue)
}

// historyReadAfter is a migration that read its database after the instant
// given, to the second. A migration with no reading read it at the epoch.
func historyReadAfter(migration *ptahv1alpha1.PtahMigration, after time.Time) bool {
	observed := time.Unix(0, 0)
	if history := migration.Status.History; history != nil && !history.ObservedAt.IsZero() {
		observed = history.ObservedAt.Time
	}
	return observed.Unix() > after.Unix()
}

// latePlanPublished is a migration waiting for a decision on a published
// plan, and the plan.
func latePlanPublished(migration *ptahv1alpha1.PtahMigration) (string, bool) {
	status := migration.Status
	if status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval || status.Plan == nil || status.Plan.Name == "" {
		return "", false
	}
	return status.Plan.Name, true
}

// lateApplyClaim is an Apply claimed with its own Job and the absolute window
// it has to start in, as the resource persisted it beside the claim.
type lateApplyClaim struct {
	jobName, jobUID  string
	dispatchNotAfter time.Time
}

// lateApplyClaimed reads the claim, when the migration made one with a
// window.
func lateApplyClaimed(migration *ptahv1alpha1.PtahMigration) (lateApplyClaim, bool) {
	jobName, jobUID, ok := uncertainApplyClaimed(migration)
	active := migration.Status.ActiveOperation
	if !ok || active.DispatchNotAfter == nil || active.DispatchNotAfter.IsZero() {
		return lateApplyClaim{}, false
	}
	return lateApplyClaim{jobName: jobName, jobUID: jobUID, dispatchNotAfter: active.DispatchNotAfter.Time}, true
}

// anyPodPlaced is a Pod list with a Pod that reached a node, which waiting
// longer for a gate to hold it does not fix.
func anyPodPlaced(pods []corev1.Pod) bool {
	return slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return pod.Spec.NodeName != "" })
}

// dispatchDeadlineExpired is the runner's own word for an Apply that started
// after its window closed.
var dispatchDeadlineExpired = regexp.MustCompile(`dispatch_deadline_expired`)

// lateRefusalRecorded is the runner's refusal of an Apply that started after
// its window closed, as the resource records it: the unresolved run of that
// Job, and a Blocked condition by ApplyOutcomeUnknown whose message names the
// expired dispatch window. Blocked alone would be satisfied by a realm
// conflict, a dirty history or a Job that merely failed.
func lateRefusalRecorded(status ptahv1alpha1.PtahMigrationStatus, jobUID string) bool {
	run := status.UnresolvedRun
	if run == nil || !presentIs(string(run.JobUID), jobUID) {
		return false
	}
	return slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionMigrationBlocked && condition.Status == metav1.ConditionTrue &&
			condition.Reason == "ApplyOutcomeUnknown" && dispatchDeadlineExpired.MatchString(condition.Message)
	})
}

// restoreInSyncApplied is a migration in sync after an Applied run.
func restoreInSyncApplied(status ptahv1alpha1.PtahMigrationStatus) bool {
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && status.LastRun != nil &&
		status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied
}

// restoredHistoryApplied cannot be satisfied by the initial [1 2] run or the
// refused [3] run. The new decision must finish exactly the restored [2 3]
// selection, with no work or unresolved evidence left in flight.
func restoredHistoryApplied(resource *ptahv1alpha1.PtahMigration, initialJobUID, refusedJobUID string) bool {
	if resource == nil || resource.Generation < 1 || initialJobUID == "" || refusedJobUID == "" || initialJobUID == refusedJobUID {
		return false
	}
	status := resource.Status
	if !restoreInSyncApplied(status) || status.ObservedGeneration != resource.Generation ||
		status.ActiveOperation != nil || status.UnresolvedRun != nil {
		return false
	}
	run := status.LastRun
	return run.JobName != "" && run.JobUID != "" && string(run.JobUID) != initialJobUID && string(run.JobUID) != refusedJobUID &&
		run.FinishedAt != nil && !run.FinishedAt.IsZero() && len(run.AppliedVersions) == 2 &&
		slices.Contains(run.AppliedVersions, int64(2)) && slices.Contains(run.AppliedVersions, int64(3))
}

// decisionOnNewPlan is a migration asking for a decision on a plan other than
// the previous one, and the plan. The condition is what is asserted: the phase
// moves on every read the resource makes while it waits.
func decisionOnNewPlan(status ptahv1alpha1.PtahMigrationStatus, previous string) (string, bool) {
	if status.Plan == nil || status.Plan.Name == "" || status.Plan.Name == previous ||
		!conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "AwaitingApproval") {
		return "", false
	}
	return status.Plan.Name, true
}

// planVersionList is the migrations a plan approves, in order, as "2 3".
func planVersionList(plan *ptahv1alpha1.PtahMigrationPlan) string {
	versions := make([]string, 0, len(plan.Spec.Migrations))
	for _, planned := range plan.Spec.Migrations {
		versions = append(versions, strconv.FormatInt(planned.Version, 10))
	}
	return strings.Join(versions, " ")
}

// restoreSelectionRefusal opens the message of a run refused because Ptah
// selected, under the migration lock, more than the plan approved.
const restoreSelectionRefusal = "Ptah selected [2 3] under the migration lock and the plan approved [3], so it ran nothing"

// restoreRefused is the approved run of the Job named, refused by that
// selection and recorded as Failed. A first migration failing also reads as
// Failed with nothing applied; only the refusal names both lists.
func restoreRefused(status ptahv1alpha1.PtahMigrationStatus, jobUID string) bool {
	run := status.LastRun
	return run != nil && presentIs(string(run.JobUID), jobUID) && run.Outcome == ptahv1alpha1.MigrationRunOutcomeFailed &&
		strings.HasPrefix(run.Message, restoreSelectionRefusal)
}

// restoreRevisionsQuery lists every revision row a database holds as "1,2".
// The value helper strips whitespace, which would run the rows together, so
// the engine joins them. The state is not filtered: a row a refused run left
// behind in any state is a row it wrote.
func restoreRevisionsQuery(engine string) string {
	if engine == "mysql" {
		return "SELECT COALESCE(GROUP_CONCAT(version ORDER BY version SEPARATOR ','), '') FROM schema_migrations"
	}
	return "SELECT COALESCE(string_agg(version::text, ',' ORDER BY version), '') FROM schema_migrations"
}

// lateDispatchReport is what a late-dispatch or restore wait prints when it
// runs out: the phase, the operation, the plan and the conditions, the Jobs'
// counts and conditions, the Pods' nodes and unmet conditions, and the nodes
// the gate is open on. None of it is a row or a statement.
func lateDispatchReport(migration *ptahv1alpha1.PtahMigration, jobs []batchv1.Job, pods []corev1.Pod, gateOpen []string) map[string]any {
	report := map[string]any{"gateOpenOn": gateOpen}
	if migration != nil {
		status := migration.Status
		conditions := make([]map[string]string, 0, len(status.Conditions))
		for _, condition := range status.Conditions {
			message := condition.Message
			if len(message) > 160 {
				message = message[:160]
			}
			conditions = append(conditions, map[string]string{
				"type": condition.Type, "status": string(condition.Status), "reason": condition.Reason, "message": message,
			})
		}
		operation := map[string]any{}
		if active := status.ActiveOperation; active != nil {
			operation = map[string]any{
				"type": active.Type, "jobName": active.JobName, "attempt": active.Attempt,
				"dispatchNotAfter": active.DispatchNotAfter,
			}
		}
		run := map[string]any{}
		if last := status.LastRun; last != nil {
			run = map[string]any{"outcome": last.Outcome, "message": last.Message}
		}
		plan := ""
		if status.Plan != nil {
			plan = status.Plan.Name
		}
		report["migration"] = map[string]any{
			"phase": status.Phase, "observedGeneration": status.ObservedGeneration, "activeOperation": operation,
			"plan": plan, "lastRun": run, "unresolvedRun": status.UnresolvedRun != nil, "conditions": conditions,
		}
	}
	jobReports := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		var conditions []string
		for _, condition := range job.Status.Conditions {
			conditions = append(conditions, fmt.Sprintf("%s=%s(%s)", condition.Type, condition.Status, condition.Reason))
		}
		jobReports = append(jobReports, map[string]any{
			"name": job.Name, "operation": job.Labels[labelOperation], "suspend": job.Spec.Suspend,
			"active": job.Status.Active, "failed": job.Status.Failed, "succeeded": job.Status.Succeeded,
			"startTime": job.Status.StartTime, "conditions": conditions,
		})
	}
	report["jobs"] = jobReports
	podReports := make([]map[string]any, 0, len(pods))
	for _, pod := range pods {
		var reasons []string
		for _, condition := range pod.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				reasons = append(reasons, fmt.Sprintf("%s:%s", condition.Type, condition.Reason))
			}
		}
		podReports = append(podReports, map[string]any{
			"name": pod.Name, "phase": pod.Status.Phase, "node": pod.Spec.NodeName, "reasons": reasons,
		})
	}
	report["pods"] = podReports
	return report
}
