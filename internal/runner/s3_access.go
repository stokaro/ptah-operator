package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stokaro/ptah-operator/internal/s3source"
	"github.com/stokaro/ptah-operator/internal/sourcearchive"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcehost"
)

// The environment an S3 source reaches a runner through. The manager sets
// the source's coordinates from the resource; the Secret owner's grants come
// from the credential Secret through the kubelet, and the manager never reads
// them.
const (
	// EnvS3Endpoint is the S3 API's address, https:// and an authority.
	EnvS3Endpoint = "PTAH_OPERATOR_S3_ENDPOINT"
	// EnvS3Region is the region a request is signed for.
	EnvS3Region = "PTAH_OPERATOR_S3_REGION"
	// EnvS3Bucket and EnvS3Key name the object.
	EnvS3Bucket = "PTAH_OPERATOR_S3_BUCKET"
	EnvS3Key    = "PTAH_OPERATOR_S3_KEY"
	// EnvS3VersionID is the version to read: the one the spec names at a
	// Resolve, the one the resolution bound at a fetch. Unset, the current
	// version.
	EnvS3VersionID = "PTAH_OPERATOR_S3_VERSION_ID"
	// EnvS3Digest is the SHA-256 of the bytes GetObject returns.
	EnvS3Digest = "PTAH_OPERATOR_S3_DIGEST"
	// EnvS3Format is how the object carries the files, as the API names it:
	// File, TarGzip or Zip.
	EnvS3Format = "PTAH_OPERATOR_S3_FORMAT"
	// EnvS3Path is what the object selects.
	EnvS3Path = "PTAH_OPERATOR_S3_PATH"
	// EnvS3Selects says whether the path is a schema file ("file") or a
	// migration directory ("directory").
	EnvS3Selects = "PTAH_OPERATOR_S3_SELECTS"

	// EnvS3HostGrant carries the Secret's fixed host key: the endpoint
	// authority the key pair is for.
	EnvS3HostGrant = "PTAH_OPERATOR_S3_HOST_GRANT"
	// EnvS3BucketGrant carries the Secret's optional bucket key: the only
	// bucket the key pair may sign for.
	EnvS3BucketGrant = "PTAH_OPERATOR_S3_BUCKET_GRANT"
	// EnvS3CASHA256Grant carries the Secret's digest of the CA its key pair
	// may be sent under.
	EnvS3CASHA256Grant = "PTAH_OPERATOR_S3_CA_SHA256_GRANT"
	// EnvS3CASourceFile is the runner-only path of the mutable ConfigMap
	// projection. The fetch reads only a verified private copy.
	EnvS3CASourceFile = "PTAH_OPERATOR_S3_CA_SOURCE_FILE"

	// The variables the runner reads the key pair into from the credential
	// files the kubelet projects into the fetching container only. No child
	// process receives them: the runner signs the request itself.
	EnvS3AccessKeyID     = "PTAH_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "PTAH_S3_SECRET_ACCESS_KEY" // #nosec G101 -- the name of a variable, not a credential
	EnvS3SessionToken    = "PTAH_S3_SESSION_TOKEN"     // #nosec G101 -- the name of a variable, not a credential

	s3SelectsFile      = "file"
	s3SelectsDirectory = "directory"

	// CodeS3SourceRefused prefixes every refusal of the S3 fetch, and is the
	// code of a Resolve the endpoint or the object refused.
	CodeS3SourceRefused = "s3_source_refused"
)

// s3Formats maps the API's formats to the archive reader's.
var s3Formats = map[string]sourcearchive.Format{
	"File": sourcearchive.FormatFile, "TarGzip": sourcearchive.FormatTarGzip, "Zip": sourcearchive.FormatZip,
}

// S3Source is an S3 source's coordinates, as the manager set them.
type S3Source struct {
	Endpoint, Region, Bucket, Key, VersionID string
	Digest, Format, Path                     string
	ExpectedContentDigest                    string
	// Schema says the path is a schema file; otherwise it is a migration
	// directory.
	Schema  bool
	selects string
}

// S3SourceFromEnvironment reads the coordinates, and reports whether the
// operation has an S3 source at all.
func S3SourceFromEnvironment(environment []string) (S3Source, bool) {
	values := environmentMap(environment)
	source := S3Source{
		Endpoint: values[EnvS3Endpoint], Region: values[EnvS3Region], Bucket: values[EnvS3Bucket],
		Key: values[EnvS3Key], VersionID: values[EnvS3VersionID],
		Digest: values[EnvS3Digest], Format: values[EnvS3Format], Path: values[EnvS3Path],
		ExpectedContentDigest: values[EnvExpectedContentDigest],
		Schema:                values[EnvS3Selects] == s3SelectsFile, selects: values[EnvS3Selects],
	}
	return source, source.Endpoint != ""
}

func (s S3Source) object() s3source.Object {
	return s3source.Object{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Key: s.Key, VersionID: s.VersionID}
}

func (s S3Source) archive() sourcearchive.Options {
	kind := sourcearchive.Directory
	if s.Schema {
		kind = sourcearchive.File
	}
	return sourcearchive.Options{Format: s3Formats[s.Format], Path: s.Path, Kind: kind}
}

// validate refuses coordinates the manager could not have set, before any
// request is made.
func (s S3Source) validate() error {
	if err := s.object().Validate(); err != nil {
		return fmt.Errorf("the S3 source: %w", err)
	}
	if !validProtocolDigest(s.Digest) {
		return errors.New("the S3 source digest is not a lowercase SHA-256 digest")
	}
	if _, known := s3Formats[s.Format]; !known {
		return errors.New("the S3 source format is not File, TarGzip or Zip")
	}
	switch {
	case s.selects != s3SelectsFile && s.selects != s3SelectsDirectory:
		return errors.New("an S3 source selects neither a file nor a directory")
	case s.Path == "" || strings.HasPrefix(s.Path, "/") || path.Clean(s.Path) != s.Path:
		return errors.New("the S3 source path is not a clean relative path")
	case s.Format == "File" && (s.Path == "." || strings.Contains(s.Path, "/")):
		return errors.New("with File, the S3 source path is the one name the object is given")
	case s.Format == "File" && !s.Schema:
		return errors.New("a migration directory comes in an archive, not a File")
	case s.Path == "." && s.Schema:
		return errors.New("a schema selects one file, not the archive's root")
	case s.ExpectedContentDigest != "" && !validProtocolDigest(s.ExpectedContentDigest):
		return errors.New("the expected content digest is not a lowercase SHA-256 digest")
	}
	if err := sourcearchive.Check(s.archive()); err != nil {
		return fmt.Errorf("the S3 source path: %w", err)
	}
	return nil
}

// s3Access is what a fetch is sent with: the key pair, if any, and the
// verified private copy of the CA.
type s3Access struct {
	credentials s3source.Credentials
	caFile      string
}

// PrepareS3SourceAccess holds an S3 source to the credential owner's grants
// and copies the CA it selects to a private file, then returns them with a
// cleanup to call. A key pair is whole, a session token comes with one, and
// the pair needs a host grant equal to the endpoint's authority, as printable
// ASCII without case; a bucket grant, when the Secret holds one, has to name
// the spec's bucket; a CA beside a key pair needs a digest grant equal to its
// bytes, and a digest grant needs the CA. The Secret's owner, not the
// resource's author, decides where the key pair signs, and a grant with no
// key pair is refused rather than ignored.
func PrepareS3SourceAccess(source S3Source, environment []string, tempDir string) (s3Access, func(), error) {
	if err := source.validate(); err != nil {
		return s3Access{}, nil, err
	}
	values := environmentMap(environment)
	credentials := s3source.Credentials{
		AccessKeyID: values[EnvS3AccessKeyID], SecretAccessKey: values[EnvS3SecretAccessKey], SessionToken: values[EnvS3SessionToken],
	}
	switch {
	case credentials.AccessKeyID != "" && credentials.SecretAccessKey == "":
		return s3Access{}, nil, errors.New("the S3 credential holds an access key ID without a secret access key")
	case credentials.SecretAccessKey != "" && credentials.AccessKeyID == "":
		return s3Access{}, nil, errors.New("the S3 credential holds a secret access key without an access key ID")
	case credentials.SessionToken != "" && credentials.AccessKeyID == "":
		return s3Access{}, nil, errors.New("the S3 credential holds a session token without a key pair")
	}
	if err := credentials.Validate(); err != nil {
		return s3Access{}, nil, err
	}
	hostGrant, bucketGrant := values[EnvS3HostGrant], values[EnvS3BucketGrant]
	credentialed := credentials.Present()
	if credentialed {
		authority, err := source.object().Authority()
		if err != nil {
			return s3Access{}, nil, err
		}
		// A grant is not a credential, so the refusal names it, escaped: a
		// Secret written by kubectl create secret --from-file ends in a
		// newline, and only the escaped value shows it.
		if hostGrant != "" {
			if err := sourcehost.CheckASCII(hostGrant); err != nil {
				return s3Access{}, nil, fmt.Errorf("the S3 credential's host grant is refused: %w", err)
			}
		}
		if !sourcehost.Equal(hostGrant, authority) {
			return s3Access{}, nil, errors.New("the S3 credential is not granted for the source's endpoint")
		}
		if _, granted := values[EnvS3BucketGrant]; granted && bucketGrant != source.Bucket {
			return s3Access{}, nil, fmt.Errorf("the S3 credential is granted for the bucket %+q, not the source's", bucketGrant)
		}
	} else if hostGrant != "" || bucketGrant != "" || values[EnvS3CASHA256Grant] != "" {
		return s3Access{}, nil, errors.New("an S3 grant was supplied without a key pair")
	}
	caSource, caGrant := values[EnvS3CASourceFile], values[EnvS3CASHA256Grant]
	if caSource == "" {
		if caGrant != "" {
			return s3Access{}, nil, errors.New("an S3 certificate authority digest grant was supplied without the certificate authority: " +
				"the credential's Secret pins caSHA256, so the source has to select that CA in transport.caFrom")
		}
		return s3Access{credentials: credentials}, func() {}, nil
	}
	bytes, err := readTrustFile(caSource, "S3", "certificate authority")
	if err != nil {
		return s3Access{}, nil, err
	}
	if credentialed {
		sum := sha256.Sum256(bytes)
		if !validProtocolDigest(caGrant) || caGrant != "sha256:"+hex.EncodeToString(sum[:]) {
			return s3Access{}, nil, errors.New("the S3 certificate authority is not the one the credential's owner granted")
		}
	}
	snapshot, err := writeTrustFile(tempDir, bytes, "S3", "certificate authority")
	if err != nil {
		return s3Access{}, nil, err
	}
	return s3Access{credentials: credentials, caFile: snapshot}, func() { _ = os.Remove(snapshot) }, nil
}

// readS3Source reads what an S3 source names under the access it was
// granted: the object held to the pinned digest and, at a fetch, to the
// bound content digest.
func readS3Source(ctx context.Context, source S3Source, access s3Access, tempDir string) (s3source.Result, error) {
	return s3source.Fetch(ctx, s3source.Options{
		Object:               source.object(),
		Credentials:          access.credentials,
		CAFile:               access.caFile,
		ExpectDownloadDigest: source.Digest,
		Archive:              source.archive(),
		ExpectDigest:         source.ExpectedContentDigest,
		TempDir:              tempDir,
	})
}

// s3Refusal is why reading an S3 source failed, as one sentence the status
// may carry: the runner's own words, redacted and bounded as a pull's last
// line is.
func s3Refusal(err error, redactor Redactor) error {
	return errors.New(freeText(err.Error(), redactor, maxPullRefusalBytes))
}

// FetchS3Source delivers the object version an S3 source was resolved to
// into destination, for the operation container that runs after it. It holds
// the source to the credential owner's grants, reads the object held to the
// object digest and the bound content digest, and writes the selected files
// to destination only if they give that digest. The container that holds the
// database URL never sees the key pair, and reads only files that gave the
// digest.
func FetchS3Source(ctx context.Context, environment []string, destination, tempDir string, diagnostics io.Writer) error {
	environment, err := ResolveCredentialFiles(environment)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeS3SourceRefused, err)
	}
	source, isS3 := S3SourceFromEnvironment(environment)
	if !isS3 {
		return fmt.Errorf("%s: no S3 source is configured", CodeS3SourceRefused)
	}
	if _, isGit := GitSourceFromEnvironment(environment); isGit {
		return fmt.Errorf("%s: the fetch names a Git source as well", CodeS3SourceRefused)
	}
	if _, isHTTPS := HTTPSSourceFromEnvironment(environment); isHTTPS {
		return fmt.Errorf("%s: the fetch names an HTTPS source as well", CodeS3SourceRefused)
	}
	switch {
	case source.Digest == "" || source.ExpectedContentDigest == "":
		return fmt.Errorf("%s: a fetch needs the object digest and the resolved content digest", CodeS3SourceRefused)
	case source.selects == "":
		return fmt.Errorf("%s: a fetch needs to know whether the path is a file or a directory", CodeS3SourceRefused)
	case !filepath.IsAbs(destination):
		return fmt.Errorf("%s: the destination is not an absolute path", CodeS3SourceRefused)
	}
	redactor := NewRedactor(environment)
	// The access check is the one a Resolve makes, and its refusal leaves
	// the record that carries the Resolve's code and words to the status.
	access, cleanup, err := PrepareS3SourceAccess(source, environment, tempDir)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeS3SourceRefused, accessRefused(CodeInvalidS3Access, err, redactor))
	}
	defer cleanup()
	read, err := readS3Source(ctx, source, access, tempDir)
	if err != nil {
		refusal := s3Refusal(err, redactor)
		if diagnostics != nil {
			_, _ = io.WriteString(diagnostics, refusal.Error()+"\n")
		}
		return fmt.Errorf("%s: %w", CodeS3SourceRefused, refusal)
	}
	// The read holds the files to the content digest already; this holds the
	// fetch to the digest of what it is about to write, which is what the
	// operation container reads.
	digest, err := sourcecontent.Digest(read.Files)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeS3SourceRefused, err)
	}
	if digest != source.ExpectedContentDigest || read.DownloadDigest != source.Digest {
		return fmt.Errorf("%s: the fetch read %s from the object %s, not %s from %s",
			CodeS3SourceRefused, digest, read.DownloadDigest, source.ExpectedContentDigest, source.Digest)
	}
	if err := sourcecontent.Write(destination, read.Files); err != nil {
		return fmt.Errorf("%s: %w", CodeS3SourceRefused, err)
	}
	return nil
}

// runS3Resolve reads the object an S3 source names, held to the pinned
// digest, and reports the content digest of the selected files and the
// version the endpoint served. Every later operation reads that version
// again and is held to that digest. Nothing is written to disk but the
// object itself, which the read removes.
func runS3Resolve(ctx context.Context, config Config, environment []string, source S3Source, redactor Redactor, result Result) Result {
	access, cleanup, err := PrepareS3SourceAccess(source, environment, config.TempDir)
	if err != nil {
		setResultError(&result, CodeInvalidS3Access, err, redactor, config.Diagnostics)
		return result
	}
	defer cleanup()
	read, err := readS3Source(ctx, source, access, config.TempDir)
	if err != nil {
		setResultError(&result, CodeS3SourceRefused, s3Refusal(err, redactor), redactor, config.Diagnostics)
		return result
	}
	if read.DownloadDigest != source.Digest {
		setResultError(&result, "invalid_resolve_output",
			fmt.Errorf("the read gave the object %s, not %s", read.DownloadDigest, source.Digest), redactor, config.Diagnostics)
		return result
	}
	result.ChildExitCode = 0
	result.ResolvedDownloadDigest = read.DownloadDigest
	result.ResolvedContentDigest = read.ContentDigest
	result.ResolvedVersionID = read.VersionID
	if source.VersionID != "" {
		result.ResolvedVersionID = source.VersionID
	}
	for _, file := range read.Files {
		result.ResolvedContentBytes += int64(len(file.Data))
	}
	return result
}
