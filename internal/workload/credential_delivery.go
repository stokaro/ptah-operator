package workload

import (
	"path"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// A credential reaches an operation Pod as a file in a read-only Secret
// volume, and the container's environment holds only the file's path
// (runner.CredentialFiles). A value the kubelet resolves into a container's
// environment is written into the container runtime's record of that
// container, and stays there, readable by root on the node, until the Pod is
// removed: a failed Apply's Pod is kept a day. A Secret volume is a tmpfs the
// kubelet unmounts when the Pod stops.
//
// Every file is mode 0440. The Pod runs with fsGroup 65532, and the kubelet
// gives every file of a Secret volume to root and that group, with group read,
// whatever mode the Job declares: 0400 reads back as 0440. Owning the file by
// the operation user needs the volume user fields, which are alpha in every
// supported minor and which the Job write guard refuses.
const (
	databaseURLVolumeName            = "database-url"
	developmentDatabaseURLVolumeName = "development-database-url"
	registryCredentialsVolumeName    = "registry-credentials"

	databaseURLDirectory            = "/credentials/database"
	developmentDatabaseURLDirectory = "/credentials/development-database"
	registryCredentialsDirectory    = "/credentials/registry"

	// databaseURLFile is the name a database URL is projected under.
	databaseURLFile = "url"
	// credentialFileMode is the mode of every credential file.
	credentialFileMode int32 = 0o440
)

// ociFetchPrefix is what the runner is told before the pull an OCI fetch
// container runs: the runner reads the registry credential from its files and
// starts Ptah with it.
var ociFetchPrefix = []string{"--fetch-oci-source", "--ptah-binary", ptahBinaryPath, "--"}

// databaseCredential is the environment variable, volume and mount that give
// one container the database URL selector names, as a file.
//
// The selector's optional flag is carried to the volume. An optional key the
// Secret lacks then leaves no file, and the runner leaves the variable unset,
// as the kubelet did when it resolved an optional key into the environment.
func databaseCredential(fileVariable, volumeName, directory string, selector corev1.SecretKeySelector) (
	corev1.EnvVar, corev1.Volume, corev1.VolumeMount,
) {
	mode := credentialFileMode
	volume := corev1.Volume{Name: volumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: selector.Name,
		Items:      []corev1.KeyToPath{{Key: selector.Key, Path: databaseURLFile, Mode: &mode}},
		Optional:   copyBoolPointer(selector.Optional),
	}}}
	return literalEnv(fileVariable, directory+"/"+databaseURLFile),
		volume,
		corev1.VolumeMount{Name: volumeName, MountPath: directory, ReadOnly: true}
}

// credentialItem is one optional key of a Secret, projected under name.
type credentialItem struct {
	fileVariable, key, name string
}

// optionalCredentials is a Secret volume of keys any of which the Secret may
// lack, with the variables that name each file. A Secret that lacks one gives
// no file, and the runner leaves the credential unset, as the kubelet did when
// it resolved each key into the environment as optional. The Secret itself may
// be missing on the same terms; whatever grant the Job reads from it is a
// required environment reference, which the kubelet refuses on its own.
func optionalCredentials(volumeName, directory, secret string, items []credentialItem) (
	[]corev1.EnvVar, corev1.Volume, corev1.VolumeMount,
) {
	optional := true
	environment := make([]corev1.EnvVar, 0, len(items))
	projected := make([]corev1.KeyToPath, 0, len(items))
	for _, item := range items {
		mode := credentialFileMode
		projected = append(projected, corev1.KeyToPath{Key: item.key, Path: item.name, Mode: &mode})
		environment = append(environment, literalEnv(item.fileVariable, directory+"/"+item.name))
	}
	return environment,
		corev1.Volume{Name: volumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secret,
			Items:      projected,
			Optional:   &optional,
		}}},
		corev1.VolumeMount{Name: volumeName, MountPath: directory, ReadOnly: true}
}

func copyBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// CarryCredentialDelivery rewrites an expected Job into the shape a release
// before file delivery built, when live has that shape, and reports whether it
// did. It is CarryManagerIdentity's counterpart for the one other difference
// between a Job 0.2.10 dispatched and the Job this release rebuilds from the
// same claim: where each credential sits.
//
// A Job 0.2.10 built reads every credential from a secretKeyRef in its
// container's environment and runs the OCI fetch as Ptah itself. This release
// builds the same Job with each credential in a Secret volume, a variable
// naming the file, and the runner in front of the OCI fetch. The rewrite puts
// back the reference each file came from -- the same Secret, key and optional
// flag -- removes the volumes and mounts that only carried those files, and
// gives the fetch container back to Ptah. Nothing else is touched, so any
// other difference still fails the comparison that follows.
//
// What that comparison accepts is still pinned to the claim: the live Pod
// template has to digest to what the claim's admission snapshot recorded, and
// this release records the digest of the file-delivery template. A Job in the
// old shape therefore matches only a claim a release that built that shape
// made, which is the Job an upgrade finds running.
//
// The callers are the comparisons that hold a live Job to its claim after a
// restart or an upgrade. A Job this manager submits is never rewritten: it
// has to be the shape this release builds.
func CarryCredentialDelivery(expected, live *batchv1.Job) bool {
	if expected == nil || live == nil || !environmentDelivery(live) || environmentDelivery(expected) {
		return false
	}
	template := &expected.Spec.Template.Spec
	released := map[string][]string{}
	for _, containers := range [][]corev1.Container{template.InitContainers, template.Containers} {
		for index := range containers {
			container := &containers[index]
			if rewriteCredentialFiles(container, template.Volumes, released) {
				sortEnvironment(container.Env)
			}
		}
	}
	for index := range template.InitContainers {
		if container := &template.InitContainers[index]; wrapsOCIFetch(*container) {
			unwrapOCIFetch(container)
		}
	}
	releaseCredentialItems(template, released)
	return true
}

// environmentDelivery is a Job that names no credential by file and runs no
// OCI fetch behind the runner: the shape a release before file delivery built.
func environmentDelivery(job *batchv1.Job) bool {
	template := job.Spec.Template.Spec
	for _, container := range slices.Concat(template.InitContainers, template.Containers) {
		for _, variable := range container.Env {
			if slices.ContainsFunc(runner.CredentialFiles(), func(credential runner.CredentialFile) bool {
				return credential.File == variable.Name
			}) {
				return false
			}
		}
		if wrapsOCIFetch(container) {
			return false
		}
	}
	return true
}

// wrapsOCIFetch is a container that runs an OCI pull behind the runner.
func wrapsOCIFetch(container corev1.Container) bool {
	return slices.Equal(container.Command, []string{runnerPath}) &&
		len(container.Args) >= len(ociFetchPrefix) && slices.Equal(container.Args[:len(ociFetchPrefix)], ociFetchPrefix)
}

// rewriteCredentialFiles replaces each variable of container that names a
// credential file with the secretKeyRef the file was projected from, and
// records the volume item it came from in released. It reports whether it
// replaced any.
func rewriteCredentialFiles(container *corev1.Container, volumes []corev1.Volume, released map[string][]string) bool {
	rewrote := false
	for index, variable := range container.Env {
		credential, named := credentialNamedBy(variable.Name)
		if !named {
			continue
		}
		directory, file := path.Split(variable.Value)
		directory = strings.TrimSuffix(directory, "/")
		mount := slices.IndexFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
			return mount.MountPath == directory
		})
		if mount < 0 {
			continue
		}
		volumeName := container.VolumeMounts[mount].Name
		volume := slices.IndexFunc(volumes, func(volume corev1.Volume) bool { return volume.Name == volumeName })
		if volume < 0 || volumes[volume].Secret == nil {
			continue
		}
		source := volumes[volume].Secret
		item := slices.IndexFunc(source.Items, func(item corev1.KeyToPath) bool { return item.Path == file })
		if item < 0 {
			continue
		}
		container.Env[index] = corev1.EnvVar{Name: credential.Value, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: source.SecretName},
			Key:                  source.Items[item].Key,
			Optional:             copyBoolPointer(source.Optional),
		}}}
		released[volumeName] = append(released[volumeName], file)
		rewrote = true
	}
	return rewrote
}

func credentialNamedBy(name string) (runner.CredentialFile, bool) {
	for _, credential := range runner.CredentialFiles() {
		if credential.File == name {
			return credential, true
		}
	}
	return runner.CredentialFile{}, false
}

// unwrapOCIFetch gives an OCI fetch container back to Ptah: the pull without
// the runner in front of it, and without the runner's mount and protocol.
func unwrapOCIFetch(container *corev1.Container) {
	container.Command = []string{ptahBinaryPath}
	container.Args = slices.Clone(container.Args[len(ociFetchPrefix):])
	container.VolumeMounts = nilWhenEmpty(slices.DeleteFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == runnerVolumeName
	}))
	container.Env = nilWhenEmpty(slices.DeleteFunc(container.Env, func(variable corev1.EnvVar) bool {
		return variable.Name == runner.EnvRunnerProtocolVersion
	}))
}

// releaseCredentialItems removes the items the rewrite put back into the
// environment, and every volume left with none, with its mounts.
func releaseCredentialItems(template *corev1.PodSpec, released map[string][]string) {
	emptied := map[string]bool{}
	for index := range template.Volumes {
		volume := &template.Volumes[index]
		files, ok := released[volume.Name]
		if !ok || volume.Secret == nil {
			continue
		}
		volume.Secret.Items = slices.DeleteFunc(volume.Secret.Items, func(item corev1.KeyToPath) bool {
			return slices.Contains(files, item.Path)
		})
		if len(volume.Secret.Items) == 0 {
			emptied[volume.Name] = true
		}
	}
	template.Volumes = nilWhenEmpty(slices.DeleteFunc(template.Volumes, func(volume corev1.Volume) bool { return emptied[volume.Name] }))
	for _, containers := range [][]corev1.Container{template.InitContainers, template.Containers} {
		for index := range containers {
			containers[index].VolumeMounts = nilWhenEmpty(slices.DeleteFunc(containers[index].VolumeMounts, func(mount corev1.VolumeMount) bool {
				return emptied[mount.Name]
			}))
		}
	}
}

// nilWhenEmpty is what an empty list reads back as from the API server, which
// omits it: the comparison that follows tells nil from empty.
func nilWhenEmpty[S ~[]E, E any](list S) S {
	if len(list) == 0 {
		return nil
	}
	return list
}
