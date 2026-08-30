// Package admission implements the independently authorized approval boundary.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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
	"github.com/stokaro/ptah-operator/internal/policy"
)

const maxRecordedGroups = 64

// Clock makes admission timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ApprovalHandler stamps authenticated identity and rejects stale approval
// tuples against direct, uncached API reads.
type ApprovalHandler struct {
	Reader  client.Reader
	Decoder cradmission.Decoder
	Clock   Clock
	Mutate  bool
}

// Handle implements controller-runtime admission.Handler.
func (h *ApprovalHandler) Handle(ctx context.Context, req cradmission.Request) cradmission.Response {
	if h.Reader == nil || h.Decoder == nil {
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
		if err := h.hydrateDerivedBindings(ctx, approval); err != nil {
			return denialFor(err)
		}
		if err := h.validateBinding(ctx, approval); err != nil {
			return denialFor(err)
		}
		mutated, err := json.Marshal(approval)
		if err != nil {
			return cradmission.Errored(http.StatusInternalServerError, fmt.Errorf("encode stamped approval: %w", err))
		}
		return cradmission.PatchResponseFromRaw(req.Object.Raw, mutated)
	}

	if err := identityMatchesRequest(approval.Spec, req.UserInfo, req.UID); err != nil {
		return cradmission.Denied(err.Error())
	}
	if err := h.validateBinding(ctx, approval); err != nil {
		return denialFor(err)
	}
	return cradmission.Allowed("approval is bound to the current immutable plan")
}

// hydrateDerivedBindings removes error-prone transcription from an approval
// without weakening its explicit decision. The approver must name immutable
// schema and plan UIDs plus the exact plan fingerprint. Every other binding is
// copied from that plan only when omitted; a conflicting value is refused
// rather than silently corrected.
func (h *ApprovalHandler) hydrateDerivedBindings(
	ctx context.Context,
	approval *operatorv1alpha1.PtahSchemaApproval,
) error {
	if strings.TrimSpace(approval.Spec.SchemaRef.Name) == "" || approval.Spec.SchemaRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the schema name and UID")
	}
	if strings.TrimSpace(approval.Spec.PlanRef.Name) == "" || approval.Spec.PlanRef.UID == "" {
		return fmt.Errorf("approval must explicitly identify the plan name and UID")
	}
	if strings.TrimSpace(approval.Spec.PlanFingerprint) == "" {
		return fmt.Errorf("approval must explicitly identify the plan fingerprint")
	}

	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: approval.Namespace, Name: approval.Spec.PlanRef.Name}, plan); err != nil {
		return fmt.Errorf("read referenced plan for approval defaults: %w", err)
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

	bindings := []struct {
		name  string
		value *string
		want  string
	}{
		{"artifact digest", &approval.Spec.ArtifactDigest, plan.Spec.ArtifactDigest},
		{"target identity digest", &approval.Spec.TargetIdentityDigest, plan.Spec.TargetIdentityDigest},
		{"actual state fingerprint", &approval.Spec.ActualStateFingerprint, plan.Spec.ActualStateFingerprint},
		{"desired state fingerprint", &approval.Spec.DesiredStateFingerprint, plan.Spec.DesiredStateFingerprint},
		{"policy fingerprint", &approval.Spec.PolicyFingerprint, plan.Spec.PolicyFingerprint},
		{"verification policy digest", &approval.Spec.VerificationPolicyDigest, plan.Spec.VerificationPolicyDigest},
		{"Ptah version", &approval.Spec.PtahVersion, plan.Spec.PtahVersion},
		{"executor image", &approval.Spec.ExecutorImage, plan.Spec.ExecutorImage},
		{"runner image", &approval.Spec.RunnerImage, plan.Spec.RunnerImage},
	}
	for _, binding := range bindings {
		if *binding.value != "" && *binding.value != binding.want {
			return fmt.Errorf("approval %s conflicts with the immutable plan", binding.name)
		}
		*binding.value = binding.want
	}
	if approval.Spec.RunnerProtocolVersion != 0 && approval.Spec.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion {
		return fmt.Errorf("approval runner protocol version conflicts with the immutable plan")
	}
	approval.Spec.RunnerProtocolVersion = plan.Spec.RunnerProtocolVersion
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
	approval.Spec.AdmissionRequestUID = string(requestUID)
}

func identityMatchesRequest(
	spec operatorv1alpha1.PtahSchemaApprovalSpec,
	user authenticationv1.UserInfo,
	requestUID types.UID,
) error {
	if strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("authenticated approval username is empty")
	}
	if spec.Approver.Username != strings.TrimSpace(user.Username) ||
		spec.Approver.UID != strings.TrimSpace(user.UID) ||
		!reflect.DeepEqual(spec.Approver.Groups, normalizedGroups(user.Groups)) ||
		spec.AdmissionRequestUID != string(requestUID) {
		return fmt.Errorf("reserved approver identity fields do not match the authenticated request")
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

func (h *ApprovalHandler) validateBinding(
	ctx context.Context,
	approval *operatorv1alpha1.PtahSchemaApproval,
) error {
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
		schema.Status.Plan.Fingerprint != plan.Spec.Fingerprint {
		return fmt.Errorf("referenced plan is no longer current for the schema")
	}

	if err := approvalMatchesPlan(approval.Spec, plan.Spec); err != nil {
		return err
	}
	if schema.Status.Source.Digest != plan.Spec.ArtifactDigest ||
		schema.Status.Target.IdentityDigest != plan.Spec.TargetIdentityDigest ||
		schema.Status.Target.ObservedStateFingerprint != plan.Spec.ActualStateFingerprint {
		return fmt.Errorf("schema source or target changed after the plan was generated")
	}
	policyDigest, err := policy.ConfigMapDigest(ctx, h.Reader, namespace, schema.Spec.Desired.VerificationPolicyFrom)
	if err != nil {
		return err
	}
	if policyDigest != plan.Spec.VerificationPolicyDigest {
		return fmt.Errorf("verification policy changed after the plan was generated")
	}
	return nil
}

func approvalMatchesPlan(
	approval operatorv1alpha1.PtahSchemaApprovalSpec,
	plan operatorv1alpha1.PtahSchemaPlanSpec,
) error {
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"plan fingerprint", approval.PlanFingerprint, plan.Fingerprint},
		{"artifact digest", approval.ArtifactDigest, plan.ArtifactDigest},
		{"target identity digest", approval.TargetIdentityDigest, plan.TargetIdentityDigest},
		{"actual state fingerprint", approval.ActualStateFingerprint, plan.ActualStateFingerprint},
		{"desired state fingerprint", approval.DesiredStateFingerprint, plan.DesiredStateFingerprint},
		{"policy fingerprint", approval.PolicyFingerprint, plan.PolicyFingerprint},
		{"verification policy digest", approval.VerificationPolicyDigest, plan.VerificationPolicyDigest},
		{"Ptah version", approval.PtahVersion, plan.PtahVersion},
		{"executor image", approval.ExecutorImage, plan.ExecutorImage},
		{"runner image", approval.RunnerImage, plan.RunnerImage},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.got) == "" || check.got != check.want {
			return fmt.Errorf("approval %s does not match the immutable plan", check.name)
		}
	}
	if approval.RunnerProtocolVersion != plan.RunnerProtocolVersion {
		return fmt.Errorf("approval runner protocol version does not match the immutable plan")
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
