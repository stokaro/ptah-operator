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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings the deletion, stop, lost-log, retry, suspension and
// release-fault rows of the migration phase hold a resource to. Each ports the
// jq a row of the migration script ran on the document it had just read.

// stoppedApplyWindowSeconds is how long the stopped row's Apply may run: long
// enough for the Pod to reach its second migration on a node that pulls
// nothing, and far shorter than that migration's sleep, so the only thing
// that ends the run is the window closing.
const stoppedApplyWindowSeconds = 90

// mfResultMarker opens the runner's result frame in its log, and
// mfSummaryMarker the summary it leaves in the termination message.
const (
	mfResultMarker  = "PTAH_RUNNER_RESULT_V1 "
	mfSummaryMarker = "PTAH_RUNNER_SUMMARY_V1 "
)

// mfDispatchedApply is an Apply the migration claimed and bound to its own Job:
// the claim names the Job and its UID. A Job found by label could be a later
// attempt under the same name.
func mfDispatchedApply(migration *ptahv1alpha1.PtahMigration) (jobName, jobUID string, ok bool) {
	active := migration.Status.ActiveOperation
	if active == nil || active.Type != ptahv1alpha1.MigrationOperationApply || active.JobName == "" || active.JobUID == "" {
		return "", "", false
	}
	return active.JobName, string(active.JobUID), true
}

// stopRowApply is mfDispatchedApply with the execution deadline the claim
// carries, which the stopped row dates the stop by.
func stopRowApply(migration *ptahv1alpha1.PtahMigration) (jobUID string, notAfter time.Time, ok bool) {
	_, uid, dispatched := mfDispatchedApply(migration)
	active := migration.Status.ActiveOperation
	if !dispatched || active.ExecutionNotAfter == nil || active.ExecutionNotAfter.IsZero() {
		return "", time.Time{}, false
	}
	return uid, active.ExecutionNotAfter.Time, true
}

// mfRunRecordedFor is a migration whose last run is the named Job's.
func mfRunRecordedFor(migration *ptahv1alpha1.PtahMigration, jobUID string) bool {
	run := migration.Status.LastRun
	return run != nil && jobUID != "" && string(run.JobUID) == jobUID
}

// stoppedRunRecorded is what a
// run stopped at its execution deadline left in the record. The run the claim
// dispatched, by its Job's UID; the outcome its report gave, Failed rather than
// Partial because the migration stopped is a single sleep that committed
// nothing; and every version below the one it stopped in as applied. A run
// with no report is Unknown with nothing applied, which is what every stopped
// run recorded before the runner passed SIGTERM on and read what Ptah wrote.
// The phase is left out on purpose: the reading after the run moves the
// resource on.
func stoppedRunRecorded(status ptahv1alpha1.PtahMigrationStatus, jobUID string, stoppedAt int64) bool {
	run := status.LastRun
	if run == nil || jobUID == "" || string(run.JobUID) != jobUID || run.Outcome != ptahv1alpha1.MigrationRunOutcomeFailed {
		return false
	}
	want := make([]int64, 0, max(stoppedAt-1, 0))
	for version := int64(1); version < stoppedAt; version++ {
		want = append(want, version)
	}
	return slices.Equal(run.AppliedVersions, want)
}

// lostLogRunRecorded is what
// a run whose log was lost left in the record once the termination summary
// stood in for its frame. The run the claim dispatched; Applied, the outcome
// the summary carried; and the message the controller writes only when it
// settles a run from a summary, naming the frame the summary was bound to and
// the versions it reported. A run settled from its frame says neither, and a
// run settled from nothing is Unknown, so a record that passes came from the
// termination message and nowhere else. stoppedAt is the last version of the
// sequence, all of which the run applied.
func lostLogRunRecorded(status ptahv1alpha1.PtahMigrationStatus, jobUID, digest string, stoppedAt int64) bool {
	run := status.LastRun
	if run == nil || jobUID == "" || string(run.JobUID) != jobUID || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied {
		return false
	}
	count := strconv.FormatInt(stoppedAt, 10)
	return strings.Contains(run.Message, "termination message, bound to frame "+digest+",") &&
		strings.Contains(run.Message, count+" migrations recorded applied, from version 1 to version "+count)
}

// mfRunnerFinishedAt is when the Pod's first ptah container that terminated
// finished, as the kubelet recorded it.
func mfRunnerFinishedAt(pod *corev1.Pod) (time.Time, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "ptah" && status.State.Terminated != nil && !status.State.Terminated.FinishedAt.IsZero() {
			return status.State.Terminated.FinishedAt.Time, true
		}
	}
	return time.Time{}, false
}

// runnerEndedInGrace is a runner the Pod's own record shows ended after its
// execution deadline and inside the grace the Pod had: the stop was the
// deadline, and the runner answered it rather than being killed. Both sides
// are whole seconds, as the API server records them.
func runnerEndedInGrace(pod *corev1.Pod, notAfter time.Time) bool {
	finished, ok := mfRunnerFinishedAt(pod)
	if !ok {
		return false
	}
	var grace int64
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		grace = *pod.Spec.TerminationGracePeriodSeconds
	}
	return finished.Unix() >= notAfter.Unix() && finished.Unix() < notAfter.Unix()+grace
}

// mfTerminationMessage is the message the kubelet kept for the Pod's ptah
// container once it terminated. An empty message is none: the API cannot tell
// it from a message that was never written.
func mfTerminationMessage(pod *corev1.Pod) (string, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "ptah" && status.State.Terminated != nil {
			return status.State.Terminated.Message, status.State.Terminated.Message != ""
		}
	}
	return "", false
}

// summaryFrameDigest reads the frame digest out of a runner's termination
// message: the JSON after the summary marker on the first line that carries
// one, as sed and jq -er read it.
func summaryFrameDigest(message string) (string, error) {
	for line := range strings.SplitSeq(message, "\n") {
		index := strings.LastIndex(line, mfSummaryMarker)
		if index < 0 {
			continue
		}
		var summary struct {
			FrameDigest *string `json:"frameDigest"`
		}
		if err := json.Unmarshal([]byte(line[index+len(mfSummaryMarker):]), &summary); err != nil {
			return "", fmt.Errorf("the summary does not parse: %w", err)
		}
		if summary.FrameDigest == nil {
			return "", errors.New("the summary names no frame digest")
		}
		return *summary.FrameDigest, nil
	}
	return "", errors.New("the termination message carries no runner summary")
}

// mfSessionCount reads a count a query printed; anything else counts as none,
// as a shell test of a non-number was false.
func mfSessionCount(output string) int {
	count, err := strconv.Atoi(output)
	if err != nil {
		return -1
	}
	return count
}

// deletionRetainsClaim is a deleted migration still holding the claim that
// accounts for its running Apply: marked for deletion, the operation
// finalizer on it, and the Apply still active.
func deletionRetainsClaim(migration *ptahv1alpha1.PtahMigration) bool {
	active := migration.Status.ActiveOperation
	return migration.DeletionTimestamp != nil && slices.Contains(migration.Finalizers, migrationFinalizer) &&
		active != nil && active.Type == ptahv1alpha1.MigrationOperationApply
}

// mfOnePodRunning is kubectl's jsonpath {.items[*].status.phase} reading exactly
// Running: one Pod, and running. No Pod, or two, reads as something else.
func mfOnePodRunning(pods []corev1.Pod) bool {
	return len(pods) == 1 && pods[0].Status.Phase == corev1.PodRunning
}

// suspensionRetainsClaim is a suspended migration still holding the claim of
// the Apply dispatched before the suspension.
func suspensionRetainsClaim(migration *ptahv1alpha1.PtahMigration, jobUID string) bool {
	active := migration.Status.ActiveOperation
	return migration.Spec.Suspend && slices.Contains(migration.Finalizers, migrationFinalizer) &&
		active != nil && active.Type == ptahv1alpha1.MigrationOperationApply && jobUID != "" && string(active.JobUID) == jobUID
}

var mfInputsChanged = regexp.MustCompile(`inputs changed`)

// suspendedAfterApply is a migration that settled Suspended with the
// dispatched Job's run recorded as an unknown outcome and held unresolved,
// because the suspension changed the inputs the claim was fingerprinted from.
func suspendedAfterApply(migration *ptahv1alpha1.PtahMigration, jobUID string) bool {
	status := migration.Status
	return suspendedQuiet(migration, jobUID) && status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeUnknown &&
		slices.ContainsFunc(status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == ptahv1alpha1.ConditionMigrationBlocked && condition.Status == metav1.ConditionTrue &&
				condition.Reason == "ApplyOutcomeUnknown" && mfInputsChanged.MatchString(condition.Message)
		})
}

// suspendedQuiet is a suspended migration that claims nothing and still holds
// the dispatched Job's run as its last, unresolved.
func suspendedQuiet(migration *ptahv1alpha1.PtahMigration, jobUID string) bool {
	status := migration.Status
	return status.Phase == ptahv1alpha1.MigrationPhaseSuspended && status.ActiveOperation == nil &&
		mfRunRecordedFor(migration, jobUID) && status.UnresolvedRun != nil && string(status.UnresolvedRun.JobUID) == jobUID
}

// resumedSettled is a resumed migration that settled its unresolved run from
// a reading of the database, for its current generation.
func resumedSettled(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && status.UnresolvedRun == nil &&
		status.ObservedGeneration == migration.Generation
}

// retryScheduled is a migration that scheduled a retry of a failed operation:
// a second attempt at least, carrying the deadline the resource asked for.
func retryScheduled(migration *ptahv1alpha1.PtahMigration) bool {
	active := migration.Status.ActiveOperation
	return active != nil && active.Attempt >= 2 && active.RetryNotBefore != nil && !active.RetryNotBefore.IsZero()
}

// retryDeadline is the retry deadline a migration's claim carries, if any.
func retryDeadline(migration *ptahv1alpha1.PtahMigration) (time.Time, bool) {
	active := migration.Status.ActiveOperation
	if active == nil || active.RetryNotBefore == nil || active.RetryNotBefore.IsZero() {
		return time.Time{}, false
	}
	return active.RetryNotBefore.Time, true
}

// planPublishedForApproval is a migration waiting for a person on a plan it
// published.
func planPublishedForApproval(migration *ptahv1alpha1.PtahMigration) (string, bool) {
	status := migration.Status
	if status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval || status.Plan == nil || status.Plan.Name == "" {
		return "", false
	}
	return status.Plan.Name, true
}

// applyClaimUnderLease is an Apply the migration claimed under a realm Lease:
// its Job, the Job's UID, and the epoch of the Lease acquisition.
func applyClaimUnderLease(migration *ptahv1alpha1.PtahMigration) (jobName, jobUID, epoch string, ok bool) {
	name, uid, dispatched := mfDispatchedApply(migration)
	if !dispatched || migration.Status.ActiveOperation.LeaseEpoch == "" {
		return "", "", "", false
	}
	return name, uid, migration.Status.ActiveOperation.LeaseEpoch, true
}

// releaseOwed is a migration whose run ended and was recorded as Applied, and
// which owes the release of the Lease its claim took at the epoch given.
func releaseOwed(migration *ptahv1alpha1.PtahMigration, jobUID, epoch string) bool {
	status := migration.Status
	return status.ActiveOperation == nil && mfRunRecordedFor(migration, jobUID) &&
		status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied && releaseStillOwed(migration, epoch)
}

// releaseStillOwed is a migration still owing the release at the epoch, and
// claiming no new work while it does.
func releaseStillOwed(migration *ptahv1alpha1.PtahMigration, epoch string) bool {
	release := migration.Status.PendingLockRelease
	return migration.Status.ActiveOperation == nil && release != nil && epoch != "" && release.LeaseEpoch == epoch
}

// leaseAtEpoch is the one Lease in the cluster whose acquisition epoch is the
// one a claim recorded, found by the epoch rather than by a name worked out
// by the harness. Its holder is empty when it has none.
func leaseAtEpoch(leases []coordinationv1.Lease, epoch string) (namespace, name, holder string, err error) {
	var found []*coordinationv1.Lease
	for index := range leases {
		if annotationIs(&leases[index], annotationLeaseEpoch, epoch) {
			found = append(found, &leases[index])
		}
	}
	if len(found) != 1 {
		return "", "", "", fmt.Errorf("expected exactly one realm Lease for the epoch of the claim, found %d", len(found))
	}
	lease := found[0]
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	return lease.Namespace, lease.Name, holder, nil
}

// sharedManagerAccount is the one ServiceAccount every manager Pod runs as.
func sharedManagerAccount(pods []corev1.Pod) (string, error) {
	var accounts []string
	for _, pod := range pods {
		accounts = append(accounts, pod.Spec.ServiceAccountName)
	}
	slices.Sort(accounts)
	accounts = slices.Compact(accounts)
	if len(accounts) != 1 {
		return "", errors.New("the manager Pods do not share one service account")
	}
	return accounts[0], nil
}

// runningManagerPod is the first running manager Pod that is not being
// deleted, whose token the dry run impersonates.
func runningManagerPod(pods []corev1.Pod) (name, uid string, ok bool) {
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			return pod.Name, string(pod.UID), pod.Name != "" && pod.UID != ""
		}
	}
	return "", "", false
}

// releaseFaultMessage is what the release fault answers a refused release
// with, and what the row waits to read before it calls the fault in force.
const releaseFaultMessage = "e2e fault: this realm Lease may not be released"

// releaseFaultPolicyDocuments is the fault the release row installs: a
// ValidatingAdmissionPolicy scoped to one realm Lease and to the manager's own
// identity, refusing exactly an update that empties a holder, and its
// binding. Acquiring and renewing still pass, so the fault is the release
// failing and nothing else.
func releaseFaultPolicyDocuments(name, lease, namespace, manager string) []map[string]any {
	quoted := func(value string) string { return "'" + value + "'" }
	return []map[string]any{
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy",
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{
				"failurePolicy": "Fail",
				"matchConstraints": map[string]any{"resourceRules": []any{map[string]any{
					"apiGroups": []any{"coordination.k8s.io"}, "apiVersions": []any{"v1"},
					"operations": []any{"UPDATE"}, "resources": []any{"leases"},
				}}},
				"matchConditions": []any{
					map[string]any{
						"name": "this-realm-lease",
						"expression": "object.metadata.name == " + quoted(lease) +
							" && object.metadata.namespace == " + quoted(namespace),
					},
					map[string]any{
						"name":       "written-by-the-manager",
						"expression": "request.userInfo.username == " + quoted(manager),
					},
				},
				"validations": []any{map[string]any{
					"expression": "!(has(oldObject.spec.holderIdentity) && oldObject.spec.holderIdentity != " + quoted("") +
						" && (!has(object.spec.holderIdentity) || object.spec.holderIdentity == " + quoted("") + "))",
					"message": releaseFaultMessage,
					"reason":  "Forbidden",
				}},
			},
		},
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding",
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"policyName": name, "validationActions": []any{"Deny"}},
		},
	}
}

// mfAddedUIDs is every UID in current that before does not hold. Only an
// addition counts: the cleanup TTL may remove a finished Job meanwhile.
func mfAddedUIDs(current, before []string) []string {
	var added []string
	for _, uid := range current {
		if !slices.Contains(before, uid) {
			added = append(added, uid)
		}
	}
	return added
}

// mfStateLines are the credential-free fields a row prints when a
// check about a migration fails: the phase, the claim, the release it owes,
// the last and the unresolved run, and each condition's type, status and
// reason.
func mfStateLines(migration *ptahv1alpha1.PtahMigration) []string {
	status := migration.Status
	active := "<none>"
	if operation := status.ActiveOperation; operation != nil {
		active = fmt.Sprintf("%s/%s epoch=%s", operation.Type, mfOrNone(operation.JobName), mfOrNone(operation.LeaseEpoch))
	}
	release := "<none>"
	if status.PendingLockRelease != nil {
		release = status.PendingLockRelease.LeaseEpoch
	}
	run := "<none>"
	if last := status.LastRun; last != nil {
		run = fmt.Sprintf("%s/%s/%s", last.Outcome, mfOrNone(last.JobName), mfOrNone(string(last.JobUID)))
	}
	unresolved := "<none>"
	if status.UnresolvedRun != nil {
		unresolved = mfOrNone(string(status.UnresolvedRun.JobUID))
	}
	lines := []string{
		fmt.Sprintf("  suspend=%t phase=%s activeOperation=%s", migration.Spec.Suspend, mfOrNone(string(status.Phase)), active),
		fmt.Sprintf("  pendingLockRelease=%s lastRun=%s unresolvedRun=%s", release, run, unresolved),
	}
	for _, condition := range status.Conditions {
		lines = append(lines, fmt.Sprintf("  condition %s=%s reason=%s at=%s",
			condition.Type, condition.Status, condition.Reason, instantOf(condition.LastTransitionTime)))
	}
	return lines
}

func mfOrNone(value string) string {
	if value == "" {
		return "<none>"
	}
	return value
}
