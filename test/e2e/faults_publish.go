package e2e

import (
	"regexp"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// principalPublisherIsolated is the handcrafted publisher Job as it has to
// run: no retry and no service account token, one container of the fixture
// image running the handcrafting command on the principal's file, every
// secret environment variable from the registry Secret and none from another,
// and the artifact mounted from the principal's Secret.
func principalPublisherIsolated(job *batchv1.Job, image, reference, schemaSecret, registrySecret string) bool {
	spec := job.Spec.Template.Spec
	if !int32PointerIs(job.Spec.BackoffLimit, 0) || spec.AutomountServiceAccountToken == nil ||
		*spec.AutomountServiceAccountToken || len(spec.Containers) != 1 {
		return false
	}
	container := spec.Containers[0]
	if container.Image != image || !slices.Equal(container.Command, []string{"/e2e-handcraft-oci"}) ||
		!slices.Equal(container.Args, []string{reference, "/schema/schema.hcl"}) {
		return false
	}
	// jq read [.env[] | .valueFrom.secretKeyRef.name] | unique, so a variable
	// with no Secret reference contributed a null the registry Secret alone
	// does not equal.
	var names []string
	nulls := false
	for _, variable := range container.Env {
		if reference := envSecretRef(variable); reference != nil {
			names = append(names, reference.Name)
		} else {
			nulls = true
		}
	}
	slices.Sort(names)
	if nulls || !slices.Equal(slices.Compact(names), []string{registrySecret}) {
		return false
	}
	if !slices.ContainsFunc(spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == "schema" && volume.Secret != nil && volume.Secret.SecretName == schemaSecret
	}) {
		return false
	}
	for _, variable := range allEnv(spec) {
		if reference := envSecretRef(variable); reference != nil && reference.Name != registrySecret {
			return false
		}
	}
	return true
}

// allEnv is every environment variable of every container a Pod template
// declares.
func allEnv(spec corev1.PodSpec) []corev1.EnvVar {
	var variables []corev1.EnvVar
	for _, container := range spec.InitContainers {
		variables = append(variables, container.Env...)
	}
	for _, container := range spec.Containers {
		variables = append(variables, container.Env...)
	}
	for _, container := range spec.EphemeralContainers {
		variables = append(variables, container.Env...)
	}
	return variables
}

// containsAll is text that carries every fragment given.
func containsAll(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			return false
		}
	}
	return true
}

// Only the handcrafted publisher's fixed diagnostics may enter public CI
// output. Never return an arbitrary log line, URL, or registry response.
var principalPublisherError = regexp.MustCompile(`^e2e-handcraft-oci: (?:read bounded schema input|invalid registry reference|registry credentials are required|encode OCI manifest|parse OCI upload base|registry returned (?:no|an unsafe) upload location|(?:start OCI blob upload|complete OCI blob upload|store OCI manifest): (?:HTTP [1-5][0-9]{2}|create registry request|execute registry request: (?:canceled|deadline exceeded|DNS timeout|DNS not found|DNS failure|network timeout|connection refused|connection reset|connection closed|transport failure)))$`)

func principalPublisherFailure(logs []byte) string {
	var found string
	for line := range strings.SplitSeq(string(logs), "\n") {
		if !principalPublisherError.MatchString(line) {
			continue
		}
		if found != "" {
			return "publisher failure detail is ambiguous"
		}
		found = line
	}
	if found == "" {
		return "publisher failure detail unavailable"
	}
	return found
}
