package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// The variables a Job names a credential file by. The kubelet projects each
// file from its Secret into a read-only volume, and the container's
// environment holds only the path.
//
// A credential the kubelet resolves into a container's environment stays in
// the container runtime's record of that container until the Pod is removed,
// for anyone with root on the node to read, and a failed Apply's Pod is kept a
// day. A Secret volume is unmounted when the Pod stops. So the Job carries the
// path, the runner reads the file, and the value reaches Ptah only in the
// environment of the child process the runner starts, which ends with it:
// Ptah v0.23.0 takes the database URL from --db-url or PTAH_DB_URL and nothing
// else, and a command line is visible to every process in the Pod.
const (
	// EnvDatabaseURLFile names the file holding the target database URL.
	EnvDatabaseURLFile = "PTAH_OPERATOR_DB_URL_FILE"
	// EnvDevelopmentDatabaseURLFile names the file holding the development
	// database URL a Plan may use.
	EnvDevelopmentDatabaseURLFile = "PTAH_OPERATOR_DEV_URL_FILE"
	// EnvOCIUsernameFile, EnvOCIPasswordFile and EnvOCITokenFile name the
	// files an Environment-mode registry credential is projected into.
	EnvOCIUsernameFile = "PTAH_OPERATOR_OCI_USERNAME_FILE"
	EnvOCIPasswordFile = "PTAH_OPERATOR_OCI_PASSWORD_FILE" // #nosec G101 -- the name of a variable, not a credential
	EnvOCITokenFile    = "PTAH_OPERATOR_OCI_TOKEN_FILE"    // #nosec G101 -- the name of a variable, not a credential
	// EnvGitUsernameFile and EnvGitPasswordFile name the files an https:// Git
	// credential is projected into.
	EnvGitUsernameFile = "PTAH_OPERATOR_GIT_USERNAME_FILE"
	EnvGitPasswordFile = "PTAH_OPERATOR_GIT_PASSWORD_FILE" // #nosec G101 -- the name of a variable, not a credential

	// envOCIUsername is the variable Ptah reads a registry username from.
	envOCIUsername = "PTAH_OCI_USERNAME"

	// maxCredentialFileBytes bounds one credential file. The kernel refuses to
	// start a process with any one environment string over 128 KiB, and a
	// Secret holds at most 1 MiB, so a larger file is refused here, by name,
	// rather than by the exec of the child.
	maxCredentialFileBytes = 64 << 10
)

// CredentialFile pairs a variable that names a credential file with the
// variable Ptah reads the credential from.
type CredentialFile struct {
	// File is the variable holding the file's path.
	File string
	// Value is the variable Ptah reads.
	Value string
}

// CredentialFiles is every credential a Job may name by file. The workload
// builder writes these pairs and nothing else, and the runner reads them and
// nothing else.
func CredentialFiles() []CredentialFile {
	return []CredentialFile{
		{File: EnvDatabaseURLFile, Value: EnvDatabaseURL},
		{File: EnvDevelopmentDatabaseURLFile, Value: EnvDevelopmentDatabaseURL},
		{File: EnvOCIUsernameFile, Value: envOCIUsername},
		{File: EnvOCIPasswordFile, Value: EnvOCIPassword},
		{File: EnvOCITokenFile, Value: EnvOCIToken},
		{File: EnvGitUsernameFile, Value: EnvGitUsername},
		{File: EnvGitPasswordFile, Value: EnvGitPassword},
	}
}

// ResolveCredentialFiles returns environment with every credential the Job
// names by file read into the variable Ptah reads, and the variables naming
// the files removed. Everything after it -- the Redactor, the target identity
// and the child's environment -- reads the credential from the returned
// environment, so what is redacted is the value the child receives.
//
// A file the kubelet did not write is a credential the Secret does not hold:
// an optional key it left out. The variable stays unset, as it did when the
// kubelet resolved an optional key into the environment, and whatever needs
// it refuses as it did then. A Job that names a credential both ways, a path
// that is not absolute, a file that cannot be read, holds a NUL byte or is
// larger than an environment string may be is refused, by the variable's
// name and never by its content.
//
// A Job built before credentials moved to files names none, and its
// environment is returned unchanged.
func ResolveCredentialFiles(environment []string) ([]string, error) {
	values := environmentMap(environment)
	files := CredentialFiles()
	names := make([]string, 0, len(files))
	resolved := make([]string, 0, len(files))
	for _, credential := range files {
		path, named := values[credential.File]
		if !named {
			continue
		}
		names = append(names, credential.File)
		if _, direct := values[credential.Value]; direct {
			return nil, fmt.Errorf("%s is set and %s names a file for it as well", credential.Value, credential.File)
		}
		value, present, err := readCredentialFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", credential.File, err)
		}
		if present {
			resolved = append(resolved, credential.Value+"="+value)
		}
	}
	if len(names) == 0 {
		return append([]string(nil), environment...), nil
	}
	return append(environmentWithout(environment, names...), resolved...), nil
}

func readCredentialFile(path string) (string, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false, errors.New("the credential path is not a clean absolute path")
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errors.New("the credential file cannot be opened")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false, errors.New("the credential path is not a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return "", false, errors.New("the credential file cannot be read")
	}
	if len(content) > maxCredentialFileBytes {
		return "", false, fmt.Errorf("the credential file is larger than %d bytes", maxCredentialFileBytes)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return "", false, errors.New("the credential file holds a NUL byte, which no environment variable can carry")
	}
	return string(content), true, nil
}

// credentialFileVariables are the variables naming credential files, which no
// child receives: ResolveCredentialFiles removes them, and childEnvironment
// removes them again from an environment that was never resolved.
func credentialFileVariables() []string {
	files := CredentialFiles()
	names := make([]string, 0, len(files))
	for _, credential := range files {
		names = append(names, credential.File)
	}
	return names
}
