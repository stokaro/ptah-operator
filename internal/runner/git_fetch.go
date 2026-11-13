package runner

import (
	"context"
	"fmt"
	"io"
)

// CodeGitSourceRefused prefixes every refusal of the Git fetch.
const CodeGitSourceRefused = "git_source_refused"

// FetchGitSource delivers the commit a Git source was resolved to into
// destination, for the operation container that runs after it: Ptah pulls
// that commit held to the bound content digest, and the files reach
// destination only if the bytes Ptah wrote give it again.
func FetchGitSource(ctx context.Context, ptahBinary string, environment []string, destination, tempDir string, executor Executor, diagnostics io.Writer) error {
	environment, err := ResolveCredentialFiles(environment)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	source, isGit := GitSourceFromEnvironment(environment)
	if !isGit {
		return fmt.Errorf("%s: no Git source is configured", CodeGitSourceRefused)
	}
	if _, isHTTPS := HTTPSSourceFromEnvironment(environment); isHTTPS {
		return fmt.Errorf("%s: the fetch names an HTTPS source as well", CodeGitSourceRefused)
	}
	return fetchRemoteSource(ctx, ptahBinary, environment, source, destination, tempDir, executor, diagnostics)
}
