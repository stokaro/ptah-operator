package podintent_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// classTemplate is the resolved fixture's template with the database URL
// volume a SecretProviderClass target gets, mounted in the operation
// container alone, and the snapshot stamped for that template the way Resolve
// stamps one.
func classTemplate(t *testing.T) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()
	template, snapshot := resolvedFixture(t)
	template.Volumes = append(template.Volumes, corev1.Volume{
		Name: "database-url",
		VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:           workload.SecretsStoreCSIDriver,
			ReadOnly:         ptr.To(true),
			VolumeAttributes: map[string]string{workload.SecretProviderClassVolumeAttribute: "orders-store"},
		}},
	})
	template.Containers[0].VolumeMounts = append(template.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name: "database-url", MountPath: "/var/run/ptah/database", ReadOnly: true,
	})
	digest, err := podintent.DigestTemplate(podTemplate(template))
	if err != nil {
		t.Fatal(err)
	}
	snapshot.TemplateDigest = digest
	snapshot.Digest = ""
	if snapshot.Digest, err = fingerprint.DigestCanonicalJSON(*snapshot); err != nil {
		t.Fatal(err)
	}
	return template, snapshot
}

// TestValidatePodSpecAdmitsExactlyTheSecretProviderClassVolume holds the Pod
// guard to the CSI volume the Job template declares: the Pod that carries it
// as declared is admitted, and each change a mutating webhook could make to it,
// or a second mount of it, is refused.
func TestValidatePodSpecAdmitsExactlyTheSecretProviderClassVolume(t *testing.T) {
	t.Parallel()

	csiOf := func(spec *corev1.PodSpec) *corev1.CSIVolumeSource {
		for index := range spec.Volumes {
			if spec.Volumes[index].Name == "database-url" {
				return spec.Volumes[index].CSI
			}
		}
		t.Fatal("the Pod has no database-url volume")
		return nil
	}
	template, snapshot := classTemplate(t)
	if err := podintent.ValidatePodSpec(admittedPodSpec(template, snapshot), podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() refused the Pod its template declares: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func(*corev1.PodSpec)
	}{
		{"another class", func(spec *corev1.PodSpec) {
			csiOf(spec).VolumeAttributes[workload.SecretProviderClassVolumeAttribute] = "payments-store"
		}},
		{"a second attribute", func(spec *corev1.PodSpec) { csiOf(spec).VolumeAttributes["usePodIdentity"] = "true" }},
		{"another driver", func(spec *corev1.PodSpec) { csiOf(spec).Driver = "csi.example.com" }},
		{"a writable volume", func(spec *corev1.PodSpec) { csiOf(spec).ReadOnly = ptr.To(false) }},
		{"a filesystem type", func(spec *corev1.PodSpec) { csiOf(spec).FSType = ptr.To("ext4") }},
		{"a node publish Secret", func(spec *corev1.PodSpec) {
			csiOf(spec).NodePublishSecretRef = &corev1.LocalObjectReference{Name: "store-credentials"}
		}},
		{"a second CSI volume", func(spec *corev1.PodSpec) {
			volume := *spec.Volumes[len(spec.Volumes)-1].DeepCopy()
			volume.Name = "database-url-copy"
			spec.Volumes = append(spec.Volumes, volume)
		}},
		{"the volume mounted in an init container", func(spec *corev1.PodSpec) {
			spec.InitContainers[0].VolumeMounts = append(spec.InitContainers[0].VolumeMounts, corev1.VolumeMount{
				Name: "database-url", MountPath: "/var/run/ptah/database", ReadOnly: true,
			})
		}},
		{"the volume mounted writable", func(spec *corev1.PodSpec) {
			mounts := spec.Containers[0].VolumeMounts
			mounts[len(mounts)-1].ReadOnly = false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			actual := admittedPodSpec(template, snapshot)
			test.edit(actual)
			if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
				t.Fatal("ValidatePodSpec() admitted a Pod whose SecretProviderClass volume differs from its template")
			}
		})
	}
}
