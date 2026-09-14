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
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
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
	Reader                 client.Reader
	Decoder                cradmission.Decoder
	Clock                  Clock
	Mutate                 bool
	ControllerImage        string
	ControllerRevision     string
	ControllerStateVersion int32
}

// Handle implements controller-runtime admission.Handler.
func (h *MigrationApprovalHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil ||
		!imageDigestPattern.MatchString(h.ControllerImage) ||
		controllerstate.ValidateRevision(h.ControllerRevision) != nil || h.ControllerStateVersion < 1 {
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
		if err := h.hydrateMigrationBindings(ctx, approval); err != nil {
			return denialFor(err)
		}
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

// hydrateMigrationBindings copies the plan's own bindings onto an approval that
// omitted them, and refuses one that names them differently. The approver still
// has to identify the migration, the plan and its fingerprint explicitly: those
// three are the decision, and the rest is transcription.
func (h *MigrationApprovalHandler) hydrateMigrationBindings(
	ctx context.Context,
	approval *operatorv1alpha1.PtahMigrationApproval,
) error {
	if strings.TrimSpace(approval.Spec.MigrationRef.Name) == "" || approval.Spec.MigrationRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the migration name and UID")
	}
	if strings.TrimSpace(approval.Spec.PlanRef.Name) == "" || approval.Spec.PlanRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the plan name and UID")
	}
	if strings.TrimSpace(approval.Spec.PlanFingerprint) == "" {
		return fmt.Errorf("approval must explicitly identify the plan fingerprint")
	}

	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := client.ObjectKey{Namespace: approval.Namespace, Name: approval.Spec.PlanRef.Name}
	if err := h.Reader.Get(ctx, key, plan); err != nil {
		return fmt.Errorf("read referenced migration plan for approval defaults: %w", err)
	}
	if plan.UID != approval.Spec.PlanRef.UID {
		return fmt.Errorf("referenced plan UID does not match; the plan was replaced")
	}
	if plan.Spec.ContractVersion != migrationplan.ContractVersion {
		return fmt.Errorf("referenced plan contract version %d is not the one this manager publishes", plan.Spec.ContractVersion)
	}
	if plan.Spec.ControllerImage != h.ControllerImage ||
		plan.Spec.ControllerRevision != h.ControllerRevision ||
		plan.Spec.ControllerStateVersion != h.ControllerStateVersion {
		return fmt.Errorf("referenced plan manager identity is not current")
	}
	if plan.Spec.MigrationRef != approval.Spec.MigrationRef {
		return fmt.Errorf("approval migration reference does not match the plan")
	}
	if approval.Spec.PlanFingerprint != plan.Spec.Fingerprint {
		return fmt.Errorf("approval plan fingerprint does not match the immutable plan")
	}

	for _, binding := range []struct {
		name  string
		value *string
		want  string
	}{
		{"history fingerprint", &approval.Spec.HistoryFingerprint, plan.Spec.HistoryFingerprint},
		{"artifact digest", &approval.Spec.ArtifactDigest, plan.Spec.ArtifactDigest},
		{"coordination digest", &approval.Spec.CoordinationDigest, plan.Spec.CoordinationDigest},
		{"target identity digest", &approval.Spec.TargetIdentityDigest, plan.Spec.TargetIdentityDigest},
		{"policy fingerprint", &approval.Spec.PolicyFingerprint, plan.Spec.PolicyFingerprint},
		{"verification policy digest", &approval.Spec.VerificationPolicyDigest, plan.Spec.VerificationPolicyDigest},
		{"execution binding ID", &approval.Spec.ExecutionBindingID, plan.Spec.ExecutionBindingID},
		{"controller image", &approval.Spec.ControllerImage, plan.Spec.ControllerImage},
		{"controller revision", &approval.Spec.ControllerRevision, plan.Spec.ControllerRevision},
		{"Ptah version", &approval.Spec.PtahVersion, plan.Spec.PtahVersion},
		{"executor image", &approval.Spec.ExecutorImage, plan.Spec.ExecutorImage},
		{"runner image", &approval.Spec.RunnerImage, plan.Spec.RunnerImage},
	} {
		if *binding.value != "" && *binding.value != binding.want {
			return fmt.Errorf("approval %s conflicts with the immutable plan", binding.name)
		}
		*binding.value = binding.want
	}
	if approval.Spec.VerificationPolicyUID != "" && approval.Spec.VerificationPolicyUID != plan.Spec.VerificationPolicyUID {
		return fmt.Errorf("approval verification policy UID conflicts with the immutable plan")
	}
	approval.Spec.VerificationPolicyUID = plan.Spec.VerificationPolicyUID
	if approval.Spec.RunnerProtocolVersion != 0 && approval.Spec.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("approval runner protocol version conflicts with the immutable plan")
	}
	approval.Spec.RunnerProtocolVersion = plan.Spec.RunnerProtocolVersion
	if approval.Spec.ControllerStateVersion != 0 && approval.Spec.ControllerStateVersion != plan.Spec.ControllerStateVersion {
		return fmt.Errorf("approval controller state version conflicts with the immutable plan")
	}
	approval.Spec.ControllerStateVersion = plan.Spec.ControllerStateVersion
	return nil
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
// still support: a replaced plan, a history that moved, a changed artifact,
// policy or execution binding, or a migration that is not waiting for one.
func (h *MigrationApprovalHandler) validateMigrationBinding(
	ctx context.Context,
	approval *operatorv1alpha1.PtahMigrationApproval,
) error {
	namespace := approval.Namespace
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: approval.Spec.PlanRef.Name}, plan); err != nil {
		return fmt.Errorf("read referenced migration plan: %w", err)
	}
	if plan.DeletionTimestamp != nil {
		return fmt.Errorf("referenced plan is being deleted")
	}
	if plan.Spec.ContractVersion != migrationplan.ContractVersion {
		return fmt.Errorf("referenced plan contract version %d is not the one this manager publishes", plan.Spec.ContractVersion)
	}
	if plan.UID != approval.Spec.PlanRef.UID {
		return fmt.Errorf("referenced plan UID does not match; the plan was replaced")
	}
	if plan.Spec.ControllerImage != h.ControllerImage ||
		plan.Spec.ControllerRevision != h.ControllerRevision ||
		plan.Spec.ControllerStateVersion != h.ControllerStateVersion {
		return fmt.Errorf("referenced plan manager identity is not current")
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
		binding.ControllerImage != plan.Spec.ControllerImage ||
		binding.ControllerRevision != plan.Spec.ControllerRevision ||
		binding.ControllerStateVersion != plan.Spec.ControllerStateVersion ||
		binding.PtahVersion != plan.Spec.PtahVersion ||
		binding.ExecutorImage != plan.Spec.ExecutorImage ||
		binding.RunnerImage != plan.Spec.RunnerImage ||
		binding.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("an execution component changed after the plan was generated")
	}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest(
		string(migration.Spec.Target.Engine), migration.Spec.Target.CoordinationKey,
	)
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
	return migrationApprovalMatchesPlan(approval.Spec, plan.Spec)
}

func migrationApprovalMatchesPlan(
	approval operatorv1alpha1.PtahMigrationApprovalSpec,
	plan operatorv1alpha1.PtahMigrationPlanSpec,
) error {
	for name, pair := range map[string][2]string{
		"plan fingerprint":           {approval.PlanFingerprint, plan.Fingerprint},
		"history fingerprint":        {approval.HistoryFingerprint, plan.HistoryFingerprint},
		"artifact digest":            {approval.ArtifactDigest, plan.ArtifactDigest},
		"coordination digest":        {approval.CoordinationDigest, plan.CoordinationDigest},
		"target identity digest":     {approval.TargetIdentityDigest, plan.TargetIdentityDigest},
		"policy fingerprint":         {approval.PolicyFingerprint, plan.PolicyFingerprint},
		"verification policy digest": {approval.VerificationPolicyDigest, plan.VerificationPolicyDigest},
		"execution binding ID":       {approval.ExecutionBindingID, plan.ExecutionBindingID},
		"controller image":           {approval.ControllerImage, plan.ControllerImage},
		"controller revision":        {approval.ControllerRevision, plan.ControllerRevision},
		"Ptah version":               {approval.PtahVersion, plan.PtahVersion},
		"executor image":             {approval.ExecutorImage, plan.ExecutorImage},
		"runner image":               {approval.RunnerImage, plan.RunnerImage},
	} {
		if pair[0] != pair[1] {
			return fmt.Errorf("approval %s does not match the immutable plan", name)
		}
	}
	if approval.VerificationPolicyUID != plan.VerificationPolicyUID {
		return fmt.Errorf("approval verification policy UID does not match the immutable plan")
	}
	if approval.ControllerStateVersion != plan.ControllerStateVersion {
		return fmt.Errorf("approval controller state version does not match the immutable plan")
	}
	if approval.RunnerProtocolVersion != plan.RunnerProtocolVersion {
		return fmt.Errorf("approval runner protocol version does not match the immutable plan")
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
