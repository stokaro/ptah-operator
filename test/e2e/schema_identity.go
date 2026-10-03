package e2e

import (
	"errors"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func replacedSchemaDecision(old, current *ptahv1alpha1.PtahSchema, oldPlan, currentPlan *ptahv1alpha1.PtahSchemaPlan) error {
	if old == nil || current == nil || old.Name == "" || old.Namespace == "" || old.UID == "" || current.UID == "" ||
		old.UID == current.UID || old.Name != current.Name || old.Namespace != current.Namespace || !reflect.DeepEqual(old.Spec, current.Spec) {
		return errors.New("schema replacement did not preserve its name, namespace and spec with a new UID")
	}
	if oldPlan == nil || currentPlan == nil || oldPlan.Name == "" || currentPlan.Name == "" || oldPlan.UID == "" || currentPlan.UID == "" || oldPlan.UID == currentPlan.UID ||
		oldPlan.Namespace != old.Namespace || currentPlan.Namespace != current.Namespace ||
		oldPlan.Spec.SchemaRef != (ptahv1alpha1.ImmutableObjectReference{Name: old.Name, UID: old.UID}) ||
		currentPlan.Spec.SchemaRef != (ptahv1alpha1.ImmutableObjectReference{Name: current.Name, UID: current.UID}) ||
		old.Status.Plan == nil || old.Status.Plan.Name != oldPlan.Name || old.Status.Plan.UID != oldPlan.UID ||
		!changedSchemaApprovalRefused(current, string(oldPlan.UID), current.Generation) || current.Status.Plan.Name != currentPlan.Name || current.Status.Plan.UID != currentPlan.UID {
		return errors.New("replacement schema did not reach its own exact approval gate")
	}
	currentApproval := false
	for _, condition := range current.Status.Conditions {
		if condition.Type == "ApprovalRequired" && condition.Status == "True" && condition.ObservedGeneration == current.Generation {
			currentApproval = true
		}
	}
	if !currentApproval {
		return errors.New("replacement approval condition describes another generation")
	}
	a, b := oldPlan.Spec, currentPlan.Spec
	if a.Fingerprint == "" || b.Fingerprint == "" || a.Fingerprint == b.Fingerprint ||
		a.ArtifactDigest == "" || a.ArtifactDigest != b.ArtifactDigest || a.TargetIdentityDigest == "" || a.TargetIdentityDigest != b.TargetIdentityDigest ||
		a.CoordinationDigest == "" || a.CoordinationDigest != b.CoordinationDigest || a.PolicyFingerprint == "" || a.PolicyFingerprint != b.PolicyFingerprint ||
		a.ActualStateFingerprint == "" || a.ActualStateFingerprint != b.ActualStateFingerprint || a.DesiredStateFingerprint == "" || a.DesiredStateFingerprint != b.DesiredStateFingerprint ||
		a.VerificationPolicyUID == "" || a.VerificationPolicyUID != b.VerificationPolicyUID || a.VerificationPolicyDigest == "" || a.VerificationPolicyDigest != b.VerificationPolicyDigest ||
		a.StatementCount <= 0 || a.StatementCount != b.StatementCount || a.Dialect == "" || a.Dialect != b.Dialect || a.Destructive != b.Destructive {
		return errors.New("schema replacement changed the planned work or reused its old fingerprint")
	}
	return nil
}

func schemaIdentityApprovalRefusal(err error, reason string) bool {
	return reason != "" && apierrors.IsForbidden(err) &&
		strings.Contains(err.Error(), `admission webhook "mapproval.operator.ptah.run" denied the request: `+reason)
}
