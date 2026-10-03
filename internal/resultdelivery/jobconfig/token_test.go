package jobconfig_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func tokenServerTrust(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestPodTokenProjectionForEveryOperation(t *testing.T) {
	trust := tokenServerTrust(t)
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, name, trust)
			b := f.Identity.Binding
			config, err := jobconfig.Read(f.Job, b.UID, b.OperationID)
			if err != nil || !config.PodToken || config.Generation != b.Generation || config.SecretName != jobconfig.CredentialName(b.UID, b.OperationID, b.JobName) {
				t.Fatalf("token projection does not retain the operation: %+v, %v", config, err)
			}
			if err := (resultauthority.Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); err != nil {
				t.Fatalf("token projection broke the existing admission/claim binding: %v", err)
			}
			var template resultdelivery.Identity
			for _, env := range f.Job.Spec.Template.Spec.Containers[0].Env {
				if env.Name == jobconfig.IdentityTemplate {
					if err := json.Unmarshal([]byte(env.Value), &template); err != nil {
						t.Fatal(err)
					}
				}
			}
			if template.Binding.JobUID != "" || template.Binding.PodName != "" || template.Binding.PodUID != "" {
				t.Fatal("template impersonates API-assigned Job or Pod identity")
			}
			template.Binding.JobUID, template.Binding.PodName, template.Binding.PodUID = b.JobUID, b.PodName, b.PodUID
			if template != f.Identity {
				t.Fatal("the public template differs from the controller's operation identity")
			}
			for _, v := range f.Job.Spec.Template.Spec.Volumes {
				if v.Secret != nil && v.Secret.SecretName == config.SecretName {
					t.Fatal("token delivery still needs a per-operation Secret")
				}
			}
		})
	}
}

func TestPodTokenProjectionRefusesBroaderOrDifferentAuthority(t *testing.T) {
	trust := tokenServerTrust(t)
	for name, mutate := range map[string]func(*batchv1.Job){
		"API audience":         func(j *batchv1.Job) { tokenProjection(j).Audience = "https://kubernetes.default.svc" },
		"unbounded expiration": func(j *batchv1.Job) { tokenProjection(j).ExpirationSeconds = nil },
		"different expiration": func(j *batchv1.Job) { tokenProjection(j).ExpirationSeconds = ptr.To(int64(86400)) },
		"different token path": func(j *batchv1.Job) { tokenProjection(j).Path = "other" },
		"default API token":    func(j *batchv1.Job) { j.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"additional projected token": func(j *batchv1.Job) {
			alias := tokenVolumeOf(j).DeepCopy()
			alias.Name = "another-token"
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, *alias)
		},
		"init token access": func(j *batchv1.Job) {
			j.Spec.Template.Spec.InitContainers[0].VolumeMounts = append(j.Spec.Template.Spec.InitContainers[0].VolumeMounts,
				corev1.VolumeMount{Name: jobconfig.VolumeName, MountPath: "/token", ReadOnly: true})
		},
		"debug token access": func(j *batchv1.Job) {
			j.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name: "debug", VolumeMounts: []corev1.VolumeMount{{Name: jobconfig.VolumeName, MountPath: "/token", ReadOnly: true}},
			}}}
		},
		"literal Job UID": func(j *batchv1.Job) {
			for i, env := range j.Spec.Template.Spec.Containers[0].Env {
				if env.Name == jobconfig.JobUID {
					j.Spec.Template.Spec.Containers[0].Env[i] = corev1.EnvVar{Name: jobconfig.JobUID, Value: "forged-job-uid"}
				}
			}
		},
		"different operation identity": func(j *batchv1.Job) {
			for i, env := range j.Spec.Template.Spec.Containers[0].Env {
				if env.Name == jobconfig.IdentityTemplate {
					j.Spec.Template.Spec.Containers[0].Env[i].Value = "{}"
				}
			}
		},
		"certificate fallback flags": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Args = append(j.Spec.Template.Spec.Containers[0].Args, "--result-credentials", jobconfig.MountPath)
		},
		"missing token flag": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Args = slices.DeleteFunc(j.Spec.Template.Spec.Containers[0].Args, func(arg string) bool {
				return arg == "--result-token" || arg == jobconfig.TokenPath
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.NewPodToken(t, "schema-apply-admitted-scheduling", trust)
			mutate(f.Job)
			if !jobconfig.UsesPodToken(f.Job) {
				t.Fatal("partial token configuration selected the certificate transport")
			}
			if _, err := jobconfig.Read(f.Job, f.Identity.Binding.UID, f.Identity.Binding.OperationID); err == nil {
				t.Fatal("altered token authority passed projection validation")
			}
		})
	}
}

func tokenVolumeOf(job *batchv1.Job) *corev1.Volume {
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == jobconfig.VolumeName {
			return &job.Spec.Template.Spec.Volumes[i]
		}
	}
	panic("fixture has no token volume")
}

func tokenProjection(job *batchv1.Job) *corev1.ServiceAccountTokenProjection {
	return tokenVolumeOf(job).Projected.Sources[0].ServiceAccountToken
}
