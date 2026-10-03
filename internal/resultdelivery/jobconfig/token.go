package jobconfig

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const (
	JobUID           = "PTAH_RESULT_JOB_UID"
	ServerTrust      = "PTAH_RESULT_SERVER_TRUST"
	IdentityTemplate = "PTAH_RESULT_IDENTITY_TEMPLATE"
	TokenPath        = MountPath + "/token"
)

// UsesPodToken recognizes partial token configuration too. A damaged token
// projection must fail validation, never select certificate authentication.
func UsesPodToken(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, arg := range c.Args {
			if strings.HasPrefix(arg, "--result-token") {
				return true
			}
		}
		for _, env := range c.Env {
			if env.Name == JobUID || env.Name == ServerTrust || env.Name == IdentityTemplate {
				return true
			}
		}
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == VolumeName && volume.Projected != nil {
			return true
		}
	}
	return false
}

func tokenVolume() corev1.Volume {
	return corev1.Volume{Name: VolumeName, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: ptr.To(int32(0440)), Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: resultdelivery.TokenAudience, ExpirationSeconds: ptr.To(int64(3600)), Path: "token",
		}}},
	}}}
}

func validServerTrust(trust []byte) bool {
	return len(trust) > 0 && len(trust) <= 64<<10 && x509.NewCertPool().AppendCertsFromPEM(trust)
}

func tokenTemplate(job *batchv1.Job, ownerUID types.UID, generation int64, operationID string) (resultdelivery.Identity, error) {
	invalid := errors.New("result delivery has no exact operation identity")
	if job == nil || len(job.OwnerReferences) != 1 || len(job.Spec.Template.Spec.Containers) != 1 || generation < 1 {
		return resultdelivery.Identity{}, invalid
	}
	owner := job.OwnerReferences[0]
	if owner.UID != ownerUID || owner.Controller == nil || !*owner.Controller || owner.APIVersion != "operator.ptah.run/v1alpha1" ||
		job.Annotations["operator.ptah.run/operation-id"] != operationID {
		return resultdelivery.Identity{}, invalid
	}
	main := job.Spec.Template.Spec.Containers[0]
	operation, engine := "", ""
	for index, arg := range main.Args {
		if arg == "--operation" {
			if operation != "" || index+1 >= len(main.Args) {
				return resultdelivery.Identity{}, invalid
			}
			operation = main.Args[index+1]
		}
	}
	engineSeen := false
	for _, env := range main.Env {
		if env.Name == runner.EnvExpectedDatabaseEngine {
			if engineSeen || env.ValueFrom != nil {
				return resultdelivery.Identity{}, invalid
			}
			engine, engineSeen = strings.ToLower(env.Value), true
		}
	}
	identity := resultdelivery.Identity{Binding: resultstore.Binding{Namespace: job.Namespace, Kind: owner.Kind, Name: owner.Name,
		UID: ownerUID, Generation: generation, ExecutionBindingID: job.Annotations["operator.ptah.run/execution-binding-id"],
		InputFingerprint: job.Annotations["operator.ptah.run/input-fingerprint"], Operation: operation, OperationID: operationID,
		JobName: job.Name, JobUID: "assigned-job-uid", PodName: "assigned-pod-name", PodUID: "assigned-pod-uid"}, Engine: engine}
	if _, err := resultdelivery.CertificateURI(identity); err != nil {
		return resultdelivery.Identity{}, invalid
	}
	// Kubernetes supplies these after Job creation. They are not literals a
	// controller or an upload request may choose instead of the actual Pod.
	identity.Binding.JobUID, identity.Binding.PodName, identity.Binding.PodUID = "", "", ""
	return identity, nil
}

func tokenVariables(job *batchv1.Job, ownerUID types.UID, generation int64, operationID string, trust []byte) ([]corev1.EnvVar, error) {
	if !validServerTrust(trust) {
		return nil, errors.New("result delivery requires bounded public server trust")
	}
	identity, err := tokenTemplate(job, ownerUID, generation, operationID)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(identity)
	if err != nil || len(data) > 4<<10 {
		return nil, errors.New("result delivery identity exceeds its bound")
	}
	return append(variables(generation),
		corev1.EnvVar{Name: JobUID, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			APIVersion: "v1", FieldPath: "metadata.labels['batch.kubernetes.io/controller-uid']"}}},
		corev1.EnvVar{Name: ServerTrust, Value: string(trust)},
		corev1.EnvVar{Name: IdentityTemplate, Value: string(data)}), nil
}

// AttachPodToken fixes public trust and the operation identity before the
// admission snapshot is computed. Only the runner mounts the receiver-audience
// Pod token. No per-operation Secret or API-audience token is projected.
func AttachPodToken(job *batchv1.Job, ownerUID types.UID, generation int64, operationID, endpoint string, serverTrust []byte) error {
	if job == nil || len(job.Spec.Template.Spec.Containers) != 1 || job.Spec.Template.Spec.Containers[0].Name != "ptah" || ValidateEndpoint(endpoint) != nil {
		return errors.New("invalid result token projection")
	}
	values, err := tokenVariables(job, ownerUID, generation, operationID, serverTrust)
	if err != nil {
		return err
	}
	spec := &job.Spec.Template.Spec
	main := &spec.Containers[0]
	main.Args = append(main.Args, "--result-endpoint", endpoint, "--result-token", TokenPath)
	main.Env = append(main.Env, values...)
	sort.Slice(main.Env, func(i, j int) bool { return main.Env[i].Name < main.Env[j].Name })
	main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: VolumeName, MountPath: MountPath, ReadOnly: true})
	spec.Volumes = append(spec.Volumes, tokenVolume())
	_, err = readPodToken(job, ownerUID, operationID)
	return err
}

func readPodToken(job *batchv1.Job, ownerUID types.UID, operationID string) (Config, error) {
	invalid := errors.New("Job has no exact receiver-audience Pod token projection")
	if job == nil || ownerUID == "" || operationID == "" {
		return Config{}, invalid
	}
	spec := &job.Spec.Template.Spec
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "ptah" || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		return Config{}, invalid
	}
	main := spec.Containers[0]
	args := map[string]string{}
	for index, arg := range main.Args {
		if strings.HasPrefix(arg, "--result-") {
			if arg != "--result-endpoint" && arg != "--result-token" || index+1 >= len(main.Args) {
				return Config{}, invalid
			}
			if _, found := args[arg]; found {
				return Config{}, invalid
			}
			args[arg] = main.Args[index+1]
		}
	}
	if len(args) != 2 || ValidateEndpoint(args["--result-endpoint"]) != nil || args["--result-token"] != TokenPath {
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
	if err != nil || generation < 1 || len(selected) != 7 {
		return Config{}, invalid
	}
	expected, err := tokenVariables(job, ownerUID, generation, operationID, []byte(selected[ServerTrust].Value))
	if err != nil {
		return Config{}, invalid
	}
	for _, env := range expected {
		if !reflect.DeepEqual(selected[env.Name], env) {
			return Config{}, invalid
		}
	}
	seen := 0
	for _, volume := range spec.Volumes {
		if volume.Name == VolumeName {
			seen++
			if !reflect.DeepEqual(volume, tokenVolume()) {
				return Config{}, invalid
			}
		} else if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					return Config{}, invalid
				}
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
	for _, container := range spec.InitContainers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == VolumeName {
				return Config{}, invalid
			}
		}
	}
	for _, container := range spec.EphemeralContainers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == VolumeName {
				return Config{}, invalid
			}
		}
	}
	return Config{Endpoint: args["--result-endpoint"], Generation: generation,
		SecretName: CredentialName(ownerUID, operationID, job.Name), PodToken: true}, nil
}
