package e2e

import (
	"errors"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The proofs below port the jq the fault phase held schemas, results and
// watch histories to. A field the API omits when it is empty is null to jq,
// and null equals no string, so a comparison of such a field with a value
// the API always writes, or with a value a proof supplies, requires the
// field present. Two omitted fields compared with each other are both null,
// and equal, as two empty strings are.

// jobRef is a Job a pending observation names.
type jobRef struct{ name, uid string }

// podEvidence is the Apply Pod evidence a pending observation has to carry:
// exactly these UIDs, or, when optional, these or none, with the recorded
// count matching the recorded list either way.
type podEvidence struct {
	uids     []string
	optional bool
}

// holds reads the recorded list and count as jq did, with an absent list as
// empty and an absent count as zero.
func (e podEvidence) holds(recorded []types.UID, count int32) bool {
	uids := make([]string, 0, len(recorded))
	for _, uid := range recorded {
		uids = append(uids, string(uid))
	}
	matches := slices.Equal(uids, e.uids) || (e.optional && len(uids) == 0)
	return matches && int(count) == len(uids)
}

// presentIs is a field the API omits when empty that is present and equal to
// the value given.
func presentIs(value, want string) bool {
	return value != "" && value == want
}

// noneConverged is jq's all(.status.conditions[]; ...) over the InSync and
// Ready conditions: neither may be True. jq cannot iterate a null list, so a
// schema with no conditions fails it.
func noneConverged(conditions []metav1.Condition) bool {
	if len(conditions) == 0 {
		return false
	}
	return !slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return (condition.Type == ptahv1alpha1.ConditionInSync || condition.Type == ptahv1alpha1.ConditionReady) &&
			condition.Status == metav1.ConditionTrue
	})
}

// describeActive names an active operation in a refusal without anything a
// credential could hide in.
func describeActive(active *ptahv1alpha1.ActiveOperationStatus) string {
	if active == nil {
		return "no active operation"
	}
	return fmt.Sprintf("%s %s on Job UID %q at epoch %q", active.Type, active.ID, active.JobUID, active.LeaseEpoch)
}

// heldProof is what a schema holds while its status writes are paused right
// after the controller harvested one proof Job of a pending observation: the
// proof operation active on its Job at the Apply's lease epoch, the pending
// observation still naming the Apply, and nothing converged, applied or
// released. It ports the jq the shell ran on the schema after pausing status
// writes in assert_fault_convergence_result_pair, capture_uncertain_read_proof_pair
// and the runner-termination scenario.
type heldProof struct {
	// stage is Observe or Plan. A Plan stage requires planRequired, an
	// Observe stage requires it false or absent.
	stage            ptahv1alpha1.OperationType
	operationID      string
	jobUID           string
	leaseEpoch       string
	outcome          ptahv1alpha1.PendingObservationOutcome
	applyOperationID string
	// applyJob, when not nil, is the Apply Job the pending observation names.
	applyJob *jobRef
	// applyPods, when not nil, is the Pod evidence it carries.
	applyPods *podEvidence
	// controller, when not nil, holds the pending plan to the schema's
	// execution binding and to this manager.
	controller   *controllerIdentity
	stateVersion int32
}

// check holds the schema to the proof and names the first field that
// differs.
func (p heldProof) check(schema *ptahv1alpha1.PtahSchema) error {
	status := schema.Status
	active, pending := status.ActiveOperation, status.PendingObservation
	switch {
	case p.stage != ptahv1alpha1.OperationObserve && p.stage != ptahv1alpha1.OperationPlan:
		return fmt.Errorf("a held proof is an Observe or a Plan, not %q", p.stage)
	case active == nil || active.Type != p.stage || active.ID != p.operationID ||
		!presentIs(string(active.JobUID), p.jobUID) || !presentIs(active.LeaseEpoch, p.leaseEpoch):
		return fmt.Errorf("the active operation is %s, not %s %s on Job UID %s at epoch %s",
			describeActive(active), p.stage, p.operationID, p.jobUID, p.leaseEpoch)
	case pending == nil:
		return errors.New("the pending observation is gone")
	case pending.Outcome != p.outcome:
		return fmt.Errorf("the pending observation's outcome is %q, not %q", pending.Outcome, p.outcome)
	case pending.ApplyOperationID != p.applyOperationID:
		return fmt.Errorf("the pending observation names Apply %s, not %s", pending.ApplyOperationID, p.applyOperationID)
	case p.applyJob != nil && (!presentIs(pending.ApplyJobName, p.applyJob.name) ||
		!presentIs(string(pending.ApplyJobUID), p.applyJob.uid)):
		return fmt.Errorf("the pending observation names Apply Job %q UID %q, not %s UID %s",
			pending.ApplyJobName, pending.ApplyJobUID, p.applyJob.name, p.applyJob.uid)
	case p.applyPods != nil && !p.applyPods.holds(pending.ApplyPodUIDs, pending.ApplyPodCount):
		return fmt.Errorf("the pending observation records Apply Pods %v counted %d, not %v (optional %t)",
			pending.ApplyPodUIDs, pending.ApplyPodCount, p.applyPods.uids, p.applyPods.optional)
	case !presentIs(pending.LeaseEpoch, p.leaseEpoch):
		return fmt.Errorf("the pending observation is at epoch %q, not %s", pending.LeaseEpoch, p.leaseEpoch)
	case p.controller != nil && (status.ExecutionBinding == nil ||
		pending.Plan.ExecutionBindingID != status.ExecutionBinding.Epoch):
		return fmt.Errorf("the pending plan is bound to %q, not to the schema's execution binding",
			pending.Plan.ExecutionBindingID)
	case p.controller != nil && (pending.Plan.ControllerImage != p.controller.image ||
		pending.Plan.ControllerRevision != p.controller.revision ||
		pending.Plan.ControllerStateVersion != p.stateVersion):
		return errors.New("the pending plan was not published by this manager")
	case pending.PlanRequired != (p.stage == ptahv1alpha1.OperationPlan):
		return fmt.Errorf("the pending observation has planRequired %t during the %s", pending.PlanRequired, p.stage)
	case status.Phase != ptahv1alpha1.PhaseVerifyingConvergence:
		return fmt.Errorf("the schema is %s, not VerifyingConvergence", status.Phase)
	case status.Applied != nil:
		return errors.New("the schema records an applied plan")
	case status.PendingLockRelease != nil:
		return errors.New("the schema asks for its lock to be released")
	case !noneConverged(status.Conditions):
		return errors.New("the schema is InSync or Ready, or carries no conditions")
	}
	return nil
}

// schemaDocuments is every document the schema watch recorded for the named
// schema, in order.
func schemaDocuments(events []watchEvent[*ptahv1alpha1.PtahSchema], name string) []*ptahv1alpha1.PtahSchema {
	var documents []*ptahv1alpha1.PtahSchema
	for _, event := range events {
		if event.Object.Name == name {
			documents = append(documents, event.Object)
		}
	}
	return documents
}

// firstDocument is the index of the first document at or after start that
// matches, or -1: jq's .[0] of a selection, null when it is empty.
func firstDocument(documents []*ptahv1alpha1.PtahSchema, start int, match func(*ptahv1alpha1.PtahSchema) bool) int {
	for index := max(start, 0); index < len(documents); index++ {
		if match(documents[index]) {
			return index
		}
	}
	return -1
}

// appliedAt is a schema whose active operation is the Apply of the operation
// and Job given, at the epoch given.
func appliedAt(schema *ptahv1alpha1.PtahSchema, operationID, jobUID, epoch string) bool {
	active := schema.Status.ActiveOperation
	return active != nil && active.Type == ptahv1alpha1.OperationApply && active.ID == operationID &&
		presentIs(string(active.JobUID), jobUID) && presentIs(active.LeaseEpoch, epoch)
}

// pendingFor is a schema whose pending observation has the outcome given for
// the Apply of the operation and Job given, at the epoch given.
func pendingFor(schema *ptahv1alpha1.PtahSchema, outcome ptahv1alpha1.PendingObservationOutcome,
	operationID, jobUID, epoch string,
) bool {
	pending := schema.Status.PendingObservation
	return pending != nil && pending.Outcome == outcome && pending.ApplyOperationID == operationID &&
		presentIs(string(pending.ApplyJobUID), jobUID) && presentIs(pending.LeaseEpoch, epoch)
}

// provingWith is a schema whose active operation is the proof operation of
// the type given on the Job given, while its pending observation names the
// Apply at the epoch given. A Plan proof requires planRequired.
func provingWith(schema *ptahv1alpha1.PtahSchema, stage ptahv1alpha1.OperationType, jobUID, applyOperationID, epoch string) bool {
	active, pending := schema.Status.ActiveOperation, schema.Status.PendingObservation
	return active != nil && active.Type == stage && presentIs(string(active.JobUID), jobUID) &&
		pending != nil && pending.ApplyOperationID == applyOperationID && presentIs(pending.LeaseEpoch, epoch) &&
		(stage != ptahv1alpha1.OperationPlan || pending.PlanRequired)
}

// releaseRequested is a schema asking for the Apply's lock to be released at
// its epoch and realm.
func releaseRequested(schema *ptahv1alpha1.PtahSchema, operationID, epoch, coordinationDigest string) bool {
	release := schema.Status.PendingLockRelease
	return release != nil && release.OperationID == operationID && release.LeaseEpoch == epoch &&
		release.CoordinationDigest == coordinationDigest
}

// activeMatchesPending is jq's active_matches_pending: the proof operation
// reads the target, source and observation settings the pending observation
// recorded, under the same lease.
func activeMatchesPending(active *ptahv1alpha1.ActiveOperationStatus, pending *ptahv1alpha1.PendingObservationStatus) error {
	switch {
	case active == nil || pending == nil:
		return errors.New("the proof operation or its pending observation is gone")
	case !presentIs(active.CoordinationDigest, pending.CoordinationDigest):
		return fmt.Errorf("the %s reads realm %q, not %s", active.Type, active.CoordinationDigest, pending.CoordinationDigest)
	case !presentIs(active.TargetIdentityDigest, pending.Plan.TargetIdentityDigest):
		return fmt.Errorf("the %s reads target %q, not %s", active.Type, active.TargetIdentityDigest,
			pending.Plan.TargetIdentityDigest)
	case !sameJSON(active.Target, pending.Target):
		return fmt.Errorf("the %s reads another target binding", active.Type)
	case !sameJSON(active.Source, pending.Source):
		return fmt.Errorf("the %s reads another source binding", active.Type)
	case !sameJSON(active.ObservationDev, pending.Dev):
		return fmt.Errorf("the %s reads another dev database", active.Type)
	case !slices.Equal(active.ObservationExclude, pending.Exclude):
		return fmt.Errorf("the %s excludes %v, not %v", active.Type, active.ObservationExclude, pending.Exclude)
	case active.ObservationSeverity != pending.DriftSeverity:
		return fmt.Errorf("the %s reads severity %q, not %q", active.Type, active.ObservationSeverity, pending.DriftSeverity)
	case active.ObservationConnectTimeout.Duration != pending.ConnectTimeout.Duration:
		return fmt.Errorf("the %s connects within %s, not %s", active.Type, active.ObservationConnectTimeout.Duration,
			pending.ConnectTimeout.Duration)
	case active.ObservationLockTimeout.Duration != pending.LockTimeout.Duration:
		return fmt.Errorf("the %s locks within %s, not %s", active.Type, active.ObservationLockTimeout.Duration,
			pending.LockTimeout.Duration)
	case active.LeaseDurationSeconds == 0 || active.LeaseDurationSeconds != pending.LeaseDurationSeconds:
		return fmt.Errorf("the %s leases for %d seconds, not %d", active.Type, active.LeaseDurationSeconds,
			pending.LeaseDurationSeconds)
	case active.LeaseEpoch != pending.LeaseEpoch:
		return fmt.Errorf("the %s is at epoch %q, not %q", active.Type, active.LeaseEpoch, pending.LeaseEpoch)
	case active.LeaseContinuityLost:
		return fmt.Errorf("the %s lost lease continuity", active.Type)
	}
	return nil
}

// verifyingUnderBinding is a schema verifying convergence of a plan bound to
// its execution binding and this manager, with nothing applied, released,
// converged or ready.
func verifyingUnderBinding(schema *ptahv1alpha1.PtahSchema, controller controllerIdentity, stateVersion int32) error {
	status := schema.Status
	var plan *ptahv1alpha1.CurrentPlanStatus
	if status.PendingObservation != nil {
		plan = &status.PendingObservation.Plan
	}
	switch {
	case status.Phase != ptahv1alpha1.PhaseVerifyingConvergence:
		return fmt.Errorf("a document of the proof is %s, not VerifyingConvergence", status.Phase)
	case !exactControllerPlan(plan, status.ExecutionBinding, controller, stateVersion):
		return errors.New("a document of the proof holds a pending plan not bound to this manager's execution binding")
	case status.Applied != nil:
		return errors.New("a document of the proof records an applied plan")
	case status.PendingLockRelease != nil:
		return errors.New("a document of the proof asks for its lock to be released")
	case !noneConverged(status.Conditions):
		return errors.New("a document of the proof is InSync or Ready, or carries no conditions")
	}
	return nil
}

// jobEventIndex is the index of the first Job event that matches, and
// lastJobEventIndex of the last, or -1.
func jobEventIndex(jobs []watchEvent[*batchv1.Job], match func(watchEvent[*batchv1.Job]) bool) int {
	return slices.IndexFunc(jobs, match)
}

func lastJobEventIndex(jobs []watchEvent[*batchv1.Job], match func(watchEvent[*batchv1.Job]) bool) int {
	for index := len(jobs) - 1; index >= 0; index-- {
		if match(jobs[index]) {
			return index
		}
	}
	return -1
}

// proofJobsOrdered holds the proof Jobs to their order in the Job watch: the
// Observe Job and the Plan Job added, each under the operation ID the schema
// named it by, the Observe Job complete before the Plan Job was added, and
// the Plan Job completed at some point.
func proofJobsOrdered(jobs []watchEvent[*batchv1.Job], observeJobUID, observeID, planJobUID, planID string) error {
	added := func(uid string) int {
		return jobEventIndex(jobs, func(event watchEvent[*batchv1.Job]) bool {
			return event.Type == watch.Added && uidIs(event.Object, uid)
		})
	}
	complete := func(event watchEvent[*batchv1.Job], uid string) bool {
		return uidIs(event.Object, uid) && conditionTrue(event.Object.Status.Conditions, batchv1.JobComplete)
	}
	observeAdded, planAdded := added(observeJobUID), added(planJobUID)
	observeComplete := jobEventIndex(jobs, func(event watchEvent[*batchv1.Job]) bool { return complete(event, observeJobUID) })
	planComplete := lastJobEventIndex(jobs, func(event watchEvent[*batchv1.Job]) bool { return complete(event, planJobUID) })
	switch {
	case observeAdded < 0 || observeComplete < 0:
		return fmt.Errorf("the Job watch did not see Observe Job UID %s added and complete", observeJobUID)
	case planAdded < 0 || planComplete < 0:
		return fmt.Errorf("the Job watch did not see Plan Job UID %s added and complete", planJobUID)
	case observeID == "" || planID == "":
		return errors.New("a proof operation has no operation ID")
	case !annotationIs(jobs[observeAdded].Object, annotationOperationID, observeID):
		return fmt.Errorf("Observe Job UID %s was added for another operation than %s", observeJobUID, observeID)
	case !annotationIs(jobs[planAdded].Object, annotationOperationID, planID):
		return fmt.Errorf("Plan Job UID %s was added for another operation than %s", planJobUID, planID)
	case observeComplete >= planAdded:
		return fmt.Errorf("Plan Job UID %s was added before Observe Job UID %s completed", planJobUID, observeJobUID)
	}
	return nil
}

// leaseHeldUntilRelease holds the target Lease to one acquisition: from the
// first event that shows it held by the holder at the epoch, every event of
// its UID keeps it so until the first that shows it unheld, which modifies it
// and keeps the epoch.
func leaseHeldUntilRelease(leases []watchEvent[*coordinationv1.Lease], lease leaseIdentity) error {
	held := slices.IndexFunc(leases, func(event watchEvent[*coordinationv1.Lease]) bool {
		return uidIs(event.Object, lease.uid) && heldAs(event.Object, lease.holder, lease.epoch)
	})
	if held < 0 {
		return fmt.Errorf("the Lease watch never saw %s", lease)
	}
	released := -1
	for index := held + 1; index < len(leases); index++ {
		if uidIs(leases[index].Object, lease.uid) && holderEmpty(leases[index].Object) {
			released = index
			break
		}
	}
	switch {
	case released < 0:
		return fmt.Errorf("the Lease watch never saw %s released", lease)
	case leases[released].Type != watch.Modified:
		return fmt.Errorf("%s was released by a %s event, not a modification", lease, leases[released].Type)
	case !annotationIs(leases[released].Object, annotationLeaseEpoch, lease.epoch):
		return fmt.Errorf("%s was released at another epoch", lease)
	}
	for _, event := range leases[held:released] {
		if uidIs(event.Object, lease.uid) && (event.Type == watch.Deleted || !heldAs(event.Object, lease.holder, lease.epoch)) {
			return fmt.Errorf("%s changed hands or was deleted before its release", lease)
		}
	}
	return nil
}

// harvestedApply is the successful Apply assert_successful_apply_result holds
// the schema watch to.
type harvestedApply struct {
	schema, operationID, jobUID, podUID string
	controller                          controllerIdentity
	stateVersion                        int32
}

// successfulApplyHarvested ports assert_successful_apply_result's jq: the
// schema watch holds the Apply bound to its plan and the pending
// observation that harvested the result, and the result is that Apply's.
func successfulApplyHarvested(schemas []watchEvent[*ptahv1alpha1.PtahSchema], want harvestedApply, result runner.Result) error {
	documents := schemaDocuments(schemas, want.schema)
	applyIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		active := schema.Status.ActiveOperation
		return active != nil && active.Type == ptahv1alpha1.OperationApply && active.ID == want.operationID &&
			presentIs(string(active.JobUID), want.jobUID) && schema.Status.Plan != nil
	})
	pendingIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		pending := schema.Status.PendingObservation
		return pending != nil && pending.Outcome == ptahv1alpha1.PendingObservationApplySucceeded &&
			pending.ApplyOperationID == want.operationID && presentIs(string(pending.ApplyJobUID), want.jobUID)
	})
	if applyIndex < 0 {
		return fmt.Errorf("the schema watch holds no Apply %s on Job UID %s with a plan", want.operationID, want.jobUID)
	}
	if pendingIndex < 0 {
		return fmt.Errorf("the schema watch holds no ApplySucceeded observation of Apply %s", want.operationID)
	}
	apply, harvested := documents[applyIndex], documents[pendingIndex]
	binding, active, plan := apply.Status.ExecutionBinding, apply.Status.ActiveOperation, apply.Status.Plan
	pending := harvested.Status.PendingObservation
	switch {
	case binding == nil || !executionEpoch.MatchString(binding.Epoch):
		return errors.New("the Apply has no valid execution binding")
	case binding.ControllerStateVersion != want.stateVersion:
		return fmt.Errorf("the Apply's execution binding is of controller state %d, not %d",
			binding.ControllerStateVersion, want.stateVersion)
	case active.ExecutionBindingID != binding.Epoch:
		return fmt.Errorf("the Apply runs under binding %s, not %s", active.ExecutionBindingID, binding.Epoch)
	case plan.ExecutionBindingID != binding.Epoch || plan.ControllerImage != want.controller.image ||
		plan.ControllerRevision != want.controller.revision || plan.ControllerStateVersion != want.stateVersion:
		return errors.New("the Apply's plan is not bound to this manager's execution binding")
	case !sameJSON(harvested.Status.ExecutionBinding, binding):
		return errors.New("the harvest runs under another execution binding than the Apply")
	case active.DispatchNotAfter == nil || active.ExecutionNotAfter == nil:
		return errors.New("the Apply has no dispatch or execution deadline")
	case instantOf(*active.ExecutionNotAfter) != instantOf(*active.DispatchNotAfter):
		return fmt.Errorf("the Apply's execution deadline %s is not its dispatch deadline %s",
			instantOf(*active.ExecutionNotAfter), instantOf(*active.DispatchNotAfter))
	case active.TerminationGracePeriodSeconds != 30:
		return fmt.Errorf("the Apply has a termination grace of %d seconds, not 30", active.TerminationGracePeriodSeconds)
	case result.OperationID != active.ID:
		return fmt.Errorf("the result is of operation %s, not %s", result.OperationID, active.ID)
	case result.Error != nil:
		return fmt.Errorf("the result carries error %s", result.Error.Code)
	case result.ChildExitCode != 0:
		return fmt.Errorf("the child exited %d", result.ChildExitCode)
	case !result.MutationStarted || result.Uncertain:
		return fmt.Errorf("the result has mutationStarted %t and uncertain %t", result.MutationStarted, result.Uncertain)
	case result.PlanOutcome != "":
		return fmt.Errorf("an Apply result names plan outcome %s", result.PlanOutcome)
	case result.Truncation != nil:
		return errors.New("the result was truncated")
	case !presentIs(result.PlanContentDigest, plan.ContentDigest):
		return fmt.Errorf("the result applied content %q, not %s", result.PlanContentDigest, plan.ContentDigest)
	case result.CoordinationDigest != active.CoordinationDigest || !presentIs(result.CoordinationDigest, plan.CoordinationDigest):
		return fmt.Errorf("the result names realm %q, not the Apply's", result.CoordinationDigest)
	case result.TargetIdentityDigest != active.TargetIdentityDigest ||
		!presentIs(result.TargetIdentityDigest, plan.TargetIdentityDigest):
		return fmt.Errorf("the result names target %q, not the Apply's", result.TargetIdentityDigest)
	case !sameJSON(pending.Plan, plan):
		return errors.New("the harvested plan is not the plan the Apply ran")
	case pending.Plan.ExecutionBindingID != binding.Epoch || pending.Plan.ControllerImage != want.controller.image ||
		pending.Plan.ControllerRevision != want.controller.revision ||
		pending.Plan.ControllerStateVersion != want.stateVersion:
		return errors.New("the harvested plan is not bound to this manager's execution binding")
	case !presentIs(pending.ApplyJobName, active.JobName):
		return fmt.Errorf("the harvest names Apply Job %q, not %s", pending.ApplyJobName, active.JobName)
	case !presentIs(string(pending.ApplyJobUID), want.jobUID):
		return fmt.Errorf("the harvest names Apply Job UID %q, not %s", pending.ApplyJobUID, want.jobUID)
	case !(podEvidence{uids: []string{want.podUID}}).holds(pending.ApplyPodUIDs, pending.ApplyPodCount) ||
		pending.ApplyPodCount != 1:
		return fmt.Errorf("the harvest records Apply Pods %v counted %d, not Pod UID %s", pending.ApplyPodUIDs,
			pending.ApplyPodCount, want.podUID)
	case pending.CoordinationDigest != result.CoordinationDigest:
		return fmt.Errorf("the harvest names realm %s, not the result's", pending.CoordinationDigest)
	case pending.Plan.TargetIdentityDigest != result.TargetIdentityDigest:
		return fmt.Errorf("the harvested plan names target %s, not the result's", pending.Plan.TargetIdentityDigest)
	case pending.LeaseEpoch != active.LeaseEpoch:
		return fmt.Errorf("the harvest is at epoch %q, the Apply at %q", pending.LeaseEpoch, active.LeaseEpoch)
	case harvested.Status.Phase != ptahv1alpha1.PhaseVerifyingConvergence:
		return fmt.Errorf("the harvest is %s, not VerifyingConvergence", harvested.Status.Phase)
	case harvested.Status.Applied != nil:
		return errors.New("the harvest records an applied plan")
	case harvested.Status.PendingLockRelease != nil:
		return errors.New("the harvest asks for its lock to be released")
	case !noneConverged(harvested.Status.Conditions):
		return errors.New("the harvest is InSync or Ready, or carries no conditions")
	}
	return nil
}

// postgresDialects are the dialects a PostgreSQL Observe reports.
var postgresDialects = []string{"postgres", "postgresql"}

// cleanObserve is an Observe of the schema's target that found no drift, in
// one of the dialects given.
func cleanObserve(schema *ptahv1alpha1.PtahSchema, observe runner.Result, dialects []string) error {
	target := schema.Status.Target
	switch {
	case observe.Error != nil:
		return fmt.Errorf("the Observe result carries error %s", observe.Error.Code)
	case observe.ChildExitCode != 0:
		return fmt.Errorf("the Observe child exited %d", observe.ChildExitCode)
	case observe.Stdout != "":
		return errors.New("the Observe result carries output")
	case observe.ObservedDrift || observe.HighestDriftSeverity != "" || observe.DriftFindingCount != 0:
		return fmt.Errorf("the Observe found drift: %d findings, highest %q", observe.DriftFindingCount,
			observe.HighestDriftSeverity)
	case !slices.Contains(dialects, observe.ObservedDialect):
		return fmt.Errorf("the Observe read dialect %q, not one of %v", observe.ObservedDialect, dialects)
	case observe.CoordinationDigest != target.CoordinationDigest || observe.TargetIdentityDigest != target.IdentityDigest ||
		observe.DriftReportDigest != target.DriftReportDigest:
		return errors.New("the Observe result names another target, realm or drift report than the schema")
	}
	return nil
}

// noChangesPlan is a Plan of the schema's target that found nothing to do.
func noChangesPlan(schema *ptahv1alpha1.PtahSchema, plan runner.Result) error {
	target := schema.Status.Target
	switch {
	case plan.Error != nil:
		return fmt.Errorf("the Plan result carries error %s", plan.Error.Code)
	case plan.ChildExitCode != 0:
		return fmt.Errorf("the Plan child exited %d", plan.ChildExitCode)
	case plan.PlanOutcome != runner.PlanOutcomeNoChanges:
		return fmt.Errorf("the Plan outcome is %q, not NoChanges", plan.PlanOutcome)
	case plan.Stdout != "" || plan.PlanContentDigest != "":
		return errors.New("the NoChanges Plan result carries a plan")
	case plan.CoordinationDigest != target.CoordinationDigest || plan.TargetIdentityDigest != target.IdentityDigest:
		return errors.New("the Plan result names another target or realm than the schema")
	}
	return nil
}

// convergedAfterApply ports the final check of
// assert_fault_convergence_result_pair: InSync by ScopedConverged, the
// applied evidence bound to this manager, a clean PostgreSQL Observe and a
// NoChanges Plan, both of the schema's target.
func convergedAfterApply(schema *ptahv1alpha1.PtahSchema, observe, plan runner.Result,
	controller controllerIdentity, stateVersion int32,
) error {
	status := schema.Status
	switch {
	case status.Phase != ptahv1alpha1.PhaseInSync:
		return fmt.Errorf("the schema is %s, not InSync", status.Phase)
	case status.PendingObservation != nil || status.ActiveOperation != nil || status.Plan != nil:
		return errors.New("the schema still carries an observation, an operation or a plan")
	case status.Applied == nil || status.ExecutionBinding == nil ||
		status.Applied.ExecutionBindingID != status.ExecutionBinding.Epoch:
		return errors.New("the applied evidence is not bound to the schema's execution binding")
	case status.Applied.ControllerImage != controller.image || status.Applied.ControllerRevision != controller.revision ||
		status.Applied.ControllerStateVersion != stateVersion:
		return errors.New("the applied evidence is not this manager's")
	case !conditionIs(status.Conditions, ptahv1alpha1.ConditionInSync, metav1.ConditionTrue, "ScopedConverged"):
		return errors.New("the schema is not InSync by ScopedConverged")
	}
	if err := cleanObserve(schema, observe, postgresDialects); err != nil {
		return err
	}
	return noChangesPlan(schema, plan)
}

// principalFailedClosed ports the wait run_credential_principal_refusal makes:
// the Plan failed on its second attempt with no Job bound, and the next try
// is at least 3500 seconds after now, under a one-hour failure retry.
//
// The shell compared the stored interval with "1h" and "1h0m0s"; a typed read
// has the duration alone, so another spelling of an hour passes here. The
// phase writes "1h".
func principalFailedClosed(schema *ptahv1alpha1.PtahSchema, now time.Time) bool {
	status, active := schema.Status, schema.Status.ActiveOperation
	return schema.Spec.Execution.FailureRetryInterval.Duration == time.Hour &&
		status.Phase == ptahv1alpha1.PhaseFailed && active != nil &&
		active.Type == ptahv1alpha1.OperationPlan && active.Attempt == 2 && active.JobUID == "" &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, "OperationFailed") &&
		status.NextReconciliationTime != nil && status.NextReconciliationTime.After(now.Add(3500*time.Second))
}

// principalRefusal ports the refusal check of
// run_credential_principal_refusal: the artifact resolved and verified, no
// plan published, and the Plan result the runner's invalid_plan_output
// refusal of the active operation.
func principalRefusal(schema *ptahv1alpha1.PtahSchema, result runner.Result, artifactDigest string) error {
	status := schema.Status
	switch {
	case !presentIs(status.Source.Digest, artifactDigest) || !status.Source.Verified ||
		status.Source.ArtifactType != schemaArtifactType:
		return fmt.Errorf("the schema holds source %q verified %t of type %q, not the verified artifact %s",
			status.Source.Digest, status.Source.Verified, status.Source.ArtifactType, artifactDigest)
	case !sha256Pattern.MatchString(status.Target.DriftReportDigest):
		return errors.New("the schema holds no drift report digest")
	case status.Plan != nil:
		return errors.New("the schema published a plan")
	case status.ActiveOperation == nil || status.ActiveOperation.ID != result.OperationID:
		return fmt.Errorf("the result is of operation %s, not the schema's active one (%s)", result.OperationID,
			describeActive(status.ActiveOperation))
	case result.ChildExitCode != 0 || result.Stdout != "":
		return fmt.Errorf("the child exited %d and left output of %d bytes", result.ChildExitCode, len(result.Stdout))
	case result.PlanContentDigest != "" || result.PlanOutcome != "":
		return errors.New("the refused Plan result names a plan")
	case result.MutationStarted || result.Uncertain:
		return errors.New("the refused Plan result claims a mutation")
	case result.Truncation != nil:
		return errors.New("the result was truncated")
	case result.Error == nil || result.Error.Code != "invalid_plan_output":
		return fmt.Errorf("the result is not the invalid_plan_output refusal: %v", resultErrorCode(result))
	case result.CoordinationDigest != status.Target.CoordinationDigest ||
		result.TargetIdentityDigest != status.Target.IdentityDigest:
		return errors.New("the result names another target or realm than the schema")
	}
	return nil
}

// resultErrorCode is a result's error code, or none.
func resultErrorCode(result runner.Result) string {
	if result.Error == nil {
		return "no error"
	}
	return result.Error.Code
}

// principalSuspended is the refused schema suspended by request, idle.
func principalSuspended(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return schema.Spec.Suspend && status.Phase == ptahv1alpha1.PhaseSuspended &&
		status.ActiveOperation == nil && status.PendingObservation == nil &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionSuspended, metav1.ConditionTrue, "Requested")
}

// stalePlanResult ports the stale_plan result check the shared-alias
// contender and the manual-drift Apply are held to: exit 2, the old plan's
// digests, a mutation started and uncertain, and nothing else.
func stalePlanResult(result runner.Result, contentDigest, coordinationDigest, targetIdentityDigest string) error {
	switch {
	case result.ChildExitCode != 2:
		return fmt.Errorf("the child exited %d, not 2", result.ChildExitCode)
	case result.Stdout != "":
		return errors.New("the stale Apply result carries output")
	case !presentIs(result.PlanContentDigest, contentDigest) || !presentIs(result.CoordinationDigest, coordinationDigest) ||
		!presentIs(result.TargetIdentityDigest, targetIdentityDigest):
		return fmt.Errorf("the result names content %q, realm %q and target %q, not the old plan's",
			result.PlanContentDigest, result.CoordinationDigest, result.TargetIdentityDigest)
	case !result.MutationStarted || !result.Uncertain:
		return fmt.Errorf("the result has mutationStarted %t and uncertain %t, not both", result.MutationStarted,
			result.Uncertain)
	case result.PlanOutcome != "":
		return fmt.Errorf("an Apply result names plan outcome %s", result.PlanOutcome)
	case result.Truncation != nil:
		return errors.New("the result was truncated")
	case result.Error == nil || result.Error.Code != "stale_plan":
		return fmt.Errorf("the result is not the stale_plan refusal: %v", resultErrorCode(result))
	}
	return nil
}

// driftedObserveBound is an Observe that found drift on the schema's target,
// in one of the dialects given.
func driftedObserveBound(schema *ptahv1alpha1.PtahSchema, observe runner.Result, dialects ...string) error {
	target := schema.Status.Target
	switch {
	case observe.Error != nil:
		return fmt.Errorf("the Observe result carries error %s", observe.Error.Code)
	case observe.Stdout != "":
		return errors.New("the Observe result carries output")
	case observe.ChildExitCode != 0 && observe.ChildExitCode != 1:
		return fmt.Errorf("the Observe child exited %d", observe.ChildExitCode)
	case !observe.ObservedDrift || observe.DriftFindingCount <= 0:
		return fmt.Errorf("the Observe found no drift: drift %t, %d findings", observe.ObservedDrift,
			observe.DriftFindingCount)
	case !slices.Contains(dialects, observe.ObservedDialect):
		return fmt.Errorf("the Observe read dialect %q, not one of %v", observe.ObservedDialect, dialects)
	case observe.CoordinationDigest != target.CoordinationDigest || observe.TargetIdentityDigest != target.IdentityDigest ||
		observe.DriftReportDigest != target.DriftReportDigest:
		return errors.New("the Observe result names another target, realm or drift report than the schema")
	}
	return nil
}

// observationAdvanced is a schema that observed its target after the instant
// given, holds a drift report digest, and has no approval on its plan.
func observationAdvanced(schema *ptahv1alpha1.PtahSchema, before time.Time) error {
	status := schema.Status
	switch {
	case status.Target.LastObservedAt == nil || !status.Target.LastObservedAt.After(before):
		return fmt.Errorf("the schema last observed its target at %v, not after %s", status.Target.LastObservedAt,
			before.UTC().Format(time.RFC3339))
	case status.Plan != nil && status.Plan.Approval != nil:
		return fmt.Errorf("the schema's plan carries approval %s", status.Plan.Approval.Name)
	case !sha256Pattern.MatchString(status.Target.DriftReportDigest):
		return errors.New("the schema holds no drift report digest")
	}
	return nil
}

// freshPlanBound is a fresh plan bound to the Plan result that produced it
// and to the document rebuilt from its chunks, with one content digest
// naming the document in the result, on the schema and on the plan.
//
// A document without its fingerprint keys was null to jq and matched no
// plan; here an empty fingerprint in the document matches none either.
func freshPlanBound(schema *ptahv1alpha1.PtahSchema, plan *ptahv1alpha1.PtahSchemaPlan, result runner.Result,
	document planDocument, contentDigest string,
) error {
	spec := plan.Spec
	switch {
	case result.Error != nil:
		return fmt.Errorf("the Plan result carries error %s", result.Error.Code)
	case result.ChildExitCode != 0:
		return fmt.Errorf("the Plan child exited %d", result.ChildExitCode)
	case result.PlanOutcome != runner.PlanOutcomeChanges:
		return fmt.Errorf("the Plan outcome is %q, not Changes", result.PlanOutcome)
	case !presentIs(result.PlanContentDigest, contentDigest):
		return fmt.Errorf("the result names content %q, not the document's %s", result.PlanContentDigest, contentDigest)
	case schema.Status.Plan == nil || schema.Status.Plan.ContentDigest != contentDigest:
		return errors.New("the schema's current plan does not name the document's content digest")
	case spec.ContentDigest != contentDigest:
		return fmt.Errorf("the plan names content %s, not the document's %s", spec.ContentDigest, contentDigest)
	case !presentIs(result.CoordinationDigest, spec.CoordinationDigest) ||
		!presentIs(result.TargetIdentityDigest, spec.TargetIdentityDigest):
		return errors.New("the result names another realm or target than the plan")
	case !presentIs(document.FromFingerprint, spec.ActualStateFingerprint):
		return fmt.Errorf("the plan's actual state %s is not the document's %q", spec.ActualStateFingerprint,
			document.FromFingerprint)
	case !presentIs(document.ToFingerprint, spec.DesiredStateFingerprint):
		return fmt.Errorf("the plan's desired state %s is not the document's %q", spec.DesiredStateFingerprint,
			document.ToFingerprint)
	}
	return nil
}

// contenderRecovered ports the shared-alias contender's final check: InSync,
// idle, nothing applied or planned, through a clean PostgreSQL Observe and a
// NoChanges Plan of its target.
func contenderRecovered(schema *ptahv1alpha1.PtahSchema, observe, plan runner.Result) error {
	status := schema.Status
	switch {
	case status.Phase != ptahv1alpha1.PhaseInSync:
		return fmt.Errorf("the schema is %s, not InSync", status.Phase)
	case status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil:
		return errors.New("the schema still carries an operation, an observation or a lock")
	case status.Applied != nil || status.Plan != nil:
		return errors.New("the schema records an applied plan or a current one")
	}
	if err := cleanObserve(schema, observe, postgresDialects); err != nil {
		return err
	}
	return noChangesPlan(schema, plan)
}

// manualDriftSettled ports the schema and approval readings of
// wait_for_manual_drift_contract: a fresh unapproved plan other than the old
// one, and the old approval consumed and stale.
func manualDriftSettled(schema *ptahv1alpha1.PtahSchema, approval *ptahv1alpha1.PtahSchemaApproval, oldPlanUID string) bool {
	status := schema.Status
	return status.Plan != nil && status.Plan.Name != "" &&
		status.PendingObservation == nil && status.ActiveOperation == nil && status.PendingLockRelease == nil &&
		status.Applied == nil && status.Phase == ptahv1alpha1.PhaseAwaitingApproval &&
		string(status.Plan.UID) != oldPlanUID && status.Plan.Approval == nil &&
		conditionIs(approval.Status.Conditions, "Consumed", metav1.ConditionTrue, "DispatchCommitted") &&
		conditionIs(approval.Status.Conditions, "Stale", metav1.ConditionTrue, "PlanNoLongerCurrent")
}

// manualFreshPlan is the fresh plan manual drift produced: another UID, and
// an actual-state fingerprint other than the old one.
func manualFreshPlan(plan *ptahv1alpha1.PtahSchemaPlan, oldPlanUID, oldActual string) bool {
	return string(plan.UID) != oldPlanUID && plan.Spec.ActualStateFingerprint != oldActual &&
		sha256Pattern.MatchString(plan.Spec.ActualStateFingerprint)
}

// postApplyProof is what assert_post_apply_proof_history holds the watches
// to.
type postApplyProof struct {
	schema, applyOperationID, applyJobUID string
	lease                                 leaseIdentity
	observeJobUID, planJobUID             string
	controller                            controllerIdentity
	stateVersion                          int32
}

// postApplyProofHistory ports assert_post_apply_proof_history: the schema
// watch saw the Apply, its harvest, an Observe and a Plan of that harvest
// reading what the Apply recorded, a request to release the Apply's lock, and
// the schema InSync on the applied plan; the Job watch saw the proof Jobs in
// order; the Lease watch saw the lock held once and then released.
func postApplyProofHistory(schemas []watchEvent[*ptahv1alpha1.PtahSchema], jobs []watchEvent[*batchv1.Job],
	leases []watchEvent[*coordinationv1.Lease], want postApplyProof,
) error {
	documents := schemaDocuments(schemas, want.schema)
	epoch := want.lease.epoch
	applyIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return appliedAt(schema, want.applyOperationID, want.applyJobUID, epoch)
	})
	pendingIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return pendingFor(schema, ptahv1alpha1.PendingObservationApplySucceeded, want.applyOperationID, want.applyJobUID, epoch)
	})
	observeIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return provingWith(schema, ptahv1alpha1.OperationObserve, want.observeJobUID, want.applyOperationID, epoch)
	})
	planIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return provingWith(schema, ptahv1alpha1.OperationPlan, want.planJobUID, want.applyOperationID, epoch)
	})
	switch {
	case applyIndex < 0:
		return fmt.Errorf("the schema watch holds no Apply %s on Job UID %s at epoch %s", want.applyOperationID,
			want.applyJobUID, epoch)
	case pendingIndex < 0:
		return fmt.Errorf("the schema watch holds no ApplySucceeded harvest of Apply %s at epoch %s",
			want.applyOperationID, epoch)
	case observeIndex < 0:
		return fmt.Errorf("the schema watch holds no Observe on Job UID %s for Apply %s", want.observeJobUID,
			want.applyOperationID)
	case planIndex < 0:
		return fmt.Errorf("the schema watch holds no required Plan on Job UID %s for Apply %s", want.planJobUID,
			want.applyOperationID)
	}
	apply, harvested := documents[applyIndex], documents[pendingIndex]
	observe, plan := documents[observeIndex], documents[planIndex]
	pending, active := harvested.Status.PendingObservation, apply.Status.ActiveOperation
	releaseIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return releaseRequested(schema, want.applyOperationID, epoch, pending.CoordinationDigest)
	})
	if releaseIndex < 0 {
		return fmt.Errorf("the schema watch holds no request to release the lock of Apply %s at epoch %s",
			want.applyOperationID, epoch)
	}
	release := documents[releaseIndex]
	switch {
	case apply.Status.Applied != nil || apply.Status.PendingLockRelease != nil:
		return errors.New("the Apply document records an applied plan or a lock release")
	case !exactControllerPlan(apply.Status.Plan, apply.Status.ExecutionBinding, want.controller, want.stateVersion):
		return errors.New("the Apply's plan is not bound to this manager's execution binding")
	case !sameJSON(pending.Plan, apply.Status.Plan):
		return errors.New("the harvested plan is not the plan the Apply ran")
	case !exactControllerPlan(&pending.Plan, harvested.Status.ExecutionBinding, want.controller, want.stateVersion):
		return errors.New("the harvested plan is not bound to this manager's execution binding")
	case !presentIs(pending.ApplyJobName, active.JobName):
		return fmt.Errorf("the harvest names Apply Job %q, not %s", pending.ApplyJobName, active.JobName)
	case !presentIs(string(pending.ApplyJobUID), want.applyJobUID):
		return fmt.Errorf("the harvest names Apply Job UID %q, not %s", pending.ApplyJobUID, want.applyJobUID)
	case !presentIs(active.CoordinationDigest, pending.CoordinationDigest):
		return fmt.Errorf("the harvest names realm %s, the Apply %q", pending.CoordinationDigest, active.CoordinationDigest)
	case !sameJSON(pending.Target, active.Target) || !sameJSON(pending.Source, active.Source):
		return errors.New("the harvest records another target or source binding than the Apply")
	case !sameJSON(pending.Dev, active.ObservationDev) || !slices.Equal(pending.Exclude, active.ObservationExclude) ||
		pending.DriftSeverity != active.ObservationSeverity ||
		pending.ConnectTimeout.Duration != active.ObservationConnectTimeout.Duration ||
		pending.LockTimeout.Duration != active.ObservationLockTimeout.Duration:
		return errors.New("the harvest records other observation settings than the Apply")
	case active.LeaseDurationSeconds == 0 || pending.LeaseDurationSeconds != active.LeaseDurationSeconds:
		return fmt.Errorf("the harvest leases for %d seconds, the Apply for %d", pending.LeaseDurationSeconds,
			active.LeaseDurationSeconds)
	case active.LeaseContinuityLost:
		return errors.New("the Apply lost lease continuity")
	}
	if err := activeMatchesPending(observe.Status.ActiveOperation, observe.Status.PendingObservation); err != nil {
		return err
	}
	if err := activeMatchesPending(plan.Status.ActiveOperation, plan.Status.PendingObservation); err != nil {
		return err
	}
	for _, document := range documents {
		if document.Status.PendingObservation != nil &&
			document.Status.PendingObservation.ApplyOperationID == want.applyOperationID {
			if err := verifyingUnderBinding(document, want.controller, want.stateVersion); err != nil {
				return err
			}
		}
	}
	switch {
	case release.Status.PendingObservation != nil || release.Status.ActiveOperation != nil:
		return errors.New("the lock release was requested while an observation or an operation was still open")
	case !slices.ContainsFunc(documents, func(schema *ptahv1alpha1.PtahSchema) bool {
		return convergedOnHarvest(schema, &pending.Plan, want.controller, want.stateVersion)
	}):
		return errors.New("the schema watch never saw the schema InSync on the harvested plan")
	}
	if err := proofJobsOrdered(jobs, want.observeJobUID, observe.Status.ActiveOperation.ID, want.planJobUID,
		plan.Status.ActiveOperation.ID); err != nil {
		return err
	}
	return leaseHeldUntilRelease(leases, want.lease)
}

// convergedOnHarvest is a schema InSync by ScopedConverged with nothing open,
// its applied evidence the harvested plan's, recorded by this manager.
func convergedOnHarvest(schema *ptahv1alpha1.PtahSchema, plan *ptahv1alpha1.CurrentPlanStatus,
	controller controllerIdentity, stateVersion int32,
) bool {
	status, applied := schema.Status, schema.Status.Applied
	return status.Phase == ptahv1alpha1.PhaseInSync && status.PendingObservation == nil &&
		status.ActiveOperation == nil && status.PendingLockRelease == nil && applied != nil &&
		applied.ArtifactDigest == plan.ArtifactDigest && applied.PlanFingerprint == plan.Fingerprint &&
		applied.CoordinationDigest == plan.CoordinationDigest && applied.TargetIdentityDigest == plan.TargetIdentityDigest &&
		applied.ExecutionBindingID == plan.ExecutionBindingID && applied.ControllerImage == controller.image &&
		applied.ControllerRevision == controller.revision && applied.ControllerStateVersion == stateVersion &&
		applied.PtahVersion == plan.PtahVersion && applied.ExecutorImage == plan.ExecutorImage &&
		applied.RunnerImage == plan.RunnerImage && applied.RunnerProtocolVersion == plan.RunnerProtocolVersion &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionInSync, metav1.ConditionTrue, "ScopedConverged")
}

// uncertainMode is how an uncertain Apply's proof ends.
type uncertainMode string

const (
	// uncertainNoChanges ends InSync by ConvergedAfterUnknownOutcome with no
	// plan left.
	uncertainNoChanges uncertainMode = "no-changes"
	// uncertainSamePlan ends at a fresh plan of the same content.
	uncertainSamePlan uncertainMode = "same-plan"
	// uncertainManualDrift ends at a fresh plan of a changed actual state.
	uncertainManualDrift uncertainMode = "manual-drift"
)

// uncertainProof is what assert_uncertain_apply_proof_history holds the
// watches, the final schema and the fresh plan to.
type uncertainProof struct {
	schema, applyOperationID, applyJobUID string
	lease                                 leaseIdentity
	observeJobUID, planJobUID             string
	freshPlanUID                          string
	applyPods                             podEvidence
	mode                                  uncertainMode
	// oldActual is the actual-state fingerprint of the plan manual drift
	// retired.
	oldActual    string
	controller   controllerIdentity
	stateVersion int32
}

// pendingBindingMatches is jq's pending_binding_matches: a later document's
// pending observation is the one the uncertain Apply recorded, field for
// field.
func pendingBindingMatches(candidate, origin *ptahv1alpha1.PendingObservationStatus) error {
	switch {
	case candidate.Outcome != origin.Outcome || candidate.ApplyOperationID != origin.ApplyOperationID:
		return fmt.Errorf("a later observation records outcome %s of Apply %s, not %s of %s", candidate.Outcome,
			candidate.ApplyOperationID, origin.Outcome, origin.ApplyOperationID)
	case candidate.ApplyJobName != origin.ApplyJobName || candidate.ApplyJobUID != origin.ApplyJobUID:
		return errors.New("a later observation names another Apply Job")
	case !slices.Equal(candidate.ApplyPodUIDs, origin.ApplyPodUIDs) || candidate.ApplyPodCount != origin.ApplyPodCount:
		return errors.New("a later observation records other Apply Pod evidence")
	case candidate.ApplyGeneration != origin.ApplyGeneration:
		return fmt.Errorf("a later observation records generation %d, not %d", candidate.ApplyGeneration,
			origin.ApplyGeneration)
	case !sameJSON(candidate.ObserveAfter, origin.ObserveAfter):
		return errors.New("a later observation observes after another instant")
	case !sameJSON(candidate.Plan, origin.Plan):
		return errors.New("a later observation records another plan")
	case !sameJSON(candidate.Target, origin.Target) || candidate.CoordinationDigest != origin.CoordinationDigest ||
		!sameJSON(candidate.Source, origin.Source):
		return errors.New("a later observation records another target, realm or source")
	case !sameJSON(candidate.Dev, origin.Dev) || !slices.Equal(candidate.Exclude, origin.Exclude) ||
		candidate.DriftSeverity != origin.DriftSeverity ||
		candidate.ConnectTimeout.Duration != origin.ConnectTimeout.Duration ||
		candidate.LockTimeout.Duration != origin.LockTimeout.Duration:
		return errors.New("a later observation records other observation settings")
	case candidate.LeaseDurationSeconds != origin.LeaseDurationSeconds || candidate.LeaseEpoch != origin.LeaseEpoch:
		return errors.New("a later observation records another lease")
	}
	return nil
}

// finalAwaitsFresh is jq's final_awaits_fresh: the final schema waits for a
// person on the fresh plan, and its current plan is that plan, field for
// field, published by this manager.
func finalAwaitsFresh(final *ptahv1alpha1.PtahSchema, fresh *ptahv1alpha1.PtahSchemaPlan, uid string,
	controller controllerIdentity, stateVersion int32,
) error {
	if fresh == nil {
		return errors.New("there is no fresh plan to compare the final schema with")
	}
	status, current, spec := final.Status, final.Status.Plan, fresh.Spec
	switch {
	case status.Phase != ptahv1alpha1.PhaseAwaitingApproval:
		return fmt.Errorf("the final schema is %s, not AwaitingApproval", status.Phase)
	case status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil ||
		status.Applied != nil:
		return errors.New("the final schema still carries an operation, an observation, a lock or an applied plan")
	case current == nil || string(current.UID) != uid || current.Name != fresh.Name || current.UID != fresh.UID:
		return fmt.Errorf("the final schema's plan is not the fresh plan %s UID %s", fresh.Name, uid)
	case current.Approval != nil:
		return errors.New("the fresh plan carries an approval")
	case spec.SchemaRef.Name != final.Name || spec.SchemaRef.UID != final.UID:
		return errors.New("the fresh plan belongs to another schema")
	case current.Fingerprint != spec.Fingerprint || current.ContentDigest != spec.ContentDigest ||
		current.ArtifactDigest != spec.ArtifactDigest || current.CoordinationDigest != spec.CoordinationDigest ||
		current.TargetIdentityDigest != spec.TargetIdentityDigest ||
		current.ActualStateFingerprint != spec.ActualStateFingerprint ||
		current.DesiredStateFingerprint != spec.DesiredStateFingerprint ||
		current.PolicyFingerprint != spec.PolicyFingerprint ||
		current.VerificationPolicyUID != spec.VerificationPolicyUID ||
		current.VerificationPolicyDigest != spec.VerificationPolicyDigest:
		return errors.New("the final schema's plan disagrees with the fresh plan's digests")
	case spec.ContractVersion != 3:
		return fmt.Errorf("the fresh plan is contract version %d", spec.ContractVersion)
	case !exactControllerPlan(current, status.ExecutionBinding, controller, stateVersion):
		return errors.New("the final schema's plan is not bound to this manager's execution binding")
	case current.ExecutionBindingID != spec.ExecutionBindingID || current.ControllerImage != spec.ControllerImage ||
		current.ControllerRevision != spec.ControllerRevision ||
		current.ControllerStateVersion != spec.ControllerStateVersion || current.PtahVersion != spec.PtahVersion ||
		current.ExecutorImage != spec.ExecutorImage || current.RunnerImage != spec.RunnerImage ||
		current.RunnerProtocolVersion != spec.RunnerProtocolVersion:
		return errors.New("the final schema's plan disagrees with the fresh plan's execution identity")
	case current.Destructive != spec.Destructive || current.StatementCount != spec.StatementCount:
		return errors.New("the final schema's plan disagrees with the fresh plan's statements")
	case instantOf(current.CreatedAt) != instantOf(fresh.CreationTimestamp):
		return fmt.Errorf("the final schema's plan was created at %s, the fresh plan at %s", instantOf(current.CreatedAt),
			instantOf(fresh.CreationTimestamp))
	}
	return nil
}

// immutablePlanInputsMatch is jq's immutable_plan_inputs_match: the fresh
// plan was made from the same artifact, realm, target, desired state, policy
// and execution identity as the plan the uncertain Apply ran.
func immutablePlanInputsMatch(fresh *ptahv1alpha1.PtahSchemaPlan, old ptahv1alpha1.CurrentPlanStatus) bool {
	spec := fresh.Spec
	return spec.ArtifactDigest == old.ArtifactDigest && spec.CoordinationDigest == old.CoordinationDigest &&
		spec.TargetIdentityDigest == old.TargetIdentityDigest &&
		spec.DesiredStateFingerprint == old.DesiredStateFingerprint &&
		spec.PolicyFingerprint == old.PolicyFingerprint && spec.VerificationPolicyUID == old.VerificationPolicyUID &&
		spec.VerificationPolicyDigest == old.VerificationPolicyDigest &&
		spec.ExecutionBindingID == old.ExecutionBindingID && spec.ControllerImage == old.ControllerImage &&
		spec.ControllerRevision == old.ControllerRevision && spec.ControllerStateVersion == old.ControllerStateVersion &&
		spec.PtahVersion == old.PtahVersion && spec.ExecutorImage == old.ExecutorImage &&
		spec.RunnerImage == old.RunnerImage && spec.RunnerProtocolVersion == old.RunnerProtocolVersion
}

// uncertainApplyProofHistory ports assert_uncertain_apply_proof_history. fresh
// is nil in the no-changes mode.
//
// The schema watch saw the Apply, then the outcome recorded as unknown, then
// an Observe and after it a required Plan carrying that record unchanged,
// then a request to release the Apply's lock; the Job watch saw the proof
// Jobs in order; the Lease watch saw the lock held once and then released;
// and the final schema ended the way the mode says, never having recorded an
// applied plan.
func uncertainApplyProofHistory(schemas []watchEvent[*ptahv1alpha1.PtahSchema], jobs []watchEvent[*batchv1.Job],
	leases []watchEvent[*coordinationv1.Lease], want uncertainProof,
	final *ptahv1alpha1.PtahSchema, fresh *ptahv1alpha1.PtahSchemaPlan,
) error {
	documents := schemaDocuments(schemas, want.schema)
	epoch := want.lease.epoch
	applyIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return appliedAt(schema, want.applyOperationID, want.applyJobUID, epoch)
	})
	unknownIndex := firstDocument(documents, 0, func(schema *ptahv1alpha1.PtahSchema) bool {
		return pendingFor(schema, ptahv1alpha1.PendingObservationOutcomeUnknown, want.applyOperationID, want.applyJobUID, epoch)
	})
	if applyIndex < 0 {
		return fmt.Errorf("the schema watch holds no Apply %s on Job UID %s at epoch %s", want.applyOperationID,
			want.applyJobUID, epoch)
	}
	if unknownIndex < 0 {
		return fmt.Errorf("the schema watch holds no OutcomeUnknown record of Apply %s at epoch %s",
			want.applyOperationID, epoch)
	}
	observeIndex := firstDocument(documents, unknownIndex+1, func(schema *ptahv1alpha1.PtahSchema) bool {
		return provingWith(schema, ptahv1alpha1.OperationObserve, want.observeJobUID, want.applyOperationID, epoch)
	})
	if observeIndex < 0 {
		return fmt.Errorf("the schema watch holds no Observe on Job UID %s after the OutcomeUnknown record",
			want.observeJobUID)
	}
	planIndex := firstDocument(documents, observeIndex+1, func(schema *ptahv1alpha1.PtahSchema) bool {
		return provingWith(schema, ptahv1alpha1.OperationPlan, want.planJobUID, want.applyOperationID, epoch)
	})
	if planIndex < 0 {
		return fmt.Errorf("the schema watch holds no required Plan on Job UID %s after the Observe", want.planJobUID)
	}
	apply, unknown := documents[applyIndex], documents[unknownIndex]
	observe, plan := documents[observeIndex], documents[planIndex]
	origin, active := unknown.Status.PendingObservation, apply.Status.ActiveOperation
	releaseIndex := firstDocument(documents, planIndex+1, func(schema *ptahv1alpha1.PtahSchema) bool {
		return releaseRequested(schema, want.applyOperationID, epoch, origin.CoordinationDigest)
	})
	if releaseIndex < 0 {
		return fmt.Errorf("the schema watch holds no request to release the lock of Apply %s after the Plan",
			want.applyOperationID)
	}
	release := documents[releaseIndex]
	switch {
	case !exactControllerPlan(apply.Status.Plan, apply.Status.ExecutionBinding, want.controller, want.stateVersion):
		return errors.New("the Apply's plan is not bound to this manager's execution binding")
	case !exactControllerPlan(&origin.Plan, unknown.Status.ExecutionBinding, want.controller, want.stateVersion):
		return errors.New("the uncertain record's plan is not bound to this manager's execution binding")
	case active.DispatchNotAfter == nil:
		return errors.New("the Apply has no dispatch deadline")
	case active.ExecutionNotAfter == nil || instantOf(*active.ExecutionNotAfter) != instantOf(*active.DispatchNotAfter):
		return errors.New("the Apply's execution deadline is not its dispatch deadline")
	case active.TerminationGracePeriodSeconds != 30:
		return fmt.Errorf("the Apply has a termination grace of %d seconds, not 30", active.TerminationGracePeriodSeconds)
	case !presentIs(origin.ApplyJobName, active.JobName):
		return fmt.Errorf("the uncertain record names Apply Job %q, not %s", origin.ApplyJobName, active.JobName)
	case !presentIs(string(origin.ApplyJobUID), want.applyJobUID):
		return fmt.Errorf("the uncertain record names Apply Job UID %q, not %s", origin.ApplyJobUID, want.applyJobUID)
	case !want.applyPods.holds(origin.ApplyPodUIDs, origin.ApplyPodCount):
		return fmt.Errorf("the uncertain record holds Apply Pods %v counted %d, not %v (optional %t)",
			origin.ApplyPodUIDs, origin.ApplyPodCount, want.applyPods.uids, want.applyPods.optional)
	case unknown.Status.Phase != ptahv1alpha1.PhaseVerifyingConvergence:
		return fmt.Errorf("the uncertain record is %s, not VerifyingConvergence", unknown.Status.Phase)
	case unknown.Status.Applied != nil || unknown.Status.PendingLockRelease != nil:
		return errors.New("the uncertain record carries an applied plan or a lock release")
	}
	for _, later := range []*ptahv1alpha1.PtahSchema{observe, plan} {
		if err := pendingBindingMatches(later.Status.PendingObservation, origin); err != nil {
			return err
		}
	}
	for _, later := range []*ptahv1alpha1.PtahSchema{observe, plan} {
		if err := activeMatchesPending(later.Status.ActiveOperation, later.Status.PendingObservation); err != nil {
			return err
		}
	}
	for _, document := range []*ptahv1alpha1.PtahSchema{unknown, observe, plan} {
		if err := verifyingUnderBinding(document, want.controller, want.stateVersion); err != nil {
			return err
		}
	}
	if release.Status.PendingObservation != nil || release.Status.ActiveOperation != nil {
		return errors.New("the lock release was requested while an observation or an operation was still open")
	}
	if err := proofJobsOrdered(jobs, want.observeJobUID, observe.Status.ActiveOperation.ID, want.planJobUID,
		plan.Status.ActiveOperation.ID); err != nil {
		return err
	}
	if err := leaseHeldUntilRelease(leases, want.lease); err != nil {
		return err
	}
	if final == nil {
		return errors.New("there is no final schema to hold to the proof's end")
	}
	switch want.mode {
	case uncertainNoChanges:
		status := final.Status
		switch {
		case status.Phase != ptahv1alpha1.PhaseInSync || status.ActiveOperation != nil:
			return fmt.Errorf("the final schema is %s with %s, not InSync and idle", status.Phase,
				describeActive(status.ActiveOperation))
		case !exactControllerBinding(status.ExecutionBinding, want.stateVersion):
			return errors.New("the final schema has no valid execution binding of this controller state")
		case status.PendingObservation != nil || status.PendingLockRelease != nil || status.Applied != nil ||
			status.Plan != nil:
			return errors.New("the final schema still carries an observation, a lock, an applied plan or a plan")
		case !conditionIs(status.Conditions, ptahv1alpha1.ConditionInSync, metav1.ConditionTrue,
			"ConvergedAfterUnknownOutcome"):
			return errors.New("the final schema is not InSync by ConvergedAfterUnknownOutcome")
		}
	case uncertainSamePlan:
		if err := finalAwaitsFresh(final, fresh, want.freshPlanUID, want.controller, want.stateVersion); err != nil {
			return err
		}
		spec := fresh.Spec
		switch {
		case spec.Fingerprint != origin.Plan.Fingerprint || spec.ContentDigest != origin.Plan.ContentDigest ||
			spec.ActualStateFingerprint != origin.Plan.ActualStateFingerprint:
			return errors.New("the fresh plan is not the plan the uncertain Apply ran")
		case spec.Destructive != origin.Plan.Destructive || spec.StatementCount != origin.Plan.StatementCount:
			return errors.New("the fresh plan's statements differ from the plan the uncertain Apply ran")
		case !immutablePlanInputsMatch(fresh, origin.Plan):
			return errors.New("the fresh plan was made from other inputs than the plan the uncertain Apply ran")
		}
	case uncertainManualDrift:
		if err := finalAwaitsFresh(final, fresh, want.freshPlanUID, want.controller, want.stateVersion); err != nil {
			return err
		}
		switch {
		case want.oldActual == "" || origin.Plan.ActualStateFingerprint != want.oldActual:
			return fmt.Errorf("the uncertain Apply ran a plan of actual state %s, not the old %q",
				origin.Plan.ActualStateFingerprint, want.oldActual)
		case fresh.Spec.ActualStateFingerprint == want.oldActual:
			return errors.New("the fresh plan was made from the old actual state")
		case !immutablePlanInputsMatch(fresh, origin.Plan):
			return errors.New("the fresh plan was made from other inputs than the plan the uncertain Apply ran")
		}
	default:
		return fmt.Errorf("unsupported uncertain proof mode %q", want.mode)
	}
	if slices.ContainsFunc(documents, func(schema *ptahv1alpha1.PtahSchema) bool { return schema.Status.Applied != nil }) {
		return errors.New("the schema watch saw an applied plan recorded for the uncertain Apply")
	}
	return nil
}
