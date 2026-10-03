package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	corev1 "k8s.io/api/core/v1"
)

func TestAdmissionEvidenceAcceptsOnlyTheBoundReceiverToken(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	f := resulttest.NewPodToken(t, "schema-observe", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	pod := admittedPod()
	defaults := pod.Spec.Containers[0].Resources.DeepCopy()
	for i := range f.Job.Spec.Template.Spec.Containers {
		f.Job.Spec.Template.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
	}
	for i := range f.Job.Spec.Template.Spec.InitContainers {
		f.Job.Spec.Template.Spec.InitContainers[i].Resources = corev1.ResourceRequirements{}
	}
	pod.ObjectMeta = *f.Pod.ObjectMeta.DeepCopy()
	pod.Spec.Containers = f.Job.Spec.Template.Spec.DeepCopy().Containers
	pod.Spec.InitContainers = f.Job.Spec.Template.Spec.DeepCopy().InitContainers
	pod.Spec.Volumes = f.Job.Spec.Template.Spec.DeepCopy().Volumes
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Resources = *defaults.DeepCopy()
	}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Resources = *defaults.DeepCopy()
	}
	identity := controllerIdentity{image: pod.Annotations[annotationControllerImage], revision: pod.Annotations[annotationControllerRev], stateVersion: pod.Annotations[annotationControllerState]}
	if !podAdmissionApplied(f.Job, pod, identity, registryPullSecret) {
		t.Fatal("exact receiver-token projection failed the admission evidence predicate")
	}
	for name, change := range map[string]func(*corev1.Pod){
		"API audience": func(p *corev1.Pod) {
			p.Spec.Volumes[len(p.Spec.Volumes)-1].Projected.Sources[0].ServiceAccountToken.Audience = "kubernetes"
		},
		"extra token": func(p *corev1.Pod) {
			v := *p.Spec.Volumes[len(p.Spec.Volumes)-1].DeepCopy()
			v.Name = "extra-token"
			p.Spec.Volumes = append(p.Spec.Volumes, v)
		},
		"init mount": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].VolumeMounts = append(p.Spec.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: jobconfig.VolumeName, MountPath: jobconfig.MountPath, ReadOnly: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			change(changed)
			if podAdmissionApplied(f.Job, changed, identity, registryPullSecret) {
				t.Fatal("broader token authority passed evidence")
			}
		})
	}
}
