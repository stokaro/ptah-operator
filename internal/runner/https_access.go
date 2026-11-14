package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/sourcehost"
)

// The environment an HTTPS source reaches a runner through. The manager sets
// the source's coordinates from the resource; the Secret owner's grants come
// from the credential Secret through the kubelet, and the manager never reads
// them.
const (
	// EnvHTTPSURL is the download, https://.
	EnvHTTPSURL = "PTAH_OPERATOR_HTTPS_URL"
	// EnvHTTPSDigest is the SHA-256 of the bytes the URL serves.
	EnvHTTPSDigest = "PTAH_OPERATOR_HTTPS_DIGEST"
	// EnvHTTPSFormat is how the download carries the files, as the API names
	// it: File, TarGzip or Zip.
	EnvHTTPSFormat = "PTAH_OPERATOR_HTTPS_FORMAT"
	// EnvHTTPSPath is what the download selects.
	EnvHTTPSPath = "PTAH_OPERATOR_HTTPS_PATH"
	// EnvHTTPSSelects says whether the path is a schema file ("file") or a
	// migration directory ("directory").
	EnvHTTPSSelects = "PTAH_OPERATOR_HTTPS_SELECTS"

	// EnvHTTPSHostGrant carries the Secret's fixed host key: the authority
	// the credential is for. It is never passed to Ptah.
	EnvHTTPSHostGrant = "PTAH_OPERATOR_HTTPS_HOST_GRANT"
	// EnvHTTPSCASHA256Grant carries the Secret's digest of the CA its
	// credential may be sent under.
	EnvHTTPSCASHA256Grant = "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT"
	// EnvHTTPSCASourceFile is the runner-only path of the mutable ConfigMap
	// projection. Ptah reads only a verified private copy.
	EnvHTTPSCASourceFile = "PTAH_OPERATOR_HTTPS_CA_SOURCE_FILE"

	// The credential variables Ptah reads. The runner sets them from the
	// credential files the kubelet projects into the fetch container only.
	EnvHTTPSUsername = "PTAH_HTTPS_USERNAME"
	EnvHTTPSPassword = "PTAH_HTTPS_PASSWORD" // #nosec G101 -- the name of a variable, not a credential
	EnvHTTPSToken    = "PTAH_HTTPS_TOKEN"    // #nosec G101 -- the name of a variable, not a credential

	httpsSelectsFile      = "file"
	httpsSelectsDirectory = "directory"

	// CodeHTTPSSourceRefused prefixes every refusal of the HTTPS fetch.
	CodeHTTPSSourceRefused = "https_source_refused"
)

// httpsFormats maps the API's formats to the --format Ptah's pull takes.
var httpsFormats = map[string]string{"File": "file", "TarGzip": "tar.gz", "Zip": "zip"}

// HTTPSSource is an HTTPS source's coordinates, as the manager set them.
type HTTPSSource struct {
	URL, Digest, Format, Path string
	ExpectedContentDigest     string
	// Schema says the path is a schema file; otherwise it is a migration
	// directory.
	Schema  bool
	selects string
}

// HTTPSSourceFromEnvironment reads the coordinates, and reports whether the
// operation has an HTTPS source at all.
func HTTPSSourceFromEnvironment(environment []string) (HTTPSSource, bool) {
	values := environmentMap(environment)
	source := HTTPSSource{
		URL: values[EnvHTTPSURL], Digest: values[EnvHTTPSDigest], Format: values[EnvHTTPSFormat], Path: values[EnvHTTPSPath],
		ExpectedContentDigest: values[EnvExpectedContentDigest],
		Schema:                values[EnvHTTPSSelects] == httpsSelectsFile, selects: values[EnvHTTPSSelects],
	}
	return source, source.URL != ""
}

// validate refuses coordinates Ptah would read differently from the manager,
// or refuse only after it downloaded something.
func (s HTTPSSource) validate() error {
	parsed, err := url.Parse(s.URL)
	switch {
	case err != nil, parsed.Scheme != "https", parsed.Host == "", parsed.Hostname() == "", parsed.Opaque != "":
		return errors.New("the HTTPS source URL is not an https:// download")
	case parsed.User != nil:
		return errors.New("the HTTPS source URL carries a credential")
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return errors.New("the HTTPS source URL has a query or a fragment")
	case parsed.Path == "" || parsed.Path == "/":
		return errors.New("the HTTPS source URL names no path")
	}
	if err := sourcehost.CheckASCII(parsed.Host); err != nil {
		return fmt.Errorf("the HTTPS source URL: %w", err)
	}
	if !validProtocolDigest(s.Digest) {
		return errors.New("the HTTPS source digest is not a lowercase SHA-256 digest")
	}
	if _, known := httpsFormats[s.Format]; !known {
		return errors.New("the HTTPS source format is not File, TarGzip or Zip")
	}
	if s.selects != "" && s.selects != httpsSelectsFile && s.selects != httpsSelectsDirectory {
		return errors.New("an HTTPS source selects neither a file nor a directory")
	}
	if s.Path == "" || strings.HasPrefix(s.Path, "/") || path.Clean(s.Path) != s.Path {
		return errors.New("the HTTPS source path is not a clean relative path")
	}
	switch {
	case s.Format == "File" && (s.Path == "." || strings.Contains(s.Path, "/")):
		return errors.New("with File, the HTTPS source path is the one name the download is given")
	case s.Format == "File" && s.selects == httpsSelectsDirectory:
		return errors.New("a migration directory comes in an archive, not a File")
	case s.Path == "." && s.selects == httpsSelectsFile:
		return errors.New("a schema selects one file, not the archive's root")
	}
	if s.ExpectedContentDigest != "" && !validProtocolDigest(s.ExpectedContentDigest) {
		return errors.New("the expected content digest is not a lowercase SHA-256 digest")
	}
	return nil
}

// authority is the host[:port] a credential has to be granted for, or ""
// when the URL names none validate admits.
func (s HTTPSSource) authority() string {
	authority, err := sourcehost.Authority(s.URL)
	if err != nil {
		return ""
	}
	return authority
}

// httpsTrust is the verified private copy of the CA a pull runs under.
type httpsTrust struct {
	caFile string
}

// PrepareHTTPSSourceAccess holds an HTTPS source to the credential owner's
// grants and copies the CA it selects to a private file, then returns it with
// a cleanup to call. A credential is Basic or Bearer, never both and never
// half of Basic, and needs a host grant equal to the URL's authority, as
// printable ASCII without case; a CA beside a credential needs a digest grant
// equal to its bytes. The Secret's
// owner, not the resource's author, decides where the credential goes, and a
// grant with no credential is refused rather than ignored.
func PrepareHTTPSSourceAccess(source HTTPSSource, environment []string, tempDir string) (httpsTrust, func(), error) {
	if err := source.validate(); err != nil {
		return httpsTrust{}, nil, err
	}
	values := environmentMap(environment)
	username, password, token := values[EnvHTTPSUsername], values[EnvHTTPSPassword], values[EnvHTTPSToken]
	switch {
	case token != "" && (username != "" || password != ""):
		return httpsTrust{}, nil, errors.New("the HTTPS credential holds a token beside a username or a password")
	case username != "" && password == "":
		return httpsTrust{}, nil, errors.New("the HTTPS credential holds a username without a password")
	case password != "" && username == "":
		return httpsTrust{}, nil, errors.New("the HTTPS credential holds a password without a username")
	}
	credentialed := username != "" || token != ""
	if credentialed {
		grant := values[EnvHTTPSHostGrant]
		if grant != "" && sourcehost.CheckASCII(grant) != nil {
			return httpsTrust{}, nil, fmt.Errorf("the HTTPS credential's host grant is not printable ASCII: %w", sourcehost.ErrNotASCII)
		}
		if !sourcehost.Equal(grant, source.authority()) {
			return httpsTrust{}, nil, errors.New("the HTTPS credential is not granted for the source's host")
		}
	} else if values[EnvHTTPSHostGrant] != "" || values[EnvHTTPSCASHA256Grant] != "" {
		return httpsTrust{}, nil, errors.New("an HTTPS grant was supplied without a credential")
	}
	caSource, caGrant := values[EnvHTTPSCASourceFile], values[EnvHTTPSCASHA256Grant]
	if caSource == "" {
		if caGrant != "" {
			return httpsTrust{}, nil, errors.New("an HTTPS certificate authority digest grant was supplied without the certificate authority")
		}
		return httpsTrust{}, func() {}, nil
	}
	bytes, err := readTrustFile(caSource, "HTTPS", "certificate authority")
	if err != nil {
		return httpsTrust{}, nil, err
	}
	if credentialed {
		sum := sha256.Sum256(bytes)
		if !validProtocolDigest(caGrant) || caGrant != "sha256:"+hex.EncodeToString(sum[:]) {
			return httpsTrust{}, nil, errors.New("the HTTPS certificate authority is not the one the credential's owner granted")
		}
	}
	snapshot, err := writeTrustFile(tempDir, bytes, "HTTPS", "certificate authority")
	if err != nil {
		return httpsTrust{}, nil, err
	}
	return httpsTrust{caFile: snapshot}, func() { _ = os.Remove(snapshot) }, nil
}

// HTTPSPull is the Ptah command that fetches an HTTPS source: a schema file
// to out, or a migration directory under it, reporting as JSON. It is always
// held to the download digest, and with an expected content digest it
// refuses other files.
func HTTPSPull(ptahBinary string, source HTTPSSource, out string, trust httpsTrust) (CommandSpec, error) {
	if err := source.validate(); err != nil {
		return CommandSpec{}, err
	}
	if !filepath.IsAbs(out) {
		return CommandSpec{}, errors.New("an HTTPS pull needs an absolute output path")
	}
	family := "migrations"
	if source.Schema {
		family = "schema"
	}
	args := []string{
		family, "pull", source.URL,
		"--format", httpsFormats[source.Format], "--path", source.Path,
		"--expect-download-digest", source.Digest,
	}
	for _, flag := range []struct{ name, value string }{
		{"--expect-digest", source.ExpectedContentDigest}, {"--ca-file", trust.caFile},
	} {
		if flag.value != "" {
			args = append(args, flag.name, flag.value)
		}
	}
	args = append(args, "--out", out, "--json")
	return CommandSpec{Path: ptahBinary, Args: args}, nil
}

// FetchHTTPSSource delivers the download an HTTPS source was resolved to into
// destination, for the operation container that runs after it: Ptah
// downloads it held to the download digest and the bound content digest, and
// the files reach destination only if the bytes Ptah wrote give the content
// digest again.
func FetchHTTPSSource(ctx context.Context, ptahBinary string, environment []string, destination, tempDir string, executor Executor, diagnostics io.Writer) error {
	environment, err := ResolveCredentialFiles(environment)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeHTTPSSourceRefused, err)
	}
	source, isHTTPS := HTTPSSourceFromEnvironment(environment)
	if !isHTTPS {
		return fmt.Errorf("%s: no HTTPS source is configured", CodeHTTPSSourceRefused)
	}
	if _, isGit := GitSourceFromEnvironment(environment); isGit {
		return fmt.Errorf("%s: the fetch names a Git source as well", CodeHTTPSSourceRefused)
	}
	return fetchRemoteSource(ctx, ptahBinary, environment, source, destination, tempDir, executor, diagnostics)
}

// The HTTPS part of a remote source.

func (s HTTPSSource) kind() string          { return "HTTPS" }
func (s HTTPSSource) selectsFile() bool     { return s.Schema }
func (s HTTPSSource) selectsKnown() bool    { return s.selects != "" }
func (s HTTPSSource) selected() string      { return s.Path }
func (s HTTPSSource) contentDigest() string { return s.ExpectedContentDigest }
func (s HTTPSSource) accessCode() string    { return "invalid_https_access" }
func (s HTTPSSource) refusedCode() string   { return CodeHTTPSSourceRefused }

func (s HTTPSSource) fetchable() error {
	if s.Digest == "" || s.ExpectedContentDigest == "" {
		return errors.New("a fetch needs the download digest and the resolved content digest")
	}
	return nil
}

func (s HTTPSSource) prepare(environment []string, tempDir string) (func(string, string) (CommandSpec, error), func(), error) {
	trust, cleanup, err := PrepareHTTPSSourceAccess(s, environment, tempDir)
	if err != nil {
		return nil, nil, err
	}
	return func(ptahBinary, out string) (CommandSpec, error) { return HTTPSPull(ptahBinary, s, out, trust) }, cleanup, nil
}

func (s HTTPSSource) request() dataplane.PullRequest {
	return dataplane.PullRequest{Source: dataplane.PullSourceHTTPS, Reference: s.URL, Path: s.Path, Format: httpsFormats[s.Format]}
}

// holdsResolve refuses a Resolve whose download is not the one the spec
// pins. Ptah holds the download to the same digest, so this refuses only a
// report that does not say what Ptah was asked.
func (s HTTPSSource) holdsResolve(report dataplane.SourcePullReport) error {
	if report.DownloadDigest != s.Digest {
		return fmt.Errorf("the pull read the download %s, not %s", report.DownloadDigest, s.Digest)
	}
	return nil
}

func (s HTTPSSource) holdsFetch(report dataplane.SourcePullReport) error {
	if report.DownloadDigest != s.Digest || report.ContentDigest != s.ExpectedContentDigest {
		return fmt.Errorf("the pull read %s from the download %s, not %s from %s",
			report.ContentDigest, report.DownloadDigest, s.ExpectedContentDigest, s.Digest)
	}
	return nil
}

func (s HTTPSSource) record(result *Result, report dataplane.SourcePullReport) {
	result.ResolvedDownloadDigest = report.DownloadDigest
}
