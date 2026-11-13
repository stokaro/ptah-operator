package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

// CodeOCIFetchRefused prefixes every refusal of the OCI fetch wrapper.
const CodeOCIFetchRefused = "oci_fetch_refused"

// FetchOCISource runs the pull an OCI fetch container exists for, with the
// registry credential the Job names by file.
//
// The fetch is Ptah's own pull, which verifies the digest-pinned artifact and
// writes it into the source volume. Ptah reads a registry credential from its
// environment, and the kubelet would have to put it there from the container
// spec, where the container runtime keeps it after the container stops. So the
// runner reads the files the kubelet projected, starts Ptah with the
// credential in the child's environment only, and returns Ptah's exit status.
// Ptah's output goes to the container's own streams, as it did when the fetch
// was Ptah alone.
//
// Only the two pulls the workload builder writes are run: `schema pull` and
// `migrations pull`, with the arguments the manager built.
func FetchOCISource(
	ctx context.Context,
	ptahBinary string,
	environment []string,
	arguments []string,
	executor Executor,
	stdout, stderr io.Writer,
) (int, error) {
	if len(arguments) < 2 || arguments[1] != "pull" || !slices.Contains([]string{"schema", "migrations"}, arguments[0]) {
		return -1, fmt.Errorf("%s: only schema pull and migrations pull are fetched", CodeOCIFetchRefused)
	}
	if ptahBinary == "" {
		return -1, fmt.Errorf("%s: the Ptah binary path is empty", CodeOCIFetchRefused)
	}
	environment, err := ResolveCredentialFiles(environment)
	if err != nil {
		return -1, fmt.Errorf("%s: %w", CodeOCIFetchRefused, err)
	}
	if err := ensureNoCredentialsInArguments(arguments, environmentMap(environment)); err != nil {
		return -1, fmt.Errorf("%s: %w", CodeOCIFetchRefused, err)
	}
	if executor == nil {
		return -1, errors.New(CodeOCIFetchRefused + ": no executor")
	}
	spec := CommandSpec{Path: ptahBinary, Args: slices.Clone(arguments), Env: childEnvironment(environment)}
	exitCode, err := executor.Execute(ctx, spec, stdout, stderr)
	if err != nil {
		return -1, fmt.Errorf("%s: run the pull: %w", CodeOCIFetchRefused, err)
	}
	return exitCode, nil
}
