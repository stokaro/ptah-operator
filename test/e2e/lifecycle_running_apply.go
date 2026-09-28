package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The Apply the uninstall phase holds running across the next-release
// upgrade: a schema of its own, the database it reaches through a Service
// routed to the external PostgreSQL container, and a barrier that holds the
// advisory lock the Apply's one statement waits for.
const (
	predecessorApplySchema     = "running-apply-across-upgrade"
	predecessorApplyDatabase   = "running-apply-database"
	predecessorApplyPolicy     = "running-apply-verification-policy"
	predecessorApplyPullSecret = "running-apply-registry"
	predecessorApplyPlanSource = "running-apply-plan-source"
	// predecessorApplyBarrierKey is the advisory key the barrier holds and the
	// Apply's one statement asks for.
	predecessorApplyBarrierKey = 742019370001
	// predecessorApplyBarrierApplication is the application name the barrier
	// connects with, which every barrier query selects by.
	predecessorApplyBarrierApplication = "ptah-operator-running-apply-barrier"
	predecessorApplyArtifactDigest     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// predecessorApplyPolicyContent is the verification policy the schema
	// reads, and the bytes its digest is taken over.
	predecessorApplyPolicyContent = "version: 1\n"
	// predecessorApplyDispatcherAnnotation records, on the Job's Pod template,
	// the manager that dispatched it.
	predecessorApplyDispatcherAnnotation = "operator.ptah.run/controller-image"
)

var (
	predecessorApplyDatabaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	predecessorApplyContainerID  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	predecessorApplyPinnedImage  = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

// predecessorApplyBarrierDatabase is resolve_running_apply_database: the
// database the external PostgreSQL credentials name, which the barrier holds
// its lock in. A PostgreSQL advisory lock is per database, so a barrier held
// in any other one blocks nothing.
func predecessorApplyBarrierDatabase(credentials []byte) (string, error) {
	missing := errors.New("external PostgreSQL credentials name no database for the running Apply barrier")
	var document map[string]any
	if err := json.Unmarshal(credentials, &document); err != nil {
		return "", missing
	}
	value, found := document["database"]
	if !found || value == nil || value == false {
		return "", missing
	}
	database, isString := value.(string)
	if !isString || !predecessorApplyDatabaseName.MatchString(database) {
		return "", errors.New("external PostgreSQL credentials name an unusable database for the running Apply barrier")
	}
	return database, nil
}

// predecessorApplyDatabaseCredentials reads the external PostgreSQL login the
// fixture's database Secret is built from: exactly the fields the driver
// writes, a user and password the data plane's parser would accept, which the
// URL below carries unescaped, and the database the barrier holds its lock in.
func predecessorApplyDatabaseCredentials(content []byte) (externalPostgresCredentials, error) {
	refusal := errors.New("E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE has an invalid shape")
	var credentials externalPostgresCredentials
	if err := strictUnmarshal(content, &credentials); err != nil {
		return externalPostgresCredentials{}, refusal
	}
	if !credentialName.MatchString(credentials.Username) || !credentialName.MatchString(credentials.Password) ||
		!predecessorApplyDatabaseName.MatchString(credentials.Database) {
		return externalPostgresCredentials{}, refusal
	}
	return credentials, nil
}

// predecessorApplyDatabaseURL is the URL the fixture's database Secret holds:
// the external PostgreSQL through the fixture's own Service.
func predecessorApplyDatabaseURL(credentials externalPostgresCredentials, authority string) string {
	return "postgres://" + credentials.Username + ":" + credentials.Password + "@" + authority + "/" +
		credentials.Database + "?sslmode=disable"
}

// predecessorApplyHeldQuery counts the advisory locks the barrier holds.
func predecessorApplyHeldQuery() string {
	return "SELECT count(*) FROM pg_locks AS lock JOIN pg_stat_activity AS activity USING (pid) " +
		"WHERE lock.locktype = 'advisory' AND lock.granted AND activity.application_name = '" +
		predecessorApplyBarrierApplication + "'"
}

// predecessorApplyContentionQuery counts the waiters on a lock the barrier
// holds: one is the Apply blocked inside the engine.
func predecessorApplyContentionQuery() string {
	return "SELECT count(*) FROM pg_locks AS waiting JOIN pg_locks AS held USING (locktype, database, classid, objid, objsubid) " +
		"JOIN pg_stat_activity AS holder ON holder.pid = held.pid WHERE held.locktype = 'advisory' AND held.granted " +
		"AND NOT waiting.granted AND waiting.pid <> held.pid AND holder.application_name = '" +
		predecessorApplyBarrierApplication + "'"
}

// predecessorApplyReleaseQuery terminates the barrier's backend, which is
// what releases its lock.
func predecessorApplyReleaseQuery() string {
	return "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '" +
		predecessorApplyBarrierApplication + "' AND pid <> pg_backend_pid()"
}

// predecessorApplyValue is what psql -Atq printed, read as the shell's
// command substitution read it: trailing newlines removed.
func predecessorApplyValue(output string) string {
	return strings.TrimRight(output, "\n")
}

// predecessorApplyDockerTarget refuses a Docker context or container the
// barrier may not reach: the context has to be an explicit remote one, and
// the container an exact ID.
func predecessorApplyDockerTarget(dockerContext, containerID string) error {
	switch dockerContext {
	case "", "default", "orbstack":
		return errors.New("E2E_DOCKER_CONTEXT must name an explicit allowed remote context")
	}
	if !predecessorApplyContainerID.MatchString(containerID) {
		return errors.New("E2E_EXTERNAL_POSTGRES_CONTAINER_ID must be an exact Docker container ID")
	}
	return nil
}

// predecessorApplyExecutorImage is the executor the live release configured:
// the one --executor-image argument of the manager container.
func predecessorApplyExecutorImage(deployment *appsv1.Deployment) (string, error) {
	var images []string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "manager" {
			continue
		}
		for _, argument := range container.Args {
			if image, found := strings.CutPrefix(argument, "--executor-image="); found {
				images = append(images, image)
			}
		}
	}
	if len(images) != 1 || images[0] == "" {
		return "", errors.New("the controller must carry one executor image")
	}
	return images[0], nil
}

// predecessorApplyRegistry is the registry an image reference names: what
// comes before its first slash.
func predecessorApplyRegistry(image string) string {
	registry, _, _ := strings.Cut(image, "/")
	return registry
}

// predecessorApplyPlan turns the native plan `ptah schema plan` wrote into the
// plan the Apply runs: the same state fingerprints, one statement that waits on
// the barrier's lock, and nothing destructive.
func predecessorApplyPlan(native []byte, planName string) ([]byte, error) {
	refusal := errors.New("native plan lacks exact state fingerprints")
	decoder := json.NewDecoder(bytes.NewReader(native))
	decoder.UseNumber()
	var plan map[string]any
	if err := decoder.Decode(&plan); err != nil || decoder.More() {
		return nil, refusal
	}
	version, isNumber := plan["format_version"].(json.Number)
	from, fromString := plan["from_fingerprint"].(string)
	to, toString := plan["to_fingerprint"].(string)
	if !isNumber || version.String() != "1" || !fromString || !sha256Pattern.MatchString(from) ||
		!toString || !sha256Pattern.MatchString(to) {
		return nil, refusal
	}
	plan["name"] = planName
	plan["destructive"] = false
	plan["statements"] = []any{map[string]any{
		"sql":      fmt.Sprintf("SELECT pg_advisory_lock(%d)", predecessorApplyBarrierKey),
		"severity": "safe",
		"reason":   "upgrade quiescence proof",
	}}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	// jq -c ends its output with a newline, and the plan's content digest and
	// size are taken over exactly these bytes.
	return append(encoded, '\n'), nil
}

// predecessorApplyManager is the release of the manager the plan records as
// its publisher, read from a Job that manager dispatched.
type predecessorApplyManager struct {
	controllerImage    string
	controllerRevision string
	runnerImage        string
}

// predecessorApplyBundle is the plan and the schema status that make the Apply
// the manager's own decision.
type predecessorApplyBundle struct {
	Plan         ptahv1alpha1.PtahSchemaPlan
	SchemaStatus ptahv1alpha1.PtahSchemaStatus
}

// predecessorApplyManagerOf reads the identity a manager-dispatched Job
// records, and refuses a Job that records none.
func predecessorApplyManagerOf(job *batchv1.Job) (predecessorApplyManager, error) {
	controllerImage, controllerRevision, runnerImage := workload.ManagerIdentityOf(job)
	if controllerImage == "" || controllerRevision == "" || runnerImage == "" {
		return predecessorApplyManager{}, errors.New("manager Job records no complete manager identity")
	}
	return predecessorApplyManager{
		controllerImage: controllerImage, controllerRevision: controllerRevision, runnerImage: runnerImage,
	}, nil
}

// predecessorApplyFixture is what hack/predecessorapplyfixture computed: the
// current-contract plan and the status bundle for the schema, bound to what
// the manager re-derives. It runs in the phase, so the database URL the target
// identity binds to never reaches a command line.
func predecessorApplyFixture(
	schema *ptahv1alpha1.PtahSchema,
	manager predecessorApplyManager,
	planData []byte,
	policyUID string,
	policyData []byte,
	databaseURL string,
	now time.Time,
) (predecessorApplyBundle, error) {
	if schema == nil || schema.Name == "" || schema.Namespace == "" || schema.UID == "" {
		return predecessorApplyBundle{}, errors.New("schema must have a namespace, name, and UID")
	}
	if databaseURL == "" {
		return predecessorApplyBundle{}, errors.New("the database URL is required")
	}
	if schema.Status.ExecutionBinding == nil {
		return predecessorApplyBundle{}, errors.New("schema lacks a predecessor execution binding")
	}
	binding := schema.Status.ExecutionBinding.DeepCopy()
	// The plan records the manager that published it, and the admission guard
	// over plan writes requires the record. The binding carries what the plan
	// binds; a binding without it belongs to a release this fixture cannot
	// describe.
	if binding.ControllerStateVersion == 0 || binding.PtahVersion == "" || binding.ExecutorImage == "" ||
		binding.RunnerProtocolVersion == 0 {
		return predecessorApplyBundle{}, errors.New("schema execution binding is incomplete")
	}
	if manager.controllerImage == "" || manager.controllerRevision == "" || manager.runnerImage == "" {
		return predecessorApplyBundle{}, errors.New("the publishing manager's identity is incomplete")
	}
	if strings.TrimSpace(policyUID) == "" || len(policyData) == 0 {
		return predecessorApplyBundle{}, errors.New("verification policy identity and bytes are required")
	}
	decoded, err := dataplane.DecodePlan(planData, string(schema.Spec.Target.Engine))
	if err != nil {
		return predecessorApplyBundle{}, fmt.Errorf("validate plan: %w", err)
	}
	if decoded.Destructive {
		return predecessorApplyBundle{}, errors.New("upgrade fixture plan must be non-destructive")
	}
	// The fixture writes the plan as ReadyToApply under Always, which a plan
	// that changes privileges never is.
	if len(decoded.PrivilegeChanges) > 0 {
		return predecessorApplyBundle{}, errors.New("upgrade fixture plan must change no privilege")
	}
	coordinationDigest, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil {
		return predecessorApplyBundle{}, fmt.Errorf("derive coordination digest: %w", err)
	}
	// The controller's own policy binding: a plan the controller cannot
	// reproduce is refused by the write webhook before it is stored.
	policyFingerprint, err := controller.PolicyFingerprint(schema)
	if err != nil {
		return predecessorApplyBundle{}, err
	}
	contentDigest := fingerprint.DigestBytes(planData)
	desiredReference, err := ocireference.Parse(schema.Spec.Desired.OCIRef)
	if err != nil || !desiredReference.IsDigest {
		return predecessorApplyBundle{}, errors.New("schema desired reference must be digest-pinned")
	}
	artifactDigest := desiredReference.Selector
	// The runner recomputes this from the database URL it resolves and refuses
	// the operation when it differs from what planning recorded. It is computed
	// with the runner's own function rather than restated: a placeholder here
	// once made every predecessor Apply exit before opening a connection.
	targetIdentityDigest, err := runner.TargetIdentityDigest(databaseURL)
	if err != nil {
		return predecessorApplyBundle{}, fmt.Errorf("derive predecessor Apply target identity: %w", err)
	}
	verificationPolicyDigest := fingerprint.DigestBytes(policyData)
	planFingerprint, err := (fingerprint.PlanBinding{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        contentDigest,
		ArtifactDigest:           artifactDigest,
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     targetIdentityDigest,
		ActualStateFingerprint:   decoded.FromFingerprint,
		DesiredStateFingerprint:  decoded.ToFingerprint,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyUID,
		VerificationPolicyDigest: verificationPolicyDigest,
		ExecutionBindingID:       binding.Epoch,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		Destructive:              false,
		StatementCount:           int32(len(decoded.Statements)), //nolint:gosec // One statement.
	}).Fingerprint()
	if err != nil {
		return predecessorApplyBundle{}, fmt.Errorf("fingerprint plan: %w", err)
	}
	planName := "ptah-plan-" + strings.TrimPrefix(planFingerprint, "sha256:")[:24]
	controllerOwner, blockDeletion := true, true
	statementCount := int32(len(decoded.Statements)) //nolint:gosec // One statement.
	plan := ptahv1alpha1.PtahSchemaPlan{
		TypeMeta: metav1.TypeMeta{APIVersion: ptahv1alpha1.GroupVersion.String(), Kind: "PtahSchemaPlan"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      planName,
			Labels:    map[string]string{planstore.LabelSchema: schema.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ptahv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
				Name: schema.Name, UID: schema.UID, Controller: &controllerOwner, BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: ptahv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          fingerprint.CurrentPlanContractVersion,
			SchemaRef:                ptahv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              planFingerprint,
			ContentDigest:            contentDigest,
			Size:                     int64(len(planData)),
			ArtifactDigest:           artifactDigest,
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     targetIdentityDigest,
			ActualStateFingerprint:   decoded.FromFingerprint,
			DesiredStateFingerprint:  decoded.ToFingerprint,
			PolicyFingerprint:        policyFingerprint,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ExecutionBindingID:       binding.Epoch,
			ControllerImage:          manager.controllerImage,
			ControllerRevision:       manager.controllerRevision,
			ControllerStateVersion:   binding.ControllerStateVersion,
			PtahVersion:              binding.PtahVersion,
			ExecutorImage:            binding.ExecutorImage,
			RunnerImage:              manager.runnerImage,
			RunnerProtocolVersion:    binding.RunnerProtocolVersion,
			Dialect:                  decoded.Dialect,
			Destructive:              false,
			StatementCount:           statementCount,
			Chunks: []ptahv1alpha1.PlanChunkReference{{
				Name: planName + "-000", Index: 0, Digest: contentDigest, Size: int32(len(planData)), //nolint:gosec // A small plan.
			}},
		},
	}
	transition := metav1.NewTime(now)
	nextReconciliation := metav1.NewTime(now.Add(24 * time.Hour))
	status := ptahv1alpha1.PtahSchemaStatus{
		ObservedGeneration:     schema.Generation,
		Phase:                  ptahv1alpha1.PhaseReadyToApply,
		ExecutionBinding:       binding,
		NextReconciliationTime: &nextReconciliation,
		Source: ptahv1alpha1.SchemaSourceStatus{
			RequestedReference:       schema.Spec.Desired.OCIRef,
			ResolvedReference:        schema.Spec.Desired.OCIRef,
			Digest:                   artifactDigest,
			MediaType:                "application/vnd.oci.image.manifest.v1+json",
			ArtifactType:             dataplane.SchemaArtifactType,
			Size:                     1,
			Verified:                 true,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ResolvedAt:               &transition,
			VerifiedAt:               &transition,
		},
		Target: ptahv1alpha1.TargetStatus{
			Engine:             schema.Spec.Target.Engine,
			CoordinationDigest: coordinationDigest,
			IdentityDigest:     targetIdentityDigest,
			DriftReportDigest:  fingerprint.DigestBytes([]byte("predecessor Apply upgrade drift report")),
			LastObservedAt:     &transition,
		},
		Plan: &ptahv1alpha1.CurrentPlanStatus{
			Name:                     planName,
			Fingerprint:              planFingerprint,
			ContentDigest:            contentDigest,
			ArtifactDigest:           artifactDigest,
			CoordinationDigest:       coordinationDigest,
			TargetIdentityDigest:     targetIdentityDigest,
			ActualStateFingerprint:   decoded.FromFingerprint,
			DesiredStateFingerprint:  decoded.ToFingerprint,
			PolicyFingerprint:        policyFingerprint,
			VerificationPolicyUID:    types.UID(policyUID),
			VerificationPolicyDigest: verificationPolicyDigest,
			ExecutionBindingID:       binding.Epoch,
			ControllerImage:          manager.controllerImage,
			ControllerRevision:       manager.controllerRevision,
			ControllerStateVersion:   binding.ControllerStateVersion,
			PtahVersion:              binding.PtahVersion,
			ExecutorImage:            binding.ExecutorImage,
			RunnerImage:              manager.runnerImage,
			RunnerProtocolVersion:    binding.RunnerProtocolVersion,
			Destructive:              false,
			StatementCount:           statementCount,
			CreatedAt:                transition,
		},
		Conditions: []metav1.Condition{
			{
				Type: "PlanReady", Status: metav1.ConditionTrue, Reason: "CurrentPlan",
				Message: "Exact predecessor plan is ready", ObservedGeneration: schema.Generation,
				LastTransitionTime: transition,
			},
			{
				Type: "ApprovalRequired", Status: metav1.ConditionFalse, Reason: "NotRequired",
				Message: "Policy permits this non-destructive plan", ObservedGeneration: schema.Generation,
				LastTransitionTime: transition,
			},
			{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "ApplyPending",
				Message: "Exact predecessor plan is ready to apply", ObservedGeneration: schema.Generation,
				LastTransitionTime: transition,
			},
		},
	}
	return predecessorApplyBundle{Plan: plan, SchemaStatus: status}, nil
}

// predecessorApplyPlanCarriesContract is the script's check on the generated
// plan: the current contract, a digest-pinned publishing manager with a
// revision, a controller-state version, and exactly one chunk.
func predecessorApplyPlanCarriesContract(plan *ptahv1alpha1.PtahSchemaPlan, contract int32) bool {
	spec := plan.Spec
	return spec.ContractVersion == contract && predecessorApplyPinnedImage.MatchString(spec.ControllerImage) &&
		spec.ControllerRevision != "" && spec.ControllerStateVersion >= 1 && len(spec.Chunks) == 1
}

// predecessorApplyReadyStatus is the status start_running_apply_fixture hands
// the manager: the bundle's, observed at the schema's current generation and
// bound to the plan's API-assigned UID.
func predecessorApplyReadyStatus(bundle ptahv1alpha1.PtahSchemaStatus, generation int64, planUID types.UID) ptahv1alpha1.PtahSchemaStatus {
	status := *bundle.DeepCopy()
	status.ObservedGeneration = generation
	if status.Plan == nil {
		status.Plan = &ptahv1alpha1.CurrentPlanStatus{}
	}
	status.Plan.UID = planUID
	for index := range status.Conditions {
		status.Conditions[index].ObservedGeneration = generation
	}
	return status
}

// predecessorApplyAt reads a path of a stored document the way jq read it: a
// missing key, or a step through something that is not an object, is null.
func predecessorApplyAt(document any, path ...string) any {
	value := document
	for _, key := range path {
		object, isObject := value.(map[string]any)
		if !isObject {
			return nil
		}
		value = object[key]
	}
	return value
}

// predecessorApplyHas is jq's has(key) on the object at path. has on anything
// but an object is an error, which -e reads as false; the second result says
// whether the question could be asked at all.
func predecessorApplyHas(document any, key string, path ...string) (has, asked bool) {
	object, isObject := predecessorApplyAt(document, path...).(map[string]any)
	if !isObject {
		return false, false
	}
	_, has = object[key]
	return has, true
}

// predecessorApplyLacks is jq's `has(key) | not` under -e: true only when the
// object exists and does not carry the key.
func predecessorApplyLacks(document any, key string, path ...string) bool {
	has, asked := predecessorApplyHas(document, key, path...)
	return asked && !has
}

// predecessorApplyConditions is `.status.conditions // []`.
func predecessorApplyConditions(document any) []map[string]any {
	list, _ := predecessorApplyAt(document, "status", "conditions").([]any)
	conditions := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if condition, isObject := entry.(map[string]any); isObject {
			conditions = append(conditions, condition)
		} else {
			conditions = append(conditions, map[string]any{})
		}
	}
	return conditions
}

// predecessorApplyHasCondition is `any(.type == t and .status == s)` over the
// document's conditions.
func predecessorApplyHasCondition(document any, conditionType, status string) bool {
	for _, condition := range predecessorApplyConditions(document) {
		if condition["type"] == conditionType && condition["status"] == status {
			return true
		}
	}
	return false
}

// predecessorApplyTerminalFailure is the reading start_running_apply_fixture
// refuses to wait past: the Apply's outcome already unknown, or the schema
// reporting a reconciliation failure.
func predecessorApplyTerminalFailure(schema map[string]any) bool {
	return predecessorApplyAt(schema, "status", "pendingObservation", "outcome") == "OutcomeUnknown" ||
		predecessorApplyHasCondition(schema, "ReconciliationFailed", "True")
}

// predecessorApplyDispatchedJob is the Apply Job the schema's claim names once
// its dispatch started, and the UID the claim committed; either is empty until
// it is there.
func predecessorApplyDispatchedJob(schema map[string]any) (name, uid string) {
	operation := predecessorApplyAt(schema, "status", "activeOperation")
	if predecessorApplyAt(operation, "type") == "Apply" && predecessorApplyAt(operation, "dispatchStarted") == true {
		name, _ = predecessorApplyAt(operation, "jobName").(string)
	}
	uid, _ = predecessorApplyAt(operation, "jobUID").(string)
	return name, uid
}

// predecessorApplyRunningPod is the one Running Pod the Job with the UID
// controls.
func predecessorApplyRunningPod(pods []corev1.Pod, jobUID types.UID) (corev1.Pod, bool) {
	var running []corev1.Pod
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, owner := range pod.OwnerReferences {
			if owner.APIVersion == "batch/v1" && owner.Kind == "Job" && owner.UID == jobUID &&
				owner.Controller != nil && *owner.Controller {
				running = append(running, pod)
				break
			}
		}
	}
	if len(running) != 1 {
		return corev1.Pod{}, false
	}
	return running[0], true
}

// predecessorApplyJobCarriesIdentity is the dispatched operation identity the
// Apply Job has to carry: the schema and operation labels, the plan
// fingerprint and admission snapshot digests, the same annotations on its Pod
// template, and no finished-Job lifetime yet.
func predecessorApplyJobCarriesIdentity(job map[string]any, schema string) bool {
	labels := predecessorApplyAt(job, "metadata", "labels")
	annotations := predecessorApplyAt(job, "metadata", "annotations")
	planFingerprint, _ := predecessorApplyAt(annotations, "operator.ptah.run/plan-fingerprint").(string)
	snapshot, _ := predecessorApplyAt(annotations, "operator.ptah.run/admission-snapshot-digest").(string)
	return predecessorApplyAt(labels, "operator.ptah.run/schema") == schema &&
		predecessorApplyAt(labels, "operator.ptah.run/operation") == "apply" &&
		sha256Pattern.MatchString(planFingerprint) && sha256Pattern.MatchString(snapshot) &&
		reflect.DeepEqual(predecessorApplyAt(job, "spec", "template", "metadata", "annotations"), annotations) &&
		predecessorApplyLacks(job, "ttlSecondsAfterFinished", "spec")
}

// predecessorApplyJobFinished is `any((.type == "Complete" or .type ==
// "Failed") and .status == "True")` over the Job's conditions.
func predecessorApplyJobFinished(job map[string]any) bool {
	return predecessorApplyHasCondition(job, "Complete", "True") || predecessorApplyHasCondition(job, "Failed", "True")
}

// predecessorApplyJobRunning is the Apply Job still running: the UID, no
// finished condition, and no finished-Job lifetime.
func predecessorApplyJobRunning(job map[string]any, uid string) bool {
	return predecessorApplyAt(job, "metadata", "uid") == uid && !predecessorApplyJobFinished(job) &&
		predecessorApplyLacks(job, "ttlSecondsAfterFinished", "spec")
}

// predecessorApplyJobTerminal is the Apply Job with the UID finished.
func predecessorApplyJobTerminal(job map[string]any, uid string) bool {
	return predecessorApplyAt(job, "metadata", "uid") == uid && predecessorApplyJobFinished(job)
}

// predecessorApplyPodIn is the Pod with the UID in one of the phases.
func predecessorApplyPodIn(pod *corev1.Pod, uid types.UID, phases ...corev1.PodPhase) bool {
	if pod.UID != uid {
		return false
	}
	for _, phase := range phases {
		if pod.Status.Phase == phase {
			return true
		}
	}
	return false
}

// predecessorApplyJobEvidence is what the successor may not change about the
// adopted Job: its identity, metadata and spec, the finished-Job lifetime
// aside, which is the one field it is expected to set.
func predecessorApplyJobEvidence(job map[string]any) ([]byte, error) {
	finalizers := predecessorApplyAt(job, "metadata", "finalizers")
	if finalizers == nil {
		finalizers = []any{}
	}
	var spec any
	if object, isObject := predecessorApplyAt(job, "spec").(map[string]any); isObject {
		copied := make(map[string]any, len(object))
		for key, value := range object {
			if key != "ttlSecondsAfterFinished" {
				copied[key] = value
			}
		}
		spec = copied
	}
	return json.Marshal(map[string]any{
		"uid":             predecessorApplyAt(job, "metadata", "uid"),
		"name":            predecessorApplyAt(job, "metadata", "name"),
		"namespace":       predecessorApplyAt(job, "metadata", "namespace"),
		"labels":          predecessorApplyAt(job, "metadata", "labels"),
		"annotations":     predecessorApplyAt(job, "metadata", "annotations"),
		"ownerReferences": predecessorApplyAt(job, "metadata", "ownerReferences"),
		"finalizers":      finalizers,
		"spec":            spec,
	})
}

// predecessorApplyStagedGapCheck is one condition of the staged Job UID gap,
// with the reason that names it when it does not hold.
type predecessorApplyStagedGapCheck struct {
	holds  func(schema map[string]any, jobName string) bool
	reason string
}

// predecessorApplyStagedGapChecks are the gap's conditions in the script's
// order: still an Apply claim with a started dispatch naming the Job, no Job
// UID recorded, and nothing retired into a pending observation.
var predecessorApplyStagedGapChecks = []predecessorApplyStagedGapCheck{
	{func(schema map[string]any, _ string) bool {
		return predecessorApplyAt(schema, "status", "activeOperation", "type") == "Apply"
	}, "the claim is no longer an Apply"},
	{func(schema map[string]any, _ string) bool {
		return predecessorApplyAt(schema, "status", "activeOperation", "dispatchStarted") == true
	}, "the claim no longer records a started dispatch"},
	{func(schema map[string]any, jobName string) bool {
		return predecessorApplyAt(schema, "status", "activeOperation", "jobName") == jobName
	}, "the claim names another Job"},
	{func(schema map[string]any, _ string) bool {
		return predecessorApplyLacks(schema, "jobUID", "status", "activeOperation")
	}, "the manager recorded the Job UID again before the upgrade"},
	{func(schema map[string]any, _ string) bool {
		return predecessorApplyLacks(schema, "pendingObservation", "status")
	}, "the Apply was already retired into a pending observation"},
}

// predecessorApplyBindingKept is the successor keeping the epoch the Apply was
// dispatched in: the same epoch as the staged reading, and no condition that
// says the binding changed.
func predecessorApplyBindingKept(schema, staged map[string]any) bool {
	if !reflect.DeepEqual(predecessorApplyAt(schema, "status", "executionBinding", "epoch"),
		predecessorApplyAt(staged, "status", "executionBinding", "epoch")) {
		return false
	}
	for _, condition := range predecessorApplyConditions(schema) {
		if condition["reason"] == "ExecutionBindingChanged" {
			return false
		}
	}
	return true
}

// predecessorApplyExclusive is the successor adopting the running Apply as its
// own: the claim the staged reading had, the Job and the UID the harness
// removed recorded again, the claim under the current epoch, and nothing
// retired while the Apply is still inside the engine.
func predecessorApplyExclusive(schema, staged map[string]any, jobName, jobUID string) bool {
	operation := predecessorApplyAt(schema, "status", "activeOperation")
	return predecessorApplyBindingKept(schema, staged) &&
		predecessorApplyAt(operation, "type") == "Apply" &&
		reflect.DeepEqual(predecessorApplyAt(operation, "id"), predecessorApplyAt(staged, "status", "activeOperation", "id")) &&
		predecessorApplyAt(operation, "jobName") == jobName &&
		predecessorApplyAt(operation, "jobUID") == jobUID &&
		predecessorApplyAt(operation, "dispatchStarted") == true &&
		reflect.DeepEqual(predecessorApplyAt(operation, "executionBindingID"),
			predecessorApplyAt(schema, "status", "executionBinding", "epoch")) &&
		predecessorApplyLacks(schema, "pendingObservation", "status")
}

// predecessorApplyDispatcher is the manager the Job's Pod template records as
// the one that dispatched it.
func predecessorApplyDispatcher(job map[string]any) string {
	dispatcher, _ := predecessorApplyAt(job, "spec", "template", "metadata", "annotations",
		predecessorApplyDispatcherAnnotation).(string)
	return dispatcher
}

// predecessorApplyAccounted is the adopted Apply accounted for through its own
// result under the epoch it was dispatched in: a pending observation naming
// the Job, applied or unknown, or the applied record naming its plan, either
// one naming the manager that dispatched it.
func predecessorApplyAccounted(schema, staged map[string]any, jobName, jobUID, planUID, dispatcher string) bool {
	if !predecessorApplyBindingKept(schema, staged) {
		return false
	}
	epoch := predecessorApplyAt(schema, "status", "executionBinding", "epoch")
	pending := predecessorApplyAt(schema, "status", "pendingObservation")
	outcome := predecessorApplyAt(pending, "outcome")
	observed := predecessorApplyAt(pending, "applyJobName") == jobName &&
		predecessorApplyAt(pending, "applyJobUID") == jobUID &&
		(outcome == "ApplySucceeded" || outcome == "OutcomeUnknown") &&
		reflect.DeepEqual(predecessorApplyAt(pending, "plan", "executionBindingID"), epoch) &&
		predecessorApplyAt(pending, "dispatchedBy", "controllerImage") == dispatcher
	applied := predecessorApplyAt(schema, "status", "applied")
	recorded := predecessorApplyAt(applied, "planRef", "uid") == planUID &&
		reflect.DeepEqual(predecessorApplyAt(applied, "executionBindingID"), epoch) &&
		predecessorApplyAt(applied, "dispatchedBy", "controllerImage") == dispatcher
	return observed || recorded
}

// predecessorApplyTTL is `.spec.ttlSecondsAfterFinished // 0`.
func predecessorApplyTTL(job map[string]any) int64 {
	value, found, err := unstructuredInt64(job, "spec", "ttlSecondsAfterFinished")
	if !found || err != nil {
		return 0
	}
	return value
}

// predecessorApplySchemaDiagnostic and predecessorApplyJobsDiagnostic are the
// lines emit_running_apply_diagnostic wrote: what the schema decided and what
// its Jobs report, neither of which carries a row, SQL or a credential.
func predecessorApplySchemaDiagnostic(schema map[string]any) ([]byte, error) {
	conditions := []any{}
	for _, condition := range predecessorApplyConditions(schema) {
		conditions = append(conditions, map[string]any{
			"type": condition["type"], "status": condition["status"], "reason": condition["reason"],
		})
	}
	return json.Marshal(map[string]any{
		"phase":              predecessorApplyAt(schema, "status", "phase"),
		"activeOperation":    predecessorApplyAt(schema, "status", "activeOperation"),
		"pendingObservation": predecessorApplyAt(schema, "status", "pendingObservation"),
		"conditions":         conditions,
	})
}

func predecessorApplyJobsDiagnostic(jobs []map[string]any) ([]byte, error) {
	entries := []any{}
	for _, job := range jobs {
		conditions := []any{}
		for _, condition := range predecessorApplyConditions(job) {
			conditions = append(conditions, map[string]any{"type": condition["type"], "status": condition["status"]})
		}
		entries = append(entries, map[string]any{
			"name": predecessorApplyAt(job, "metadata", "name"), "uid": predecessorApplyAt(job, "metadata", "uid"),
			"conditions": conditions,
		})
	}
	return json.Marshal(entries)
}
