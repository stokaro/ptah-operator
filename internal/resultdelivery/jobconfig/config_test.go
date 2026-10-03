package jobconfig_test

import (
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"testing"
)

func TestProjectionRefusesAlteredCredentialAccess(t *testing.T) {
	changes := map[string]func(*batchv1.Job, string){
		"duplicate flag": func(j *batchv1.Job, _ string) {
			j.Spec.Template.Spec.Containers[0].Args = append(j.Spec.Template.Spec.Containers[0].Args, "--result-endpoint", "https://other.invalid")
		},
		"duplicate generation": func(j *batchv1.Job, _ string) {
			j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: jobconfig.Generation, Value: "2"})
		},
		"generation not canonical": func(j *batchv1.Job, _ string) {
			for i, e := range j.Spec.Template.Spec.Containers[0].Env {
				if e.Name == jobconfig.Generation {
					j.Spec.Template.Spec.Containers[0].Env[i].Value = "02"
				}
			}
		},
		"literal Pod UID": func(j *batchv1.Job, _ string) {
			for i, e := range j.Spec.Template.Spec.Containers[0].Env {
				if e.Name == jobconfig.PodUID {
					j.Spec.Template.Spec.Containers[0].Env[i] = corev1.EnvVar{Name: e.Name, Value: "pod-uid"}
				}
			}
		},
		"optional credential": func(j *batchv1.Job, _ string) {
			for i, v := range j.Spec.Template.Spec.Volumes {
				if v.Name == jobconfig.VolumeName {
					j.Spec.Template.Spec.Volumes[i].Secret.Optional = ptr.To(true)
				}
			}
		},
		"wrong secret": func(j *batchv1.Job, _ string) {
			for i, v := range j.Spec.Template.Spec.Volumes {
				if v.Name == jobconfig.VolumeName {
					j.Spec.Template.Spec.Volumes[i].Secret.SecretName = "other"
				}
			}
		},
		"writable mount": func(j *batchv1.Job, _ string) {
			for i, m := range j.Spec.Template.Spec.Containers[0].VolumeMounts {
				if m.Name == jobconfig.VolumeName {
					j.Spec.Template.Spec.Containers[0].VolumeMounts[i].ReadOnly = false
				}
			}
		},
		"init mount": func(j *batchv1.Job, _ string) {
			j.Spec.Template.Spec.InitContainers[0].VolumeMounts = append(j.Spec.Template.Spec.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: jobconfig.VolumeName, MountPath: "/stolen", ReadOnly: true})
		},
		"ephemeral mount": func(j *batchv1.Job, _ string) {
			j.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", VolumeMounts: []corev1.VolumeMount{{Name: jobconfig.VolumeName, MountPath: "/stolen", ReadOnly: true}}}}}
		},
		"Secret alias": func(j *batchv1.Job, name string) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "alias", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}}})
		},
		"projected alias": func(j *batchv1.Job, name string) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "alias", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}}}})
		},
		"env credential": func(j *batchv1.Job, name string) {
			j.Spec.Template.Spec.InitContainers[0].Env = append(j.Spec.Template.Spec.InitContainers[0].Env, corev1.EnvVar{Name: "LEAK", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "tls.key"}}})
		},
		"envFrom credential": func(j *batchv1.Job, name string) {
			j.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}
		},
		"image pull credential": func(j *batchv1.Job, name string) {
			j.Spec.Template.Spec.ImagePullSecrets = append(j.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
		},
		"API token": func(j *batchv1.Job, _ string) { j.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, "schema-apply-admitted-scheduling")
			b := f.Identity.Binding
			bound, err := jobconfig.Read(f.Job, b.UID, b.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			change(f.Job, bound.SecretName)
			if _, err := jobconfig.Read(f.Job, b.UID, b.OperationID); err == nil {
				t.Fatal("altered credential projection accepted")
			}
		})
	}
}
