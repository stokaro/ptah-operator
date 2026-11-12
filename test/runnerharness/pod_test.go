package runnerharness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/test/runnerharness"
)

const goldenJobs = "../../internal/workload/testdata/jobs"

func goldenJob(t *testing.T, name string) *batchv1.Job {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenJobs, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	return &job
}

// everyReference is a cluster that holds every Secret and ConfigMap key a Job
// names, so that a Translate failure is about the Job's shape.
func everyReference(job *batchv1.Job) runnerharness.Cluster {
	cluster := runnerharness.Cluster{Secrets: map[string]map[string][]byte{}, ConfigMaps: map[string]map[string][]byte{}}
	secret := func(name, key string) {
		if cluster.Secrets[name] == nil {
			cluster.Secrets[name] = map[string][]byte{}
		}
		cluster.Secrets[name][key] = []byte("value")
	}
	configMap := func(name, key string) {
		if cluster.ConfigMaps[name] == nil {
			cluster.ConfigMaps[name] = map[string][]byte{}
		}
		cluster.ConfigMaps[name][key] = []byte("value")
	}
	spec := job.Spec.Template.Spec
	for _, container := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, variable := range container.Env {
			if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
				secret(variable.ValueFrom.SecretKeyRef.Name, variable.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
	for _, volume := range spec.Volumes {
		switch {
		case volume.Secret != nil:
			secret(volume.Secret.SecretName, "seed")
			for _, item := range volume.Secret.Items {
				secret(volume.Secret.SecretName, item.Key)
			}
		case volume.ConfigMap != nil:
			configMap(volume.ConfigMap.Name, "seed")
			for _, item := range volume.ConfigMap.Items {
				configMap(volume.ConfigMap.Name, item.Key)
			}
		case volume.Projected != nil:
			for _, projection := range volume.Projected.Sources {
				if projection.ConfigMap == nil {
					continue
				}
				for _, item := range projection.ConfigMap.Items {
					configMap(projection.ConfigMap.Name, item.Key)
				}
			}
		}
	}
	return cluster
}

// Every Job the builder's golden set holds translates, so a new volume kind or
// security field reaches this package offline rather than on a run.
func TestTranslateTakesEveryOperationJobTheBuilderMakes(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(goldenJobs)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := goldenJob(t, name)
			pod, err := runnerharness.Translate(job, everyReference(job))
			if err != nil {
				t.Fatalf("Translate() = %v", err)
			}
			for _, container := range pod.Containers {
				if container.User != "65532:65532" || !container.ReadOnlyRoot || !container.NoNewPrivilege ||
					!slices.Equal(container.DropCapability, []string{"ALL"}) {
					t.Errorf("container %s runs as %+v", container.Name, container)
				}
			}
		})
		read++
	}
	if read == 0 {
		t.Fatal("read no golden Job")
	}
}

// The flags are the Job's values, and HOME and TMPDIR are on a volume the
// container can write.
func TestRunArgsAreTheJobsShape(t *testing.T) {
	t.Parallel()
	job := goldenJob(t, "schema-observe")
	pod, err := runnerharness.Translate(job, everyReference(job))
	if err != nil {
		t.Fatal(err)
	}
	main := pod.Containers[len(pod.Containers)-1]
	if main.Name != "ptah" {
		t.Fatalf("the last container is %s", main.Name)
	}
	args, err := main.RunArgs("a-name", "a-network", func(mount runnerharness.Mount) ([]string, error) {
		return []string{"--mount", mount.Volume + ":" + mount.Path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, flag := range []string{
		"--read-only", "--user 65532:65532", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--network a-network", "--workdir /work", "--entrypoint /runner/ptah-runner",
		"--mount work:/work", "--operation observe",
	} {
		if !strings.Contains(joined, flag) {
			t.Errorf("the command line %q has no %q", joined, flag)
		}
	}
	values := map[string]string{}
	for _, variable := range main.Env {
		values[variable.Name] = variable.Value
	}
	// A credential is a file; what a container still reads from a Secret into
	// its environment is a grant, and it goes by name as a credential did.
	secrets := 0
	for _, container := range pod.Containers {
		containerArgs, err := container.RunArgs("a-name", "a-network", func(mount runnerharness.Mount) ([]string, error) {
			return []string{"--mount", mount.Volume + ":" + mount.Path}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range container.Env {
			if !variable.Secret {
				continue
			}
			secrets++
			if strings.Contains(strings.Join(containerArgs, " "), variable.Name+"=") {
				t.Errorf("the Secret value of %s is on the command line of %s", variable.Name, container.Name)
			}
			if !slices.Contains(container.SecretEnv(), variable.Name+"="+variable.Value) {
				t.Errorf("the docker client is not given %s for %s", variable.Name, container.Name)
			}
		}
	}
	if secrets == 0 {
		t.Error("the row shows nothing: the Job reads no Secret into an environment")
	}
	for _, name := range []string{"HOME", "TMPDIR"} {
		mounted := slices.ContainsFunc(main.Mounts, func(mount runnerharness.Mount) bool {
			return mount.Path == values[name] && !mount.ReadOnly
		})
		if !mounted {
			t.Errorf("%s is %q, and no writable volume is mounted there", name, values[name])
		}
	}
	if pod.InstallTo != "/runner/ptah-runner" || main.Command[0] != pod.InstallTo {
		t.Errorf("the runner installs to %q and the container runs %q", pod.InstallTo, main.Command[0])
	}
}

// The stand-in refuses what it would not reproduce. Each row weakens the real
// Job the way a change to the builder could, and an ignored field would go on
// reporting a shape the Job no longer has.
func TestTranslateRefusesWhatItWouldNotReproduce(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	root := int64(0)
	main := func(job *batchv1.Job) *corev1.Container { return &job.Spec.Template.Spec.Containers[0] }
	pod := func(job *batchv1.Job) *corev1.PodSpec { return &job.Spec.Template.Spec }
	for _, row := range []struct {
		name   string
		mutate func(*batchv1.Job)
		want   string
	}{
		{"a writable root", func(job *batchv1.Job) { main(job).SecurityContext.ReadOnlyRootFilesystem = &no }, "root file system"},
		{"no read-only setting", func(job *batchv1.Job) { main(job).SecurityContext.ReadOnlyRootFilesystem = nil }, "root file system"},
		{"privilege escalation", func(job *batchv1.Job) { main(job).SecurityContext.AllowPrivilegeEscalation = &yes }, "escalation"},
		{"privileged", func(job *batchv1.Job) { main(job).SecurityContext.Privileged = &yes }, "privileged"},
		{"root", func(job *batchv1.Job) { main(job).SecurityContext.RunAsUser = &root }, "root"},
		{"no user", func(job *batchv1.Job) { main(job).SecurityContext.RunAsUser = nil }, "user"},
		{"a capability kept", func(job *batchv1.Job) {
			main(job).SecurityContext.Capabilities.Drop = []corev1.Capability{"NET_RAW"}
		}, "capability"},
		{"a capability added", func(job *batchv1.Job) {
			main(job).SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}, "capability"},
		{"no seccomp profile", func(job *batchv1.Job) { main(job).SecurityContext.SeccompProfile = nil }, "seccomp"},
		{"no security context", func(job *batchv1.Job) { main(job).SecurityContext = nil }, "security context"},
		{"the host network", func(job *batchv1.Job) { pod(job).HostNetwork = true }, "host namespace"},
		{"a service account token", func(job *batchv1.Job) { pod(job).AutomountServiceAccountToken = &yes }, "service account"},
		{"a restart", func(job *batchv1.Job) { pod(job).RestartPolicy = corev1.RestartPolicyOnFailure }, "restarts"},
		{"no install container", func(job *batchv1.Job) { pod(job).InitContainers = pod(job).InitContainers[1:] }, "installing the runner"},
		{"another install", func(job *batchv1.Job) { pod(job).InitContainers[0].Args = []string{"--install-to", "relative"} }, "install"},
		{"an entrypoint the image chooses", func(job *batchv1.Job) { main(job).Command = nil }, "no command"},
		{"environment from a field", func(job *batchv1.Job) {
			main(job).Env = append(main(job).Env, corev1.EnvVar{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}})
		}, "other than a Secret"},
		{"environment from a whole Secret", func(job *batchv1.Job) {
			main(job).EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{}}}
		}, "does not reproduce"},
		{"a required Secret key nothing provides", func(job *batchv1.Job) {
			main(job).Env = append(main(job).Env, corev1.EnvVar{Name: "ABSENT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "key",
			}}})
		}, "did not provide"},
		{"a subpath mount", func(job *batchv1.Job) { main(job).VolumeMounts[0].SubPath = "part" }, "subpath"},
		{"a volume on disk", func(job *batchv1.Job) {
			for index := range pod(job).Volumes {
				if pod(job).Volumes[index].EmptyDir != nil {
					pod(job).Volumes[index].EmptyDir.Medium = ""
				}
			}
		}, "emptyDir"},
		{"a host path", func(job *batchv1.Job) {
			for index := range pod(job).Volumes {
				if pod(job).Volumes[index].EmptyDir != nil {
					pod(job).Volumes[index].VolumeSource = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}
				}
			}
		}, "HostPath"},
		{"a mount of a volume the Pod lacks", func(job *batchv1.Job) { main(job).VolumeMounts[0].Name = "absent" }, "does not declare"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			job := goldenJob(t, "schema-observe")
			cluster := everyReference(job)
			row.mutate(job)
			if _, err := runnerharness.Translate(job, cluster); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Translate() = %v, want it to name %q", err, row.want)
			}
		})
	}
}

// A projection is the kubelet's: each item at its path, with its mode, owned by
// root and the Pod's group.
func TestTranslateProjectsWhatTheKubeletWould(t *testing.T) {
	t.Parallel()
	job := goldenJob(t, "schema-apply-admitted-scheduling")
	cluster := everyReference(job)
	pod, err := runnerharness.Translate(job, cluster)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(pod.Volumes, func(volume runnerharness.Volume) bool { return volume.Name == "plan" })
	if index < 0 {
		t.Fatalf("no plan volume in %+v", pod.Volumes)
	}
	plan := pod.Volumes[index]
	if plan.Empty || len(plan.Files) == 0 {
		t.Fatalf("the plan volume is %+v", plan)
	}
	for position, file := range plan.Files {
		wantPath := strings.Repeat("0", 3-len(strconv.Itoa(position))) + strconv.Itoa(position) + ".plan"
		if file.Path != wantPath || file.Mode != 0o440 || file.GID != 65532 || file.UID != 0 || string(file.Data) != "value" {
			t.Errorf("file %d is %+v, want %s", position, file, wantPath)
		}
	}
}
