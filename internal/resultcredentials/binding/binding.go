// Package binding describes public credential metadata and Pod references. It
// carries no key material, issuer, or Kubernetes client.
package binding

import (
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	corev1 "k8s.io/api/core/v1"
	"strings"
)

const (
	PodUID      = "operator.ptah.run/result-pod-uid"
	PodName     = "operator.ptah.run/result-pod-name"
	JobUID      = "operator.ptah.run/result-job-uid"
	OperationID = "operator.ptah.run/result-operation-id"
)

// References includes every direct Secret reference in the pinned Pod API,
// including legacy inline storage sources and CSI authentication. Duplicates
// matter: the permitted Job has exactly one credential reference.
func References(pod *corev1.Pod) []string {
	var result []string
	if pod == nil {
		return result
	}
	add := func(name string) {
		if strings.HasPrefix(name, jobconfig.SecretPrefix) {
			result = append(result, name)
		}
	}
	env := func(values []corev1.EnvVar, from []corev1.EnvFromSource) {
		for _, v := range values {
			if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
				add(v.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, v := range from {
			if v.SecretRef != nil {
				add(v.SecretRef.Name)
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		env(c.Env, c.EnvFrom)
	}
	for _, c := range pod.Spec.InitContainers {
		env(c.Env, c.EnvFrom)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		env(c.Env, c.EnvFrom)
	}
	for _, s := range pod.Spec.ImagePullSecrets {
		add(s.Name)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			add(v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil {
					add(s.Secret.Name)
				}
			}
		}
		if v.CSI != nil && v.CSI.NodePublishSecretRef != nil {
			add(v.CSI.NodePublishSecretRef.Name)
		}
		if v.AzureFile != nil {
			add(v.AzureFile.SecretName)
		}
		if v.CephFS != nil && v.CephFS.SecretRef != nil {
			add(v.CephFS.SecretRef.Name)
		}
		if v.Cinder != nil && v.Cinder.SecretRef != nil {
			add(v.Cinder.SecretRef.Name)
		}
		if v.FlexVolume != nil && v.FlexVolume.SecretRef != nil {
			add(v.FlexVolume.SecretRef.Name)
		}
		if v.ISCSI != nil && v.ISCSI.SecretRef != nil {
			add(v.ISCSI.SecretRef.Name)
		}
		if v.RBD != nil && v.RBD.SecretRef != nil {
			add(v.RBD.SecretRef.Name)
		}
		if v.ScaleIO != nil && v.ScaleIO.SecretRef != nil {
			add(v.ScaleIO.SecretRef.Name)
		}
		if v.StorageOS != nil && v.StorageOS.SecretRef != nil {
			add(v.StorageOS.SecretRef.Name)
		}
	}
	return result
}
