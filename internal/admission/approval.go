// Package admission implements the independently authorized approval boundary.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/policy"
)

const maxRecordedGroups = 64

// Execution is what the manager serving the webhook executes with: the
// components a plan binds. A plan computed under anything else is not
// approvable here.
//
// The manager's own image and revision are deliberately not part of it. A
// plan records the manager that published it, and a later release of the
// manager that shares this execution may accept an approval for that plan and
// apply it.
type Execution struct {
	ControllerStateVersion int32
	PtahVersion            string
	ExecutorImage          string
	RunnerProtocolVersion  int32
}

// valid refuses an execution nothing could have been planned under. The
// manager validated the configured images at startup; this only keeps a
// zero-valued handler from matching a zero-valued plan.
func (e Execution) valid() bool {
	return e.ControllerStateVersion >= 1 && e.RunnerProtocolVersion >= 1 &&
		strings.TrimSpace(e.PtahVersion) != "" && strings.TrimSpace(e.PtahVersion) == e.PtahVersion &&
		strings.TrimSpace(e.ExecutorImage) != "" && strings.TrimSpace(e.ExecutorImage) == e.ExecutorImage
}

// binds refuses a plan computed under another execution than this one.
func (e Execution) binds(controllerStateVersion int32, ptahVersion, executorImage string, runnerProtocolVersion int32) error {
	if controllerStateVersion != e.ControllerStateVersion || ptahVersion != e.PtahVersion ||
		executorImage != e.ExecutorImage || runnerProtocolVersion != e.RunnerProtocolVersion {
		return fmt.Errorf("referenced plan was computed under an execution binding this manager does not run")
	}
	return nil
}

// Clock makes admission timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ApprovalHandler stamps authenticated identity and rejects an approval whose
// three identifiers -- schema, plan and plan fingerprint -- do not name the
// live plan, against direct, uncached API reads.
type ApprovalHandler struct {
	Reader    client.Reader
	Decoder   cradmission.Decoder
	Clock     Clock
	Mutate    bool
	Execution Execution

	// RequireDistinctApprover is the four-eyes control, set once for the whole
	// installation from the manager's own flag: an author who can also reach
	// the RBAC an approver needs cannot turn this off from inside a schema's
	// own spec, because it is not there to turn off. When true, every approval
	// is refused if its approver is exactly the identity refuseSelfApproval
	// finds recorded as the schema's last spec writer, or if none is recorded.
	RequireDistinctApprover bool
}

// Handle implements controller-runtime admission.Handler.
func (h *ApprovalHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil || !h.Execution.valid() {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("approval webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and metadata-preserving updates are supported")
	}

	approval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := h.Decoder.Decode(req, approval); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode approval: %w", err))
	}
	if approval.Namespace != req.Namespace {
		return cradmission.Denied("approval namespace does not match the admission request")
	}

	if req.Operation == admissionv1.Update {
		oldApproval := &operatorv1alpha1.PtahSchemaApproval{}
		if err := h.Decoder.DecodeRaw(req.OldObject, oldApproval); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode previous approval: %w", err))
		}
		if !reflect.DeepEqual(approval.Spec, oldApproval.Spec) {
			return cradmission.Denied("approval spec is immutable; create a new approval")
		}
		return cradmission.Allowed("approval metadata update preserves the immutable decision")
	}

	if h.Mutate {
		h.stampIdentity(approval, req.UserInfo, req.UID)
		if err := h.validateBinding(ctx, approval); err != nil {
			return denialFor(err)
		}
		mutated, err := json.Marshal(approval)
		if err != nil {
			return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped approval: %w", err))
		}
		return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
	}

	if err := identityMatchesRequest(approval.Spec, req.UserInfo); err != nil {
		return cradmission.Denied(err.Error())
	}
	if err := h.validateBinding(ctx, approval); err != nil {
		return denialFor(err)
	}
	return cradmission.Allowed("approval is bound to the current immutable plan")
}

// requireExplicitDecision refuses an approval that leaves any part of the
// decision to be inferred. The approver names the resource, the plan and the
// plan's fingerprint. Nothing is filled in from the plan: the fingerprint
// already names everything the plan was decided from.
func requireExplicitDecision(
	kind string,
	ownerRef, planRef operatorv1alpha1.ImmutableObjectReference,
	planFingerprint string,
) error {
	if strings.TrimSpace(ownerRef.Name) == "" || ownerRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the %s name and UID", kind)
	}
	if strings.TrimSpace(planRef.Name) == "" || planRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the plan name and UID")
	}
	if strings.TrimSpace(planFingerprint) == "" {
		return fmt.Errorf("approval must explicitly identify the plan fingerprint")
	}
	return nil
}

func (h *ApprovalHandler) stampIdentity(
	approval *operatorv1alpha1.PtahSchemaApproval,
	user authenticationv1.UserInfo,
	requestUID types.UID,
) {
	clock := h.Clock
	if clock == nil {
		clock = realClock{}
	}
	approval.Spec.Approver = operatorv1alpha1.ApprovalIdentity{
		Username: strings.TrimSpace(user.Username),
		UID:      strings.TrimSpace(user.UID),
		Groups:   normalizedGroups(user.Groups),
	}
	approval.Spec.ApprovedAt = metav1.NewTime(clock.Now().UTC())
	approval.Spec.MutationRequestUID = string(requestUID)
}

func identityMatchesRequest(
	spec operatorv1alpha1.PtahSchemaApprovalSpec,
	user authenticationv1.UserInfo,
) error {
	if strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("authenticated approval username is empty")
	}
	if spec.Approver.Username != strings.TrimSpace(user.Username) ||
		spec.Approver.UID != strings.TrimSpace(user.UID) ||
		!slices.Equal(spec.Approver.Groups, normalizedGroups(user.Groups)) {
		return fmt.Errorf("reserved approver identity fields do not match the authenticated request")
	}
	if strings.TrimSpace(spec.MutationRequestUID) == "" {
		return fmt.Errorf("approval identity was not stamped by the mutating admission webhook")
	}
	if spec.ApprovedAt.IsZero() {
		return fmt.Errorf("approvedAt was not stamped by the admission webhook")
	}
	return nil
}

func normalizedGroups(groups []string) []string {
	seen := make(map[string]struct{}, len(groups))
	normalized := make([]string, 0, min(len(groups), maxRecordedGroups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		normalized = append(normalized, group)
	}
	sort.Strings(normalized)
	if len(normalized) > maxRecordedGroups {
		normalized = normalized[:maxRecordedGroups]
	}
	return normalized
}

// validateBinding refuses every approval the current evidence cannot support:
// one that names a plan by a UID or a fingerprint the live plan does not
// have, a plan of another schema, a plan this manager cannot execute, or a
// schema that is not waiting for exactly this decision. It reads the plan
// and the schema directly from the API server, in both admission passes.
func (h *ApprovalHandler) validateBinding(
	ctx context.Context,
	approval *operatorv1alpha1.PtahSchemaApproval,
) error {
	if err := requireExplicitDecision(
		"schema", approval.Spec.SchemaRef, approval.Spec.PlanRef, approval.Spec.PlanFingerprint,
	); err != nil {
		return err
	}
	namespace := approval.Namespace
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: approval.Spec.PlanRef.Name}, plan); err != nil {
		return fmt.Errorf("read referenced plan: %w", err)
	}
	if plan.DeletionTimestamp != nil {
		return fmt.Errorf("referenced plan is being deleted")
	}
	if plan.UID != approval.Spec.PlanRef.UID {
		return fmt.Errorf("referenced plan UID does not match; the plan was replaced")
	}
	if plan.Spec.SchemaRef != approval.Spec.SchemaRef {
		return fmt.Errorf("approval schema reference does not match the plan")
	}
	if approval.Spec.PlanFingerprint != plan.Spec.Fingerprint {
		return fmt.Errorf("approval plan fingerprint does not match the immutable plan")
	}
	if err := requireCurrentPlanContract(plan.Spec.ContractVersion); err != nil {
		return err
	}
	if err := h.Execution.binds(
		plan.Spec.ControllerStateVersion, plan.Spec.PtahVersion, plan.Spec.ExecutorImage, plan.Spec.RunnerProtocolVersion,
	); err != nil {
		return err
	}
	if plan.Status.ObservedGeneration != plan.Generation ||
		!meta.IsStatusConditionTrue(plan.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady) {
		return fmt.Errorf("referenced plan storage is not ready")
	}

	schema := &operatorv1alpha1.PtahSchema{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: approval.Spec.SchemaRef.Name}, schema); err != nil {
		return fmt.Errorf("read referenced schema: %w", err)
	}
	if schema.DeletionTimestamp != nil {
		return fmt.Errorf("referenced schema is being deleted")
	}
	if schema.UID != approval.Spec.SchemaRef.UID || plan.Spec.SchemaRef.UID != schema.UID ||
		plan.Spec.SchemaRef.Name != schema.Name {
		return fmt.Errorf("schema UID does not match the plan binding")
	}
	if schema.Status.Plan == nil || schema.Status.Plan.UID != plan.UID ||
		schema.Status.Plan.Fingerprint != plan.Spec.Fingerprint ||
		schema.Status.ExecutionBinding == nil || schema.Status.ExecutionBinding.Epoch == "" ||
		schema.Status.ExecutionBinding.Epoch != plan.Spec.ExecutionBindingID ||
		schema.Status.Plan.ExecutionBindingID != plan.Spec.ExecutionBindingID ||
		schema.Status.Plan.ControllerStateVersion < 1 ||
		schema.Status.Plan.ControllerStateVersion != plan.Spec.ControllerStateVersion {
		return fmt.Errorf("referenced plan is no longer current for the schema")
	}
	if schema.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		return fmt.Errorf("referenced schema is not awaiting approval")
	}
	if !meta.IsStatusConditionTrue(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired) {
		return fmt.Errorf("referenced schema does not currently require approval")
	}
	if schema.Status.ActiveOperation != nil {
		return fmt.Errorf("referenced schema has an active operation")
	}
	if schema.Status.Plan.Approval != nil {
		return fmt.Errorf("referenced plan already has a recorded approval")
	}
	if h.RequireDistinctApprover {
		if err := refuseSelfApproval("schema", schema.Annotations, approval.Spec.Approver); err != nil {
			return err
		}
	}

	coordinationDigest, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil {
		return fmt.Errorf("derive current database coordination digest: %w", err)
	}
	if schema.Status.Source.Digest != plan.Spec.ArtifactDigest ||
		coordinationDigest != plan.Spec.CoordinationDigest ||
		schema.Status.Target.CoordinationDigest != plan.Spec.CoordinationDigest ||
		schema.Status.Target.IdentityDigest != plan.Spec.TargetIdentityDigest ||
		schema.Status.ExecutionBinding.ControllerStateVersion != plan.Spec.ControllerStateVersion ||
		schema.Status.ExecutionBinding.PtahVersion != plan.Spec.PtahVersion ||
		schema.Status.ExecutionBinding.ExecutorImage != plan.Spec.ExecutorImage ||
		schema.Status.ExecutionBinding.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("schema source or target changed after the plan was generated")
	}
	policyBinding, err := policy.ConfigMapBinding(ctx, h.Reader, namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil {
		return err
	}
	if policyBinding.UID != plan.Spec.VerificationPolicyUID || policyBinding.Digest != plan.Spec.VerificationPolicyDigest {
		return fmt.Errorf("verification policy changed after the plan was generated")
	}
	return nil
}

func requireCurrentPlanContract(version int32) error {
	if err := fingerprint.ValidatePlanContractVersion(version); err != nil {
		return fmt.Errorf("referenced plan contract is not supported: %w", err)
	}
	return nil
}

func denialFor(err error) cradmission.Response {
	if apierrors.IsNotFound(err) {
		return cradmission.Errored(http.StatusNotFound, err)
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return cradmission.Errored(http.StatusForbidden, err)
	}
	return cradmission.Denied(err.Error())
}
