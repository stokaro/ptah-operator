package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// schemaParseFailure opens the one sentence of a failed OCI schema pull that
	// leaves the fetch container: Ptah's words for a layer it fetched and could
	// not parse. Ptah writes them after the registry has answered, and every
	// refusal of the registry opens with `pull <reference>:` instead.
	schemaParseFailure = "parse schema artifact: "
	// fetchReasonMarker opens that sentence in the termination message. The
	// runtime may put its own text in front, so the reader looks for it.
	fetchReasonMarker = "PTAH_RUNNER_FETCH_REASON_V1 "
	// ptahErrorPrefix is how Ptah opens what it prints when a command fails.
	ptahErrorPrefix = "error: "
	// maxSchemaFetchOutputBytes bounds what the runner keeps of each stream of
	// the pull. Ptah prints a few lines, and the sentence that may cross is the
	// first of them.
	maxSchemaFetchOutputBytes int64 = 16 << 10
)

// SchemaFetch is how a fetch of an OCI schema ended.
type SchemaFetch struct {
	// ExitCode is what the container exits with: what the pull exited with.
	ExitCode int
	// TerminationMessage is what the container leaves in its termination
	// message, and is empty for every failure but a schema the executor could
	// not parse.
	TerminationMessage []byte
}

// FetchOCISchema runs `ptah schema pull` for the Job's fetch step, with the
// credentials the step holds, and reports the one failure an author can act on.
//
// The pull parses the artifact before it writes it, so a schema from a newer
// Ptah than the executor stops here, in the container that holds the registry
// credentials. Nothing else of what this container's child prints leaves it:
// Ptah's sentence for any other failure can quote the registry and what it
// answered, and the redactor knows the credentials of the environment and not
// the one a mounted Docker config holds. Those sentences go to the container's
// log, where they went when Ptah was the container.
func FetchOCISchema(
	ctx context.Context,
	ptahBinary string,
	environment []string,
	reference, out string,
	executor Executor,
	diagnostics io.Writer,
) (SchemaFetch, error) {
	if err := validateReference(reference, "schema artifact reference"); err != nil {
		return SchemaFetch{}, err
	}
	if !filepath.IsAbs(out) {
		return SchemaFetch{}, errors.New("the schema output is not an absolute path")
	}
	spec := CommandSpec{
		Path: ptahBinary,
		Args: []string{"schema", "pull", reference, "--out", out},
		Env:  childEnvironment(environment),
	}
	if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
		return SchemaFetch{}, err
	}
	stdout := newBoundedBuffer(maxSchemaFetchOutputBytes)
	stderr := newBoundedBuffer(maxSchemaFetchOutputBytes)
	exitCode, err := executor.Execute(ctx, spec, stdout, stderr)
	redactor := NewRedactor(environment)
	for _, stream := range []*boundedBuffer{stdout, stderr} {
		if line := freeText(string(stream.bytes()), redactor, maxErrorMessageBytes); line != "" && diagnostics != nil {
			_, _ = io.WriteString(diagnostics, line+"\n")
		}
	}
	if err != nil {
		return SchemaFetch{}, fmt.Errorf("run the schema pull: %w", err)
	}
	fetched := SchemaFetch{ExitCode: exitCode}
	if exitCode != 0 {
		if reason := schemaParseReason(stderr.bytes(), redactor); reason != "" {
			fetched.TerminationMessage = []byte(fetchReasonMarker + reason + "\n")
		}
	}
	return fetched, nil
}

// schemaParseReason reads the sentence a failed pull may carry out of its
// container, and returns nothing for any other failure.
//
// Standard error has to open with the words. A registry that answers with a
// line break and `parse schema artifact: <anything>` writes that into a later
// line, after `error: pull <reference>:`, so a search of every line would let
// the registry choose what crosses. The first line is all that crosses of a
// sentence that does qualify. It is redacted while the text is still whole, since
// a credential a line break splits no longer matches.
func schemaParseReason(stderr []byte, redactor Redactor) string {
	first, _, _ := strings.Cut(redactor.redactFolded(string(stderr)), "\n")
	sentence, opened := strings.CutPrefix(first, ptahErrorPrefix)
	if !opened {
		return ""
	}
	reason := freeText(sentence, redactor, maxDesiredSourceReasonBytes)
	if !strings.HasPrefix(reason, schemaParseFailure) {
		return ""
	}
	return reason
}

// ParseFetchReason reads the sentence out of a schema fetch's termination
// message, and reports false for a message that does not hold one this build
// can stand behind.
//
// The runner only ever writes one sentence, one line, bounded and already
// handled by freeText. The reader holds the message to that rather than to where
// it came from: status repeats whatever this returns, so a message cut short, a
// second marker a file quoted, or a sentence about anything other than the parse
// reads as no reason at all.
func ParseFetchReason(message string) (string, bool) {
	start := strings.Index(message, fetchReasonMarker)
	if start < 0 {
		return "", false
	}
	reason := strings.TrimSuffix(message[start+len(fetchReasonMarker):], "\n")
	switch {
	case !strings.HasPrefix(reason, schemaParseFailure),
		strings.Contains(reason, fetchReasonMarker),
		len(reason) > maxDesiredSourceReasonBytes,
		!utf8.ValidString(reason),
		strings.ContainsFunc(reason, func(character rune) bool {
			return unicode.IsControl(character) || unicode.Is(unicode.Cf, character)
		}):
		return "", false
	}
	return reason, true
}
