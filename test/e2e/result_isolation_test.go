package e2e

import (
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
)

func attachIsolationResult(t *testing.T, job *batchv1.Job) {
	t.Helper()
	job.OwnerReferences = []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema",
		Name: job.Labels[labelSchema], UID: "schema-owner", Controller: ptr.To(true)}}
	if err := jobconfig.Attach(job, "schema-owner", 3, job.Annotations[annotationOperationID], "https://receiver.test:9444"); err != nil {
		t.Fatal(err)
	}
}

func TestSourceJobIsolationWithResultDelivery(t *testing.T) {
	for _, mode := range []string{"Environment", "DockerConfigJSON"} {
		t.Run(mode, func(t *testing.T) {
			jobs := sourceJobFixture(t, mode).jobs()
			for i := range jobs {
				jobs[i].Name = "source-" + jobs[i].Labels[labelOperation]
				attachIsolationResult(t, &jobs[i])
			}
			inputs := sourceIsolationInputs(mode)
			if !sourceJobIsolation(jobs, inputs) {
				t.Fatal("valid durable source Jobs lost isolation")
			}
			for _, test := range []struct {
				name string
				edit func(*batchv1.Job)
			}{
				{"writable result credential", func(j *batchv1.Job) {
					mounts := j.Spec.Template.Spec.Containers[0].VolumeMounts
					mounts[len(mounts)-1].ReadOnly = false
				}},
				{"result credential in installer", func(j *batchv1.Job) {
					j.Spec.Template.Spec.InitContainers[0].VolumeMounts = append(j.Spec.Template.Spec.InitContainers[0].VolumeMounts,
						corev1.VolumeMount{Name: jobconfig.VolumeName, MountPath: jobconfig.MountPath, ReadOnly: true})
				}},
				{"another operation credential", func(j *batchv1.Job) {
					volumes := j.Spec.Template.Spec.Volumes
					volumes[len(volumes)-1].Secret.SecretName = "another-operation"
				}},
				{"partial projection", func(j *batchv1.Job) {
					j.Spec.Template.Spec.Containers[0].Args = j.Spec.Template.Spec.Containers[0].Args[:8]
				}},
				{"database credential remains forbidden", func(j *batchv1.Job) {
					j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "PTAH_DB_URL", Value: "forbidden"})
				}},
				{"extra volume remains forbidden", func(j *batchv1.Job) {
					j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "extra"})
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					changed := append([]batchv1.Job(nil), jobs...)
					changed[0] = *jobs[0].DeepCopy()
					test.edit(&changed[0])
					if sourceJobIsolation(changed, inputs) {
						t.Fatal("accepted broken isolation")
					}
				})
			}
		})
	}
}

func TestCustomCAPodIsolationWithResultDelivery(t *testing.T) {
	pods := customCAPodFixture(t).pods()
	var jobs []batchv1.Job
	for i := range pods {
		pod := &pods[i]
		job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: "job-" + pod.Labels[labelOperation], UID: types.UID("job-uid-" + pod.Labels[labelOperation]),
			Labels: pod.Labels, Annotations: map[string]string{annotationOperationID: "operation-" + pod.Labels[labelOperation]},
		}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: *pod.Spec.DeepCopy()}}}
		attachIsolationResult(t, &job)
		pod.Spec = *job.Spec.Template.Spec.DeepCopy()
		pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = job.Name, job.UID
		jobs = append(jobs, job)
	}
	inputs := customCAPodIsolationInputs{jobs: jobs, databaseSecret: "database-url", registrySecret: "registry-auth",
		registryAuthority: "registry.example", caConfigMap: "registry-ca", resolvedReference: fixtureResolvedReference}
	original := pods[0].DeepCopy()
	if !customCAPodIsolation(pods, inputs) {
		t.Fatal("valid durable custom-CA Pods lost isolation")
	}
	if !reflect.DeepEqual(original, &pods[0]) {
		t.Fatal("isolation check mutated its reading")
	}
	for _, test := range []struct {
		name string
		edit func(*corev1.Pod)
	}{
		{"writable credential", func(p *corev1.Pod) {
			mounts := p.Spec.Containers[0].VolumeMounts
			mounts[len(mounts)-1].ReadOnly = false
		}},
		{"credential exposed to fetch", func(p *corev1.Pod) {
			p.Spec.InitContainers[2].VolumeMounts = append(p.Spec.InitContainers[2].VolumeMounts,
				corev1.VolumeMount{Name: jobconfig.VolumeName, MountPath: jobconfig.MountPath, ReadOnly: true})
		}},
		{"endpoint differs from Job", func(p *corev1.Pod) {
			args := p.Spec.Containers[0].Args
			for i, arg := range args {
				if arg == "--result-endpoint" {
					args[i+1] = "https://other.test:9444"
				}
			}
		}},
		{"projection removed from Pod", func(p *corev1.Pod) {
			normalized, err := resultJobWithoutProjection(&jobs[0])
			if err != nil {
				t.Fatal(err)
			}
			p.Spec = normalized.Spec.Template.Spec
		}},
		{"wrong Job UID", func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" }},
		{"registry credential remains forbidden", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "PTAH_OCI_REGISTRY", Value: "registry.example"})
		}},
		{"service account token remains forbidden", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := append([]corev1.Pod(nil), pods...)
			changed[0] = *pods[0].DeepCopy()
			test.edit(&changed[0])
			if customCAPodIsolation(changed, inputs) {
				t.Fatal("accepted broken isolation")
			}
		})
	}
}
