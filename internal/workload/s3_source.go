package workload

import (
	"errors"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// An S3 source reaches an operation through one container that holds its
// key pair: the Resolve Job's operation container, or the fetch container of
// every later operation. Nothing else in the Pod mounts or reads it.
const (
	s3FetchContainerName    = "fetch-s3-source"
	s3CredentialsVolumeName = "s3-credentials"
	s3CAVolumeName          = "s3-ca"

	s3CredentialsDirectory = "/credentials/s3"
	s3AccessKeyIDFile      = "access-key-id"
	s3SecretAccessKeyFile  = "secret-access-key"
	s3SessionTokenFile     = "session-token"
	s3CADirectory          = "/credentials/s3-ca"
	s3CAPath               = s3CADirectory + "/ca.pem"
)

// s3Request is what one S3 access is told: the object, its digest, its
// format and the selected path; for a fetch, the version and the content
// the resolution bound; and the selectors of the credential and the trust.
type s3Request struct {
	endpoint, region, bucket, key, versionID string
	digest, path                             string
	format                                   operatorv1alpha1.HTTPSFormat
	contentDigest                            string
	schema                                   bool
	authFrom                                 *operatorv1alpha1.S3AuthSource
	transport                                operatorv1alpha1.S3TransportSpec
}

// s3ResolveRequest reads the version the spec names, or the current one.
func s3ResolveRequest(spec operatorv1alpha1.S3SourceSpec, schema bool) s3Request {
	return s3Request{
		endpoint: spec.Endpoint, region: spec.Region, bucket: spec.Bucket, key: spec.Key, versionID: spec.VersionID,
		digest: spec.Digest, format: spec.Format, path: spec.Path, schema: schema,
		authFrom: spec.AuthFrom, transport: spec.Transport,
	}
}

// s3FetchRequest reads the version the resolution bound, so an object
// overwritten since is read as it was resolved, where the bucket keeps it.
func s3FetchRequest(binding operatorv1alpha1.S3SourceBinding, schema bool) s3Request {
	return s3Request{
		endpoint: binding.Endpoint, region: binding.Region, bucket: binding.Bucket, key: binding.Key,
		versionID: binding.VersionID, digest: binding.Digest, format: binding.Format, path: binding.Path,
		contentDigest: binding.ContentDigest, schema: schema, authFrom: binding.AuthFrom, transport: binding.Transport,
	}
}

// addS3Access adds an S3 source's coordinates, its key pair and its trust to
// one container's environment, volumes and mounts. The credential Secret's
// fixed keys grant the endpoint, the bucket and the CA: the runner refuses an
// endpoint, a bucket or a CA the Secret's owner did not name, before it sends
// a request. The key pair is a file in a Secret volume; only the grants are
// read into the environment.
func addS3Access(
	request s3Request,
	environment []corev1.EnvVar,
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
	if !strings.HasPrefix(request.endpoint, "https://") || request.bucket == "" || request.key == "" || request.region == "" {
		return nil, nil, nil, errors.New("an S3 source needs an https:// endpoint, a region, a bucket and a key")
	}
	selects := gitSelectsDirectory
	if request.schema {
		selects = gitSelectsFile
	}
	environment = append(environment,
		literalEnv(runner.EnvS3Endpoint, request.endpoint),
		literalEnv(runner.EnvS3Region, request.region),
		literalEnv(runner.EnvS3Bucket, request.bucket),
		literalEnv(runner.EnvS3Key, request.key),
		literalEnv(runner.EnvS3Digest, request.digest),
		literalEnv(runner.EnvS3Format, string(request.format)),
		literalEnv(runner.EnvS3Path, request.path),
		literalEnv(runner.EnvS3Selects, selects),
	)
	if request.versionID != "" {
		environment = append(environment, literalEnv(runner.EnvS3VersionID, request.versionID))
	}
	if request.contentDigest != "" {
		environment = append(environment, literalEnv(runner.EnvExpectedContentDigest, request.contentDigest))
	}
	if auth := request.authFrom; auth != nil {
		if strings.TrimSpace(auth.Name) == "" {
			return nil, nil, nil, errors.New("the S3 credential Secret name is required")
		}
		environment = append(environment,
			requiredSecretEnv(runner.EnvS3HostGrant, auth.Name, operatorv1alpha1.S3HostSecretKey),
			optionalSecretEnv(runner.EnvS3BucketGrant, auth.Name, operatorv1alpha1.S3BucketSecretKey),
		)
		variables, volume, mount := optionalCredentials(s3CredentialsVolumeName, s3CredentialsDirectory, auth.Name, []credentialItem{
			{fileVariable: runner.EnvS3AccessKeyIDFile, key: operatorv1alpha1.S3AccessKeyIDSecretKey, name: s3AccessKeyIDFile},
			{fileVariable: runner.EnvS3SecretAccessKeyFile, key: operatorv1alpha1.S3SecretAccessKeySecretKey, name: s3SecretAccessKeyFile},
			{fileVariable: runner.EnvS3SessionTokenFile, key: operatorv1alpha1.S3SessionTokenSecretKey, name: s3SessionTokenFile},
		})
		environment = append(environment, variables...)
		volumes = append(volumes, volume)
		mounts = append(mounts, mount)
		environment = append(environment, caGrantEnv(
			runner.EnvS3CASHA256Grant, auth.Name, operatorv1alpha1.S3CASHA256SecretKey, request.transport.CAFrom != nil))
	}
	if selector := request.transport.CAFrom; selector != nil {
		if selector.Name == "" || selector.Key == "" || (selector.Optional != nil && *selector.Optional) {
			return nil, nil, nil, errors.New("an S3 source's trust names a required ConfigMap key")
		}
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: s3CAVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: selector.LocalObjectReference,
				Items:                []corev1.KeyToPath{{Key: selector.Key, Path: path.Base(s3CAPath), Mode: &mode}},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: s3CAVolumeName, MountPath: s3CADirectory, ReadOnly: true})
		environment = append(environment, literalEnv(runner.EnvS3CASourceFile, s3CAPath))
	}
	return environment, volumes, mounts, nil
}

// s3Fetch builds the init container that delivers a resolved S3 object
// version to the source volume: the runner, holding the key pair, reads the
// object held to the object digest and the bound content digest, and writes
// the files only if they give that digest. The operation container that
// follows holds the database URL and never the key pair.
func (b Builder) s3Fetch(
	binding operatorv1alpha1.S3SourceBinding,
	schema bool,
	destination string,
	resources corev1.ResourceRequirements,
) (corev1.Container, []corev1.Volume, error) {
	if err := sourcebinding.ValidateS3(binding); err != nil {
		return corev1.Container{}, nil, err
	}
	return b.remoteFetch(s3FetchContainerName, "--fetch-s3-source", destination, resources,
		func(environment []corev1.EnvVar, volumes []corev1.Volume, mounts []corev1.VolumeMount) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
			return addS3Access(s3FetchRequest(binding, schema), environment, volumes, mounts)
		})
}

// s3SchemaFile is where an operation reads an S3 schema file after the
// fetch: under the destination, by the file's own name, whose extension says
// its format.
func s3SchemaFile(destination string, binding operatorv1alpha1.S3SourceBinding) string {
	return destination + "/" + path.Base(binding.Path)
}
