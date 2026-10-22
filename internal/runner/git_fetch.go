package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const (
	// CodeGitSourceRefused prefixes every refusal of the Git fetch.
	CodeGitSourceRefused = "git_source_refused"
	// maxGitSourceBytes is what Ptah's pull admits of selected content, and
	// what the source volume holds.
	maxGitSourceBytes int64 = 64 << 20
	maxGitReportBytes int64 = 4 << 20
	maxGitStderrBytes int64 = 16 << 10
)

// FetchGitSource delivers the commit a Git source was resolved to into
// destination, for the operation container that runs after it. It holds the
// source to the credential owner's grants, has Ptah pull that commit and
// refuse other content, then reads what Ptah wrote back from disk and writes
// it to destination only if those bytes give the bound content digest. The
// container that holds the database URL never sees the credential, and reads
// only files that gave the digest.
func FetchGitSource(ctx context.Context, ptahBinary string, environment []string, destination, tempDir string, executor Executor, diagnostics io.Writer) error {
	source, isGit := GitSourceFromEnvironment(environment)
	switch {
	case !isGit:
		return fmt.Errorf("%s: no Git source is configured", CodeGitSourceRefused)
	case source.Commit == "" || source.ExpectedContentDigest == "":
		return fmt.Errorf("%s: a fetch needs the resolved commit and its content digest", CodeGitSourceRefused)
	case source.selects == "":
		return fmt.Errorf("%s: a fetch needs to know whether the path is a file or a directory", CodeGitSourceRefused)
	case !filepath.IsAbs(destination):
		return fmt.Errorf("%s: the destination is not an absolute path", CodeGitSourceRefused)
	}
	trust, cleanup, err := PrepareGitSourceAccess(source, environment, tempDir)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	defer cleanup()
	scratch, err := os.MkdirTemp(tempDir, "ptah-git-fetch-*")
	if err != nil {
		return fmt.Errorf("%s: create the fetch directory", CodeGitSourceRefused)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	out := filepath.Join(scratch, "migrations")
	if source.Schema {
		out = filepath.Join(scratch, path.Base(source.Path))
	}
	spec, err := GitPull(ptahBinary, source, out, trust)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	spec.Env = childEnvironment(environment)
	if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	stdout := newBoundedBuffer(maxGitReportBytes)
	stderr := newBoundedBuffer(maxGitStderrBytes)
	exitCode, err := executor.Execute(ctx, spec, stdout, stderr)
	if err != nil || exitCode != 0 {
		// Ptah's own message says why, and is what a person reads in the
		// Pod's status. It is redacted as the main run's output is.
		message, _ := sanitizedCapturedText(stderr.bytes(), NewRedactor(environment), false, maxErrorMessageBytes)
		if diagnostics != nil && message != "" {
			_, _ = io.WriteString(diagnostics, message+"\n")
		}
		if err != nil {
			return fmt.Errorf("%s: run the Git pull: %w", CodeGitSourceRefused, err)
		}
		return fmt.Errorf("%s: the Git pull exited with code %d", CodeGitSourceRefused, exitCode)
	}
	if stdout.dropped() > 0 {
		return fmt.Errorf("%s: the Git pull report is larger than %d bytes", CodeGitSourceRefused, maxGitReportBytes)
	}
	report, err := dataplane.DecodeGitPull(stdout.bytes(), source.reference(), source.Path)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	if report.Commit != source.Commit || report.ContentDigest != source.ExpectedContentDigest {
		return fmt.Errorf("%s: the pull read %s at %s, not %s at %s", CodeGitSourceRefused,
			report.ContentDigest, report.Commit, source.ExpectedContentDigest, source.Commit)
	}
	var files []sourcecontent.File
	if source.Schema {
		files, err = sourcecontent.ReadFile(out, path.Base(source.Path), maxGitSourceBytes)
	} else {
		files, err = sourcecontent.ReadTree(out, maxGitSourceBytes)
	}
	if err != nil {
		return fmt.Errorf("%s: read what the pull wrote: %w", CodeGitSourceRefused, err)
	}
	digest, err := sourcecontent.Digest(files)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	if digest != source.ExpectedContentDigest {
		return fmt.Errorf("%s: the files the pull wrote give %s, not %s", CodeGitSourceRefused, digest, source.ExpectedContentDigest)
	}
	if err := sourcecontent.Write(destination, files); err != nil {
		return fmt.Errorf("%s: %w", CodeGitSourceRefused, err)
	}
	return nil
}
