package e2e

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// targetChangeLogin is the second login the row moves a store to.
const targetChangeLogin = "ptah_e2e_target_login"

func schemaRetargetRefused(result runner.Result, previous string) bool {
	return sha256Pattern.MatchString(previous) && sha256Pattern.MatchString(result.TargetIdentityDigest) &&
		result.TargetIdentityDigest != previous && result.Error != nil && result.Error.Code == "target_binding_mismatch" &&
		result.ChildExitCode == -1 && !result.MutationStarted && !result.Uncertain && result.Truncation == nil
}

// proofReadClaimed is a schema whose read-only proof of the dispatched Apply has
// claimed its Job: an Observe or a Plan, bound to a Job, while the proof owed to
// that Apply is still pending.
func proofReadClaimed(resource *ptahv1alpha1.PtahSchema, applyOperationID string) bool {
	active, pending := resource.Status.ActiveOperation, resource.Status.PendingObservation
	return applyOperationID != "" && active != nil && pending != nil && pending.ApplyOperationID == applyOperationID &&
		(active.Type == ptahv1alpha1.OperationObserve || active.Type == ptahv1alpha1.OperationPlan) &&
		active.ID != "" && active.ID != applyOperationID && active.JobName != "" && active.JobUID != ""
}

// A read of another target cannot settle the dispatched Apply's outcome. The
// controller must keep the original proof binding and stop, naming the refusal
// and what ends it, even while it goes on reading.
func retargetProofRefused(
	resource *ptahv1alpha1.PtahSchema,
	outcome ptahv1alpha1.PendingObservationOutcome,
	operationID, jobUID, target string,
) bool {
	pending := resource.Status.PendingObservation
	if operationID == "" || jobUID == "" || !sha256Pattern.MatchString(target) || pending == nil ||
		pending.Outcome != outcome || resource.Status.Phase != ptahv1alpha1.PhaseBlocked ||
		pending.ApplyOperationID != operationID || string(pending.ApplyJobUID) != jobUID ||
		pending.Plan.TargetIdentityDigest != target {
		return false
	}
	for _, condition := range resource.Status.Conditions {
		if condition.Type == ptahv1alpha1.ConditionReconciliationFailed && condition.Status == metav1.ConditionTrue &&
			condition.Reason == string(ptahv1alpha1.ReasonTargetIdentityChanged) {
			_, err := printedSettleCommand(condition.Message, resource.Name, resource.Namespace, operationID)
			return err == nil
		}
	}
	return false
}

// printedSettleCommand is the kubectl arguments the stop's message ends with, held
// to this resource and this Apply, so that a row runs what a person would copy.
func printedSettleCommand(message, name, namespace, operationID string) ([]string, error) {
	at := strings.LastIndex(message, "kubectl annotate ")
	if at < 0 {
		return nil, errors.New("the message prints no command")
	}
	fields := strings.Fields(message[at:])
	prefix := ptahv1alpha1.SettledApplyAnnotation + "=" + operationID + "/"
	if len(fields) != 8 || fields[2] != "ptahschema" || fields[3] != name || fields[4] != "-n" || fields[5] != namespace ||
		!strings.HasPrefix(fields[6], prefix) || !sha256Pattern.MatchString(strings.TrimPrefix(fields[6], prefix)) ||
		fields[7] != "--overwrite" {
		return nil, fmt.Errorf("the message prints another command: %q", message[at:])
	}
	return fields[1:], nil
}

// proofReadAgain is the proof of operationID still owed by an attempt later than
// the one a row saw, so whatever the row wrote in between has had a pass to act on.
func proofReadAgain(resource *ptahv1alpha1.PtahSchema, operationID string, attempt int32) bool {
	pending, operation := resource.Status.PendingObservation, resource.Status.ActiveOperation
	return pending != nil && pending.ApplyOperationID == operationID && operation != nil && operation.Attempt > attempt &&
		resource.Status.Phase == ptahv1alpha1.PhaseBlocked
}

// planUnderAnotherTarget is a plan waiting for an approval, bound to a target
// other than previous, with the proof behind it gone.
func planUnderAnotherTarget(resource *ptahv1alpha1.PtahSchema, previous string) bool {
	if !sha256Pattern.MatchString(previous) || !planAwaitingApproval(resource) {
		return false
	}
	target := resource.Status.Plan.TargetIdentityDigest
	return sha256Pattern.MatchString(target) && target != previous && resource.Status.PendingObservation == nil
}

// proofSettledInJournal is the journal saying a plan's proof ended with nobody
// having established what its Apply did.
func proofSettledInJournal(resource *ptahv1alpha1.PtahSchema, plan string) bool {
	if resource.Status.Journal == nil || plan == "" {
		return false
	}
	for _, entry := range resource.Status.Journal.Entries {
		if entry.Type == ptahv1alpha1.JournalVerified && entry.Outcome == ptahv1alpha1.JournalUnknown && entry.Plan == plan {
			return true
		}
	}
	return false
}

// proofFinishedUnderItsTarget is a resource that settled in sync with the Apply
// of the plan credited.
func proofFinishedUnderItsTarget(resource *ptahv1alpha1.PtahSchema, fingerprint string) bool {
	status := resource.Status
	return fingerprint != "" && status.Phase == ptahv1alpha1.PhaseInSync && status.PendingObservation == nil &&
		status.ActiveOperation == nil && status.Applied != nil && status.Applied.PlanFingerprint == fingerprint &&
		conditionStatus(status.Conditions, "Ready", "True")
}

// urlWithLogin is a database URL with another login, the password and all else
// as they were, so what it changes is the target identity and nothing a server
// would read differently.
func urlWithLogin(raw, login string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil || login == "" {
		return "", errors.New("the database URL names no login to replace")
	}
	password, set := parsed.User.Password()
	if !set || parsed.User.Username() == login {
		return "", errors.New("the database URL has no password, or names this login already")
	}
	parsed.User = url.UserPassword(login, password)
	return parsed.String(), nil
}

func targetPlanAwaitingApproval(resource *ptahv1alpha1.PtahSchema, target string) bool {
	return sha256Pattern.MatchString(target) && planAwaitingApproval(resource) &&
		resource.Status.Plan.TargetIdentityDigest == target &&
		resource.Status.PendingObservation == nil && resource.Status.ActiveOperation == nil
}

func freshApprovalConverged(resource *ptahv1alpha1.PtahSchema) bool {
	status := resource.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.ObservedGeneration == resource.Generation &&
		status.ActiveOperation == nil && status.PendingObservation == nil && conditionStatus(status.Conditions, "Ready", "True")
}
