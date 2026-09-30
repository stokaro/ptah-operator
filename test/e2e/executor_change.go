package e2e

import (
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func replaceControllerExecutor(deployment *appsv1.Deployment, expected, replacement string) error {
	return replaceControllerExecutionComponent(deployment, executionComponentChange{"executor-image", expected, replacement})
}

func changedSchemaExecutorDecision(before, current *ptahv1alpha1.PtahSchema, old, fresh *ptahv1alpha1.PtahSchemaPlan, original, replacement string) error {
	return changedSchemaExecutionDecision(before, current, old, fresh, executionComponentChange{"executor-image", original, replacement})
}

func changedSchemaExecutionDecision(before, current *ptahv1alpha1.PtahSchema, old, fresh *ptahv1alpha1.PtahSchemaPlan, change executionComponentChange) error {
	if before == nil || current == nil || old == nil || fresh == nil || before.UID == "" || before.UID != current.UID ||
		before.Name != current.Name || before.Namespace != current.Namespace || !equality.Semantic.DeepEqual(before.Spec, current.Spec) ||
		before.Generation != current.Generation || current.Status.ObservedGeneration != current.Generation || !planAwaitingApproval(before) || !planAwaitingApproval(current) ||
		current.Status.PendingBindingRetirement != nil || current.Status.ActiveOperation != nil {
		return errors.New("executor transition changed the schema or did not settle at a fresh approval gate")
	}
	previous, next := before.Status.ExecutionBinding, current.Status.ExecutionBinding
	if !change.binding(previous, next) {
		return errors.New("execution transition did not rotate exactly the changed component's epoch")
	}
	imageMatches := old.Spec.ExecutorImage == fresh.Spec.ExecutorImage
	versionMatches := old.Spec.PtahVersion == fresh.Spec.PtahVersion
	if change.argument == "executor-image" {
		imageMatches = old.Spec.ExecutorImage == change.original && fresh.Spec.ExecutorImage == change.replacement
	} else {
		versionMatches = old.Spec.PtahVersion == change.original && fresh.Spec.PtahVersion == change.replacement
	}
	if old.UID == "" || fresh.UID == "" || old.UID == fresh.UID || before.Status.Plan.UID != old.UID || current.Status.Plan.UID != fresh.UID ||
		old.Spec.SchemaRef.Name != before.Name || old.Spec.SchemaRef.UID != before.UID || fresh.Spec.SchemaRef != old.Spec.SchemaRef ||
		old.Spec.Fingerprint == fresh.Spec.Fingerprint || old.Spec.ExecutionBindingID != previous.Epoch || fresh.Spec.ExecutionBindingID != next.Epoch ||
		!imageMatches || !versionMatches || old.Spec.ArtifactDigest != fresh.Spec.ArtifactDigest ||
		old.Spec.TargetIdentityDigest != fresh.Spec.TargetIdentityDigest || old.Spec.ActualStateFingerprint != fresh.Spec.ActualStateFingerprint ||
		old.Spec.DesiredStateFingerprint != fresh.Spec.DesiredStateFingerprint || old.Spec.CoordinationDigest != fresh.Spec.CoordinationDigest ||
		old.Spec.PolicyFingerprint != fresh.Spec.PolicyFingerprint || old.Spec.VerificationPolicyUID != fresh.Spec.VerificationPolicyUID || old.Spec.VerificationPolicyDigest != fresh.Spec.VerificationPolicyDigest ||
		old.Spec.ControllerStateVersion != fresh.Spec.ControllerStateVersion || old.Spec.RunnerProtocolVersion != fresh.Spec.RunnerProtocolVersion {
		return errors.New("replacement plan did not retain the target, source and state under its new execution epoch")
	}
	return nil
}

func jobUsesExecutor(job *batchv1.Job, expected string) bool {
	if job == nil || expected == "" || len(job.Spec.Template.Spec.Containers) != 1 {
		return false
	}
	return job.Spec.Template.Spec.Containers[0].Image == expected
}
