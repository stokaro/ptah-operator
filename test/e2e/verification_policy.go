package e2e

import (
	"errors"

	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

type verificationPolicyIdentity struct {
	uid    types.UID
	digest string
}

type verificationPolicyDecision struct {
	uid                     types.UID
	fingerprint             string
	resource                ptahv1alpha1.ImmutableObjectReference
	artifact, target, realm string
	verification            verificationPolicyIdentity
}

func changedVerificationPolicyDecision(old, current verificationPolicyDecision, before, after verificationPolicyIdentity, mode string) error {
	if old.uid == "" || current.uid == "" || old.uid == current.uid || old.fingerprint == "" ||
		current.fingerprint == "" || old.fingerprint == current.fingerprint {
		return errors.New("the verification policy change reused its approval decision")
	}
	if before.uid == "" || after.uid == "" || !sha256Pattern.MatchString(before.digest) || !sha256Pattern.MatchString(after.digest) ||
		old.verification != before || current.verification != after {
		return errors.New("the plans do not bind the exact original and current verification policies")
	}
	switch mode {
	case "verification-policy-uid":
		if before.uid == after.uid || before.digest != after.digest {
			return errors.New("the UID replacement must preserve the policy bytes")
		}
	case "verification-policy-content":
		if before.uid != after.uid || before.digest == after.digest {
			return errors.New("the policy key change must change the bytes on the same immutable ConfigMap")
		}
	default:
		return errors.New("unsupported verification policy change")
	}
	if old.resource.Name == "" || old.resource.UID == "" || old.resource != current.resource ||
		!sha256Pattern.MatchString(old.artifact) || old.artifact != current.artifact ||
		!sha256Pattern.MatchString(old.target) || old.target != current.target ||
		!sha256Pattern.MatchString(old.realm) || old.realm != current.realm {
		return errors.New("the verification policy edit changed the resource, artifact, target or realm")
	}
	return nil
}

func migrationVerificationDecision(plan *ptahv1alpha1.PtahMigrationPlan) verificationPolicyDecision {
	return verificationPolicyDecision{
		uid: plan.UID, fingerprint: plan.Spec.Fingerprint, resource: plan.Spec.MigrationRef,
		artifact: plan.Spec.ArtifactDigest, target: plan.Spec.TargetIdentityDigest, realm: plan.Spec.CoordinationDigest,
		verification: verificationPolicyIdentity{uid: plan.Spec.VerificationPolicyUID, digest: plan.Spec.VerificationPolicyDigest},
	}
}

func schemaVerificationDecision(plan *ptahv1alpha1.PtahSchemaPlan) verificationPolicyDecision {
	return verificationPolicyDecision{
		uid: plan.UID, fingerprint: plan.Spec.Fingerprint, resource: plan.Spec.SchemaRef,
		artifact: plan.Spec.ArtifactDigest, target: plan.Spec.TargetIdentityDigest, realm: plan.Spec.CoordinationDigest,
		verification: verificationPolicyIdentity{uid: plan.Spec.VerificationPolicyUID, digest: plan.Spec.VerificationPolicyDigest},
	}
}
