package runnerharness

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

// Cluster is the Secrets and ConfigMaps a Job's Pod reads, by name and key.
type Cluster struct {
	Secrets    map[string]map[string][]byte
	ConfigMaps map[string]map[string][]byte
}

// Pod is an operation Job's Pod, as the harness runs it. It carries the values
// the Job declares rather than a second statement of them: the user, the read-
// only root file system, the dropped capabilities, the environment, the mounts,
// the command and its arguments all come from the Job the builder made.
type Pod struct {
	// InstallTo is where the Job installs the runner. The harness puts the
	// recorder there instead of running the install container.
	InstallTo string
	// Containers are the init containers after the runner's install, then the
	// operation container, in order.
	Containers []Container
	Volumes    []Volume
}

// Container is one container of a Pod.
type Container struct {
	Name, Image    string
	Command, Args  []string
	WorkingDir     string
	Env            []EnvVar
	User           string
	ReadOnlyRoot   bool
	NoNewPrivilege bool
	DropCapability []string
	Mounts         []Mount
}

// EnvVar is a resolved environment variable. Secret says the value came from a
// Secret: the Job's literals are not secrets, and these are.
type EnvVar struct {
	Name, Value string
	Secret      bool
}

// Mount is one volume in a container.
type Mount struct {
	Volume, Path string
	ReadOnly     bool
}

// Volume is one volume of a Pod. A volume with Empty set starts empty and
// writable by the Pod's group; otherwise it holds Files.
type Volume struct {
	Name  string
	Empty bool
	// SizeBytes bounds an empty volume.
	SizeBytes int64
	Files     []volumes.Entry
	// Group is the Pod's fsGroup.
	Group int
}

// The install container's fixed shape. Anything else is a change the harness has
// to be told about.
const installContainerName = "install-runner"

// Translate turns a Job into the Pod the harness runs, and refuses anything it
// does not know how to run faithfully: a stand-in that ignored a new volume kind
// or a weakened security context would go on reporting what the Job no longer
// does.
func Translate(job *batchv1.Job, cluster Cluster) (Pod, error) {
	spec := job.Spec.Template.Spec
	if err := checkPodSpec(spec); err != nil {
		return Pod{}, err
	}
	if len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != installContainerName {
		return Pod{}, errors.New("the Pod does not start by installing the runner")
	}
	install := spec.InitContainers[0]
	if len(install.Args) != 2 || install.Args[0] != "--install-to" || !path.IsAbs(install.Args[1]) {
		return Pod{}, fmt.Errorf("the runner's install takes %q", install.Args)
	}
	pod := Pod{InstallTo: install.Args[1]}

	group, err := fsGroup(spec)
	if err != nil {
		return Pod{}, err
	}
	used := map[string]bool{}
	for _, container := range append(slices.Clone(spec.InitContainers[1:]), spec.Containers...) {
		translated, err := translateContainer(container, cluster, spec.SecurityContext)
		if err != nil {
			return Pod{}, fmt.Errorf("container %s: %w", container.Name, err)
		}
		pod.Containers = append(pod.Containers, translated)
		for _, mount := range translated.Mounts {
			used[mount.Volume] = true
		}
	}
	for _, volume := range spec.Volumes {
		if !used[volume.Name] {
			continue
		}
		translated, err := translateVolume(volume, cluster, group)
		if err != nil {
			return Pod{}, fmt.Errorf("volume %s: %w", volume.Name, err)
		}
		pod.Volumes = append(pod.Volumes, translated)
	}
	for name := range used {
		if !slices.ContainsFunc(pod.Volumes, func(volume Volume) bool { return volume.Name == name }) {
			return Pod{}, fmt.Errorf("a container mounts %s, which the Pod does not declare", name)
		}
	}
	return pod, nil
}

// checkPodSpec refuses a Pod-level field that changes what a process sees and
// that a container on a Docker network would not reproduce.
func checkPodSpec(spec corev1.PodSpec) error {
	switch {
	case spec.HostNetwork, spec.HostPID, spec.HostIPC:
		return errors.New("the Pod shares a host namespace")
	case spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken:
		return errors.New("the Pod mounts a service account token")
	case spec.SecurityContext == nil:
		return errors.New("the Pod has no security context")
	case spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace:
		return errors.New("the Pod shares a process namespace")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		return fmt.Errorf("the Pod restarts %s", spec.RestartPolicy)
	}
	return nil
}

func fsGroup(spec corev1.PodSpec) (int, error) {
	if spec.SecurityContext.FSGroup == nil {
		return 0, errors.New("the Pod has no fsGroup")
	}
	return int(*spec.SecurityContext.FSGroup), nil
}

func translateContainer(container corev1.Container, cluster Cluster, podContext *corev1.PodSecurityContext) (Container, error) {
	if len(container.Command) == 0 {
		return Container{}, errors.New("it names no command: the image's entrypoint is not part of the contract")
	}
	switch {
	case len(container.EnvFrom) != 0, len(container.VolumeDevices) != 0, len(container.Ports) != 0, container.Lifecycle != nil,
		container.Stdin, container.TTY, container.RestartPolicy != nil:
		return Container{}, errors.New("it sets a field the harness does not reproduce")
	}
	translated := Container{
		Name: container.Name, Image: container.Image,
		Command: slices.Clone(container.Command), Args: slices.Clone(container.Args),
		WorkingDir: container.WorkingDir,
	}
	if err := applySecurity(&translated, container.SecurityContext, podContext); err != nil {
		return Container{}, err
	}
	for _, variable := range container.Env {
		value, present, err := resolveEnv(variable, cluster)
		if err != nil {
			return Container{}, err
		}
		if present {
			translated.Env = append(translated.Env, EnvVar{Name: variable.Name, Value: value, Secret: variable.ValueFrom != nil})
		}
	}
	for _, mount := range container.VolumeMounts {
		if mount.SubPath != "" || mount.SubPathExpr != "" || mount.MountPropagation != nil {
			return Container{}, fmt.Errorf("the mount of %s uses a subpath or propagation", mount.Name)
		}
		translated.Mounts = append(translated.Mounts, Mount{Volume: mount.Name, Path: mount.MountPath, ReadOnly: mount.ReadOnly})
	}
	return translated, nil
}

// applySecurity reads the container's security context into the flags that give
// it, and refuses a context that any of them would misstate.
func applySecurity(out *Container, context *corev1.SecurityContext, pod *corev1.PodSecurityContext) error {
	if context == nil {
		return errors.New("it has no security context")
	}
	switch {
	case context.Privileged != nil && *context.Privileged:
		return errors.New("it is privileged")
	case context.AllowPrivilegeEscalation == nil || *context.AllowPrivilegeEscalation:
		return errors.New("it allows privilege escalation")
	case context.ReadOnlyRootFilesystem == nil || !*context.ReadOnlyRootFilesystem:
		return errors.New("its root file system is writable")
	case context.RunAsUser == nil || context.RunAsGroup == nil:
		return errors.New("it does not name its user and group")
	case context.RunAsNonRoot == nil || !*context.RunAsNonRoot || *context.RunAsUser == 0:
		return errors.New("it may run as root")
	case context.Capabilities == nil || len(context.Capabilities.Add) != 0 ||
		!slices.Equal(context.Capabilities.Drop, []corev1.Capability{"ALL"}):
		return errors.New("it does not drop every capability and add none")
	case context.SeccompProfile == nil || context.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		return errors.New("its seccomp profile is not the runtime default")
	case context.SELinuxOptions != nil || context.AppArmorProfile != nil || context.ProcMount != nil:
		return errors.New("it sets a security option the harness does not reproduce")
	case pod != nil && (pod.RunAsUser == nil || pod.RunAsGroup == nil ||
		*pod.RunAsUser != *context.RunAsUser || *pod.RunAsGroup != *context.RunAsGroup):
		return errors.New("its user is not the Pod's")
	case pod != nil && len(pod.SupplementalGroups) != 0:
		return errors.New("the Pod adds supplemental groups")
	}
	out.User = strconv.FormatInt(*context.RunAsUser, 10) + ":" + strconv.FormatInt(*context.RunAsGroup, 10)
	out.ReadOnlyRoot = true
	out.NoNewPrivilege = true
	out.DropCapability = []string{"ALL"}
	return nil
}

func resolveEnv(variable corev1.EnvVar, cluster Cluster) (string, bool, error) {
	if variable.ValueFrom == nil {
		return variable.Value, true, nil
	}
	selector := variable.ValueFrom.SecretKeyRef
	if selector == nil {
		return "", false, fmt.Errorf("%s reads a source other than a Secret", variable.Name)
	}
	if data, present := cluster.Secrets[selector.Name][selector.Key]; present {
		return string(data), true, nil
	}
	if selector.Optional != nil && *selector.Optional {
		return "", false, nil
	}
	return "", false, fmt.Errorf("%s reads %s/%s, which the row did not provide", variable.Name, selector.Name, selector.Key)
}

func translateVolume(volume corev1.Volume, cluster Cluster, group int) (Volume, error) {
	out := Volume{Name: volume.Name, Group: group}
	source := volume.VolumeSource
	switch {
	case source.EmptyDir != nil:
		out.Empty = true
		if source.EmptyDir.Medium != corev1.StorageMediumMemory {
			return Volume{}, fmt.Errorf("an emptyDir on medium %q", source.EmptyDir.Medium)
		}
		if source.EmptyDir.SizeLimit != nil {
			out.SizeBytes = source.EmptyDir.SizeLimit.Value()
		}
	case source.Secret != nil:
		// An optional Secret the row did not provide is an empty volume, and
		// an optional key it lacks is a file the kubelet does not write.
		optional := source.Secret.Optional != nil && *source.Secret.Optional
		data, ok := cluster.Secrets[source.Secret.SecretName]
		if !ok && !optional {
			return Volume{}, fmt.Errorf("the row did not provide the Secret %s", source.Secret.SecretName)
		}
		if !ok {
			break
		}
		files, err := project(volume.Name, data, source.Secret.Items, source.Secret.DefaultMode, group, optional)
		if err != nil {
			return Volume{}, err
		}
		out.Files = files
	case source.ConfigMap != nil:
		data, ok := cluster.ConfigMaps[source.ConfigMap.Name]
		if !ok {
			return Volume{}, fmt.Errorf("the row did not provide the ConfigMap %s", source.ConfigMap.Name)
		}
		files, err := project(volume.Name, data, source.ConfigMap.Items, source.ConfigMap.DefaultMode, group, false)
		if err != nil {
			return Volume{}, err
		}
		out.Files = files
	case source.Projected != nil:
		for _, projection := range source.Projected.Sources {
			if projection.ConfigMap == nil || projection.Secret != nil || projection.DownwardAPI != nil || projection.ServiceAccountToken != nil {
				return Volume{}, errors.New("a projection of something other than a ConfigMap")
			}
			data, ok := cluster.ConfigMaps[projection.ConfigMap.Name]
			if !ok {
				return Volume{}, fmt.Errorf("the row did not provide the ConfigMap %s", projection.ConfigMap.Name)
			}
			files, err := project(volume.Name, data, projection.ConfigMap.Items, source.Projected.DefaultMode, group, false)
			if err != nil {
				return Volume{}, err
			}
			out.Files = append(out.Files, files...)
		}
	default:
		return Volume{}, fmt.Errorf("a source the harness does not reproduce: %s", sourceKind(source))
	}
	return out, nil
}

func sourceKind(source corev1.VolumeSource) string {
	value := reflect.ValueOf(source)
	for index := range value.NumField() {
		if !value.Field(index).IsZero() {
			return value.Type().Field(index).Name
		}
	}
	return "none"
}

// project is what the kubelet writes for a Secret or a ConfigMap: each item at
// its path, or every key at its own name, owned by root and the Pod's group. A
// key an optional source lacks is left out.
func project(volume string, data map[string][]byte, items []corev1.KeyToPath, defaultMode *int32, group int, optional bool) ([]volumes.Entry, error) {
	if len(items) == 0 {
		for key := range data {
			items = append(items, corev1.KeyToPath{Key: key, Path: key})
		}
		slices.SortFunc(items, func(a, b corev1.KeyToPath) int { return strings.Compare(a.Path, b.Path) })
	}
	mode := int64(0o644)
	if defaultMode != nil {
		mode = int64(*defaultMode)
	}
	entries := make([]volumes.Entry, 0, len(items))
	for _, item := range items {
		content, present := data[item.Key]
		if !present && optional {
			continue
		}
		if !present {
			return nil, fmt.Errorf("the key %s is missing", item.Key)
		}
		entry := volumes.Entry{Volume: volume, Path: item.Path, Data: content, Mode: mode, GID: group}
		if item.Mode != nil {
			entry.Mode = int64(*item.Mode)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// RunArgs is the docker run command line for a container of the Pod. A Secret's
// value goes by name, so that it is on no command line: the caller sets it in the
// docker client's own environment (see SecretEnv).
//
// mounts resolves each of the container's mounts to its --mount or --tmpfs
// flags.
func (c Container) RunArgs(name, network string, mounts func(Mount) ([]string, error), extraArgs ...string) ([]string, error) {
	if len(c.Command) == 0 {
		return nil, errors.New("no command")
	}
	args := []string{"run", "--rm", "--pull", "never", "--name", name, "--network", network}
	if c.ReadOnlyRoot {
		args = append(args, "--read-only")
	}
	if c.User != "" {
		args = append(args, "--user", c.User)
	}
	for _, capability := range c.DropCapability {
		args = append(args, "--cap-drop", capability)
	}
	if c.NoNewPrivilege {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	if c.WorkingDir != "" {
		args = append(args, "--workdir", c.WorkingDir)
	}
	for _, mount := range c.Mounts {
		flags, err := mounts(mount)
		if err != nil {
			return nil, err
		}
		args = append(args, flags...)
	}
	for _, variable := range c.Env {
		if variable.Secret {
			args = append(args, "--env", variable.Name)
			continue
		}
		args = append(args, "--env", variable.Name+"="+variable.Value)
	}
	args = append(args, "--entrypoint", c.Command[0], c.Image)
	args = append(args, c.Command[1:]...)
	args = append(args, c.Args...)
	return append(args, extraArgs...), nil
}

// SecretEnv is what the docker client's environment has to hold for RunArgs.
func (c Container) SecretEnv() []string {
	var env []string
	for _, variable := range c.Env {
		if variable.Secret {
			env = append(env, variable.Name+"="+variable.Value)
		}
	}
	return env
}
