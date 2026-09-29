package e2e

import (
	"errors"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func replaceControllerExecutor(deployment *appsv1.Deployment, expected, replacement string) error {
	if deployment == nil || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || expected == replacement ||
		!digestSuffix.MatchString(expected) || !digestSuffix.MatchString(replacement) {
		return errors.New("executor transition requires distinct pinned images and a Recreate manager Deployment")
	}
	managers, argument := 0, -1
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != "manager" {
			continue
		}
		managers++
		for j, value := range container.Args {
			if strings.HasPrefix(value, "--executor-image=") {
				if argument != -1 || value != "--executor-image="+expected && value != "--executor-image="+replacement {
					return errors.New("manager executor argument changed outside the controlled rollout")
				}
				argument = j
			}
		}
		if argument == -1 {
			return errors.New("manager has no executor image argument")
		}
		container.Args[argument] = "--executor-image=" + replacement
	}
	if managers != 1 {
		return errors.New("executor transition needs exactly one manager container")
	}
	return nil
}

func changedSchemaExecutorDecision(before, current *ptahv1alpha1.PtahSchema, old, fresh *ptahv1alpha1.PtahSchemaPlan, original, replacement string) error {
	if before == nil || current == nil || old == nil || fresh == nil || before.UID == "" || before.UID != current.UID ||
		before.Name != current.Name || before.Namespace != current.Namespace || !equality.Semantic.DeepEqual(before.Spec, current.Spec) ||
		before.Generation != current.Generation || current.Status.ObservedGeneration != current.Generation || !planAwaitingApproval(before) || !planAwaitingApproval(current) ||
		current.Status.PendingBindingRetirement != nil || current.Status.ActiveOperation != nil {
		return errors.New("executor transition changed the schema or did not settle at a fresh approval gate")
	}
	previous, next := before.Status.ExecutionBinding, current.Status.ExecutionBinding
	if previous == nil || next == nil || !executionEpoch.MatchString(previous.Epoch) || !executionEpoch.MatchString(next.Epoch) || next.Epoch == previous.Epoch ||
		previous.ExecutorImage != original || next.ExecutorImage != replacement || original == replacement ||
		previous.PtahVersion != next.PtahVersion || previous.RunnerProtocolVersion != next.RunnerProtocolVersion || previous.ControllerStateVersion != next.ControllerStateVersion {
		return errors.New("executor transition did not rotate exactly the image-bound epoch")
	}
	if old.UID == "" || fresh.UID == "" || old.UID == fresh.UID || before.Status.Plan.UID != old.UID || current.Status.Plan.UID != fresh.UID ||
		old.Spec.SchemaRef.Name != before.Name || old.Spec.SchemaRef.UID != before.UID || fresh.Spec.SchemaRef != old.Spec.SchemaRef ||
		old.Spec.Fingerprint == fresh.Spec.Fingerprint || old.Spec.ExecutionBindingID != previous.Epoch || fresh.Spec.ExecutionBindingID != next.Epoch ||
		old.Spec.ExecutorImage != original || fresh.Spec.ExecutorImage != replacement || old.Spec.ArtifactDigest != fresh.Spec.ArtifactDigest ||
		old.Spec.TargetIdentityDigest != fresh.Spec.TargetIdentityDigest || old.Spec.ActualStateFingerprint != fresh.Spec.ActualStateFingerprint ||
		old.Spec.DesiredStateFingerprint != fresh.Spec.DesiredStateFingerprint || old.Spec.CoordinationDigest != fresh.Spec.CoordinationDigest ||
		old.Spec.PolicyFingerprint != fresh.Spec.PolicyFingerprint || old.Spec.VerificationPolicyUID != fresh.Spec.VerificationPolicyUID || old.Spec.VerificationPolicyDigest != fresh.Spec.VerificationPolicyDigest ||
		old.Spec.PtahVersion != fresh.Spec.PtahVersion || old.Spec.ControllerStateVersion != fresh.Spec.ControllerStateVersion || old.Spec.RunnerProtocolVersion != fresh.Spec.RunnerProtocolVersion {
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
