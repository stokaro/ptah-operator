package e2e

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	handcraftImage     = "e2e.invalid/fixture@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	handcraftReference = "oci://registry.e2e.svc.cluster.local:5000/schemas/credential-principal:stable"
)

// principalPublisher is the handcrafted publisher Job as the fault phase
// creates it.
func principalPublisher() *batchv1.Job {
	registryEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: registryAuthSecret}, Key: key,
		}}}
	}
	backoff, automount := int32(0), false
	job := &batchv1.Job{}
	job.Spec.BackoffLimit = &backoff
	job.Spec.Template.Spec = corev1.PodSpec{
		AutomountServiceAccountToken: &automount,
		Containers: []corev1.Container{{
			Name: "publisher", Image: handcraftImage,
			Command: []string{"/e2e-handcraft-oci"},
			Args:    []string{handcraftReference, "/schema/schema.hcl"},
			Env: []corev1.EnvVar{
				registryEnv("PTAH_OCI_USERNAME", "username"),
				registryEnv("PTAH_OCI_PASSWORD", "password"),
				registryEnv("PTAH_OCI_REGISTRY", "registry"),
			},
		}},
		Volumes: []corev1.Volume{{Name: "schema", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: principalSchemaSecret},
		}}},
	}
	return job
}

func TestPrincipalPublisherIsolated(t *testing.T) {
	t.Parallel()
	if !principalPublisherIsolated(principalPublisher(), handcraftImage, handcraftReference, principalSchemaSecret, registryAuthSecret) {
		t.Fatal("the publisher the phase creates was refused")
	}
	for name, mutate := range map[string]func(*batchv1.Job){
		"a retry": func(job *batchv1.Job) {
			retries := int32(1)
			job.Spec.BackoffLimit = &retries
		},
		"no backoff limit":  func(job *batchv1.Job) { job.Spec.BackoffLimit = nil },
		"a mounted token":   func(job *batchv1.Job) { job.Spec.Template.Spec.AutomountServiceAccountToken = nil },
		"another image":     func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Image = "e2e.invalid/other" },
		"another command":   func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh"} },
		"another reference": func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Args[0] = "oci://elsewhere/x:y" },
		"a second container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "extra"})
		},
		"a literal variable":          func(job *batchv1.Job) { addPublisherEnv(job, corev1.EnvVar{Name: "HOME", Value: "/work"}) },
		"a database secret variable":  func(job *batchv1.Job) { addPublisherEnv(job, publisherSecretEnv("PTAH_URL", pgSecret)) },
		"no registry secret variable": func(job *batchv1.Job) { job.Spec.Template.Spec.Containers[0].Env = nil },
		"the schema from a ConfigMap": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}
		},
		"the schema from another Secret": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[0].Secret.SecretName = "other"
		},
		"a secret in an init container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "init", Env: []corev1.EnvVar{publisherSecretEnv("X", pgSecret)}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := principalPublisher()
			mutate(job)
			if principalPublisherIsolated(job, handcraftImage, handcraftReference, principalSchemaSecret, registryAuthSecret) {
				t.Fatalf("a publisher with %s was accepted", name)
			}
		})
	}
}

func addPublisherEnv(job *batchv1.Job, variable corev1.EnvVar) {
	job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, variable)
}

func publisherSecretEnv(name, secret string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "url",
	}}}
}

func TestContainsAll(t *testing.T) {
	t.Parallel()
	message := `admission webhook "vpodintent.operator.ptah.run" denied the request: ephemeral containers are refused`
	if !containsAll(message, "vpodintent.operator.ptah.run", "denied the request") {
		t.Fatal("the Pod-intent refusal was not recognized")
	}
	for _, other := range []string{
		`admission webhook "vpod.other.io" denied the request: no`,
		`vpodintent.operator.ptah.run: connection refused`,
	} {
		if containsAll(other, "vpodintent.operator.ptah.run", "denied the request") {
			t.Errorf("%q was taken for the Pod-intent refusal", other)
		}
	}
}

func TestPrincipalPublisherFailureAllowsOnlyFixedDiagnostics(t *testing.T) {
	t.Parallel()
	const prefix = "e2e-handcraft-oci: "
	for _, detail := range []string{
		"read bounded schema input", "invalid registry reference", "registry credentials are required",
		"encode OCI manifest", "parse OCI upload base", "registry returned no upload location", "registry returned an unsafe upload location",
		"start OCI blob upload: HTTP 401", "complete OCI blob upload: HTTP 503", "store OCI manifest: HTTP 500",
		"start OCI blob upload: create registry request",
		"start OCI blob upload: execute registry request: DNS timeout",
		"complete OCI blob upload: execute registry request: connection refused",
		"store OCI manifest: execute registry request: transport failure",
	} {
		if got := principalPublisherFailure([]byte(prefix + detail + "\n")); got != prefix+detail {
			t.Errorf("fixed diagnostic lost: %q -> %q", detail, got)
		}
	}
	for _, text := range []string{
		"", "private-schema-and-password", prefix + "private-schema-and-password",
		prefix + "store OCI manifest: HTTP 500 private-schema-and-password",
		prefix + "start OCI blob upload: execute registry request: https://user:password@registry.test/private",
		prefix + "start OCI blob upload: HTTP 401\rprivate-schema-and-password",
		prefix + "start OCI blob upload: HTTP 401\x1b[31m",
	} {
		if got := principalPublisherFailure([]byte(text)); got != "publisher failure detail unavailable" {
			t.Errorf("untrusted diagnostic escaped: %q -> %q", text, got)
		}
	}
	if got := principalPublisherFailure([]byte(prefix + "store OCI manifest: HTTP 500\n" + prefix + "store OCI manifest: HTTP 401\n")); got != "publisher failure detail is ambiguous" {
		t.Fatalf("conflicting causes accepted: %q", got)
	}
}
