package e2e

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// storedSpec is what a schema stores for the two fields a blocked-refresh
// proof reads as written: the interval as the string the phase set, and
// suspend as the value the phase set, absent when nothing did.
type storedSpec struct {
	interval string
	suspend  *bool
}

// blockedRefreshHeadroom is how far ahead of now a blocked schema's persisted
// deadline has to be: two thirds of the interval when the boundary is first
// captured, so the checkpoint taken after it cannot straddle the next cycle,
// and half of it when the capture is read back.
func blockedRefreshHeadroom(intervalSeconds int64) (capture, postCheckpoint int64) {
	return (intervalSeconds*2 + 2) / 3, intervalSeconds / 2
}

// blockedAtGeneration is a schema resumed at the blocked refresh interval that
// settled Blocked at the generation given, with nothing in flight and a
// refresh deadline persisted.
func blockedAtGeneration(schema *ptahv1alpha1.PtahSchema, stored storedSpec, generation int64) bool {
	status := schema.Status
	return schema.Generation == generation && status.ObservedGeneration == generation &&
		stored.suspend != nil && !*stored.suspend && stored.interval == blockedRefreshInterval &&
		status.Phase == ptahv1alpha1.PhaseBlocked && status.ActiveOperation == nil &&
		status.PendingObservation == nil && status.PendingLockRelease == nil &&
		status.NextReconciliationTime != nil
}

// aheadBy reports whether the deadline is at least headroom seconds after now,
// both counted in whole seconds as the API stores them.
func aheadBy(deadline *metav1.Time, now time.Time, headroom int64) bool {
	return deadline != nil && deadline.Unix()-now.Unix() >= headroom
}

// blockedRefreshBoundary is the reading the destructive gate starts from: the
// schema blocked at the generation, its deadline far enough ahead, and the
// refusal telling the person stopped here both ways out.
func blockedRefreshBoundary(schema *ptahv1alpha1.PtahSchema, stored storedSpec, generation int64, now time.Time,
	headroom int64,
) bool {
	return blockedAtGeneration(schema, stored, generation) &&
		aheadBy(schema.Status.NextReconciliationTime, now, headroom) &&
		slices.ContainsFunc(schema.Status.Conditions, func(condition metav1.Condition) bool {
			// Section 6 asks a refusal for a direction, not only a cause: a
			// person stopped here has to be told the two ways out.
			return condition.Type == "ApprovalRequired" && condition.Status == metav1.ConditionFalse &&
				condition.Reason == "DestructiveChangesDisabled" && strings.Contains(condition.Message, "allowDestructive")
		})
}

// blockedBoundaryStable is the boundary read back after the checkpoint: the
// same persisted deadline, still far enough ahead that no cycle started in
// between.
func blockedBoundaryStable(schema *ptahv1alpha1.PtahSchema, stored storedSpec, generation int64, deadline metav1.Time,
	now time.Time, headroom int64,
) bool {
	next := schema.Status.NextReconciliationTime
	return blockedAtGeneration(schema, stored, generation) && next != nil && next.Equal(&deadline) &&
		aheadBy(next, now, headroom) &&
		conditionIs(schema.Status.Conditions, "ApprovalRequired", metav1.ConditionFalse, "DestructiveChangesDisabled")
}

// chainRank is where an operation falls in one blocked refresh chain.
func chainRank(operation string) int {
	switch operation {
	case "resolve":
		return 0
	case "verify":
		return 1
	case "observe":
		return 2
	default:
		return 3
	}
}

// orderedRefreshChains holds the schema's Jobs since the refresh checkpoint to
// exactly three read-only chains, in order, each Resolve at least one interval
// after the one before it. The Jobs are dated by their own creation time,
// which is when the controller started each chain, rather than by when the
// proof noticed them.
func orderedRefreshChains(records []observedJob, intervalSeconds int64) error {
	sorted := slices.Clone(records)
	slices.SortStableFunc(sorted, func(a, b observedJob) int {
		return cmp.Or(cmp.Compare(a.Created, b.Created), cmp.Compare(chainRank(a.Operation), chainRank(b.Operation)),
			cmp.Compare(a.UID, b.UID))
	})
	var operations []string
	var resolves []time.Time
	for _, record := range sorted {
		operations = append(operations, record.Operation)
		if record.Operation == "resolve" {
			created, err := time.Parse(time.RFC3339, record.Created)
			if err != nil {
				return fmt.Errorf("Resolve Job %s has no creation time: %w", record.Name, err)
			}
			resolves = append(resolves, created)
		}
	}
	chain := []string{"resolve", "verify", "observe", "plan"}
	want := slices.Concat(chain, chain, chain)
	if !slices.Equal(operations, want) {
		return fmt.Errorf("the Jobs since the checkpoint ran %v, not three ordered chains", operations)
	}
	interval := time.Duration(intervalSeconds) * time.Second
	for index := 1; index < len(resolves); index++ {
		if resolves[index].Sub(resolves[index-1]) < interval {
			return fmt.Errorf("Resolve %d started %s after the one before it, inside the %s interval",
				index+1, resolves[index].Sub(resolves[index-1]), interval)
		}
	}
	return nil
}

// blockedDestructiveSettled is a schema blocked by the destructive gate with
// nothing in flight and a refresh deadline persisted.
func blockedDestructiveSettled(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseBlocked && status.ActiveOperation == nil &&
		status.PendingObservation == nil && status.PendingLockRelease == nil && status.NextReconciliationTime != nil &&
		conditionIs(status.Conditions, "ApprovalRequired", metav1.ConditionFalse, "DestructiveChangesDisabled")
}

// blockedPlanRetained holds a blocked schema to the destructive plan it was
// blocked on: the same digest, the same plan by name, UID and fingerprint.
func blockedPlanRetained(schema *ptahv1alpha1.PtahSchema, plan, planUID, fingerprint, digest string) error {
	status := schema.Status
	switch {
	case status.Phase != ptahv1alpha1.PhaseBlocked:
		return fmt.Errorf("the schema is %s, not Blocked", status.Phase)
	case status.Source.Digest != digest:
		return fmt.Errorf("the schema reads digest %s", status.Source.Digest)
	case status.Plan == nil || status.Plan.Name != plan || string(status.Plan.UID) != planUID ||
		status.Plan.Fingerprint != fingerprint || !status.Plan.Destructive:
		return errors.New("the schema names another plan, or a plan that is not destructive")
	}
	return nil
}

// destructivePlanRetained holds the destructive plan itself to what it was
// when it was published.
func destructivePlanRetained(plan *ptahv1alpha1.PtahSchemaPlan, uid, fingerprint, digest string) error {
	if string(plan.UID) != uid || plan.Spec.Fingerprint != fingerprint || plan.Spec.ArtifactDigest != digest ||
		!plan.Spec.Destructive {
		return errors.New("the plan's identity, fingerprint, artifact or destructiveness changed")
	}
	return nil
}

// blockedCadenceSettled is a blocked schema that observed its current
// generation, with nothing in flight and a refresh deadline persisted.
func blockedCadenceSettled(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.ObservedGeneration == schema.Generation && status.Phase == ptahv1alpha1.PhaseBlocked &&
		status.ActiveOperation == nil && status.PendingObservation == nil && status.PendingLockRelease == nil &&
		status.NextReconciliationTime != nil
}

// quietCadenceRestored holds a schema moved back to the quiescent interval to
// the blocked evidence it had before.
func quietCadenceRestored(schema *ptahv1alpha1.PtahSchema, interval, plan, planUID, fingerprint, digest string) error {
	if interval != quiescentInterval {
		return fmt.Errorf("the schema stores interval %q", interval)
	}
	return blockedPlanRetained(schema, plan, planUID, fingerprint, digest)
}

// blockedQuiet is a blocked schema with nothing in flight.
func blockedQuiet(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseBlocked && status.ActiveOperation == nil &&
		status.PendingObservation == nil && status.PendingLockRelease == nil
}

// mysqlDestructiveRetained holds the MySQL schema, long after the lifecycle
// moved on, to still refusing its DROP INDEX plan: the same digest and plan,
// no approval recorded on it, nothing in flight, and both refusing conditions.
func mysqlDestructiveRetained(schema *ptahv1alpha1.PtahSchema, plan, planUID, digest string) error {
	status := schema.Status
	switch {
	case !blockedQuiet(schema) || status.Source.Digest != digest:
		return errors.New("the schema is not quietly blocked on the same digest")
	case status.Plan == nil || status.Plan.Name != plan || string(status.Plan.UID) != planUID ||
		!status.Plan.Destructive || status.Plan.Approval != nil:
		return errors.New("the schema names another plan, a plan that is not destructive, or an approved one")
	case !conditionIs(status.Conditions, "ApprovalRequired", metav1.ConditionFalse, "DestructiveChangesDisabled"):
		return errors.New("ApprovalRequired is not False for DestructiveChangesDisabled")
	case !conditionStatus(status.Conditions, "Ready", metav1.ConditionFalse):
		return errors.New("the schema is Ready")
	}
	return nil
}

func mysqlDestructivePlan(plan *ptahv1alpha1.PtahSchemaPlan, uid string) error {
	if string(plan.UID) != uid || !plan.Spec.Destructive || plan.Spec.StatementCount != 1 {
		return errors.New("the plan is not the one destructive statement it was")
	}
	return nil
}

// readOnlyChain holds the schema's Jobs between two checkpoints to exactly one
// Resolve, Verify, Observe and Plan and nothing else, each completed without
// failing, each started no earlier than the one before it completed.
func readOnlyChain(jobs []batchv1.Job, before, after checkpoint) error {
	var bounded []*batchv1.Job
	for index := range jobs {
		uid := string(jobs[index].UID)
		if after.holds(uid) && !before.holds(uid) {
			bounded = append(bounded, &jobs[index])
		}
	}
	chain := make([]*batchv1.Job, 0, 4)
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		var matches []*batchv1.Job
		for _, job := range bounded {
			if job.Labels[labelOperation] == operation {
				matches = append(matches, job)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("the boundary holds %d %s Jobs", len(matches), operation)
		}
		chain = append(chain, matches[0])
	}
	if len(bounded) != 4 {
		return fmt.Errorf("the boundary holds %d Jobs, not the four of one read-only chain", len(bounded))
	}
	for index, job := range chain {
		if !jobComplete(job) {
			return fmt.Errorf("%s did not complete", job.Name)
		}
		if index > 0 && !notAfter(chain[index-1].Status.CompletionTime, job.Status.StartTime) {
			return fmt.Errorf("%s started before %s completed", job.Name, chain[index-1].Name)
		}
	}
	return nil
}

// notAfter is jq's a <= b over two timestamps the API renders, where a missing
// one sorts before any time.
func notAfter(a, b *metav1.Time) bool {
	switch {
	case a == nil:
		return true
	case b == nil:
		return false
	}
	return !a.After(b.Time)
}

// registryRefreshFailed is the reading a registry outage leaves: one failed
// Resolve retry with no Job behind it, a refresh scheduled, freshness unknown
// rather than lost, the last verification still standing, and the failure on
// both failure conditions.
func registryRefreshFailed(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	active := status.ActiveOperation
	conditions := status.Conditions
	return status.Phase == ptahv1alpha1.PhaseFailed &&
		active != nil && active.Type == ptahv1alpha1.OperationResolve && active.Attempt == 2 && active.JobUID == "" &&
		status.NextReconciliationTime != nil &&
		conditionIs(conditions, "ArtifactResolved", metav1.ConditionUnknown, "RefreshFailed") &&
		conditionIs(conditions, "ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied") &&
		conditionIs(conditions, "PlanReady", metav1.ConditionUnknown, "SourceFreshnessUnknown") &&
		conditionIs(conditions, "InSync", metav1.ConditionUnknown, "SourceFreshnessUnknown") &&
		conditionIs(conditions, "Ready", metav1.ConditionFalse, "OperationFailed") &&
		conditionIs(conditions, "ReconciliationFailed", metav1.ConditionTrue, "OperationFailed")
}

// outageEvidence is what a registry outage must leave untouched, read from
// the schema and its applied plan as the API stored them: the execution
// binding, the source, the target, the current plan and the plan object's
// identity, spec and status, the applied evidence and the last success. The
// documents are compared whole, so a field the typed API would drop still
// counts.
func outageEvidence(schema, plan map[string]any) ([]byte, error) {
	status, _ := schema["status"].(map[string]any)
	metadata, _ := plan["metadata"].(map[string]any)
	return json.Marshal(map[string]any{
		"executionBinding": status["executionBinding"],
		"source":           status["source"],
		"target":           status["target"],
		"plan": map[string]any{
			"current": status["plan"],
			"resource": map[string]any{
				"name":              metadata["name"],
				"uid":               metadata["uid"],
				"generation":        metadata["generation"],
				"creationTimestamp": metadata["creationTimestamp"],
				"spec":              plan["spec"],
				"status":            plan["status"],
			},
		},
		"applied":                      status["applied"],
		"lastSuccessfulReconciliation": status["lastSuccessfulReconciliation"],
	})
}

// jsonValue is a document as plain JSON values, so two readings compare
// whatever types they were decoded into.
func jsonValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	return decoded, json.Unmarshal(encoded, &decoded)
}

// sameJSON reports whether two documents are the same JSON value.
func sameJSON(a, b any) bool {
	left, errLeft := jsonValue(a)
	right, errRight := jsonValue(b)
	return errLeft == nil && errRight == nil && reflect.DeepEqual(left, right)
}

// outageBaseline is the fresh success a registry outage starts from: a
// converged schema that applied the plan at this Ptah version through this
// manager, retries a failure after 45 seconds, and holds its resolve, verify
// and sync conditions.
func outageBaseline(schema *ptahv1alpha1.PtahSchema, failureRetry, digest, fingerprint, ptahVersion string,
	controller controllerIdentity, stateVersion int32,
) error {
	status := schema.Status
	switch {
	case failureRetry != "45s":
		return fmt.Errorf("the schema retries a failure after %q", failureRetry)
	case status.Phase != ptahv1alpha1.PhaseInSync || status.Source.Digest != digest ||
		status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil || status.Plan != nil:
		return errors.New("the schema is not quietly InSync on the digest")
	case status.Applied == nil || status.Applied.ArtifactDigest != digest || status.Applied.PlanFingerprint != fingerprint ||
		status.Applied.PtahVersion != ptahVersion:
		return errors.New("the applied evidence names another artifact, plan or Ptah version")
	}
	if err := appliedBound(schema, controller, stateVersion); err != nil {
		return err
	}
	switch {
	case status.LastSuccessfulReconciliation == nil:
		return errors.New("the schema records no successful reconciliation")
	case !conditionIs(status.Conditions, "ArtifactResolved", metav1.ConditionTrue, "DigestPinned"),
		!conditionIs(status.Conditions, "ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied"),
		!conditionIs(status.Conditions, "InSync", metav1.ConditionTrue, "ScopedConverged"):
		return errors.New("the schema does not hold its resolve, verify and sync conditions")
	}
	return nil
}

// outageBaselinePlan is the applied plan the outage must leave in place.
func outageBaselinePlan(plan *ptahv1alpha1.PtahSchemaPlan, uid, planFingerprint, digest, ptahVersion string,
	controller controllerIdentity, stateVersion int32,
) error {
	spec := plan.Spec
	switch {
	case string(plan.UID) != uid || spec.Fingerprint != planFingerprint || spec.ArtifactDigest != digest ||
		spec.PtahVersion != ptahVersion || spec.ContractVersion != fingerprint.CurrentPlanContractVersion:
		return errors.New("the plan is not the applied one")
	case !executionEpoch.MatchString(spec.ExecutionBindingID) || spec.ControllerImage != controller.image ||
		spec.ControllerRevision != controller.revision || spec.ControllerStateVersion != stateVersion:
		return errors.New("the plan is not bound to this manager")
	case !conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue):
		return errors.New("the plan is not Ready")
	}
	return nil
}

// outageResolveFailure is the one Resolve an outage lets run: it failed, said
// why, printed nothing, and touched nothing.
func outageResolveFailure(result runner.Result) error {
	if result.ChildExitCode == 0 || result.Error == nil || result.Stdout != "" || result.MutationStarted ||
		result.Uncertain || result.Truncation != nil {
		return errors.New("the Resolve is not one exact read-only failure")
	}
	return nil
}

// retainedFields reads the execution binding and the applied evidence out of
// an outage evidence document.
func retainedFields(evidence []byte) (binding, applied any, err error) {
	var document map[string]any
	if err := json.Unmarshal(evidence, &document); err != nil {
		return nil, nil, err
	}
	return document["executionBinding"], document["applied"], nil
}

// restoredNoOp is the reading a recovered registry leaves: the schema converged
// again on the same digest without an Apply, with the execution binding and
// applied evidence exactly as they were before the outage, and every condition
// back to a fresh no-op.
func restoredNoOp(schema *ptahv1alpha1.PtahSchema, document map[string]any, retained []byte, digest string,
	controller controllerIdentity, stateVersion int32,
) error {
	binding, applied, err := retainedFields(retained)
	if err != nil {
		return err
	}
	status := schema.Status
	stored, _ := document["status"].(map[string]any)
	conditions := status.Conditions
	switch {
	case status.Phase != ptahv1alpha1.PhaseInSync || status.Source.Digest != digest:
		return errors.New("the schema is not InSync on the digest")
	case !sameJSON(stored["executionBinding"], binding) || !sameJSON(stored["applied"], applied):
		return errors.New("the execution binding or the applied evidence moved")
	case status.ExecutionBinding == nil || status.ExecutionBinding.ControllerStateVersion != stateVersion ||
		status.Applied == nil || status.Applied.ControllerImage != controller.image ||
		status.Applied.ControllerRevision != controller.revision || status.Applied.ControllerStateVersion != stateVersion:
		return errors.New("the applied evidence is not this manager's")
	case status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil || status.Plan != nil:
		return errors.New("the schema carries an operation, an observation, a lock or a plan")
	case !conditionIs(conditions, "ArtifactResolved", metav1.ConditionTrue, "DigestPinned"),
		!conditionIs(conditions, "ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied"),
		!conditionIs(conditions, "PlanReady", metav1.ConditionFalse, "NoChanges"),
		!conditionIs(conditions, "InSync", metav1.ConditionTrue, "ScopedConverged"),
		!conditionIs(conditions, "Ready", metav1.ConditionTrue, "InSync"),
		!conditionIs(conditions, "ReconciliationFailed", metav1.ConditionFalse, "Succeeded"):
		return errors.New("the conditions are not a fresh no-op")
	}
	return nil
}

// recoveredPlan is the applied plan the recovery must leave in place.
func recoveredPlan(plan *ptahv1alpha1.PtahSchemaPlan, uid, fingerprint, digest string) error {
	if string(plan.UID) != uid || plan.Spec.Fingerprint != fingerprint || plan.Spec.ArtifactDigest != digest ||
		!conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue) {
		return errors.New("the plan is not the applied one, or not Ready")
	}
	return nil
}

// digestPinRefusedCondition is ArtifactVerified False for a policy that
// requires the requested reference to be pinned.
func digestPinRefusedCondition(conditions []metav1.Condition) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == "ArtifactVerified" && condition.Status == metav1.ConditionFalse &&
			condition.Reason == "PolicyRefused" && strings.Contains(condition.Message, "require_digest_pin")
	})
}

// digestPinRefused is a schema the digest-pin policy stopped: blocked, with
// nothing in flight.
func digestPinRefused(schema *ptahv1alpha1.PtahSchema) bool {
	return blockedQuiet(schema) && digestPinRefusedCondition(schema.Status.Conditions)
}

// digestPinResolveResult is the Resolve that read the mutable reference
// through the Docker config credential: it resolved the digest and touched
// nothing.
func digestPinResolveResult(result runner.Result, digest, resolved string) error {
	if result.ChildExitCode != 0 || result.Stdout != "" || result.Error != nil || result.ResolvedDigest != digest ||
		result.ResolvedReference != resolved || result.MutationStarted || result.Uncertain || result.Truncation != nil {
		return errors.New("the Resolve did not complete through DockerConfigJSON access")
	}
	return nil
}

// digestPinSourceEvidence is the schema after the refusal: the source it
// resolved is recorded, it is not verified, the policy that refused it is
// named by digest, and nothing reached the database.
func digestPinSourceEvidence(schema *ptahv1alpha1.PtahSchema, requested, resolved, digest, policyDigest string) error {
	status := schema.Status
	source := status.Source
	switch {
	case status.Phase != ptahv1alpha1.PhaseBlocked:
		return fmt.Errorf("the schema is %s, not Blocked", status.Phase)
	case source.RequestedReference != requested || source.ResolvedReference != resolved || source.Digest != digest:
		return errors.New("the source is not the resolved requested reference")
	case source.MediaType == "" || source.Size <= 0:
		return errors.New("the source carries no media type or size")
	case source.ArtifactType != "" || source.Verified:
		return errors.New("the source was verified")
	case source.VerificationPolicyDigest != policyDigest:
		return errors.New("the source names another verification policy")
	case status.Plan != nil || status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil:
		return errors.New("the schema reached database work")
	case !digestPinRefusedCondition(status.Conditions):
		return errors.New("ArtifactVerified does not name the digest-pin refusal")
	}
	return nil
}

// digestPinVerifyResult is the Verify the runner refused: the policy by
// digest, the one requirement it failed, the refusal code, and nothing about
// the artifact past its digest.
func digestPinVerifyResult(result runner.Result, digest, policyDigest string) error {
	switch {
	case result.ChildExitCode != 0 || result.Stdout != "" || result.ResolvedDigest != digest ||
		result.VerificationPolicyDigest != policyDigest:
		return errors.New("the Verify is not the policy's reading of the digest")
	case !slices.Equal(result.VerificationRequirements, []string{"require_digest_pin"}):
		return fmt.Errorf("the Verify failed %v", result.VerificationRequirements)
	case result.Error == nil || result.Error.Code != "verification_refused":
		return errors.New("the Verify did not refuse")
	case result.ObservedArtifactType != "" || result.ResolvedReference != "" || result.ResolvedMediaType != "" ||
		result.ResolvedSize != 0:
		return errors.New("the Verify read the artifact it refused")
	case result.MutationStarted || result.Uncertain || result.Truncation != nil:
		return errors.New("the Verify touched something or lost output")
	}
	return nil
}

// repositoryOf is a reference without its tag: everything before the last
// colon, as the shell's ${reference%:*} cut it.
func repositoryOf(reference string) string {
	if index := strings.LastIndex(reference, ":"); index >= 0 {
		return reference[:index]
	}
	return reference
}
