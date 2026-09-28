package e2e

import (
	"errors"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The readings the fault waits poll a schema for. Each is one document's
// claim, so a wait asserts against the document that satisfied it.

// planAwaitingApproval is a schema with a plan waiting for a person and no
// operation running.
func planAwaitingApproval(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseAwaitingApproval && status.ActiveOperation == nil &&
		status.Plan != nil && status.Plan.Name != ""
}

// exactControllerPlan is a plan bound to a valid execution binding of this
// controller state, and published by this manager under it.
func exactControllerPlan(plan *ptahv1alpha1.CurrentPlanStatus, binding *ptahv1alpha1.ExecutionBindingStatus,
	controller controllerIdentity, stateVersion int32,
) bool {
	return exactControllerBinding(binding, stateVersion) && plan != nil &&
		plan.ExecutionBindingID == binding.Epoch && plan.ControllerImage == controller.image &&
		plan.ControllerRevision == controller.revision && plan.ControllerStateVersion == stateVersion
}

// exactControllerBinding is a valid execution binding of this controller
// state.
func exactControllerBinding(binding *ptahv1alpha1.ExecutionBindingStatus, stateVersion int32) bool {
	return binding != nil && executionEpoch.MatchString(binding.Epoch) && binding.ControllerStateVersion == stateVersion
}

// readyPlanFromController is a ready, non-destructive plan of the current
// contract with statements in it, published by this manager.
func readyPlanFromController(plan *ptahv1alpha1.PtahSchemaPlan, controller controllerIdentity, stateVersion int32) error {
	spec := plan.Spec
	switch {
	case spec.ContractVersion != 3:
		return fmt.Errorf("it is contract version %d", spec.ContractVersion)
	case !executionEpoch.MatchString(spec.ExecutionBindingID) || spec.ControllerImage != controller.image ||
		spec.ControllerRevision != controller.revision || spec.ControllerStateVersion != stateVersion:
		return errors.New("it was not published by this manager")
	case spec.Destructive || spec.StatementCount <= 0:
		return errors.New("it is destructive or has no statement")
	case !conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue):
		return errors.New("it is not Ready")
	}
	return nil
}

// approvalBindsCurrentPlan is a schema whose current plan is the plan an
// approval is about to name, bound to the binding the plan carries and to
// this manager.
func approvalBindsCurrentPlan(schema *ptahv1alpha1.PtahSchema, plan *ptahv1alpha1.PtahSchemaPlan,
	controller controllerIdentity, stateVersion int32,
) bool {
	binding, current := schema.Status.ExecutionBinding, schema.Status.Plan
	return binding != nil && binding.Epoch == plan.Spec.ExecutionBindingID &&
		binding.ControllerStateVersion == stateVersion && current != nil &&
		current.Name == plan.Name && current.UID == plan.UID && current.Fingerprint == plan.Spec.Fingerprint &&
		current.ExecutionBindingID == plan.Spec.ExecutionBindingID && current.ControllerImage == controller.image &&
		current.ControllerRevision == controller.revision && current.ControllerStateVersion == stateVersion
}

// approvablePlan is a plan of the current contract, runner protocol and
// manager, with an artifact and a coordination digest.
func approvablePlan(plan *ptahv1alpha1.PtahSchemaPlan, runnerProtocol int64, controller controllerIdentity, stateVersion int32) bool {
	spec := plan.Spec
	return spec.ContractVersion == 3 && spec.ControllerImage == controller.image &&
		spec.ControllerRevision == controller.revision && spec.ControllerStateVersion == stateVersion &&
		int64(spec.RunnerProtocolVersion) == runnerProtocol &&
		sha256Pattern.MatchString(spec.ArtifactDigest) && sha256Pattern.MatchString(spec.CoordinationDigest)
}

// applyDispatched is a schema applying one dispatched Apply bound to its Job.
func applyDispatched(schema *ptahv1alpha1.PtahSchema) bool {
	active := schema.Status.ActiveOperation
	return schema.Status.Phase == ptahv1alpha1.PhaseApplying && active != nil &&
		active.Type == ptahv1alpha1.OperationApply && active.DispatchStarted && active.JobUID != ""
}

// singlePodApplyJob is the dispatched Apply Job: the schema's, its operation's,
// and one that neither retries nor replaces its Pod.
func singlePodApplyJob(job *batchv1.Job, uid, schema, operationID string) bool {
	return uidIs(job, uid) && operationOf(job, schema, "apply") && annotationIs(job, annotationOperationID, operationID) &&
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed &&
		int32PointerIs(job.Spec.BackoffLimit, 0)
}

// runningApplyPod is the one live Pod of an Apply Job once its ptah container
// runs. More than one live Pod is an error, since the Job must never run two.
func runningApplyPod(pods []corev1.Pod) (*corev1.Pod, error) {
	var live []*corev1.Pod
	for index := range pods {
		if pods[index].DeletionTimestamp == nil {
			live = append(live, &pods[index])
		}
	}
	if len(live) > 1 {
		return nil, errors.New("overlapping executor Pods")
	}
	if len(pods) == 0 || len(live) == 0 {
		return nil, nil
	}
	// The shell read the first item of the list, whatever its deletion state,
	// once the live count was one.
	pod := &pods[0]
	if pod.Status.Phase != corev1.PodRunning || !slices.ContainsFunc(pod.Status.ContainerStatuses,
		func(status corev1.ContainerStatus) bool { return status.Name == "ptah" && status.State.Running != nil }) {
		return nil, nil
	}
	return pod, nil
}

// exactResultJob is a Job that transported one result: the exact UID and
// operation, an operation ID, this manager's identity on the Job and its
// template, no retry and no replacement, and Complete.
func exactResultJob(job *batchv1.Job, uid, operation string, controller controllerIdentity) bool {
	return uidIs(job, uid) && labelIs(job, labelOperation, operation) && job.Annotations[annotationOperationID] != "" &&
		controller.stampedOn(job.Annotations) && controller.stampedOn(job.Spec.Template.Annotations) &&
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed &&
		int32PointerIs(job.Spec.BackoffLimit, 0) && conditionTrue(job.Status.Conditions, batchv1.JobComplete)
}

// resultPod is the one Pod the Job controls for its operation, holding this
// manager's identity, never restarted, with its ptah container terminated
// once with status zero.
func resultPod(pods []corev1.Pod, jobUID, operationID string, controller controllerIdentity) (*corev1.Pod, error) {
	var bound []*corev1.Pod
	for index := range pods {
		pod := &pods[index]
		owned := slices.ContainsFunc(pod.OwnerReferences, func(reference metav1.OwnerReference) bool {
			return reference.Kind == "Job" && jobUID != "" && string(reference.UID) == jobUID && isController(reference)
		})
		if owned && annotationIs(pod, annotationOperationID, operationID) {
			bound = append(bound, pod)
		}
	}
	if len(bound) != 1 {
		return nil, errors.New("exact Job does not own one operation-bound Pod")
	}
	pod := bound[0]
	exited := 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "ptah" && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
			exited++
		}
	}
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if status.RestartCount != 0 {
			return nil, errors.New("result Pod did not terminate exactly once")
		}
	}
	if !controller.stampedOn(pod.Annotations) || exited != 1 {
		return nil, errors.New("result Pod did not terminate exactly once")
	}
	return pod, nil
}

// resultBinding is a result of this runner protocol for the operation and
// operation ID asked, whose output was not truncated.
func resultBinding(result runner.Result, protocol int64, operation, operationID string) bool {
	return int64(result.ProtocolVersion) == protocol && string(result.Operation) == operation &&
		result.OperationID == operationID && result.Truncation == nil
}

// recoveryJobComplete is the exact proof Job completed under an operation ID.
func recoveryJobComplete(job *batchv1.Job, uid string) bool {
	return uidIs(job, uid) && conditionTrue(job.Status.Conditions, batchv1.JobComplete) &&
		job.Annotations[annotationOperationID] != ""
}

// freshPlanAwaitingApproval is a schema back at a fresh plan nobody approved,
// with nothing pending, applied or locked.
func freshPlanAwaitingApproval(schema *ptahv1alpha1.PtahSchema) bool {
	return recoveredAwaitingApproval(schema) && schema.Status.Plan.Approval == nil
}

// recoveredAwaitingApproval is a schema back at a plan waiting for a person,
// with nothing pending, applied or locked.
func recoveredAwaitingApproval(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.PendingObservation == nil && status.ActiveOperation == nil &&
		status.Phase == ptahv1alpha1.PhaseAwaitingApproval && status.Plan != nil && status.Plan.Name != "" &&
		status.Applied == nil && status.PendingLockRelease == nil
}

// realmConflictRefused is a schema refused because another resource claims
// the same database: blocked, idle, and neither ready nor asking for
// approval, both for that reason.
func realmConflictRefused(schema *ptahv1alpha1.PtahSchema) bool {
	return realmConflictHeld(schema) &&
		conditionIs(schema.Status.Conditions, ptahv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "RealmConflict")
}

// realmConflictHeld is a schema still blocked and idle, not ready for the
// realm conflict.
func realmConflictHeld(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == ptahv1alpha1.PhaseBlocked && schema.Status.ActiveOperation == nil &&
		conditionIs(schema.Status.Conditions, ptahv1alpha1.ConditionReady, metav1.ConditionFalse, "RealmConflict")
}

// undispatchedApplyClaim is an Apply the schema claimed and could not
// dispatch: no Job bound, and dispatch not started.
func undispatchedApplyClaim(schema *ptahv1alpha1.PtahSchema) bool {
	active := schema.Status.ActiveOperation
	return active != nil && active.Type == ptahv1alpha1.OperationApply && active.JobUID == "" && !active.DispatchStarted
}

// heldReadOperation is a read-only operation bound to its own Job.
func heldReadOperation(schema *ptahv1alpha1.PtahSchema) bool {
	active := schema.Status.ActiveOperation
	return active != nil && slices.Contains([]ptahv1alpha1.OperationType{
		ptahv1alpha1.OperationResolve, ptahv1alpha1.OperationVerify,
		ptahv1alpha1.OperationObserve, ptahv1alpha1.OperationPlan,
	}, active.Type) && active.JobName != "" && active.JobUID != ""
}

// readOperationRetried is the same operation dispatched again under a Job
// other than the one removed.
func readOperationRetried(schema *ptahv1alpha1.PtahSchema, operation ptahv1alpha1.OperationType, removedUID string) bool {
	active := schema.Status.ActiveOperation
	return active != nil && active.Type == operation && active.JobName != "" && active.JobUID != "" &&
		string(active.JobUID) != removedUID
}

// suspendedReadDiscarded is a suspended schema that let go of its operation.
func suspendedReadDiscarded(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Spec.Suspend && schema.Status.ActiveOperation == nil && schema.Status.PendingObservation == nil
}

// inSyncFor is a schema in sync for the reason given, with nothing pending,
// active or locked.
func inSyncFor(schema *ptahv1alpha1.PtahSchema, reason string) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.PendingObservation == nil &&
		status.ActiveOperation == nil && status.PendingLockRelease == nil &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionInSync, metav1.ConditionTrue, reason)
}

// uncertainStillSafe is the uncertain MySQL schema, long after its recovery:
// idle, nothing pending or locked, and a plan nobody approved.
func uncertainStillSafe(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.ActiveOperation == nil && status.PendingObservation == nil && status.PendingLockRelease == nil &&
		status.Plan != nil && status.Plan.UID != "" && status.Plan.Approval == nil
}

// timeoutRecoveryStillSafe is the deadline schema, long after its recovery:
// still waiting for a person on a fresh plan, with nothing applied.
func timeoutRecoveryStillSafe(schema *ptahv1alpha1.PtahSchema) bool {
	return uncertainStillSafe(schema) && schema.Status.Applied == nil &&
		schema.Status.Phase == ptahv1alpha1.PhaseAwaitingApproval
}

// activeIdentityKept is a schema still running the operation it ran before
// the manager restarted, on the same Job.
func activeIdentityKept(schema *ptahv1alpha1.PtahSchema, operationID, jobName, jobUID string) bool {
	active := schema.Status.ActiveOperation
	return active != nil && active.ID == operationID && active.JobName == jobName && string(active.JobUID) == jobUID
}

// activePodForEphemeral is the running Apply Pod an ephemeral container is
// about to be refused on: the exact Pod, its operation, its one Job owner, and
// no ephemeral container yet.
func activePodForEphemeral(pod *corev1.Pod, podUID, jobName, jobUID, operationID string) bool {
	return uidIs(pod, podUID) && annotationIs(pod, annotationOperationID, operationID) &&
		exactJobOwner(pod.OwnerReferences, jobName, jobUID) && len(pod.Spec.EphemeralContainers) == 0
}

// activeOperationDispatched is a schema whose active operation is still the
// dispatched one named.
func activeOperationDispatched(schema *ptahv1alpha1.PtahSchema, operationID, jobName, jobUID string) bool {
	return activeIdentityKept(schema, operationID, jobName, jobUID) && schema.Status.ActiveOperation.DispatchStarted
}

// ephemeralContainerRefused is the Pod after the refusal: the same UID under
// the same Job, and no trace of the container that was refused.
func ephemeralContainerRefused(pod *corev1.Pod, podUID, jobUID string) bool {
	return uidIs(pod, podUID) && controlledByJob(pod.OwnerReferences, types.UID(jobUID)) &&
		!slices.ContainsFunc(pod.Spec.EphemeralContainers, func(container corev1.EphemeralContainer) bool {
			return container.Name == refusedEphemeralContainer
		})
}

// refusedEphemeralContainer is the container the Pod-intent webhook has to
// keep out of a running operation Pod.
const refusedEphemeralContainer = "ptah-admission-must-deny"
