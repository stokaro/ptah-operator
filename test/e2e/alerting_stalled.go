package e2e

import (
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The frozen profile allows 105 seconds after the operation leaves flight.
// This includes controller observation; it is distinct from delivery slack.
const alStalledResolution = 105 * time.Second

type alStalledClaim struct {
	family, namespace, name string
	uid                     types.UID
	generation              int64
	id, jobName             string
	jobUID                  types.UID
	started                 time.Time
	operation, phase        string
}

func alStalledReading(object client.Object) alStalledClaim {
	reading := alStalledClaim{namespace: object.GetNamespace(), name: object.GetName(), uid: object.GetUID(), generation: object.GetGeneration()}
	switch object := object.(type) {
	case *ptahv1.PtahSchema:
		reading.family, reading.phase = "schema", string(object.Status.Phase)
		if active := object.Status.ActiveOperation; active != nil {
			reading.id, reading.jobName, reading.jobUID = active.ID, active.JobName, active.JobUID
			reading.started, reading.operation = active.StartedAt.Time, string(active.Type)
		}
	case *ptahv1.PtahMigration:
		reading.family, reading.phase = "migration", string(object.Status.Phase)
		if active := object.Status.ActiveOperation; active != nil {
			reading.id, reading.jobName, reading.jobUID = active.ID, active.JobName, active.JobUID
			reading.started, reading.operation = active.StartedAt.Time, string(active.Type)
		}
	}
	return reading
}

func (c alStalledClaim) ready() bool {
	return (c.family == "schema" || c.family == "migration") && c.namespace != "" && c.name != "" &&
		c.uid != "" && c.generation > 0 && c.id != "" && c.jobName != "" && c.jobUID != "" &&
		!c.started.IsZero() && c.operation == "Resolve" && c.phase == "Resolving"
}

func (c alStalledClaim) sameResource(other alStalledClaim) bool {
	return c.family == other.family && c.namespace == other.namespace && c.name == other.name &&
		c.uid != "" && c.uid == other.uid && c.generation == other.generation
}

func alHeldResource(namespace, family string) map[string]any {
	if family != "schema" && family != "migration" {
		return nil
	}
	object := alHeldSchema(namespace)
	if family == "migration" {
		object["kind"] = "PtahMigration"
		spec := object["spec"].(map[string]any)
		spec["artifact"] = spec["desired"]
		delete(spec, "desired")
	}
	return object
}

func alStalledDelivered(delivery alDelivery, started time.Time) bool {
	threshold := started.Add(alStalledAfter)
	return !started.IsZero() && !delivery.StartsAt.Before(threshold) && !delivery.ReceivedAt.Before(delivery.StartsAt) &&
		!delivery.ReceivedAt.After(threshold.Add(alDetectionSlack))
}

func alStalledCleared(firing, resolved alDelivery, finished time.Time) bool {
	return !finished.IsZero() && !firing.StartsAt.IsZero() && resolved.StartsAt.Equal(firing.StartsAt) &&
		!resolved.EndsAt.Before(finished) && !resolved.ReceivedAt.Before(resolved.EndsAt) &&
		!resolved.ReceivedAt.After(finished.Add(alStalledResolution))
}

func alStalledJobMatches(job *batchv1.Job, claim alStalledClaim) bool {
	owner := metav1.GetControllerOf(job)
	kind := "PtahSchema"
	if claim.family == "migration" {
		kind = "PtahMigration"
	}
	return claim.ready() && job.Namespace == claim.namespace && job.Name == claim.jobName && job.UID == claim.jobUID &&
		job.Annotations[workload.AnnotationOperationID] == claim.id && job.Labels[workload.LabelOperation] == "resolve" &&
		job.DeletionTimestamp == nil && owner != nil && owner.APIVersion == ptahv1.GroupVersion.String() &&
		owner.Kind == kind && owner.Name == claim.name && owner.UID == claim.uid
}

func alStalledPodMatches(pod *corev1.Pod, claim alStalledClaim, uid types.UID) bool {
	owner := metav1.GetControllerOf(pod)
	return pod.Namespace == claim.namespace && pod.UID != "" && (uid == "" || pod.UID == uid) &&
		!pod.CreationTimestamp.IsZero() && !pod.CreationTimestamp.Before(&metav1.Time{Time: claim.started}) &&
		pod.Annotations[workload.AnnotationOperationID] == claim.id && pod.Labels[workload.LabelOperation] == "resolve" &&
		pod.DeletionTimestamp == nil && owner != nil && owner.APIVersion == "batch/v1" && owner.Kind == "Job" &&
		owner.Name == claim.jobName && owner.UID == claim.jobUID
}

func alStalledPodHeld(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending || pod.Spec.NodeName != "" || pod.Spec.NodeSelector[alGateLabel] != "open" {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if status.RestartCount != 0 || status.State.Running != nil || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil {
				return false
			}
		}
	}
	return true
}

// Date resolution against the original executor's own terminal timestamps.
// A later status poll must not extend the receiver's recovery deadline.
func alStalledFinished(pod *corev1.Pod, claim alStalledClaim, uid types.UID) (time.Time, error) {
	if !alStalledPodMatches(pod, claim, uid) || pod.Status.Phase != corev1.PodSucceeded ||
		len(pod.Spec.Containers) == 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return time.Time{}, errors.New("the original held executor has no complete result transport")
	}
	var finished time.Time
	for _, group := range []struct {
		containers []corev1.Container
		statuses   []corev1.ContainerStatus
	}{{pod.Spec.InitContainers, pod.Status.InitContainerStatuses}, {pod.Spec.Containers, pod.Status.ContainerStatuses}} {
		if len(group.containers) != len(group.statuses) {
			return time.Time{}, errors.New("the held executor omitted a container outcome")
		}
		wanted := make(map[string]bool)
		for _, container := range group.containers {
			if container.Name == "" || wanted[container.Name] {
				return time.Time{}, errors.New("the held executor has ambiguous container identities")
			}
			wanted[container.Name] = true
		}
		for _, status := range group.statuses {
			terminal := status.State.Terminated
			if !wanted[status.Name] || status.RestartCount != 0 || terminal == nil || status.State.Running != nil || status.State.Waiting != nil ||
				status.LastTerminationState.Terminated != nil || terminal.StartedAt.IsZero() ||
				terminal.FinishedAt.IsZero() || terminal.ExitCode != 0 || terminal.StartedAt.Before(&metav1.Time{Time: claim.started}) ||
				terminal.FinishedAt.Before(&terminal.StartedAt) {
				return time.Time{}, errors.New("the held executor has incomplete or replaced execution evidence")
			}
			delete(wanted, status.Name)
			if terminal.FinishedAt.After(finished) {
				finished = terminal.FinishedAt.Time
			}
		}
	}
	if finished.IsZero() {
		return time.Time{}, errors.New("the held executor did not finish its result transport")
	}
	return finished, nil
}

// Both controllers retain a failed read-only attempt for retry, but they
// encode the wait differently. Require the original attempt to be accounted
// for and the next one deferred; neither family needs another transient phase.
func alStalledAccounted(object client.Object, claim alStalledClaim, finished time.Time) bool {
	reading := alStalledReading(object)
	if !claim.sameResource(reading) || finished.IsZero() || reading.id != claim.id || reading.operation != claim.operation ||
		reading.jobUID != "" || reading.jobName == "" || reading.jobName == claim.jobName {
		return false
	}
	switch r := object.(type) {
	case *ptahv1.PtahSchema:
		return r.Status.Phase == ptahv1.PhaseFailed && r.Status.NextReconciliationTime != nil &&
			r.Spec.Execution.FailureRetryInterval.Duration == time.Hour && !r.Status.NextReconciliationTime.Time.Before(finished.Add(time.Hour))
	case *ptahv1.PtahMigration:
		active := r.Status.ActiveOperation
		return active != nil && r.Status.Phase == ptahv1.MigrationPhaseResolving && active.RetryNotBefore != nil &&
			r.Spec.Execution.FailureRetryInterval.Duration == time.Hour && !active.RetryNotBefore.Time.Before(finished.Add(time.Hour)) &&
			!active.StartedAt.Time.Before(finished)
	}
	return false
}
