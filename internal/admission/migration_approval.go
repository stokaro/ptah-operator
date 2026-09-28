package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/policy"
)

// MigrationApprovalHandler stamps authenticated identity onto one migration
// approval and refuses a binding the current evidence no longer supports.
//
// It is a separate handler from the schema one because it checks a different
// thing: a migration approval's premise is the history the plan was computed
// against, so an approval that named only the plan would still be valid after
// somebody else's run moved the database underneath it.
type MigrationApprovalHandler struct {
	Reader    client.Reader
	Decoder   cradmission.Decoder
	Clock     Clock
	Mutate    bool
	Execution Execution

	// RequireDistinctApprover is ApprovalHandler's field of the same name,
	// applied to PtahMigration instead of PtahSchema.
	RequireDistinctApprover bool
}

// Handle implements controller-runtime admission.Handler.
func (h *MigrationApprovalHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil || !h.Execution.valid() {
		return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("migration approval webhook is not initialized"))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return cradmission.Denied("only create and metadata-preserving updates are supported")
	}

	approval := &operatorv1alpha1.PtahMigrationApproval{}
	if err := h.Decoder.Decode(req, approval); err != nil {
		return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode migration approval: %w", err))
	}
	if approval.Namespace != req.Namespace {
		return cradmission.Denied("approval namespace does not match the admission request")
	}

	if req.Operation == admissionv1.Update {
		previous := &operatorv1alpha1.PtahMigrationApproval{}
		if err := h.Decoder.DecodeRaw(req.OldObject, previous); err != nil {
			return cradmission.Errored(http.StatusBadRequest, fmt.Errorf("decode previous migration approval: %w", err))
		}
		if !reflect.DeepEqual(approval.Spec, previous.Spec) {
			return cradmission.Denied("approval spec is immutable; create a new approval")
		}
		return cradmission.Allowed("approval metadata update preserves the immutable decision")
	}

	if h.Mutate {
		h.stampMigrationIdentity(approval, req.UserInfo, req.UID)
		if err := h.validateMigrationBinding(ctx, approval); err != nil {
			return denialFor(err)
		}
		mutated, err := json.Marshal(approval)
		if err != nil {
			return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped approval: %w", err))
		}
		return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
	}

	if err := migrationIdentityMatchesRequest(approval.Spec, req.UserInfo); err != nil {
		return cradmission.Denied(err.Error())
	}
	if err := h.validateMigrationBinding(ctx, approval); err != nil {
		return denialFor(err)
	}
	return cradmission.Allowed("approval is bound to the current immutable migration plan")
}

func (h *MigrationApprovalHandler) stampMigrationIdentity(
	approval *operatorv1alpha1.PtahMigrationApproval,
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

func migrationIdentityMatchesRequest(
	spec operatorv1alpha1.PtahMigrationApprovalSpec,
	user authenticationv1.UserInfo,
) error {
	if strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("authenticated approval username is empty")
	}
	if spec.Approver.Username != strings.TrimSpace(user.Username) ||
		spec.Approver.UID != strings.TrimSpace(user.UID) ||
		!equalGroups(spec.Approver.Groups, normalizedGroups(user.Groups)) {
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

// validateMigrationBinding refuses every approval the current evidence cannot
// still support: a plan named by a UID or a fingerprint the live plan does not
// have, a plan of another migration, a history that moved, a changed artifact,
// policy or execution binding, or a migration that is not waiting for one. It
// reads the plan and the migration directly from the API server, in both
// admission passes.
func (h *MigrationApprovalHandler) validateMigrationBinding(
	ctx context.Context,
	approval *operatorv1alpha1.PtahMigrationApproval,
) error {
	if err := requireExplicitDecision(
		"migration", approval.Spec.MigrationRef, approval.Spec.PlanRef, approval.Spec.PlanFingerprint,
	); err != nil {
		return err
	}
	namespace := approval.Namespace
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: approval.Spec.PlanRef.Name}, plan); err != nil {
		return fmt.Errorf("read referenced migration plan: %w", err)
	}
	if plan.DeletionTimestamp != nil {
		return fmt.Errorf("referenced plan is being deleted")
	}
	if plan.UID != approval.Spec.PlanRef.UID {
		return fmt.Errorf("referenced plan UID does not match; the plan was replaced")
	}
	if plan.Spec.MigrationRef != approval.Spec.MigrationRef {
		return fmt.Errorf("approval migration reference does not match the plan")
	}
	if approval.Spec.PlanFingerprint != plan.Spec.Fingerprint {
		return fmt.Errorf("approval plan fingerprint does not match the immutable plan")
	}
	if plan.Spec.ContractVersion != migrationplan.ContractVersion {
		return fmt.Errorf("referenced plan contract version %d is not the one this manager publishes", plan.Spec.ContractVersion)
	}
	if err := h.Execution.binds(
		plan.Spec.ControllerStateVersion, plan.Spec.PtahVersion, plan.Spec.ExecutorImage, plan.Spec.RunnerProtocolVersion,
	); err != nil {
		return err
	}

	migration := &operatorv1alpha1.PtahMigration{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: approval.Spec.MigrationRef.Name}, migration); err != nil {
		return fmt.Errorf("read referenced migration: %w", err)
	}
	if migration.DeletionTimestamp != nil {
		return fmt.Errorf("referenced migration is being deleted")
	}
	if migration.UID != approval.Spec.MigrationRef.UID ||
		plan.Spec.MigrationRef.UID != migration.UID || plan.Spec.MigrationRef.Name != migration.Name {
		return fmt.Errorf("migration UID does not match the plan binding")
	}
	if migration.Status.Plan == nil || migration.Status.Plan.UID != plan.UID {
		return fmt.Errorf("referenced plan is no longer the migration's current plan")
	}
	if migration.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
		return fmt.Errorf("referenced migration is not awaiting approval")
	}
	if !meta.IsStatusConditionTrue(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired) {
		return fmt.Errorf("referenced migration does not currently require approval")
	}
	if migration.Status.ActiveOperation != nil {
		return fmt.Errorf("referenced migration has an active operation")
	}
	if migration.Status.History == nil {
		return fmt.Errorf("referenced migration has no history to approve against")
	}
	if migration.Status.History.Fingerprint != plan.Spec.HistoryFingerprint {
		return fmt.Errorf("the database's history moved after the plan was generated")
	}
	if migration.Status.History.TargetIdentityDigest != plan.Spec.TargetIdentityDigest {
		return fmt.Errorf("the database identity changed after the plan was generated")
	}
	if migration.Status.Artifact == nil || migration.Status.Artifact.Digest != plan.Spec.ArtifactDigest {
		return fmt.Errorf("the resolved artifact changed after the plan was generated")
	}
	binding := migration.Status.ExecutionBinding
	if binding == nil || binding.Epoch == "" || binding.Epoch != plan.Spec.ExecutionBindingID ||
		binding.ControllerStateVersion != plan.Spec.ControllerStateVersion ||
		binding.PtahVersion != plan.Spec.PtahVersion ||
		binding.ExecutorImage != plan.Spec.ExecutorImage ||
		binding.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("an execution component changed after the plan was generated")
	}
	coordinationDigest, err := coordination.Digest(migration.Namespace, migration.Spec.Target)
	if err != nil {
		return fmt.Errorf("derive current database coordination digest: %w", err)
	}
	if coordinationDigest != plan.Spec.CoordinationDigest {
		return fmt.Errorf("the declared coordination realm changed after the plan was generated")
	}
	policyBinding, err := policy.ConfigMapBinding(ctx, h.Reader, namespace, migration.Spec.Artifact.VerificationPolicyFrom)
	if err != nil {
		return err
	}
	if policyBinding.UID != plan.Spec.VerificationPolicyUID || policyBinding.Digest != plan.Spec.VerificationPolicyDigest {
		return fmt.Errorf("the verification policy changed after the plan was generated")
	}
	if h.RequireDistinctApprover {
		if err := refuseSelfApproval("migration", migration.Annotations, approval.Spec.Approver); err != nil {
			return err
		}
	}
	return nil
}

func equalGroups(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
