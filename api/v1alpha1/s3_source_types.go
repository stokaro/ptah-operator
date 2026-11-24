package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// The fixed keys of an S3 credential Secret. S3AccessKeyIDSecretKey and
// S3SecretAccessKeySecretKey are the key pair a GetObject request is signed
// with, and S3SessionTokenSecretKey the token a temporary key pair carries.
// S3HostSecretKey is the endpoint authority, host[:port], the owner grants
// the key for; S3BucketSecretKey, when present, the only bucket it may sign
// for; S3CASHA256SecretKey the "sha256:" digest of the CA it may be sent
// under. A resource's author selects the Secret, never the keys.
const (
	S3AccessKeyIDSecretKey     = "accessKeyID"
	S3SecretAccessKeySecretKey = "secretAccessKey" // #nosec G101 -- the name of a Secret key, not a credential
	S3SessionTokenSecretKey    = "sessionToken"    // #nosec G101 -- the name of a Secret key, not a credential
	S3HostSecretKey            = "host"
	S3BucketSecretKey          = "bucket"
	S3CASHA256SecretKey        = "caSHA256"
)

// S3SourceSpec selects a file or a directory of one object in an
// S3-compatible bucket, pinned by the SHA-256 of the bytes GetObject returns.
// The request is path-style, https://<endpoint>/<bucket>/<key>.
// +kubebuilder:validation:XValidation:rule="self.format != 'File' || (self.path != '.' && !self.path.contains('/'))",message="with format File, path is the one name the object is given"
// +kubebuilder:validation:XValidation:rule="bytes(self.key).size() <= 1024",message="key holds at most 1024 bytes"
type S3SourceSpec struct {
	// Endpoint is the S3 API's address: https:// and an authority, with no
	// path, credential, query or fragment. The host is ASCII letters, digits,
	// dots and hyphens, or an IPv6 address in brackets, with an optional
	// port: write an internationalized name in its xn-- form.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=267
	// +kubebuilder:validation:Pattern=`^https://([A-Za-z0-9.-]+|\[[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*\])(:[0-9]{1,5})?$`
	Endpoint string `json:"endpoint"`

	// Region is the region a request is signed for. It names nothing else:
	// the endpoint says where the request goes. MinIO's default is
	// us-east-1, and R2 and Google Cloud Storage take auto.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Region string `json:"region"`

	// Bucket is the bucket's name.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`
	Bucket string `json:"bucket"`

	// Key is the object's key: at most 1024 bytes, with no leading "/", no
	// empty, "." or ".." element and no control character.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(?:[^./\x00-\x1f\x7f][^/\x00-\x1f\x7f]*|\.[^./\x00-\x1f\x7f][^/\x00-\x1f\x7f]*|\.\.[^/\x00-\x1f\x7f]+)(?:/(?:[^./\x00-\x1f\x7f][^/\x00-\x1f\x7f]*|\.[^./\x00-\x1f\x7f][^/\x00-\x1f\x7f]*|\.\.[^/\x00-\x1f\x7f]+))*$`
	Key string `json:"key"`

	// VersionID selects one version of the object in a versioned bucket.
	// Without it, a resolution reads the current version and records the
	// version the server says it served, and every later operation reads
	// that version. It is an upstream revision, not an identity: the digest
	// still decides.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[!-~]+$`
	VersionID string `json:"versionId,omitempty"`

	// Digest is "sha256:" and the SHA-256 of the exact bytes GetObject
	// returns, the number sha256sum prints for the uploaded file. Nothing in
	// the object is read before it matches. Neither the ETag nor a checksum
	// the bucket stores is this number.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`

	// Format says how the object carries the files, as an HTTPS download
	// does: File, the schema file itself; TarGzip or Zip, an archive.
	Format HTTPSFormat `json:"format"`

	// Path is what the object selects. With File it is the one name the
	// object is given, whose extension tells a schema's format. In an
	// archive it is the schema file, or the migration directory, where "."
	// is the root. Relative, separated by "/", with no empty or ".." element,
	// no backslash and no control character.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(\.|(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+)(?:/(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+))*)$`
	Path string `json:"path"`

	// AuthFrom names the Secret the fetch container reads the key pair from.
	// The manager never reads it. Without it, the request is unsigned, as a
	// public bucket takes.
	// +optional
	AuthFrom *S3AuthSource `json:"authFrom,omitempty"`

	// Transport pins the endpoint: the CA its certificate chains to.
	// +optional
	Transport S3TransportSpec `json:"transport,omitempty"`
}

// S3AuthSource names the Secret an S3 fetch signs with. Its keys are fixed:
// accessKeyID and secretAccessKey, sessionToken for a temporary key pair, and
// host, the endpoint authority host[:port] the key is for, which the fetch
// holds the endpoint to. bucket, when present, is the only bucket the key may
// sign for, and caSHA256 the SHA-256 of the CA the key may be sent under,
// which transport.caFrom has to select: the Secret's owner, not the
// resource's author, decides whom the key signs for.
type S3AuthSource struct {
	// Name of the Secret, in this resource's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// S3TransportSpec pins the endpoint an S3 fetch talks to.
// +kubebuilder:validation:XValidation:rule="!has(self.caFrom) || (self.caFrom.name.size() > 0 && self.caFrom.key.size() > 0 && (!has(self.caFrom.optional) || !self.caFrom.optional))",message="caFrom must name a required ConfigMap key"
type S3TransportSpec struct {
	// CAFrom selects the only roots the endpoint's certificate may chain to.
	// With authFrom, its Secret must hold caSHA256 with the selected bytes'
	// digest, and a source whose Secret holds caSHA256 is refused without
	// it.
	// +optional
	CAFrom *corev1.ConfigMapKeySelector `json:"caFrom,omitempty"`
}

// S3SourceBinding is what an operation fetches an S3 source by: the object,
// the version a resolution bound, its digest, how it carries the files, the
// selected path, the content digest a resolution found there, and the
// selectors of the credential and the trust. The operation reads the object
// again and refuses other bytes or other content.
type S3SourceBinding struct {
	// Endpoint is the S3 API's address.
	Endpoint string `json:"endpoint"`
	// Region is the region a request is signed for.
	Region string `json:"region"`
	// Bucket is the bucket's name.
	Bucket string `json:"bucket"`
	// Key is the object's key.
	Key string `json:"key"`
	// VersionID is the version every operation reads: the one the spec
	// named, or the one the server served the resolution. A bucket that
	// keeps no versions leaves it empty.
	// +optional
	VersionID string `json:"versionId,omitempty"`
	// Digest is the SHA-256 of the bytes GetObject returned.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Format says how the object carries the files.
	Format HTTPSFormat `json:"format"`
	// Path is what the object selects.
	Path string `json:"path"`
	// ContentDigest is the content digest of the selected files.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// AuthFrom names the credential Secret. It is a selector, never the
	// credential.
	// +optional
	AuthFrom *S3AuthSource `json:"authFrom,omitempty"`
	// Transport pins the endpoint.
	// +optional
	Transport S3TransportSpec `json:"transport,omitempty"`
}

// S3SourceRevision is the object an S3 source last resolved, and what it
// selected: another endpoint, region, bucket, key, digest, format or path in
// the spec is another source, and is resolved before anything reads it, as is
// a version the spec names that is not the one bound here.
type S3SourceRevision struct {
	// Endpoint is the S3 API's address.
	Endpoint string `json:"endpoint"`
	// Region is the region a request is signed for.
	Region string `json:"region"`
	// Bucket is the bucket's name.
	Bucket string `json:"bucket"`
	// Key is the object's key.
	Key string `json:"key"`
	// VersionID is the version every later operation reads: the one the spec
	// named, or the one the server served. A bucket that keeps no versions
	// leaves it empty.
	// +optional
	VersionID string `json:"versionId,omitempty"`
	// Format says how the object carries the files.
	Format HTTPSFormat `json:"format"`
	// Path is what the object selects.
	Path string `json:"path"`
	// Digest is the SHA-256 of the bytes GetObject returned.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ContentDigest is the content digest of the selected files.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
}
