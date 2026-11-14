package workload

import (
	"errors"
	"net/url"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// An HTTPS source reaches an operation through one container that holds its
// credential: the Resolve Job's operation container, or the fetch container of
// every later operation. Nothing else in the Pod mounts or reads it.
const (
	httpsFetchContainerName    = "fetch-https-source"
	httpsCredentialsVolumeName = "https-credentials"
	httpsCAVolumeName          = "https-ca"

	httpsCredentialsDirectory = "/credentials/https"
	httpsUsernameFile         = "username"
	httpsPasswordFile         = "password"
	httpsTokenFile            = "token"
	httpsCADirectory          = "/credentials/https-ca"
	httpsCAPath               = httpsCADirectory + "/ca.pem"
)

// httpsRequest is what one HTTPS access is told: the URL, the download's
// digest, its format and the selected path; for a fetch, the content it must
// give; and the selectors of the credential and the trust.
type httpsRequest struct {
	url, digest, path string
	format            operatorv1alpha1.HTTPSFormat
	contentDigest     string
	schema            bool
	authFrom          *operatorv1alpha1.HTTPSAuthSource
	transport         operatorv1alpha1.HTTPSTransportSpec
}

func httpsResolveRequest(spec operatorv1alpha1.HTTPSSourceSpec, schema bool) httpsRequest {
	return httpsRequest{
		url: spec.URL, digest: spec.Digest, format: spec.Format, path: spec.Path, schema: schema,
		authFrom: spec.AuthFrom, transport: spec.Transport,
	}
}

func httpsFetchRequest(binding operatorv1alpha1.HTTPSSourceBinding, schema bool) httpsRequest {
	return httpsRequest{
		url: binding.URL, digest: binding.Digest, format: binding.Format, path: binding.Path,
		contentDigest: binding.ContentDigest, schema: schema, authFrom: binding.AuthFrom, transport: binding.Transport,
	}
}

// addHTTPSAccess adds an HTTPS source's coordinates, its credential and its
// trust to one container's environment, volumes and mounts. The credential
// Secret's fixed keys grant the authority and the CA: the runner refuses a
// URL or a CA the Secret's owner did not name, before Ptah runs. The
// credential is a file in a Secret volume; only the grants are read into the
// environment.
func addHTTPSAccess(
	request httpsRequest,
	environment []corev1.EnvVar,
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
	parsed, err := url.Parse(request.url)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, nil, nil, errors.New("an HTTPS source needs an https:// download")
	}
	selects := gitSelectsDirectory
	if request.schema {
		selects = gitSelectsFile
	}
	environment = append(environment,
		literalEnv(runner.EnvHTTPSURL, request.url),
		literalEnv(runner.EnvHTTPSDigest, request.digest),
		literalEnv(runner.EnvHTTPSFormat, string(request.format)),
		literalEnv(runner.EnvHTTPSPath, request.path),
		literalEnv(runner.EnvHTTPSSelects, selects),
	)
	if request.contentDigest != "" {
		environment = append(environment, literalEnv(runner.EnvExpectedContentDigest, request.contentDigest))
	}
	if auth := request.authFrom; auth != nil {
		if strings.TrimSpace(auth.Name) == "" {
			return nil, nil, nil, errors.New("the HTTPS credential Secret name is required")
		}
		environment = append(environment, requiredSecretEnv(runner.EnvHTTPSHostGrant, auth.Name, operatorv1alpha1.HTTPSHostSecretKey))
		variables, volume, mount := optionalCredentials(httpsCredentialsVolumeName, httpsCredentialsDirectory, auth.Name, []credentialItem{
			{fileVariable: runner.EnvHTTPSUsernameFile, key: operatorv1alpha1.HTTPSUsernameSecretKey, name: httpsUsernameFile},
			{fileVariable: runner.EnvHTTPSPasswordFile, key: operatorv1alpha1.HTTPSPasswordSecretKey, name: httpsPasswordFile},
			{fileVariable: runner.EnvHTTPSTokenFile, key: operatorv1alpha1.HTTPSTokenSecretKey, name: httpsTokenFile},
		})
		environment = append(environment, variables...)
		volumes = append(volumes, volume)
		mounts = append(mounts, mount)
		environment = append(environment, caGrantEnv(
			runner.EnvHTTPSCASHA256Grant, auth.Name, operatorv1alpha1.HTTPSCASHA256SecretKey, request.transport.CAFrom != nil))
	}
	if selector := request.transport.CAFrom; selector != nil {
		if selector.Name == "" || selector.Key == "" || (selector.Optional != nil && *selector.Optional) {
			return nil, nil, nil, errors.New("an HTTPS source's trust names a required ConfigMap key")
		}
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: httpsCAVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: selector.LocalObjectReference,
				Items:                []corev1.KeyToPath{{Key: selector.Key, Path: path.Base(httpsCAPath), Mode: &mode}},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: httpsCAVolumeName, MountPath: httpsCADirectory, ReadOnly: true})
		environment = append(environment, literalEnv(runner.EnvHTTPSCASourceFile, httpsCAPath))
	}
	return environment, volumes, mounts, nil
}

// httpsFetch builds the init container that delivers a resolved HTTPS
// download to the source volume: the runner, holding the credential,
// downloads it through Ptah held to the download digest and the bound content
// digest, and copies the files only if they give that digest again. The
// operation container that follows holds the database URL and never the
// credential.
func (b Builder) httpsFetch(
	binding operatorv1alpha1.HTTPSSourceBinding,
	schema bool,
	destination string,
	resources corev1.ResourceRequirements,
) (corev1.Container, []corev1.Volume, error) {
	if err := sourcebinding.ValidateHTTPS(binding); err != nil {
		return corev1.Container{}, nil, err
	}
	return b.remoteFetch(httpsFetchContainerName, "--fetch-https-source", destination, resources,
		func(environment []corev1.EnvVar, volumes []corev1.Volume, mounts []corev1.VolumeMount) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
			return addHTTPSAccess(httpsFetchRequest(binding, schema), environment, volumes, mounts)
		})
}

// httpsSchemaFile is where an operation reads an HTTPS schema file after the
// fetch: under the destination, by the file's own name, whose extension says
// its format.
func httpsSchemaFile(destination string, binding operatorv1alpha1.HTTPSSourceBinding) string {
	return destination + "/" + path.Base(binding.Path)
}
