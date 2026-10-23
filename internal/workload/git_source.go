package workload

import (
	"errors"
	"net/url"
	"path"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// A Git source reaches an operation through one container that holds its
// credential: the Resolve Job's operation container, or the fetch container of
// every later operation. Nothing else in the Pod mounts or reads it.
const (
	gitFetchContainerName    = "fetch-git-source"
	gitCredentialsVolumeName = "git-credentials"
	gitCAVolumeName          = "git-ca"
	gitKnownHostsVolumeName  = "git-known-hosts"

	gitSSHKeyDirectory     = "/credentials/git"
	gitSSHKeyPath          = gitSSHKeyDirectory + "/ssh-key"
	gitCADirectory         = "/credentials/git-ca"
	gitCAPath              = gitCADirectory + "/ca.pem"
	gitKnownHostsDirectory = "/credentials/git-known-hosts"
	gitKnownHostsPath      = gitKnownHostsDirectory + "/known_hosts"

	gitSelectsFile      = "file"
	gitSelectsDirectory = "directory"
)

// gitRequest is what one Git access is told: the repository, the ref and the
// path; for a fetch, the commit and the content it must give; and the
// selectors of the credential and the trust.
type gitRequest struct {
	url, path     string
	ref           operatorv1alpha1.GitRef
	commit        string
	contentDigest string
	schema        bool
	authFrom      *operatorv1alpha1.GitAuthSource
	transport     operatorv1alpha1.GitTransportSpec
}

func gitResolveRequest(spec operatorv1alpha1.GitSourceSpec, schema bool) gitRequest {
	return gitRequest{
		url: spec.URL, path: spec.Path, ref: spec.Ref, schema: schema,
		authFrom: spec.AuthFrom, transport: spec.Transport,
	}
}

func gitFetchRequest(binding operatorv1alpha1.GitSourceBinding, schema bool) gitRequest {
	return gitRequest{
		url: binding.URL, path: binding.Path, ref: binding.Ref, commit: binding.Commit,
		contentDigest: binding.ContentDigest, schema: schema, authFrom: binding.AuthFrom, transport: binding.Transport,
	}
}

// addGitAccess adds a Git source's coordinates, its credential and its trust
// to one container's environment, volumes and mounts. The credential Secret's
// fixed keys grant the authority and the trust: the runner refuses a URL or a
// CA the Secret's owner did not name, before Ptah runs.
func addGitAccess(
	request gitRequest,
	environment []corev1.EnvVar,
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
	parsed, err := url.Parse(request.url)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" {
		return nil, nil, nil, errors.New("a Git source needs an https:// or ssh:// repository")
	}
	ssh := parsed.Scheme == "ssh"
	selects := gitSelectsDirectory
	if request.schema {
		selects = gitSelectsFile
	}
	environment = append(environment,
		literalEnv(runner.EnvGitURL, request.url),
		literalEnv(runner.EnvGitPath, request.path),
		literalEnv(runner.EnvGitSelects, selects),
	)
	for _, ref := range []struct{ name, value string }{
		{runner.EnvGitBranch, request.ref.Branch}, {runner.EnvGitTag, request.ref.Tag},
	} {
		if ref.value != "" {
			environment = append(environment, literalEnv(ref.name, ref.value))
		}
	}
	commit := request.commit
	if commit == "" {
		commit = request.ref.Commit
	}
	if commit != "" {
		environment = append(environment, literalEnv(runner.EnvGitCommit, commit))
	}
	if request.contentDigest != "" {
		environment = append(environment, literalEnv(runner.EnvExpectedContentDigest, request.contentDigest))
	}

	mode := int32(0o440)
	if auth := request.authFrom; auth != nil {
		if strings.TrimSpace(auth.Name) == "" {
			return nil, nil, nil, errors.New("the Git credential Secret name is required")
		}
		environment = append(environment, requiredSecretEnv(runner.EnvGitHostGrant, auth.Name, operatorv1alpha1.GitHostSecretKey))
		if ssh {
			volumes = append(volumes, corev1.Volume{
				Name: gitCredentialsVolumeName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: auth.Name,
					Items: []corev1.KeyToPath{{
						Key: operatorv1alpha1.GitSSHPrivateKeySecretKey, Path: path.Base(gitSSHKeyPath), Mode: &mode,
					}},
				}},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: gitCredentialsVolumeName, MountPath: gitSSHKeyDirectory, ReadOnly: true})
			environment = append(environment, literalEnv(runner.EnvGitSSHKeyFile, gitSSHKeyPath))
		} else {
			environment = append(environment,
				optionalSecretEnv(runner.EnvGitUsername, auth.Name, operatorv1alpha1.GitUsernameSecretKey),
				optionalSecretEnv(runner.EnvGitPassword, auth.Name, operatorv1alpha1.GitPasswordSecretKey),
			)
		}
		if request.transport.CAFrom != nil {
			environment = append(environment,
				requiredSecretEnv(runner.EnvGitCASHA256Grant, auth.Name, operatorv1alpha1.GitCASHA256SecretKey))
		}
		if request.transport.KnownHostsFrom != nil {
			environment = append(environment,
				requiredSecretEnv(runner.EnvGitKnownHostsSHA256Grant, auth.Name, operatorv1alpha1.GitKnownHostsSHA256SecretKey))
		}
	}

	switch {
	case ssh && request.transport.KnownHostsFrom == nil:
		return nil, nil, nil, errors.New("an ssh:// Git source needs known hosts")
	case ssh && request.transport.CAFrom != nil, !ssh && request.transport.KnownHostsFrom != nil:
		return nil, nil, nil, errors.New("a Git source's trust does not fit its scheme")
	}
	for _, trust := range []struct {
		selector         *corev1.ConfigMapKeySelector
		volume, dir, env string
		file             string
	}{
		{request.transport.CAFrom, gitCAVolumeName, gitCADirectory, runner.EnvGitCASourceFile, gitCAPath},
		{request.transport.KnownHostsFrom, gitKnownHostsVolumeName, gitKnownHostsDirectory, runner.EnvGitKnownHostsSourceFile, gitKnownHostsPath},
	} {
		if trust.selector == nil {
			continue
		}
		if trust.selector.Name == "" || trust.selector.Key == "" || (trust.selector.Optional != nil && *trust.selector.Optional) {
			return nil, nil, nil, errors.New("a Git source's trust names a required ConfigMap key")
		}
		volumes = append(volumes, corev1.Volume{
			Name: trust.volume,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: trust.selector.LocalObjectReference,
				Items:                []corev1.KeyToPath{{Key: trust.selector.Key, Path: path.Base(trust.file), Mode: &mode}},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: trust.volume, MountPath: trust.dir, ReadOnly: true})
		environment = append(environment, literalEnv(trust.env, trust.file))
	}
	return environment, volumes, mounts, nil
}

// gitFetch builds the init container that delivers a resolved Git commit to
// the source volume: the runner, holding the credential, pulls the commit
// through Ptah, and copies the files only if they give the bound content
// digest. The operation container that follows holds the database URL and
// never the credential.
func (b Builder) gitFetch(
	binding operatorv1alpha1.GitSourceBinding,
	schema bool,
	destination string,
	resources corev1.ResourceRequirements,
) (corev1.Container, []corev1.Volume, error) {
	if err := sourcebinding.ValidateGit(binding); err != nil {
		return corev1.Container{}, nil, err
	}
	environment := []corev1.EnvVar{
		literalEnv("HOME", fetchWorkPath),
		literalEnv("TMPDIR", fetchWorkPath),
		runnerProtocolEnv(),
	}
	volumes := []corev1.Volume{
		{Name: sourceVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(sourceVolumeBytes)}},
		{Name: fetchWorkVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(fetchWorkVolumeBytes)}},
	}
	mounts := []corev1.VolumeMount{
		{Name: runnerVolumeName, MountPath: "/runner", ReadOnly: true},
		{Name: sourceVolumeName, MountPath: sourcePath},
		{Name: fetchWorkVolumeName, MountPath: fetchWorkPath},
	}
	environment, volumes, mounts, err := addGitAccess(gitFetchRequest(binding, schema), environment, volumes, mounts)
	if err != nil {
		return corev1.Container{}, nil, err
	}
	sortEnvironment(environment)
	return corev1.Container{
		Name:            gitFetchContainerName,
		Image:           b.ExecutorImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{runnerPath},
		Args:            []string{"--fetch-git-source", destination, "--ptah-binary", ptahBinaryPath},
		WorkingDir:      fetchWorkPath,
		Env:             environment,
		Resources:       resources,
		SecurityContext: hardenedContainerContext(),
		VolumeMounts:    mounts,
		// The refusal a person has to read: the runner names the commit and
		// digest it was held to, and Ptah's own reason, redacted, never a
		// credential.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}, volumes, nil
}

// gitSchemaFile is where an operation reads a Git schema file after the
// fetch: under the destination, by the file's own name, whose extension says
// its format.
func gitSchemaFile(destination string, binding operatorv1alpha1.GitSourceBinding) string {
	return destination + "/" + path.Base(binding.Path)
}

func sortEnvironment(environment []corev1.EnvVar) {
	sort.Slice(environment, func(left, right int) bool { return environment[left].Name < environment[right].Name })
}
