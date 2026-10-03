package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// planDocument is the native plan Ptah writes, as the plan's chunks hold it.
type planDocument struct {
	FormatVersion   int      `json:"format_version"`
	Dialect         string   `json:"dialect"`
	FromFingerprint string   `json:"from_fingerprint"`
	ToFingerprint   string   `json:"to_fingerprint"`
	Exclude         []string `json:"exclude,omitempty"`
	// Destructive is nil when the document carries no destructive key,
	// which a check for false has to refuse as jq's `== false` did.
	Destructive *bool           `json:"destructive"`
	Statements  []planStatement `json:"statements"`
}

type planStatement struct {
	SQL      string `json:"sql"`
	Severity string `json:"severity"`
}

func parsePlanDocument(content []byte) (planDocument, error) {
	var document planDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return planDocument{}, fmt.Errorf("the plan document does not parse: %w", err)
	}
	return document, nil
}

// sealedPayloadLeak reports plan text a Plan result carries in the clear.
// The frame's stdout is the plan encrypted to the manager's per-process key,
// so the document whose digest the result names must not be readable from
// the result itself: stdout is not empty, it is not the document, and it
// holds neither the document's own "format_version" key nor the opening of
// any statement in it. Each opening is searched for both as SQL and as the
// JSON string a plan document spells it in, since a document escapes a quote
// or a newline.
//
// An opening shorter than eight characters is not searched for: a fragment
// that short can sit inside a base64 payload by chance, and the document's own
// key catches a plaintext document regardless.
func sealedPayloadLeak(stdout string, document []byte) error {
	if stdout == "" {
		return errors.New("plan result carries no sealed payload in stdout")
	}
	if stdout == string(document) {
		return errors.New("plan result carries the plan document itself in stdout, not a sealed payload")
	}
	parsed, err := parsePlanDocument(document)
	if err != nil || len(parsed.Statements) == 0 {
		return errors.New("plan document has no statements to check the sealed payload against")
	}
	for _, pattern := range planTextPatterns(parsed) {
		if strings.Contains(stdout, pattern) {
			return errors.New("plan result stdout carries plan text in the clear")
		}
	}
	return nil
}

// confidentialPlanDelivery distinguishes the private durable representation
// from output written to container logs. Both transports must keep plan text
// out of diagnostics; only the legacy representation is process-sealed.
func confidentialPlanDelivery(result runner.Result, document, logs []byte, durable bool) error {
	parsed, err := parsePlanDocument(document)
	if err != nil || len(parsed.Statements) == 0 {
		return errors.New("plan document has no statements to check delivery against")
	}
	if durable {
		if result.Stdout != string(document) || result.PlanContentDigest != sha256Digest(document) {
			return errors.New("durable result differs from the persisted plan bytes or digest")
		}
	} else if err := sealedPayloadLeak(result.Stdout, document); err != nil {
		return err
	}
	for _, pattern := range planTextPatterns(parsed) {
		if bytes.Contains(logs, []byte(pattern)) {
			return errors.New("runner diagnostics disclose plan text")
		}
	}
	return nil
}

func planTextPatterns(document planDocument) []string {
	var patterns []string
	for _, statement := range document.Statements {
		opening := []rune(statement.SQL)
		if len(opening) > 40 {
			opening = opening[:40]
		}
		for line := range strings.SplitSeq(string(opening), "\n") {
			if len([]rune(line)) >= 8 {
				patterns = append(patterns, line)
			}
		}
		if spelled := jsonStringBody(string(opening)); len([]rune(spelled)) >= 8 {
			patterns = append(patterns, spelled)
		}
	}
	return append(patterns, `"format_version"`)
}

// jsonStringBody is how a JSON document spells a string, without the quotes
// around it: jq's tojson, which escapes neither HTML nor non-ASCII text.
func jsonStringBody(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	encoded := strings.TrimSuffix(buffer.String(), "\n")
	return encoded[1 : len(encoded)-1]
}

// rebuiltPlanDocument reads a plan document back the way the controller does:
// from the PtahSchemaPlanChunk objects the plan names, in spec.chunks order,
// each chunk's bytes concatenated. A Plan result's stdout carries the plan
// sealed to the manager's per-process key, so the chunks are the only place
// the plaintext a content digest covers can be read from. A plan that names
// no chunk is refused rather than rebuilt as an empty document.
func rebuiltPlanDocument(plan *ptahv1alpha1.PtahSchemaPlan,
	chunk func(string) (*ptahv1alpha1.PtahSchemaPlanChunk, error),
) ([]byte, error) {
	if len(plan.Spec.Chunks) == 0 {
		return nil, fmt.Errorf("%s has no plan chunks to rebuild its document from", plan.Name)
	}
	var document []byte
	for position, reference := range plan.Spec.Chunks {
		if int(reference.Index) != position {
			return nil, fmt.Errorf("%s chunk at position %d carries index %d", plan.Name, position, reference.Index)
		}
		if reference.Name == "" || reference.Size == 0 {
			return nil, fmt.Errorf("%s chunk %d is an incomplete reference", plan.Name, reference.Index)
		}
		stored, err := chunk(reference.Name)
		if err != nil {
			return nil, fmt.Errorf("%s chunk %d PtahSchemaPlanChunk %s could not be read: %w",
				plan.Name, reference.Index, reference.Name, err)
		}
		if len(stored.Spec.Data) != int(reference.Size) {
			return nil, fmt.Errorf("%s chunk %d decoded to %d bytes; its manifest says %d",
				plan.Name, reference.Index, len(stored.Spec.Data), reference.Size)
		}
		document = append(document, stored.Spec.Data...)
	}
	return document, nil
}

// planProjectedExactly holds the ConfigMaps an Apply mounted its plan through
// to the plan's chunks and nothing else: one per chunk, named for it,
// immutable, owned by the plan alone, and holding exactly that chunk's bytes.
// Each chunk is read from the API rather than from the projection, so a
// projection carrying other bytes is compared with the plan and not with
// itself.
func planProjectedExactly(plan *ptahv1alpha1.PtahSchemaPlan, projections []corev1.ConfigMap,
	chunk func(string) (*ptahv1alpha1.PtahSchemaPlanChunk, error),
) error {
	if len(plan.Spec.Chunks) == 0 {
		return fmt.Errorf("%s names no chunks", plan.Name)
	}
	if len(projections) != len(plan.Spec.Chunks) {
		return fmt.Errorf("%s was projected into %d ConfigMaps for its %d chunks", plan.Name, len(projections), len(plan.Spec.Chunks))
	}
	for _, reference := range plan.Spec.Chunks {
		stored, err := chunk(reference.Name)
		if err != nil {
			return fmt.Errorf("%s chunk %s could not be read to compare with its projection", plan.Name, reference.Name)
		}
		var matches []corev1.ConfigMap
		for _, projection := range projections {
			if projection.Name == reference.Name {
				matches = append(matches, projection)
			}
		}
		if len(matches) != 1 || !projectionExact(&matches[0], plan, stored.Spec.Data) {
			return fmt.Errorf("%s projection %s is not the immutable copy of its chunk the Apply was approved for",
				plan.Name, reference.Name)
		}
	}
	return nil
}

func projectionExact(projection *corev1.ConfigMap, plan *ptahv1alpha1.PtahSchemaPlan, data []byte) bool {
	chunk, found := projection.BinaryData["chunk"]
	return projection.Immutable != nil && *projection.Immutable && len(projection.OwnerReferences) == 1 &&
		ownedExactlyOnce(projection.OwnerReferences, ptahSchemaAPIVersion, "PtahSchemaPlan", plan.Name, plan.UID) &&
		len(projection.Data) == 0 && len(projection.BinaryData) == 1 && found && bytes.Equal(chunk, data)
}

// isTrue is a flag present and true.
func isTrue(flag *bool) bool {
	return flag != nil && *flag
}

// isFalse is a flag present and false: an absent flag is not false.
func isFalse(flag *bool) bool {
	return flag != nil && !*flag
}

// holdsString reports whether any scalar in a JSON document equals value: a
// coordination key the operator must keep out of everything it writes.
func holdsString(document any, value string) bool {
	switch typed := document.(type) {
	case string:
		return typed == value
	case map[string]any:
		for _, child := range typed {
			if holdsString(child, value) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, func(child any) bool { return holdsString(child, value) })
	}
	return false
}

// asJSON is an object as a generic JSON document, the way jq reads it.
func asJSON(object any) (any, error) {
	content, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var document any
	return document, json.Unmarshal(content, &document)
}

func conditionIs(conditions []metav1.Condition, kind string, status metav1.ConditionStatus, reason string) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == kind && condition.Status == status && condition.Reason == reason
	})
}

// resolvedReference is the reference a schema reads its artifact by once the
// tag is resolved: the digest in place of the tag, or the reference itself
// when it already names a digest.
func resolvedReference(reference, digest string) string {
	if digestSuffix.MatchString(reference) {
		return reference
	}
	return tagSuffix.ReplaceAllString(reference, "@"+digest)
}

var (
	digestSuffix = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	tagSuffix    = regexp.MustCompile(`:[^/:]+$`)
)

// planStatusBound holds a schema's evidence of the source it resolved and
// verified and the target it observed, and the plan it published, to the
// execution binding and the manager that published it.
func planStatusBound(schema *ptahv1alpha1.PtahSchema, reference, digest string, controller controllerIdentity,
	stateVersion int32,
) error {
	status := schema.Status
	source, binding, plan := status.Source, status.ExecutionBinding, status.Plan
	switch {
	case source.RequestedReference != reference || source.Digest != digest ||
		source.ResolvedReference != resolvedReference(reference, digest) || !source.Verified ||
		source.ArtifactType != schemaArtifactType || source.VerificationPolicyDigest == "":
		return errors.New("the source is not the resolved and verified reference")
	case status.Target.IdentityDigest == "" || status.Target.DriftReportDigest == "":
		return errors.New("the target carries no identity or drift report")
	case binding == nil || !executionEpoch.MatchString(binding.Epoch) || binding.ControllerStateVersion != stateVersion:
		return errors.New("the execution binding is not this release's")
	case plan == nil || plan.ExecutionBindingID != binding.Epoch || plan.ControllerImage != controller.image ||
		plan.ControllerRevision != controller.revision || plan.ControllerStateVersion != stateVersion:
		return errors.New("the current plan is not bound to this manager")
	}
	return nil
}

// committedPlan holds a published plan to the contract it was published
// under: the schema, the artifact and dialect it plans, the execution binding
// and manager behind it, a fingerprint of the state it plans from, its
// destructiveness, and at least one statement in at least one chunk.
func committedPlan(plan *ptahv1alpha1.PtahSchemaPlan, schema, digest, dialect string, destructive bool,
	controller controllerIdentity, stateVersion int32,
) error {
	spec := plan.Spec
	switch {
	case spec.ContractVersion != fingerprint.CurrentPlanContractVersion || spec.SchemaRef.Name != schema || spec.ArtifactDigest != digest || spec.Dialect != dialect:
		return errors.New("the plan is not this schema's plan of this artifact")
	case !executionEpoch.MatchString(spec.ExecutionBindingID) || spec.ControllerImage != controller.image ||
		spec.ControllerRevision != controller.revision || spec.ControllerStateVersion != stateVersion:
		return errors.New("the plan is not bound to this manager")
	case !sha256Pattern.MatchString(spec.ActualStateFingerprint):
		return errors.New("the plan names no actual-state fingerprint")
	case spec.Destructive != destructive:
		return fmt.Errorf("the plan's destructive is %t", spec.Destructive)
	case spec.StatementCount <= 0 || len(spec.Chunks) == 0:
		return errors.New("the plan holds no statement")
	case !conditionStatus(plan.Status.Conditions, "Ready", metav1.ConditionTrue):
		return errors.New("the plan is not Ready")
	}
	return nil
}

// conditionStatus is a condition of the type with the status, whatever its
// reason.
func conditionStatus(conditions []metav1.Condition, kind string, status metav1.ConditionStatus) bool {
	return slices.ContainsFunc(conditions, func(condition metav1.Condition) bool {
		return condition.Type == kind && condition.Status == status
	})
}

var driftSeverities = []string{"safe", "info", "warning", "error", "destructive"}

// observedDriftBound holds a changed schema's Observe result to the drift
// evidence the schema persisted from it.
func observedDriftBound(result runner.Result, target ptahv1alpha1.TargetStatus, dialect string) error {
	switch {
	case result.Error != nil || result.Stdout != "" || (result.ChildExitCode != 0 && result.ChildExitCode != 1):
		return errors.New("the Observe result is not a clean reading")
	case result.CoordinationDigest != target.CoordinationDigest || result.TargetIdentityDigest != target.IdentityDigest ||
		result.DriftReportDigest != target.DriftReportDigest:
		return errors.New("the Observe result names another target or drift report")
	case !result.ObservedDrift || result.DriftFindingCount <= 0 || !slices.Contains(driftSeverities, result.HighestDriftSeverity):
		return errors.New("the Observe result found no drift")
	}
	switch dialect {
	case "postgres":
		if result.ObservedDialect != "postgres" && result.ObservedDialect != "postgresql" {
			return fmt.Errorf("the Observe result read dialect %q", result.ObservedDialect)
		}
	case "mysql":
		if result.ObservedDialect != "mysql" && result.ObservedDialect != "mariadb" {
			return fmt.Errorf("the Observe result read dialect %q", result.ObservedDialect)
		}
	default:
		return fmt.Errorf("no dialect %q", dialect)
	}
	return nil
}

// changedPlanBound holds a Plan result that found changes to the plan the
// schema published from it.
func changedPlanBound(result runner.Result, plan *ptahv1alpha1.CurrentPlanStatus) error {
	switch {
	case plan == nil:
		return errors.New("the schema names no current plan")
	case result.Error != nil || result.ChildExitCode != 0 || result.PlanOutcome != runner.PlanOutcomeChanges || result.Stdout == "":
		return errors.New("the Plan result is not a clean plan of changes")
	case result.PlanContentDigest != plan.ContentDigest || result.CoordinationDigest != plan.CoordinationDigest ||
		result.TargetIdentityDigest != plan.TargetIdentityDigest:
		return errors.New("the Plan result names another plan, realm or target")
	}
	return nil
}

// planBoundToDocument holds a published plan to the document its chunks
// rebuild to.
func planBoundToDocument(plan *ptahv1alpha1.PtahSchemaPlan, document planDocument, contentDigest string) error {
	spec := plan.Spec
	switch {
	case spec.ContentDigest != contentDigest:
		return errors.New("the plan's content digest is not its document's")
	case spec.ActualStateFingerprint != document.FromFingerprint || spec.DesiredStateFingerprint != document.ToFingerprint:
		return errors.New("the plan's state fingerprints are not its document's")
	case spec.Dialect != document.Dialect:
		return errors.New("the plan's dialect is not its document's")
	case isTrue(document.Destructive) && !spec.Destructive:
		return errors.New("the plan is not destructive and its document is")
	case int(spec.StatementCount) != len(document.Statements):
		return errors.New("the plan's statement count is not its document's")
	}
	return nil
}

// convergedResults holds the Observe and Plan results of a cycle that found
// nothing to do to the schema's converged status.
func convergedResults(schema *ptahv1alpha1.PtahSchema, observe, plan runner.Result) error {
	status := schema.Status
	target := status.Target
	switch {
	case observe.Error != nil || observe.ChildExitCode != 0 || observe.Stdout != "" || observe.ObservedDrift ||
		observe.HighestDriftSeverity != "" || observe.DriftFindingCount != 0:
		return errors.New("the Observe result found drift")
	case observe.CoordinationDigest != target.CoordinationDigest || observe.TargetIdentityDigest != target.IdentityDigest ||
		observe.DriftReportDigest != target.DriftReportDigest:
		return errors.New("the Observe result names another target")
	case plan.Error != nil || plan.ChildExitCode != 0 || plan.PlanOutcome != runner.PlanOutcomeNoChanges ||
		plan.Stdout != "" || plan.PlanContentDigest != "":
		return errors.New("the Plan result is not NoChanges")
	case plan.CoordinationDigest != target.CoordinationDigest || plan.TargetIdentityDigest != target.IdentityDigest:
		return errors.New("the Plan result names another target")
	case status.Plan != nil || status.PendingObservation != nil || status.ActiveOperation != nil || status.PendingLockRelease != nil:
		return errors.New("the schema still carries a plan, an observation, an operation or a lock")
	case !conditionIs(status.Conditions, "InSync", metav1.ConditionTrue, "ScopedConverged"):
		return errors.New("the schema is not InSync for ScopedConverged")
	}
	return nil
}

// appliedBound holds the applied evidence of a converged schema to the
// execution binding and the manager that applied it.
func appliedBound(schema *ptahv1alpha1.PtahSchema, controller controllerIdentity, stateVersion int32) error {
	binding, applied := schema.Status.ExecutionBinding, schema.Status.Applied
	switch {
	case binding == nil || !executionEpoch.MatchString(binding.Epoch) || binding.ControllerStateVersion != stateVersion:
		return errors.New("the execution binding is not this release's")
	case applied == nil || applied.ExecutionBindingID != binding.Epoch || applied.ControllerImage != controller.image ||
		applied.ControllerRevision != controller.revision || applied.ControllerStateVersion != stateVersion:
		return errors.New("the applied evidence is not bound to this manager")
	}
	return nil
}

// approvalConsumed is an approval whose Apply was dispatched and whose plan a
// later observation retired, with the history of both kept.
func approvalConsumed(approval *ptahv1alpha1.PtahSchemaApproval, planUID string) bool {
	conditions := approval.Status.Conditions
	return string(approval.Spec.PlanRef.UID) == planUID &&
		approval.Status.ObservedGeneration == approval.Generation &&
		conditionIs(conditions, "Accepted", metav1.ConditionFalse, "PlanNoLongerCurrent") &&
		conditionIs(conditions, "Consumed", metav1.ConditionTrue, "DispatchCommitted") &&
		conditionIs(conditions, "Stale", metav1.ConditionTrue, "PlanNoLongerCurrent")
}

// inSync is a schema converged on the digest, with nothing pending.
func inSync(schema *ptahv1alpha1.PtahSchema, digest string) bool {
	status := schema.Status
	return status.Phase == ptahv1alpha1.PhaseInSync && status.Source.Digest == digest &&
		status.Applied != nil && status.Applied.ArtifactDigest == digest &&
		status.PendingObservation == nil && status.ActiveOperation == nil && status.PendingLockRelease == nil &&
		conditionIs(status.Conditions, "InSync", metav1.ConditionTrue, "ScopedConverged")
}

// quiescentlySuspended is a schema that suspended at its current generation
// with nothing in flight.
func quiescentlySuspended(schema *ptahv1alpha1.PtahSchema) bool {
	status := schema.Status
	return status.ObservedGeneration == schema.Generation && status.Phase == ptahv1alpha1.PhaseSuspended &&
		status.ActiveOperation == nil && status.PendingObservation == nil && status.PendingLockRelease == nil
}

// due reports whether a persisted deadline has passed.
func due(deadline *metav1.Time, now time.Time) bool {
	return deadline != nil && !deadline.After(now)
}
