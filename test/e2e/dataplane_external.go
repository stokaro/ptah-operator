package e2e

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The external PostgreSQL rows run one schema under apply: Always against a
// database outside the cluster. The predicates below are what those rows hold
// the schema, its Jobs, its plan and its results to, kept apart from the
// cluster so a unit test can show each one refusing the mistake it is for.

// externalColumnsV1 is the column list the v1 artifact leaves in the external
// database: id and name, both not null.
const externalColumnsV1 = "id:bigint:NO,name:text:NO"

// automaticConditions are the conditions a schema applied under apply: Always
// holds once it converged: resolved by digest, verified, observed, nothing
// drifted or planned, no approval needed, the Apply Job done, in sync, ready,
// and no failure.
var automaticConditions = []struct {
	kind   string
	status metav1.ConditionStatus
	reason string
}{
	{"ArtifactResolved", metav1.ConditionTrue, "DigestPinned"},
	{"ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied"},
	{"DatabaseReachable", metav1.ConditionTrue, "Observed"},
	{"DriftDetected", metav1.ConditionFalse, "ScopedConverged"},
	{"PlanReady", metav1.ConditionFalse, "NoChanges"},
	{"ApprovalRequired", metav1.ConditionFalse, "Satisfied"},
	{"Applying", metav1.ConditionFalse, "JobCompleted"},
	{"InSync", metav1.ConditionTrue, "ScopedConverged"},
	{"Ready", metav1.ConditionTrue, "InSync"},
	{"ReconciliationFailed", metav1.ConditionFalse, "Succeeded"},
}

// automaticExpectation is what the automatic row holds its final schema to.
type automaticExpectation struct {
	reference          string
	digest             string
	coordinationKey    string
	coordinationDigest string
	controller         controllerIdentity
	stateVersion       int32
	runnerImage        string
}

// automaticConvergenceExact holds the schema the automatic row converged to
// its exact evidence: apply Always with destructive changes refused, the
// digest-pinned source verified, the realm and target observed, what was
// applied bound to the execution binding and this manager, nothing pending,
// every condition of a converged schema, and the coordination key nowhere in
// its status.
func automaticConvergenceExact(schema *ptahv1alpha1.PtahSchema, want automaticExpectation) error {
	status := schema.Status
	source, target, applied, binding := status.Source, status.Target, status.Applied, status.ExecutionBinding
	switch {
	case schema.Spec.Policy.Apply != ptahv1alpha1.ApplyPolicy("Always") || schema.Spec.Policy.AllowDestructive:
		return errors.New("the schema is not apply Always with destructive changes refused")
	case source.RequestedReference != want.reference || source.ResolvedReference != want.reference ||
		source.Digest != want.digest || !source.Verified || source.ArtifactType != schemaArtifactType ||
		!sha256Pattern.MatchString(source.VerificationPolicyDigest):
		return errors.New("the source is not the digest-pinned reference, verified")
	case target.CoordinationDigest != want.coordinationDigest || !sha256Pattern.MatchString(target.IdentityDigest) ||
		!sha256Pattern.MatchString(target.DriftReportDigest):
		return errors.New("the target is not the realm's, observed")
	case applied == nil || binding == nil:
		return errors.New("the schema carries no applied evidence or no execution binding")
	case !sha256Pattern.MatchString(applied.PlanFingerprint) || applied.ArtifactDigest != want.digest ||
		applied.CoordinationDigest != want.coordinationDigest || applied.TargetIdentityDigest != target.IdentityDigest:
		return errors.New("what was applied is not this artifact in this realm on this target")
	case applied.ExecutionBindingID != binding.Epoch || applied.ControllerImage != want.controller.image ||
		applied.ControllerRevision != want.controller.revision || applied.ControllerStateVersion != want.stateVersion ||
		applied.PtahVersion != binding.PtahVersion || applied.ExecutorImage != binding.ExecutorImage ||
		applied.RunnerImage != want.runnerImage || applied.RunnerProtocolVersion != binding.RunnerProtocolVersion ||
		applied.CompletedAt.IsZero():
		return errors.New("what was applied is not bound to the execution binding and this manager")
	case status.Plan != nil || status.PendingObservation != nil || status.ActiveOperation != nil || status.PendingLockRelease != nil:
		return errors.New("the schema still carries a plan, an observation, an operation or a lock")
	}
	for _, condition := range automaticConditions {
		if !conditionIs(status.Conditions, condition.kind, condition.status, condition.reason) {
			return fmt.Errorf("%s is not %s for %s", condition.kind, condition.status, condition.reason)
		}
	}
	document, err := asJSON(status)
	if err != nil {
		return err
	}
	if holdsString(document, want.coordinationKey) {
		return errors.New("the status carries the coordination key")
	}
	return nil
}

// automaticJobSequence holds the Jobs the automatic row archived to one exact
// serialized lifecycle, and returns their UIDs in the order they ran: Resolve,
// Verify, Observe, Plan, Apply, and the Observe and Plan that proved the Apply
// converged. There are exactly seven, the UIDs the ledger holds; each is the
// schema's, completed with no retry and no replacement; and each started no
// earlier than the one before it completed.
func automaticJobSequence(jobs []batchv1.Job, before checkpoint, observed []string, schema string,
	schemaUID types.UID,
) ([]string, error) {
	var inBoundary []batchv1.Job
	for _, job := range jobs {
		if !before.holds(string(job.UID)) {
			inBoundary = append(inBoundary, job)
		}
	}
	uids := make([]string, 0, len(inBoundary))
	for _, job := range inBoundary {
		uids = append(uids, string(job.UID))
	}
	slices.Sort(uids)
	uids = slices.Compact(uids)
	refusal := errors.New("automatic-policy Job history is not one exact serialized lifecycle")
	if len(observed) != 7 || len(inBoundary) != 7 || len(uids) != 7 || !slices.Equal(uids, observed) {
		return nil, refusal
	}
	for index := range inBoundary {
		job := &inBoundary[index]
		if !ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema, schemaUID) ||
			job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 ||
			job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed ||
			job.Status.StartTime == nil || job.Status.CompletionTime == nil || !jobComplete(job) {
			return nil, refusal
		}
	}
	byOperation := func(operation string) []*batchv1.Job {
		var matches []*batchv1.Job
		for index := range inBoundary {
			if inBoundary[index].Labels[labelOperation] == operation {
				matches = append(matches, &inBoundary[index])
			}
		}
		slices.SortStableFunc(matches, func(a, b *batchv1.Job) int {
			return cmp.Or(a.Status.StartTime.Compare(b.Status.StartTime.Time),
				a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.UID, b.UID))
		})
		return matches
	}
	resolve, verify, observe := byOperation("resolve"), byOperation("verify"), byOperation("observe")
	plan, apply := byOperation("plan"), byOperation("apply")
	if len(resolve) != 1 || len(verify) != 1 || len(observe) != 2 || len(plan) != 2 || len(apply) != 1 {
		return nil, refusal
	}
	sequence := []*batchv1.Job{resolve[0], verify[0], observe[0], plan[0], apply[0], observe[1], plan[1]}
	for index := 1; index < len(sequence); index++ {
		if sequence[index-1].Status.CompletionTime.After(sequence[index].Status.StartTime.Time) {
			return nil, refusal
		}
	}
	ordered := make([]string, 0, len(sequence))
	for _, job := range sequence {
		if job.UID == "" {
			return nil, errors.New("automatic-policy Job sequence contains an empty UID")
		}
		ordered = append(ordered, string(job.UID))
	}
	return ordered, nil
}

// readOnlyResult is a result that started no mutation and claims no
// uncertainty about one.
func readOnlyResult(result runner.Result) bool {
	return !result.MutationStarted && !result.Uncertain
}

// automaticResolveResult holds the Resolve result to the digest-selected source.
func automaticResolveResult(result runner.Result, reference, digest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.Stdout != "" ||
		result.ResolvedReference != reference || result.ResolvedDigest != digest ||
		result.ResolvedMediaType == "" || result.ResolvedSize <= 0 || !readOnlyResult(result) {
		return errors.New("the Resolve result is not the digest-selected source")
	}
	return nil
}

// automaticVerifyResult holds the Verify result to the digest and the policy,
// with no requirement left unmet.
func automaticVerifyResult(result runner.Result, digest, policyDigest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.Stdout != "" ||
		result.ResolvedDigest != digest || result.VerificationPolicyDigest != policyDigest ||
		result.ObservedArtifactType != schemaArtifactType || len(result.VerificationRequirements) != 0 ||
		!readOnlyResult(result) {
		return errors.New("the Verify result did not verify this digest under this policy")
	}
	return nil
}

// automaticInitialObserveResult holds the first Observe result to real drift
// found in the realm and on the target.
func automaticInitialObserveResult(result runner.Result, coordinationDigest, targetIdentityDigest string) error {
	if result.Error != nil || (result.ChildExitCode != 0 && result.ChildExitCode != 1) || result.Stdout != "" ||
		result.ObservedDialect != "postgres" || !result.ObservedDrift || result.DriftFindingCount <= 0 ||
		!slices.Contains(driftSeverities, result.HighestDriftSeverity) ||
		!sha256Pattern.MatchString(result.DriftReportDigest) ||
		result.CoordinationDigest != coordinationDigest || result.TargetIdentityDigest != targetIdentityDigest ||
		!readOnlyResult(result) {
		return errors.New("the Observe result did not find PostgreSQL drift in the realm")
	}
	return nil
}

// automaticInitialPlanResult holds the first Plan result to a plan of changes
// in the realm and on the target.
func automaticInitialPlanResult(result runner.Result, coordinationDigest, targetIdentityDigest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.PlanOutcome != runner.PlanOutcomeChanges ||
		result.Stdout == "" || !sha256Pattern.MatchString(result.PlanContentDigest) ||
		result.CoordinationDigest != coordinationDigest || result.TargetIdentityDigest != targetIdentityDigest ||
		!readOnlyResult(result) {
		return errors.New("the Plan result is not a plan of changes in the realm")
	}
	return nil
}

var (
	createTableStatement = regexp.MustCompile(`(?i)\bCREATE[[:space:]]+TABLE\b`)
	removingStatement    = regexp.MustCompile(`(?i)\b(DROP|TRUNCATE|DELETE)\b`)
)

// automaticPlanDocument holds the plan the automatic row applied to a safe
// additive change: a PostgreSQL document of a new version that is not
// destructive, rates every statement safe, creates a table, and drops,
// truncates or deletes nothing.
func automaticPlanDocument(document planDocument) error {
	switch {
	case document.FormatVersion != 1 || document.Dialect != "postgres" || !isFalse(document.Destructive):
		return errors.New("the document is not a non-destructive PostgreSQL plan")
	case !sha256Pattern.MatchString(document.FromFingerprint) || !sha256Pattern.MatchString(document.ToFingerprint) ||
		document.FromFingerprint == document.ToFingerprint:
		return errors.New("the document does not move the schema from one state to another")
	case len(document.Statements) == 0:
		return errors.New("the document holds no statement")
	}
	for _, statement := range document.Statements {
		if statement.Severity != "safe" {
			return fmt.Errorf("a statement is rated %s", statement.Severity)
		}
		if removingStatement.MatchString(statement.SQL) {
			return errors.New("a statement drops, truncates or deletes")
		}
	}
	if !slices.ContainsFunc(document.Statements, func(statement planStatement) bool {
		return createTableStatement.MatchString(statement.SQL)
	}) {
		return errors.New("no statement creates a table")
	}
	return nil
}

// automaticPlanBound holds the plan the automatic row applied to its
// immutable bindings: the artifact, the fingerprint the schema applied, the
// document its chunks hold, the realm and target, the execution binding and
// this manager, and nowhere the coordination key.
func automaticPlanBound(plan *ptahv1alpha1.PtahSchemaPlan, schema *ptahv1alpha1.PtahSchema, document planDocument,
	contentDigest string, want automaticExpectation,
) error {
	spec, status := plan.Spec, schema.Status
	binding := status.ExecutionBinding
	switch {
	case status.Applied == nil || binding == nil:
		return errors.New("the schema carries no applied evidence or no execution binding")
	case spec.ContractVersion != fingerprint.CurrentPlanContractVersion || spec.ArtifactDigest != want.digest ||
		spec.Fingerprint != status.Applied.PlanFingerprint || spec.ContentDigest != contentDigest:
		return errors.New("the plan is not the one the schema applied")
	case spec.CoordinationDigest != want.coordinationDigest || spec.TargetIdentityDigest != status.Target.IdentityDigest:
		return errors.New("the plan is not bound to the realm and target")
	case spec.ActualStateFingerprint != document.FromFingerprint || spec.DesiredStateFingerprint != document.ToFingerprint ||
		spec.Destructive || int(spec.StatementCount) != len(document.Statements) || len(spec.Chunks) == 0:
		return errors.New("the plan is not bound to its document")
	case spec.ExecutionBindingID != binding.Epoch || spec.ControllerImage != want.controller.image ||
		spec.ControllerRevision != want.controller.revision || spec.ControllerStateVersion != want.stateVersion ||
		spec.PtahVersion != binding.PtahVersion || spec.ExecutorImage != binding.ExecutorImage ||
		spec.RunnerImage != want.runnerImage || spec.RunnerProtocolVersion != binding.RunnerProtocolVersion:
		return errors.New("the plan is not bound to the execution binding and this manager")
	case !conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue):
		return errors.New("the plan is not Ready")
	}
	encoded, err := asJSON(plan)
	if err != nil {
		return err
	}
	if holdsString(encoded, want.coordinationKey) {
		return errors.New("the plan carries the coordination key")
	}
	return nil
}

// applyWorkload is the plan and execution an Apply Job and its Pod carry.
type applyWorkload struct {
	schema           string
	jobName, jobUID  string
	podName, podUID  string
	planFingerprint  string
	contentDigest    string
	executionBinding string
	executorImage    string
	runnerImage      string
}

func (want applyWorkload) exactAnnotations(annotations map[string]string) bool {
	return annotations["operator.ptah.run/plan-fingerprint"] == want.planFingerprint &&
		annotations["operator.ptah.run/plan-content-digest"] == want.contentDigest &&
		annotations["operator.ptah.run/execution-binding-id"] == want.executionBinding
}

// exactRuntimeSpec is one runner installed from the runner image, one ptah
// container from the executor image, and the engine that container expects
// given as a literal PostgreSQL.
func (want applyWorkload) exactRuntimeSpec(spec corev1.PodSpec) bool {
	count := func(containers []corev1.Container, match func(corev1.Container) bool) int {
		matches := 0
		for _, container := range containers {
			if match(container) {
				matches++
			}
		}
		return matches
	}
	named := func(name string) func(corev1.Container) bool {
		return func(container corev1.Container) bool { return container.Name == name }
	}
	var engines, literal int
	for _, container := range spec.Containers {
		if container.Name != "ptah" {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name != "PTAH_EXPECTED_DATABASE_ENGINE" {
				continue
			}
			engines++
			if variable.Value == "PostgreSQL" && variable.ValueFrom == nil {
				literal++
			}
		}
	}
	return count(spec.InitContainers, named("install-runner")) == 1 &&
		count(spec.InitContainers, func(container corev1.Container) bool {
			return container.Name == "install-runner" && container.Image == want.runnerImage
		}) == 1 &&
		count(spec.Containers, named("ptah")) == 1 &&
		count(spec.Containers, func(container corev1.Container) bool {
			return container.Name == "ptah" && container.Image == want.executorImage
		}) == 1 &&
		engines == 1 && literal == 1
}

// automaticApplyWorkload holds the archived Apply Job and its Pod to the plan
// they applied and the execution they ran: the plan's fingerprint, its content
// digest and the execution binding on the Job, its template and the Pod, and
// the runner, executor and engine in both specs.
func automaticApplyWorkload(job *batchv1.Job, pod *corev1.Pod, want applyWorkload) error {
	switch {
	case job.Name != want.jobName || string(job.UID) != want.jobUID ||
		job.Labels[labelSchema] != want.schema || job.Labels[labelOperation] != "apply":
		return errors.New("the Job is not the schema's Apply Job")
	case !want.exactAnnotations(job.Annotations) || !want.exactAnnotations(job.Spec.Template.Annotations):
		return errors.New("the Job does not carry the plan and execution it applied")
	case !want.exactRuntimeSpec(job.Spec.Template.Spec):
		return errors.New("the Job template does not run the runner and executor it was bound to")
	case pod.Name != want.podName || string(pod.UID) != want.podUID ||
		!slices.ContainsFunc(pod.OwnerReferences, func(reference metav1.OwnerReference) bool {
			return reference.APIVersion == "batch/v1" && reference.Kind == "Job" && reference.Name == want.jobName &&
				string(reference.UID) == want.jobUID && isController(reference)
		}):
		return errors.New("the Pod is not the Apply Job's")
	case !want.exactAnnotations(pod.Annotations):
		return errors.New("the Pod does not carry the plan and execution it applied")
	case !want.exactRuntimeSpec(pod.Spec):
		return errors.New("the Pod does not run the runner and executor it was bound to")
	}
	return nil
}

// automaticApplyResult holds the Apply result to having executed the exact
// safe plan, and to saying so.
func automaticApplyResult(result runner.Result, contentDigest, coordinationDigest, targetIdentityDigest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.Stdout != "" ||
		result.PlanContentDigest != contentDigest || result.CoordinationDigest != coordinationDigest ||
		result.TargetIdentityDigest != targetIdentityDigest || !result.MutationStarted || result.Uncertain ||
		result.PlanOutcome != "" {
		return errors.New("the Apply result did not execute the exact plan")
	}
	return nil
}

// automaticFinalObserveResult holds the Observe after the Apply to a
// converged reading of the same target the schema recorded.
func automaticFinalObserveResult(result runner.Result, coordinationDigest, targetIdentityDigest, driftReportDigest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.Stdout != "" || result.ObservedDialect != "postgres" ||
		result.ObservedDrift || result.DriftFindingCount != 0 || result.HighestDriftSeverity != "" ||
		result.CoordinationDigest != coordinationDigest || result.TargetIdentityDigest != targetIdentityDigest ||
		result.DriftReportDigest != driftReportDigest || !readOnlyResult(result) {
		return errors.New("the Observe after the Apply found drift")
	}
	return nil
}

// automaticFinalPlanResult holds the Plan after the Apply to finding nothing
// left to change.
func automaticFinalPlanResult(result runner.Result, coordinationDigest, targetIdentityDigest string) error {
	if result.Error != nil || result.ChildExitCode != 0 || result.Stdout != "" ||
		result.PlanOutcome != runner.PlanOutcomeNoChanges || result.PlanContentDigest != "" ||
		result.CoordinationDigest != coordinationDigest || result.TargetIdentityDigest != targetIdentityDigest ||
		!readOnlyResult(result) {
		return errors.New("the Plan after the Apply found changes")
	}
	return nil
}

// approvalsFor are the approvals that name the schema by name and UID.
func approvalsFor(approvals []ptahv1alpha1.PtahSchemaApproval, schema string, uid types.UID) int {
	count := 0
	for _, approval := range approvals {
		if approval.Spec.SchemaRef.Name == schema && approval.Spec.SchemaRef.UID == uid {
			count++
		}
	}
	return count
}

// approvalTransitions are the Events that say the schema waited for, or took,
// an approval.
func approvalTransitions(events []corev1.Event, schema string, uid types.UID) int {
	count := 0
	for _, event := range events {
		if event.InvolvedObject.Kind == "PtahSchema" && event.InvolvedObject.Name == schema &&
			event.InvolvedObject.UID == uid && (event.Reason == "ApprovalRequired" || event.Reason == "ApprovalAccepted") {
			count++
		}
	}
	return count
}

// privilegeEvents are the ApprovalRequired Events about the schema whose
// message names the privilege kinds its plan changes.
func privilegeEvents(events []corev1.Event, uid types.UID, kinds string) int {
	count := 0
	for _, event := range events {
		if event.InvolvedObject.Kind == "PtahSchema" && event.InvolvedObject.UID == uid &&
			event.Reason == "ApprovalRequired" && strings.Contains(event.Message, "changes privileges ("+kinds+")") {
			count++
		}
	}
	return count
}

// privilegedPlanRecorded holds the plan the privileged row waits on to the
// plan the gate named, the digest it plans, and the one privilege kind its
// statement changes, published and not destructive.
func privilegedPlanRecorded(plan *ptahv1alpha1.PtahSchemaPlan, uid, fingerprint, digest string,
	kinds []ptahv1alpha1.PrivilegeChange,
) error {
	switch {
	case string(plan.UID) != uid || plan.Spec.Fingerprint != fingerprint || plan.Spec.ArtifactDigest != digest:
		return errors.New("the plan is not the one the gate named")
	case plan.Spec.Destructive:
		return errors.New("the plan is destructive")
	case !slices.Equal(plan.Spec.PrivilegeChanges, kinds):
		return fmt.Errorf("the plan records %v", plan.Spec.PrivilegeChanges)
	case !conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue):
		return errors.New("the plan is not Ready")
	}
	return nil
}

// samePlanHeldAfter is the privileged gate read again after the refresh
// deadline the first reading persisted: the same plan, held, with a deadline
// past that one.
func samePlanHeldAfter(planUID, fingerprint string, after time.Time) func(*ptahv1alpha1.PtahSchema) bool {
	return func(schema *ptahv1alpha1.PtahSchema) bool {
		plan, next := schema.Status.Plan, schema.Status.NextReconciliationTime
		return plan != nil && string(plan.UID) == planUID && plan.Fingerprint == fingerprint &&
			next != nil && next.After(after)
	}
}

// samePlanHeldAtGeneration is the privileged gate read at a generation the row
// set: the same plan, held after the spec changed.
func samePlanHeldAtGeneration(planUID, fingerprint string, generation int64) func(*ptahv1alpha1.PtahSchema) bool {
	return func(schema *ptahv1alpha1.PtahSchema) bool {
		plan := schema.Status.Plan
		return schema.Generation == generation && plan != nil && string(plan.UID) == planUID && plan.Fingerprint == fingerprint
	}
}

// privilegedHoldMeasured holds the privileged plan's wait to its whole
// persisted deadline, dated by the Jobs' own timestamps rather than by the
// poll that noticed them: at least one refresh started, none before the
// deadline, and the deadline a whole refresh interval after the Plan Job that
// published the plan completed.
func privilegedHoldMeasured(refreshes []observedJob, deadline, planCompleted time.Time, interval time.Duration) error {
	if len(refreshes) == 0 {
		return errors.New("no refresh started after the gate")
	}
	for _, refresh := range refreshes {
		created, err := time.Parse(time.RFC3339, refresh.Created)
		if err != nil {
			return fmt.Errorf("refresh %s has no creation time: %w", refresh.UID, err)
		}
		if created.Before(deadline) {
			return fmt.Errorf("refresh %s started at %s, before the deadline %s", refresh.UID, refresh.Created,
				deadline.UTC().Format(time.RFC3339))
		}
	}
	if deadline.Sub(planCompleted) < interval {
		return fmt.Errorf("the deadline %s is %s after the plan completed, less than the interval %s",
			deadline.UTC().Format(time.RFC3339), deadline.Sub(planCompleted), interval)
	}
	return nil
}

// latestCompletion is the latest completion among the Jobs, each of which has
// to have one.
func latestCompletion(jobs []*batchv1.Job) (time.Time, error) {
	var latest time.Time
	if len(jobs) == 0 {
		return latest, errors.New("no completed Job")
	}
	for _, job := range jobs {
		if job.Status.CompletionTime == nil {
			return latest, fmt.Errorf("job %s has no completion time", job.UID)
		}
		if job.Status.CompletionTime.After(latest) {
			latest = job.Status.CompletionTime.Time
		}
	}
	return latest, nil
}

// gatePlanReading is the part of a held plan a gate that gave up reports.
type gatePlanReading struct {
	Name             string                         `json:"name"`
	UID              types.UID                      `json:"uid"`
	Fingerprint      string                         `json:"fingerprint"`
	Destructive      bool                           `json:"destructive"`
	PrivilegeChanges []ptahv1alpha1.PrivilegeChange `json:"privilegeChanges"`
	Approved         bool                           `json:"approved"`
}

func readGatePlan(plan *ptahv1alpha1.CurrentPlanStatus) *gatePlanReading {
	if plan == nil {
		return nil
	}
	return &gatePlanReading{
		Name: plan.Name, UID: plan.UID, Fingerprint: plan.Fingerprint, Destructive: plan.Destructive,
		PrivilegeChanges: plan.PrivilegeChanges, Approved: plan.Approval != nil,
	}
}

type gateConditionReading struct {
	Type   string                 `json:"type,omitempty"`
	Status metav1.ConditionStatus `json:"status"`
	Reason string                 `json:"reason"`
}

const noReadableSchema = "no readable PtahSchema"

// privilegedGateReading is what a privileged gate that gave up says it last
// read: where the schema stood, the plan it held, and why it waited.
func privilegedGateReading(schema *ptahv1alpha1.PtahSchema) string {
	if schema == nil {
		return noReadableSchema
	}
	status := schema.Status
	reading := struct {
		Generation             int64                  `json:"generation"`
		ObservedGeneration     int64                  `json:"observedGeneration"`
		Phase                  string                 `json:"phase"`
		Digest                 string                 `json:"digest"`
		NextReconciliationTime *metav1.Time           `json:"nextReconciliationTime"`
		ActiveOperation        *string                `json:"activeOperation"`
		Plan                   *gatePlanReading       `json:"plan"`
		ApprovalRequired       []gateConditionReading `json:"approvalRequired"`
	}{
		Generation: schema.Generation, ObservedGeneration: status.ObservedGeneration, Phase: string(status.Phase),
		Digest: status.Source.Digest, NextReconciliationTime: status.NextReconciliationTime,
		ActiveOperation: activeOperationType(status.ActiveOperation), Plan: readGatePlan(status.Plan),
		ApprovalRequired: []gateConditionReading{},
	}
	for _, condition := range status.Conditions {
		if condition.Type == "ApprovalRequired" {
			reading.ApprovalRequired = append(reading.ApprovalRequired,
				gateConditionReading{Status: condition.Status, Reason: condition.Reason})
		}
	}
	return compactReading(reading)
}

// grantGateReading is what a grant-only gate that gave up says it last read:
// where the schema stood, what the observation found, the plan it held, and
// the conditions that decide whether it waits.
func grantGateReading(schema *ptahv1alpha1.PtahSchema) string {
	if schema == nil {
		return noReadableSchema
	}
	status := schema.Status
	type targetReading struct {
		HighestDriftSeverity string                            `json:"highestDriftSeverity"`
		DriftFindingCount    int32                             `json:"driftFindingCount"`
		DriftFindings        []ptahv1alpha1.DriftFindingStatus `json:"driftFindings"`
		LastObservedAt       *metav1.Time                      `json:"lastObservedAt"`
	}
	reading := struct {
		Generation         int64                  `json:"generation"`
		ObservedGeneration int64                  `json:"observedGeneration"`
		Phase              string                 `json:"phase"`
		Digest             string                 `json:"digest"`
		ActiveOperation    *string                `json:"activeOperation"`
		Target             targetReading          `json:"target"`
		Plan               *gatePlanReading       `json:"plan"`
		Conditions         []gateConditionReading `json:"conditions"`
	}{
		Generation: schema.Generation, ObservedGeneration: status.ObservedGeneration, Phase: string(status.Phase),
		Digest: status.Source.Digest, ActiveOperation: activeOperationType(status.ActiveOperation),
		Target: targetReading{
			HighestDriftSeverity: status.Target.HighestDriftSeverity, DriftFindingCount: status.Target.DriftFindingCount,
			DriftFindings: status.Target.DriftFindings, LastObservedAt: status.Target.LastObservedAt,
		},
		Plan:       readGatePlan(status.Plan),
		Conditions: []gateConditionReading{},
	}
	for _, condition := range status.Conditions {
		switch condition.Type {
		case "DriftDetected", "ApprovalRequired", "ReconciliationFailed":
			reading.Conditions = append(reading.Conditions,
				gateConditionReading{Type: condition.Type, Status: condition.Status, Reason: condition.Reason})
		}
	}
	return compactReading(reading)
}

func activeOperationType(operation *ptahv1alpha1.ActiveOperationStatus) *string {
	if operation == nil {
		return nil
	}
	kind := string(operation.Type)
	return &kind
}

func compactReading(reading any) string {
	encoded, err := json.Marshal(reading)
	if err != nil {
		return noReadableSchema
	}
	return string(encoded)
}
