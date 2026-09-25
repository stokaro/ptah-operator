package controllerwrite

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// unreviewedJobField names the first field of a Job that the compiled
// Kubernetes types know and the Job contract this webhook enforces does not.
//
// The strict decoder refuses a field the compiled types do not have, and until
// k8s.io/api 0.37 that was how every field below was refused. The types now
// carry them, so the decoder accepts them, and the refusal has to be written
// down. The list is the one the typed policy's supported-window expressions
// hold in internal/crdupgrade, so the two admission layers refuse the same Job.
// hack/verify-kubernetes-support.go refuses a dependency bump until the
// reachable Job and Pod field graph has been reviewed, and that review is where
// this list grows.
func unreviewedJobField(job *batchv1.Job) string {
	if job.Spec.Scheduling != nil {
		return "spec.scheduling"
	}
	pod := &job.Spec.Template.Spec
	if pod.EvictionResponders != nil {
		return "spec.template.spec.evictionResponders"
	}
	for index := range pod.Volumes {
		if field := unreviewedVolumeField(&pod.Volumes[index]); field != "" {
			return fmt.Sprintf("spec.template.spec.volumes[%d].%s", index, field)
		}
	}
	for index := range pod.InitContainers {
		if field := unreviewedContainerField(&pod.InitContainers[index]); field != "" {
			return fmt.Sprintf("spec.template.spec.initContainers[%d].%s", index, field)
		}
	}
	for index := range pod.Containers {
		if field := unreviewedContainerField(&pod.Containers[index]); field != "" {
			return fmt.Sprintf("spec.template.spec.containers[%d].%s", index, field)
		}
	}
	for index := range pod.EphemeralContainers {
		container := corev1.Container(pod.EphemeralContainers[index].EphemeralContainerCommon)
		if field := unreviewedContainerField(&container); field != "" {
			return fmt.Sprintf("spec.template.spec.ephemeralContainers[%d].%s", index, field)
		}
	}
	return ""
}

func unreviewedVolumeField(volume *corev1.Volume) string {
	if volume.EmptyDir != nil && volume.EmptyDir.Mode != nil {
		return "emptyDir.mode"
	}
	if source := volume.ConfigMap; source != nil {
		if source.DefaultUser != nil {
			return "configMap.defaultUser"
		}
		if field := unreviewedItemUser(source.Items); field != "" {
			return "configMap." + field
		}
	}
	if source := volume.Secret; source != nil {
		if source.DefaultUser != nil {
			return "secret.defaultUser"
		}
		if field := unreviewedItemUser(source.Items); field != "" {
			return "secret." + field
		}
	}
	if source := volume.DownwardAPI; source != nil {
		if source.DefaultUser != nil {
			return "downwardAPI.defaultUser"
		}
		if field := unreviewedFileUser(source.Items); field != "" {
			return "downwardAPI." + field
		}
	}
	if source := volume.Projected; source != nil {
		if source.DefaultUser != nil {
			return "projected.defaultUser"
		}
		for index := range source.Sources {
			if field := unreviewedProjectionField(&source.Sources[index]); field != "" {
				return fmt.Sprintf("projected.sources[%d].%s", index, field)
			}
		}
	}
	return ""
}

func unreviewedProjectionField(projection *corev1.VolumeProjection) string {
	if projection.ConfigMap != nil {
		if field := unreviewedItemUser(projection.ConfigMap.Items); field != "" {
			return "configMap." + field
		}
	}
	if projection.Secret != nil {
		if field := unreviewedItemUser(projection.Secret.Items); field != "" {
			return "secret." + field
		}
	}
	if projection.DownwardAPI != nil {
		if field := unreviewedFileUser(projection.DownwardAPI.Items); field != "" {
			return "downwardAPI." + field
		}
	}
	if projection.ServiceAccountToken != nil && projection.ServiceAccountToken.User != nil {
		return "serviceAccountToken.user"
	}
	if projection.ClusterTrustBundle != nil && projection.ClusterTrustBundle.User != nil {
		return "clusterTrustBundle.user"
	}
	if projection.PodCertificate != nil && projection.PodCertificate.User != nil {
		return "podCertificate.user"
	}
	return ""
}

func unreviewedItemUser(items []corev1.KeyToPath) string {
	for index := range items {
		if items[index].User != nil {
			return fmt.Sprintf("items[%d].user", index)
		}
	}
	return ""
}

func unreviewedFileUser(items []corev1.DownwardAPIVolumeFile) string {
	for index := range items {
		if items[index].User != nil {
			return fmt.Sprintf("items[%d].user", index)
		}
	}
	return ""
}

func unreviewedContainerField(container *corev1.Container) string {
	for index := range container.VolumeMounts {
		if container.VolumeMounts[index].BindMountOptions != nil {
			return fmt.Sprintf("volumeMounts[%d].bindMountOptions", index)
		}
	}
	for _, probe := range []struct {
		name  string
		probe *corev1.Probe
	}{
		{"livenessProbe", container.LivenessProbe},
		{"readinessProbe", container.ReadinessProbe},
		{"startupProbe", container.StartupProbe},
	} {
		if probe.probe == nil {
			continue
		}
		if field := unreviewedHandlerField(probe.probe.HTTPGet, probe.probe.GRPC); field != "" {
			return probe.name + "." + field
		}
	}
	if lifecycle := container.Lifecycle; lifecycle != nil {
		for _, hook := range []struct {
			name    string
			handler *corev1.LifecycleHandler
		}{
			{"postStart", lifecycle.PostStart},
			{"preStop", lifecycle.PreStop},
		} {
			if hook.handler == nil {
				continue
			}
			if field := unreviewedHandlerField(hook.handler.HTTPGet, nil); field != "" {
				return "lifecycle." + hook.name + "." + field
			}
		}
	}
	return ""
}

func unreviewedHandlerField(httpGet *corev1.HTTPGetAction, grpc *corev1.GRPCAction) string {
	if httpGet != nil && httpGet.Protocol != nil {
		return "httpGet.protocol"
	}
	if grpc != nil && grpc.Mode != nil {
		return "grpc.mode"
	}
	return ""
}
