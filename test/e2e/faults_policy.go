package e2e

import (
	"errors"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func destructivePolicyGate(schema *ptahv1alpha1.PtahSchema, generation int64, allowed bool) bool {
	if schema == nil || generation <= 0 || schema.Generation != generation || schema.Status.ObservedGeneration != generation ||
		schema.Spec.Policy.AllowDestructive != allowed || schema.Spec.Policy.Apply != ptahv1alpha1.ApplyPolicyOnApproval {
		return false
	}
	status := schema.Status
	if status.ActiveOperation != nil || status.Plan == nil || status.Plan.Name == "" || status.Plan.UID == "" ||
		status.Plan.Fingerprint == "" || !status.Plan.Destructive || status.Plan.Approval != nil {
		return false
	}
	if allowed {
		return status.Phase == ptahv1alpha1.PhaseAwaitingApproval && conditionStatus(status.Conditions, "ApprovalRequired", "True")
	}
	return conditionIs(status.Conditions, "ApprovalRequired", "False", "DestructiveChangesDisabled")
}

// Only the policy changed: the destructive plan must still describe the same
// artifact and observed database, but a different immutable approval decision.
func changedDestructivePolicyPlan(old, current *ptahv1alpha1.PtahSchemaPlan) error {
	if old == nil || current == nil || old.UID == "" || current.UID == "" || old.UID == current.UID ||
		old.Spec.Fingerprint == "" || current.Spec.Fingerprint == "" || old.Spec.Fingerprint == current.Spec.Fingerprint ||
		old.Spec.PolicyFingerprint == "" || current.Spec.PolicyFingerprint == "" || old.Spec.PolicyFingerprint == current.Spec.PolicyFingerprint {
		return errors.New("the policy edit did not produce a distinct immutable approval decision")
	}
	a, b := old.Spec, current.Spec
	if a.SchemaRef.Name == "" || a.SchemaRef.UID == "" || a.SchemaRef != b.SchemaRef ||
		!sha256Pattern.MatchString(a.ArtifactDigest) || a.ArtifactDigest != b.ArtifactDigest ||
		!sha256Pattern.MatchString(a.TargetIdentityDigest) || a.TargetIdentityDigest != b.TargetIdentityDigest ||
		!sha256Pattern.MatchString(a.CoordinationDigest) || a.CoordinationDigest != b.CoordinationDigest ||
		!sha256Pattern.MatchString(a.ActualStateFingerprint) || a.ActualStateFingerprint != b.ActualStateFingerprint {
		return errors.New("the policy edit changed the resource, artifact, target, realm or observed database")
	}
	if !a.Destructive || !b.Destructive || a.StatementCount <= 0 || b.StatementCount <= 0 {
		return errors.New("the policy edit has no destructive work to refuse and authorize")
	}
	return nil
}
