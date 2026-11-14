package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// The fixed keys of an HTTPS credential Secret. HTTPSHostSecretKey is the
// authority, host[:port], the credential owner grants the credential for;
// HTTPSCASHA256SecretKey is the "sha256:" digest of the CA it may be sent
// under. Username and password authenticate with Basic, a token with Bearer;
// a Secret holds one or the other. A resource's author selects the Secret,
// never the keys.
const (
	HTTPSHostSecretKey     = "host"
	HTTPSUsernameSecretKey = "username"
	HTTPSPasswordSecretKey = "password"
	HTTPSTokenSecretKey    = "token"
	HTTPSCASHA256SecretKey = "caSHA256"
)

// HTTPSFormat says how a download carries the selected files.
// +kubebuilder:validation:Enum=File;TarGzip;Zip
type HTTPSFormat string

const (
	// HTTPSFormatFile is one file, the download itself. Only a schema reads
	// one.
	HTTPSFormatFile HTTPSFormat = "File"
	// HTTPSFormatTarGzip is a tar archive compressed with gzip.
	HTTPSFormatTarGzip HTTPSFormat = "TarGzip"
	// HTTPSFormatZip is a zip archive.
	HTTPSFormatZip HTTPSFormat = "Zip"
)

// HTTPSSourceSpec selects a file or a directory of one download, pinned by
// the SHA-256 of the bytes the URL serves.
// +kubebuilder:validation:XValidation:rule="self.format != 'File' || (self.path != '.' && !self.path.contains('/'))",message="with format File, path is the one name the download is given"
type HTTPSSourceSpec struct {
	// URL of the download: https://, with no credential, query or fragment.
	// The host is ASCII letters, digits, dots and hyphens, or an IP address
	// in brackets, with an optional port: write an internationalized name in
	// its xn-- form.
	// +kubebuilder:validation:MinLength=11
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?/[^[:space:]?#]+$`
	URL string `json:"url"`

	// Digest is "sha256:" and the SHA-256 of the exact bytes the URL serves,
	// the number sha256sum prints for the file. Nothing in the download is
	// read before it matches. It is not the content digest: an archive packed
	// again with the same selected files has another digest and the same
	// content.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`

	// Format says how the download carries the files: File, the schema file
	// itself; TarGzip or Zip, an archive.
	Format HTTPSFormat `json:"format"`

	// Path is what the download selects. With File it is the one name the
	// download is given, whose extension tells a schema's format. In an
	// archive it is the schema file, or the migration directory, where "."
	// is the root. Relative, separated by "/", with no empty or ".." element,
	// no backslash and no control character.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(\.|(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+)(?:/(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+))*)$`
	Path string `json:"path"`

	// AuthFrom names the Secret the fetch container reads the credential
	// from. The manager never reads it.
	// +optional
	AuthFrom *HTTPSAuthSource `json:"authFrom,omitempty"`

	// Transport pins the server: the CA its certificate chains to.
	// +optional
	Transport HTTPSTransportSpec `json:"transport,omitempty"`
}

// HTTPSAuthSource names the Secret an HTTPS fetch authenticates with. Its keys
// are fixed: username and password for Basic, or token for Bearer, and host,
// the authority host[:port] the credential is for, which the fetch holds the
// URL to. caSHA256 is the SHA-256 of the CA the credential may be sent
// under, which transport.caFrom has to select: the Secret's owner, not the
// resource's author, decides whom the credential is sent to.
type HTTPSAuthSource struct {
	// Name of the Secret, in this resource's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// HTTPSTransportSpec pins the server an HTTPS fetch talks to.
// +kubebuilder:validation:XValidation:rule="!has(self.caFrom) || (self.caFrom.name.size() > 0 && self.caFrom.key.size() > 0 && (!has(self.caFrom.optional) || !self.caFrom.optional))",message="caFrom must name a required ConfigMap key"
type HTTPSTransportSpec struct {
	// CAFrom selects the only roots the server's certificate may chain to.
	// With authFrom, its Secret must hold caSHA256 with the selected bytes'
	// digest, and a source whose Secret holds caSHA256 is refused without
	// it.
	// +optional
	CAFrom *corev1.ConfigMapKeySelector `json:"caFrom,omitempty"`
}

// HTTPSSourceBinding is what an operation fetches an HTTPS source by: the
// download, its digest, how it carries the files, the selected path, the
// content digest a resolution found there, and the selectors of the
// credential and the trust. The operation downloads it again and refuses
// other bytes or other content.
type HTTPSSourceBinding struct {
	// URL of the download.
	URL string `json:"url"`
	// Digest is the SHA-256 of the bytes the URL served.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Format says how the download carries the files.
	Format HTTPSFormat `json:"format"`
	// Path is what the download selects.
	Path string `json:"path"`
	// ContentDigest is the content digest of the selected files.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// AuthFrom names the credential Secret. It is a selector, never the
	// credential.
	// +optional
	AuthFrom *HTTPSAuthSource `json:"authFrom,omitempty"`
	// Transport pins the server.
	// +optional
	Transport HTTPSTransportSpec `json:"transport,omitempty"`
}

// HTTPSSourceRevision is the download an HTTPS source last resolved, and what
// it selected: another URL, digest, format or path in the spec is another
// source, and is resolved before anything reads it.
type HTTPSSourceRevision struct {
	// URL of the download.
	URL string `json:"url"`
	// Format says how the download carries the files.
	Format HTTPSFormat `json:"format"`
	// Path is what the download selects.
	Path string `json:"path"`
	// Digest is the SHA-256 of the bytes the URL served.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ContentDigest is the content digest of the selected files.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
}
