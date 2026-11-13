package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const (
	// maxRemoteSourceBytes is what Ptah's pull admits of selected content,
	// and what the source volume holds.
	maxRemoteSourceBytes int64 = 64 << 20
	maxRemoteReportBytes int64 = 4 << 20
	maxRemoteStderrBytes int64 = 16 << 10
	// maxTrustFileBytes bounds a CA or known-hosts file a remote source
	// names.
	maxTrustFileBytes int64 = 1 << 20
	// maxPullRefusalBytes bounds the reason a failed pull reports.
	maxPullRefusalBytes = 512
)

// remoteSource is a source Ptah pulls over the network: one commit of a Git
// repository, or one HTTPS download. A Resolve pulls it to learn what it
// holds, and the fetch before every later operation pulls it again, held to
// what the Resolve found. The runner holds both to the credential owner's
// grants before Ptah runs, decodes Ptah's report, and holds the report to the
// pull it answers; what differs between the kinds is behind this interface.
type remoteSource interface {
	// kind names the source in messages: "Git" or "HTTPS".
	kind() string
	// selectsFile reports whether the path is a schema file rather than a
	// migration directory, and selectsKnown whether the manager said.
	selectsFile() bool
	selectsKnown() bool
	// selected is the path the source selects.
	selected() string
	// contentDigest is the content digest a fetch has to give.
	contentDigest() string
	// fetchable refuses a source a fetch could not hold to a resolution.
	fetchable() error
	// prepare holds the source to the credential owner's grants, copies the
	// trust it names to private files, and returns the pull Ptah runs under
	// that trust and the cleanup that removes the copies.
	prepare(environment []string, tempDir string) (pull func(ptahBinary, out string) (CommandSpec, error), cleanup func(), err error)
	// request is what the pull asked for, which its report has to answer.
	request() dataplane.PullRequest
	// holdsResolve refuses a Resolve's report that read other than the
	// source pins, and holdsFetch a fetch's report that read other than the
	// resolution.
	holdsResolve(report dataplane.SourcePullReport) error
	holdsFetch(report dataplane.SourcePullReport) error
	// record writes what a Resolve found into its result.
	record(result *Result, report dataplane.SourcePullReport)
	// accessCode is the result code of a source its grants refuse.
	accessCode() string
	// refusedCode prefixes every refusal of the fetch.
	refusedCode() string
}

// remoteSourceFromEnvironment reads the remote source an operation names, if
// any. A Job names one kind at most; naming both is refused.
func remoteSourceFromEnvironment(environment []string) (remoteSource, bool, error) {
	git, isGit := GitSourceFromEnvironment(environment)
	https, isHTTPS := HTTPSSourceFromEnvironment(environment)
	switch {
	case isGit && isHTTPS:
		return nil, true, errors.New("an operation names a Git and an HTTPS source")
	case isGit:
		return git, true, nil
	case isHTTPS:
		return https, true, nil
	}
	return nil, false, nil
}

// pullOutput is where a pull writes inside scratch: the schema file under its
// own name, whose extension says its format, or the migration directory.
func pullOutput(scratch string, source remoteSource) string {
	if source.selectsFile() {
		return filepath.Join(scratch, path.Base(source.selected()))
	}
	return filepath.Join(scratch, "migrations")
}

// fetchRemoteSource delivers what a remote source was resolved to into
// destination, for the operation container that runs after it. It holds the
// source to the credential owner's grants, has Ptah pull it and refuse other
// content, then reads what Ptah wrote back from disk and writes it to
// destination only if those bytes give the bound content digest. The
// container that holds the database URL never sees the credential, and reads
// only files that gave the digest.
func fetchRemoteSource(
	ctx context.Context,
	ptahBinary string,
	environment []string,
	source remoteSource,
	destination, tempDir string,
	executor Executor,
	diagnostics io.Writer,
) error {
	refused := source.refusedCode()
	switch {
	case source.fetchable() != nil:
		return fmt.Errorf("%s: %w", refused, source.fetchable())
	case !source.selectsKnown():
		return fmt.Errorf("%s: a fetch needs to know whether the path is a file or a directory", refused)
	case !filepath.IsAbs(destination):
		return fmt.Errorf("%s: the destination is not an absolute path", refused)
	}
	pull, cleanup, err := source.prepare(environment, tempDir)
	if err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	defer cleanup()
	scratch, err := os.MkdirTemp(tempDir, "ptah-"+strings.ToLower(source.kind())+"-fetch-*")
	if err != nil {
		return fmt.Errorf("%s: create the fetch directory", refused)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	out := pullOutput(scratch, source)
	spec, err := pull(ptahBinary, out)
	if err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	spec.Env = childEnvironment(environment)
	if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	stdout := newBoundedBuffer(maxRemoteReportBytes)
	stderr := newBoundedBuffer(maxRemoteStderrBytes)
	exitCode, err := executor.Execute(ctx, spec, stdout, stderr)
	if err != nil || exitCode != 0 {
		// Ptah's own message says why, and is what a person reads in the
		// Pod's status. It is redacted as the main run's output is.
		redactor := NewRedactor(environment)
		message := freeText(string(stderr.bytes()), redactor, maxErrorMessageBytes)
		if diagnostics != nil && message != "" {
			_, _ = io.WriteString(diagnostics, message+"\n")
		}
		if err != nil {
			return fmt.Errorf("%s: run the %s pull: %w", refused, source.kind(), err)
		}
		return fmt.Errorf("%s: %w", refused, pullExit(exitCode, stderr.bytes(), redactor))
	}
	if stdout.dropped() > 0 {
		return fmt.Errorf("%s: the %s pull report is larger than %d bytes", refused, source.kind(), maxRemoteReportBytes)
	}
	report, err := dataplane.DecodeSourcePull(stdout.bytes(), source.request())
	if err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	if err := source.holdsFetch(report); err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	var files []sourcecontent.File
	if source.selectsFile() {
		files, err = sourcecontent.ReadFile(out, path.Base(source.selected()), maxRemoteSourceBytes)
	} else {
		files, err = sourcecontent.ReadTree(out, maxRemoteSourceBytes)
	}
	if err != nil {
		return fmt.Errorf("%s: read what the pull wrote: %w", refused, err)
	}
	digest, err := sourcecontent.Digest(files)
	if err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	if digest != source.contentDigest() {
		return fmt.Errorf("%s: the files the pull wrote give %s, not %s", refused, digest, source.contentDigest())
	}
	if err := sourcecontent.Write(destination, files); err != nil {
		return fmt.Errorf("%s: %w", refused, err)
	}
	return nil
}

// runRemoteResolve pulls what a remote source names, through Ptah's pull,
// into a directory of its own that nothing else reads, and reports what the
// pull found and the content digest of the selected files. Every later
// operation pulls it again and is held to that digest.
func runRemoteResolve(ctx context.Context, config Config, environment []string, source remoteSource, redactor Redactor, result Result) Result {
	pull, cleanup, err := source.prepare(environment, config.TempDir)
	if err != nil {
		setResultError(&result, source.accessCode(), err, redactor, config.Diagnostics)
		return result
	}
	defer cleanup()
	scratch, err := os.MkdirTemp(config.TempDir, "ptah-"+strings.ToLower(source.kind())+"-resolve-*")
	if err != nil {
		setResultError(&result, "execution_error", fmt.Errorf("create the %s resolve directory", source.kind()), redactor, config.Diagnostics)
		return result
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	spec, err := pull(config.PtahBinary, pullOutput(scratch, source))
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	spec.Env = childEnvironment(environment)
	if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	stdout, stderr := newBoundedBuffer(config.MaxResultBytes), newBoundedBuffer(maxRemoteStderrBytes)
	exitCode, err := config.Executor.Execute(ctx, spec, stdout, stderr)
	result.ChildExitCode = exitCode
	if err == nil && exitCode != 0 {
		setResultError(&result, "child_exit", pullExit(exitCode, stderr.bytes(), redactor), redactor, config.Diagnostics)
		return result
	}
	if !outcomeSucceeded(&result, commandOutcome{exitCode: exitCode, err: err, stdout: stdout, stderr: stderr}, redactor, config.Diagnostics) {
		return result
	}
	if stdout.dropped() > 0 {
		setResultError(&result, "invalid_resolve_output", fmt.Errorf("the %s pull report exceeded the result limit", source.kind()), redactor, config.Diagnostics)
		return result
	}
	report, err := dataplane.DecodeSourcePull(stdout.bytes(), source.request())
	if err != nil {
		setResultError(&result, "invalid_resolve_output", err, redactor, config.Diagnostics)
		return result
	}
	if err := source.holdsResolve(report); err != nil {
		setResultError(&result, "invalid_resolve_output", err, redactor, config.Diagnostics)
		return result
	}
	source.record(&result, report)
	result.ResolvedContentDigest = report.ContentDigest
	for _, file := range report.Files {
		result.ResolvedContentBytes += file.Size
	}
	return result
}

// pullExit says why a pull failed: its exit status and the last line Ptah
// wrote to standard error, which names the refusal -- a submodule or an LFS
// pointer at the selected path, a host key that does not match, a download
// whose digest differs, an archive entry Ptah refuses. Nothing else of
// standard error is kept.
func pullExit(exitCode int, stderr []byte, redactor Redactor) error {
	message, _ := sanitizedCapturedText(stderr, redactor, false, maxErrorMessageBytes)
	reason := ""
	lines := strings.Split(message, "\n")
	for index := len(lines) - 1; index >= 0 && reason == ""; index-- {
		reason = freeText(lines[index], redactor, maxErrorMessageBytes)
		if prefix := "error:"; len(reason) >= len(prefix) && strings.EqualFold(reason[:len(prefix)], prefix) {
			reason = strings.TrimSpace(reason[len(prefix):])
		}
	}
	reason = freeText(reason, redactor, maxPullRefusalBytes)
	if reason == "" {
		return fmt.Errorf("ptah exited with code %d", exitCode)
	}
	return fmt.Errorf("ptah exited with code %d: %s", exitCode, reason)
}

// readTrustFile reads a CA or known-hosts file a remote source names, from
// the Job's own projection.
func readTrustFile(file, kind, name string) ([]byte, error) {
	opened, err := os.Open(file) // #nosec G304 -- the Job's own projection path
	if err != nil {
		return nil, fmt.Errorf("read the %s %s", kind, name)
	}
	defer func() { _ = opened.Close() }()
	bytes, err := io.ReadAll(io.LimitReader(opened, maxTrustFileBytes+1))
	if err != nil || int64(len(bytes)) > maxTrustFileBytes || len(bytes) == 0 {
		return nil, fmt.Errorf("the %s %s is empty, unreadable or larger than %d bytes", kind, name, maxTrustFileBytes)
	}
	return bytes, nil
}

// writeTrustFile writes checked trust bytes to a private, read-only file
// Ptah reads instead of the mutable projection.
func writeTrustFile(tempDir string, bytes []byte, kind, name string) (string, error) {
	file, err := os.CreateTemp(tempDir, "ptah-"+strings.ToLower(kind)+"-trust-*")
	if err != nil {
		return "", fmt.Errorf("create the %s %s snapshot", kind, name)
	}
	_, writeErr := file.Write(bytes)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Chmod(file.Name(), 0o400) != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("write the %s %s snapshot", kind, name)
	}
	return filepath.Clean(file.Name()), nil
}
