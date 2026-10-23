package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// The environment a Git source reaches a runner through. The manager sets the
// source's coordinates from the resource; the Secret owner's grants come from
// the credential Secret through the kubelet, and the manager never reads
// them.
const (
	// EnvGitURL is the repository, https:// or ssh://.
	EnvGitURL = "PTAH_OPERATOR_GIT_URL"
	// EnvGitBranch, EnvGitTag and EnvGitCommit say which commit: a branch or
	// a tag to resolve, or the commit to fetch, alone or with the ref that
	// named it.
	EnvGitBranch = "PTAH_OPERATOR_GIT_BRANCH"
	EnvGitTag    = "PTAH_OPERATOR_GIT_TAG"
	EnvGitCommit = "PTAH_OPERATOR_GIT_COMMIT"
	// EnvGitPath is what the commit's tree selects.
	EnvGitPath = "PTAH_OPERATOR_GIT_PATH"
	// EnvExpectedContentDigest is the content digest a fetch has to give.
	EnvExpectedContentDigest = "PTAH_OPERATOR_EXPECTED_CONTENT_DIGEST"
	// EnvGitSelects says whether the path is a schema file ("file") or a
	// migration directory ("directory").
	EnvGitSelects = "PTAH_OPERATOR_GIT_SELECTS"

	gitSelectsFile      = "file"
	gitSelectsDirectory = "directory"

	// EnvGitHostGrant carries the Secret's fixed host key: the authority the
	// credential is for. It is never passed to Ptah.
	EnvGitHostGrant = "PTAH_OPERATOR_GIT_HOST_GRANT"
	// EnvGitCASHA256Grant and EnvGitKnownHostsSHA256Grant carry the Secret's
	// digests of the trust its credential may be sent under.
	EnvGitCASHA256Grant         = "PTAH_OPERATOR_GIT_CA_SHA256_GRANT"
	EnvGitKnownHostsSHA256Grant = "PTAH_OPERATOR_GIT_KNOWN_HOSTS_SHA256_GRANT"
	// EnvGitCASourceFile and EnvGitKnownHostsSourceFile are the runner-only
	// paths of the mutable ConfigMap projections. Ptah reads only verified
	// private copies.
	EnvGitCASourceFile         = "PTAH_OPERATOR_GIT_CA_SOURCE_FILE"
	EnvGitKnownHostsSourceFile = "PTAH_OPERATOR_GIT_KNOWN_HOSTS_SOURCE_FILE"

	// The credential variables Ptah reads. The kubelet sets them from the
	// credential Secret, in the fetch container only.
	EnvGitUsername   = "PTAH_GIT_USERNAME"
	EnvGitPassword   = "PTAH_GIT_PASSWORD" // #nosec G101 -- the name of a variable, not a credential
	EnvGitSSHKeyFile = "PTAH_GIT_SSH_KEY_FILE"

	maxGitTrustBytes int64 = 1 << 20
)

var gitCommitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitSource is a Git source's coordinates, as the manager set them.
type GitSource struct {
	URL, Branch, Tag, Commit, Path string
	ExpectedContentDigest          string
	// Schema says the path is a schema file; otherwise it is a migration
	// directory.
	Schema  bool
	selects string
}

// GitSourceFromEnvironment reads the coordinates, and reports whether the
// operation has a Git source at all.
func GitSourceFromEnvironment(environment []string) (GitSource, bool) {
	values := environmentMap(environment)
	source := GitSource{
		URL: values[EnvGitURL], Branch: values[EnvGitBranch], Tag: values[EnvGitTag], Commit: values[EnvGitCommit],
		Path: values[EnvGitPath], ExpectedContentDigest: values[EnvExpectedContentDigest],
		Schema: values[EnvGitSelects] == gitSelectsFile, selects: values[EnvGitSelects],
	}
	return source, source.URL != ""
}

// validate refuses coordinates Ptah would read differently from the manager.
func (s GitSource) validate() error {
	parsed, err := url.Parse(s.URL)
	switch {
	case err != nil, parsed.Scheme != "https" && parsed.Scheme != "ssh", parsed.Host == "":
		return errors.New("the Git source URL is not an https:// or ssh:// repository")
	case parsed.RawQuery != "" || parsed.Fragment != "":
		return errors.New("the Git source URL has a query or a fragment")
	case parsed.Scheme == "https" && parsed.User != nil:
		return errors.New("the Git source URL carries a credential")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return errors.New("the Git source URL carries a credential")
	}
	if s.Branch != "" && s.Tag != "" {
		return errors.New("a Git source names a branch and a tag")
	}
	if s.Branch == "" && s.Tag == "" && s.Commit == "" {
		return errors.New("a Git source names no branch, tag or commit")
	}
	if s.Commit != "" && !gitCommitID.MatchString(s.Commit) {
		return errors.New("the Git source commit is not a full lowercase id")
	}
	if s.Path == "" || strings.HasPrefix(s.Path, "/") || path.Clean(s.Path) != s.Path {
		return errors.New("the Git source path is not a clean relative path")
	}
	if s.selects != "" && s.selects != gitSelectsFile && s.selects != gitSelectsDirectory {
		return errors.New("a Git source selects neither a file nor a directory")
	}
	if s.ExpectedContentDigest != "" && !validProtocolDigest(s.ExpectedContentDigest) {
		return errors.New("the expected content digest is not a lowercase SHA-256 digest")
	}
	return nil
}

// reference is what Ptah's pull reads.
func (s GitSource) reference() string {
	return "git+" + s.URL
}

// authority is the host[:port] a credential has to be granted for.
func (s GitSource) authority() string {
	parsed, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Host)
}

// gitTrust is the verified private copies of the trust a fetch runs under.
type gitTrust struct {
	caFile, knownHostsFile string
}

// PrepareGitSourceAccess holds a Git source to the credential owner's grants
// and copies the trust it selects to private files, then returns them with a
// cleanup to call. A credential present needs a host grant equal to the
// URL's authority, and a digest grant for each trust file, equal to its bytes:
// the Secret's owner, not the resource's author, decides where the
// credential goes.
func PrepareGitSourceAccess(source GitSource, environment []string, tempDir string) (gitTrust, func(), error) {
	if err := source.validate(); err != nil {
		return gitTrust{}, nil, err
	}
	values := environmentMap(environment)
	credentialed := values[EnvGitUsername] != "" || values[EnvGitPassword] != "" || values[EnvGitSSHKeyFile] != ""
	if credentialed {
		if grant := strings.ToLower(values[EnvGitHostGrant]); grant == "" || grant != source.authority() {
			return gitTrust{}, nil, errors.New("the Git credential is not granted for the source's host")
		}
	} else if values[EnvGitHostGrant] != "" || values[EnvGitCASHA256Grant] != "" || values[EnvGitKnownHostsSHA256Grant] != "" {
		return gitTrust{}, nil, errors.New("a Git grant was supplied without a credential")
	}
	var trust gitTrust
	var created []string
	cleanup := func() {
		for _, file := range created {
			_ = os.Remove(file)
		}
	}
	for _, item := range []struct {
		source, grant, name string
		into                *string
	}{
		{values[EnvGitCASourceFile], values[EnvGitCASHA256Grant], "certificate authority", &trust.caFile},
		{values[EnvGitKnownHostsSourceFile], values[EnvGitKnownHostsSHA256Grant], "known hosts", &trust.knownHostsFile},
	} {
		if item.source == "" {
			if item.grant != "" {
				return gitTrust{}, nil, fmt.Errorf("a Git %s digest grant was supplied without the %s", item.name, item.name)
			}
			continue
		}
		bytes, err := readGitTrust(item.source, item.name)
		if err != nil {
			cleanup()
			return gitTrust{}, nil, err
		}
		if credentialed {
			sum := sha256.Sum256(bytes)
			if !validProtocolDigest(item.grant) || item.grant != "sha256:"+hex.EncodeToString(sum[:]) {
				cleanup()
				return gitTrust{}, nil, fmt.Errorf("the Git %s is not the one the credential's owner granted", item.name)
			}
		}
		snapshot, err := writeGitTrust(tempDir, bytes, item.name)
		if err != nil {
			cleanup()
			return gitTrust{}, nil, err
		}
		created = append(created, snapshot)
		*item.into = snapshot
	}
	if strings.HasPrefix(source.URL, "ssh://") && trust.knownHostsFile == "" {
		cleanup()
		return gitTrust{}, nil, errors.New("an ssh:// Git source needs known hosts")
	}
	return trust, cleanup, nil
}

func readGitTrust(file, name string) ([]byte, error) {
	opened, err := os.Open(file) // #nosec G304 -- the Job's own projection path
	if err != nil {
		return nil, fmt.Errorf("read the Git %s", name)
	}
	defer func() { _ = opened.Close() }()
	bytes, err := io.ReadAll(io.LimitReader(opened, maxGitTrustBytes+1))
	if err != nil || int64(len(bytes)) > maxGitTrustBytes || len(bytes) == 0 {
		return nil, fmt.Errorf("the Git %s is empty, unreadable or larger than %d bytes", name, maxGitTrustBytes)
	}
	return bytes, nil
}

func writeGitTrust(tempDir string, bytes []byte, name string) (string, error) {
	file, err := os.CreateTemp(tempDir, "ptah-git-trust-*")
	if err != nil {
		return "", fmt.Errorf("create the Git %s snapshot", name)
	}
	_, writeErr := file.Write(bytes)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Chmod(file.Name(), 0o400) != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("write the Git %s snapshot", name)
	}
	return filepath.Clean(file.Name()), nil
}

// GitPull is the Ptah command that fetches a Git source: a schema file to
// out, or a migration directory under it, reporting as JSON. With a commit it
// fetches that commit, by the ref that named it while the ref still does, and
// with an expected digest it refuses other content.
func GitPull(ptahBinary string, source GitSource, out string, trust gitTrust) (CommandSpec, error) {
	if err := source.validate(); err != nil {
		return CommandSpec{}, err
	}
	if !filepath.IsAbs(out) {
		return CommandSpec{}, errors.New("a Git pull needs an absolute output path")
	}
	family := "migrations"
	if source.Schema {
		family = "schema"
	}
	args := []string{family, "pull", source.reference(), "--path", source.Path}
	for _, flag := range []struct{ name, value string }{
		{"--branch", source.Branch}, {"--tag", source.Tag}, {"--commit", source.Commit},
		{"--expect-digest", source.ExpectedContentDigest},
		{"--ca-file", trust.caFile}, {"--known-hosts-file", trust.knownHostsFile},
	} {
		if flag.value != "" {
			args = append(args, flag.name, flag.value)
		}
	}
	args = append(args, "--out", out, "--json")
	return CommandSpec{Path: ptahBinary, Args: args}, nil
}
