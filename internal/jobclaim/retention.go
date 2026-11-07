package jobclaim

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// How long a migration run keeps its Pod log is decided here, for the manager
// that asks for a TTL and the webhook that judges the request.
//
// The long TTL costs one Job for as long as it lasts, so it goes where the Jobs
// cannot pile up: an Apply a person approved, which comes around only when a
// person approves again, and a run that stops its resource, which dispatches no
// Apply until a History reading or an acknowledgment settles it. A schema has only
// the first: its uncertain Apply goes to a proof, the proof plans again, and
// `apply: Always` applies that plan, so nothing stops the next run.

// executorContainer is the container of an operation Pod that runs the runner.
const executorContainer = "ptah"

// MigrationRunOutcome is the outcome a runner's report names, in the terms status
// keeps. An outcome this build does not know is one nobody can account for.
func MigrationRunOutcome(reported string) operatorv1alpha1.MigrationRunOutcome {
	switch reported {
	case dataplane.MigrationOutcomeUpToDate:
		return operatorv1alpha1.MigrationRunOutcomeUpToDate
	case dataplane.MigrationOutcomeApplied, dataplane.MigrationOutcomeDryRun:
		return operatorv1alpha1.MigrationRunOutcomeApplied
	case dataplane.MigrationOutcomeFailed:
		return operatorv1alpha1.MigrationRunOutcomeFailed
	case dataplane.MigrationOutcomePartial:
		return operatorv1alpha1.MigrationRunOutcomePartial
	default:
		return operatorv1alpha1.MigrationRunOutcomeUnknown
	}
}

// MigrationRunBlocks reports whether a run with this outcome stops its resource.
// Neither of these may be retried. A partial run committed some of a migration's
// statements and not the rest, and an unknown one cannot say whether it did, so
// running the same file again would run those statements twice.
func MigrationRunBlocks(outcome operatorv1alpha1.MigrationRunOutcome) bool {
	return outcome == operatorv1alpha1.MigrationRunOutcomePartial ||
		outcome == operatorv1alpha1.MigrationRunOutcomeUnknown
}

// MigrationRunKeepsLog reports whether a migration run that did not succeed keeps
// FailedApplyRetentionTTLSeconds. Every other run keeps CleanupTTLSeconds.
func MigrationRunKeepsLog(approved bool, outcome operatorv1alpha1.MigrationRunOutcome) bool {
	return approved || MigrationRunBlocks(outcome)
}

// ReportedMigrationRun is the outcome a migration Apply's Pod reported. A Pod that
// reported none leaves the outcome unknown, as it does for the manager: no Pod, more
// than one, a summary that does not read for this operation, and a summary with no
// report in it all end in a run nobody accounted for.
//
// operationID is the claim's, so an edit of the Job cannot make a report unreadable.
func ReportedMigrationRun(pods []corev1.Pod, jobUID types.UID, operationID string) operatorv1alpha1.MigrationRunOutcome {
	summary, read := ApplySummary(pods, jobUID, runner.OperationMigrationApply, operationID)
	if !read || summary.Migration == nil {
		return operatorv1alpha1.MigrationRunOutcomeUnknown
	}
	return MigrationRunOutcome(summary.Migration.Outcome)
}

// ApplySummary reads the termination summary the one Pod a Job owns left, and
// reports whether there was one that reads for this operation. Pods is every Pod
// the caller could see. They are matched by owner, because a label can be edited
// and an owner reference is what the Job controller wrote. A Job can start more than
// one Pod, and no one Pod's report speaks for another, so more than one reads as
// none.
//
// operationID is the full ID the runner writes. A Job's label holds a hash of it.
func ApplySummary(pods []corev1.Pod, jobUID types.UID, operation runner.Operation, operationID string) (runner.Summary, bool) {
	var owned *corev1.Pod
	for index := range pods {
		if !controlledBy(&pods[index], jobUID) {
			continue
		}
		if owned != nil {
			return runner.Summary{}, false
		}
		owned = &pods[index]
	}
	if owned == nil {
		return runner.Summary{}, false
	}
	for _, status := range owned.Status.ContainerStatuses {
		if status.Name != executorContainer || status.State.Terminated == nil {
			continue
		}
		summary, err := runner.ParseSummaryFor(status.State.Terminated.Message, operation, operationID)
		return summary, err == nil
	}
	return runner.Summary{}, false
}

func controlledBy(pod *corev1.Pod, jobUID types.UID) bool {
	for _, reference := range pod.OwnerReferences {
		if reference.UID == jobUID && reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}
