// Package jobconfig fixes the runner's durable-delivery inputs in its immutable
// Job template. It contains no certificate signer or Kubernetes API client.
package jobconfig

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const (
	PodNamespace = "PTAH_RESULT_POD_NAMESPACE"
	PodName      = "PTAH_RESULT_POD_NAME"
	PodUID       = "PTAH_RESULT_POD_UID"
	Generation   = "PTAH_RESULT_GENERATION"
	VolumeName   = "result-credentials"
	MountPath    = "/credentials/result"
	SecretPrefix = "ptah-result-key-"
)

func CredentialName(ownerUID types.UID, operationID, jobName string) string {
	digest := sha256.Sum256([]byte(string(ownerUID) + "\x00" + operationID + "\x00" + jobName))
	return fmt.Sprintf("%s%x", SecretPrefix, digest[:16])
}

func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("result endpoint must be an HTTPS origin")
	}
	return nil
}

func variables(generation int64) []corev1.EnvVar {
	result := []corev1.EnvVar{{Name: Generation, Value: strconv.FormatInt(generation, 10)}}
	for _, item := range []struct{ name, path string }{{PodNamespace, "metadata.namespace"}, {PodName, "metadata.name"}, {PodUID, "metadata.uid"}} {
		result = append(result, corev1.EnvVar{Name: item.name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: item.path}}})
	}
	return result
}

func volume(name string) corev1.Volume {
	return corev1.Volume{Name: VolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr.To(int32(0440)), Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"}}}}}
}

// Attach runs before the admission snapshot is computed. Generation comes from
// the owner that dispatches the Job, not from a later live resource read.
func Attach(job *batchv1.Job, ownerUID types.UID, generation int64, operationID, endpoint string) error {
	if job == nil || job.Name == "" || ownerUID == "" || operationID == "" || generation < 1 {
		return errors.New("result delivery requires an original operation generation")
	}
	if err := ValidateEndpoint(endpoint); err != nil {
		return err
	}
	spec := &job.Spec.Template.Spec
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "ptah" {
		return errors.New("result delivery requires the single runner container")
	}
	main := &spec.Containers[0]
	main.Args = append(main.Args, "--result-endpoint", endpoint, "--result-credentials", MountPath)
	main.Env = append(main.Env, variables(generation)...)
	sort.Slice(main.Env, func(i, j int) bool { return main.Env[i].Name < main.Env[j].Name })
	main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: VolumeName, MountPath: MountPath, ReadOnly: true})
	spec.Volumes = append(spec.Volumes, volume(CredentialName(ownerUID, operationID, job.Name)))
	_, err := Read(job, ownerUID, operationID)
	return err
}

type Config struct {
	Endpoint   string
	Generation int64
	SecretName string
}

// Read holds the delivery flags, frozen generation, actual-Pod downward API,
// and isolated credential projection to the shape Attach writes. The caller
// separately holds the whole template to the claim's admission snapshot.
func Read(job *batchv1.Job, ownerUID types.UID, operationID string) (Config, error) {
	invalid := errors.New("Job has no exact durable result credential projection")
	if job == nil || ownerUID == "" || operationID == "" {
		return Config{}, invalid
	}
	spec := &job.Spec.Template.Spec
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "ptah" {
		return Config{}, invalid
	}
	main := spec.Containers[0]
	args := map[string]string{}
	for index, arg := range main.Args {
		if strings.HasPrefix(arg, "--result-") {
			if arg != "--result-endpoint" && arg != "--result-credentials" || index+1 >= len(main.Args) {
				return Config{}, invalid
			}
			if _, found := args[arg]; found {
				return Config{}, invalid
			}
			args[arg] = main.Args[index+1]
		}
	}
	if len(args) != 2 || ValidateEndpoint(args["--result-endpoint"]) != nil || args["--result-credentials"] != MountPath {
		return Config{}, invalid
	}
	selected := map[string]corev1.EnvVar{}
	for _, env := range main.Env {
		if strings.HasPrefix(env.Name, "PTAH_RESULT_") {
			if _, found := selected[env.Name]; found {
				return Config{}, invalid
			}
			selected[env.Name] = env
		}
	}
	generation, err := strconv.ParseInt(selected[Generation].Value, 10, 64)
	if err != nil || generation < 1 || len(selected) != 4 {
		return Config{}, invalid
	}
	for _, expected := range variables(generation) {
		if !reflect.DeepEqual(selected[expected.Name], expected) {
			return Config{}, invalid
		}
	}
	name := CredentialName(ownerUID, operationID, job.Name)
	seen := 0
	for _, v := range spec.Volumes {
		if v.Name != VolumeName {
			if v.Secret != nil && v.Secret.SecretName == name {
				return Config{}, invalid
			}
			if v.Projected != nil {
				for _, source := range v.Projected.Sources {
					if source.Secret != nil && source.Secret.Name == name {
						return Config{}, invalid
					}
				}
			}
		}
		if v.Name == VolumeName {
			seen++
			if !reflect.DeepEqual(v, volume(name)) {
				return Config{}, invalid
			}
		}
	}
	if seen != 1 {
		return Config{}, invalid
	}
	seen = 0
	for _, mount := range main.VolumeMounts {
		if mount.Name == VolumeName || mount.MountPath == MountPath || strings.HasPrefix(mount.MountPath, MountPath+"/") {
			seen++
			if !reflect.DeepEqual(mount, corev1.VolumeMount{Name: VolumeName, MountPath: MountPath, ReadOnly: true}) {
				return Config{}, invalid
			}
		}
	}
	if seen != 1 {
		return Config{}, invalid
	}
	for _, pull := range spec.ImagePullSecrets {
		if pull.Name == name {
			return Config{}, invalid
		}
	}
	if credentialEnvironment(main.Env, main.EnvFrom, name) {
		return Config{}, invalid
	}
	for _, container := range spec.InitContainers {
		if credentialEnvironment(container.Env, container.EnvFrom, name) {
			return Config{}, invalid
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == VolumeName {
				return Config{}, invalid
			}
		}
	}
	for _, container := range spec.EphemeralContainers {
		if credentialEnvironment(container.Env, container.EnvFrom, name) {
			return Config{}, invalid
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == VolumeName {
				return Config{}, invalid
			}
		}
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		return Config{}, invalid
	}
	return Config{Endpoint: args["--result-endpoint"], Generation: generation, SecretName: name}, nil
}

func credentialEnvironment(env []corev1.EnvVar, from []corev1.EnvFromSource, name string) bool {
	for _, value := range env {
		if value.ValueFrom != nil && value.ValueFrom.SecretKeyRef != nil && value.ValueFrom.SecretKeyRef.Name == name {
			return true
		}
	}
	for _, source := range from {
		if source.SecretRef != nil && source.SecretRef.Name == name {
			return true
		}
	}
	return false
}
