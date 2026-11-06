package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// CodeDesiredSourceUnreadable reports a desired schema or a migration directory
// Ptah could not read. Its message is Ptah's reason, from a run that held no
// credential and could not reach the target (see the checks below).
const CodeDesiredSourceUnreadable = "desired_source_unreadable"

const (
	// desiredSourceCheckTimeout bounds a check that reads local files. The
	// failure it explains has already happened, so a check that hangs costs
	// the explanation and nothing else.
	desiredSourceCheckTimeout = 30 * time.Second
	// maxDesiredSourceCheckBytes is more than a reason takes. A check that
	// prints more than this is not reporting a reason.
	maxDesiredSourceCheckBytes = 64 << 10
	// maxDesiredSourceReasonBytes keeps the reason within what the controller
	// keeps of a frame's message.
	maxDesiredSourceReasonBytes = 512
	// scratchDatabaseURL is the database a migration check opens: in memory,
	// private to the child, and gone when it exits. `migrations status` loads
	// the directory only after it has connected, so it needs one, and this one
	// holds nothing the child could quote.
	scratchDatabaseURL = "sqlite:///:memory:"
)

// reportUnreadableSchema asks Ptah whether the desired schema loads, in a child
// that holds no database URL, and reports the reason when it does not. It
// returns false, and changes nothing, when the file loads or the check says
// anything else, so a failure with another cause keeps the code it had.
//
// A failed command's own error sentence could say the same, and could as well
// quote what the database answered: the command had a connection, and nothing
// in the sentence separates the two. This child has no connection to quote.
func reportUnreadableSchema(
	ctx context.Context,
	config Config,
	environment []string,
	inputs Inputs,
	result *Result,
	redactor Redactor,
) bool {
	file := environmentMap(environment)[envSchemaFile]
	if file == "" || inputs.ExpectedDatabaseEngine == "" {
		return false
	}
	stdout, exitCode, finished := runDesiredSourceCheck(ctx, config, CommandSpec{
		Path: config.PtahBinary,
		Args: []string{"schema", "validate", "--dialect=" + inputs.ExpectedDatabaseEngine},
		Env:  desiredSourceCheckEnvironment(environment, envSchemaFile+"="+file),
	})
	// Validate exits 1 when it found a problem. Anything else is a run that
	// found none or could not look.
	if !finished || exitCode != 1 {
		return false
	}
	reason, unreadable := dataplane.SchemaSourceProblem(stdout, inputs.ExpectedDatabaseEngine)
	if !unreadable {
		return false
	}
	setDesiredSourceUnreadable(result, reason, redactor, config.Diagnostics)
	return true
}

// reportUnreadableMigrationFiles is reportUnreadableSchema for a migration
// directory. `migrations validate` does not answer this: it compares the
// directory with its ptah.sum and reads neither a file name nor a directive.
// `migrations status` does, and it is the command that failed, so the same
// command runs again on the same directory against a scratch database.
func reportUnreadableMigrationFiles(
	ctx context.Context,
	config Config,
	environment []string,
	inputs Inputs,
	result *Result,
	redactor Redactor,
) bool {
	if inputs.MigrationsDir == "" {
		return false
	}
	stdout, exitCode, finished := runDesiredSourceCheck(ctx, config, CommandSpec{
		Path: config.PtahBinary,
		Args: []string{"migrations", "status", "--db-url=" + scratchDatabaseURL, "--json"},
		Env:  desiredSourceCheckEnvironment(environment, envMigrationsDir+"="+inputs.MigrationsDir),
	})
	if !finished || exitCode == 0 {
		return false
	}
	reason, unreadable := dataplane.MigrationFilesProblem(stdout)
	if !unreadable {
		return false
	}
	setDesiredSourceUnreadable(result, reason, redactor, config.Diagnostics)
	return true
}

// desiredSourceCheckEnvironment is everything a check's child is told: where
// the files are, and where it may write. It is built up rather than filtered
// down, so a credential added to the Pod later does not reach a check. It is
// never empty, because an empty environment makes exec pass on the runner's
// whole environment, the database URL with it.
func desiredSourceCheckEnvironment(environment []string, source string) []string {
	values := environmentMap(environment)
	checked := []string{source}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if value := values[key]; value != "" {
			checked = append(checked, key+"="+value)
		}
	}
	return checked
}

// runDesiredSourceCheck runs one check and returns its standard output. What it
// writes to standard error is a count for a person, and is not read. It reports
// false when the child did not finish on its own or printed more than a reason
// takes.
func runDesiredSourceCheck(ctx context.Context, config Config, spec CommandSpec) ([]byte, int, bool) {
	checkContext, cancel := context.WithTimeout(ctx, desiredSourceCheckTimeout)
	defer cancel()
	stdout := newBoundedBuffer(maxDesiredSourceCheckBytes)
	exitCode, err := config.Executor.Execute(checkContext, spec, stdout, io.Discard)
	if err != nil || stdout.dropped() != 0 {
		return nil, exitCode, false
	}
	return stdout.bytes(), exitCode, true
}

// setDesiredSourceUnreadable reports the reason, redacted and bounded. The
// redactor knows the credentials of the Pod's environment, which the child does
// not, in case the file quotes one.
func setDesiredSourceUnreadable(result *Result, reason string, redactor Redactor, diagnostics io.Writer) {
	reason = redactor.Redact(reason)
	if len(reason) > maxDesiredSourceReasonBytes {
		reason = strings.ToValidUTF8(reason[:maxDesiredSourceReasonBytes], "")
	}
	setResultError(result, CodeDesiredSourceUnreadable, errors.New(reason), redactor, diagnostics)
}
