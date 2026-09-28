package harness

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// progressDeadlineExceeded is the Progressing reason the Deployment controller
// sets when a rollout ran past spec.progressDeadlineSeconds.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// DeploymentRolledOut is `kubectl rollout status deployment` read once: the
// same four checks in the same order, so a phase that waited on the command
// and a phase that waits on this accept and refuse the same Deployments.
//
// It reports whether the rollout is complete and, when it is not, what it is
// waiting for in kubectl's words. A rollout past its progress deadline is an
// error, as kubectl makes it, because no amount of waiting completes it.
func DeploymentRolledOut(deployment *appsv1.Deployment) (bool, string, error) {
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return false, fmt.Sprintf("deployment %q spec update (generation %d) not yet observed (observed %d)",
			deployment.Name, deployment.Generation, deployment.Status.ObservedGeneration), nil
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Reason == progressDeadlineExceeded {
			return false, "", fmt.Errorf("deployment %q exceeded its progress deadline", deployment.Name)
		}
	}
	status := deployment.Status
	if deployment.Spec.Replicas != nil && status.UpdatedReplicas < *deployment.Spec.Replicas {
		return false, fmt.Sprintf("deployment %q: %d out of %d new replicas have been updated",
			deployment.Name, status.UpdatedReplicas, *deployment.Spec.Replicas), nil
	}
	if status.Replicas > status.UpdatedReplicas {
		return false, fmt.Sprintf("deployment %q: %d old replicas are pending termination",
			deployment.Name, status.Replicas-status.UpdatedReplicas), nil
	}
	if status.AvailableReplicas < status.UpdatedReplicas {
		return false, fmt.Sprintf("deployment %q: %d of %d updated replicas are available",
			deployment.Name, status.AvailableReplicas, status.UpdatedReplicas), nil
	}
	return true, fmt.Sprintf("deployment %q successfully rolled out", deployment.Name), nil
}

// PodReady reports whether the Pod carries a Ready condition that is True.
func PodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
