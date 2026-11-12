package runner

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// CodeDesiredSourceUnreadable reports a desired schema or a migration directory
// Ptah could not read. Its message is Ptah's reason, from a run that held no
// credential and could not reach the target (see the checks below).
const CodeDesiredSourceUnreadable = "desired_source_unreadable"

// CodeDesiredSchemaInvalid reports a desired schema Ptah read and refuses as a
// whole, such as a foreign key into a table it does not declare, when that
// fault stopped the command that failed. Its message comes from the same run.
const CodeDesiredSchemaInvalid = "desired_schema_invalid"

const (
	// desiredSourceCheckTimeout bounds a check that reads local files. The
	// failure it explains has already happened, so a check that hangs costs
	// the explanation and nothing else.
	desiredSourceCheckTimeout = 30 * time.Second
	// maxDesiredSourceCheckBytes is more than a reason takes. A check that
	// prints more than this is not reporting a reason.
	maxDesiredSourceCheckBytes = 64 << 10
	// maxDesiredSourceReasonBytes is what fits an Event with the longer code in
	// front: the controller keeps 512 bytes of the whole sentence there, so a
	// longer reason would read differently in the condition and the Event.
	maxDesiredSourceReasonBytes = 512 - len(CodeDesiredSourceUnreadable) - len(": ")
	// scratchDatabaseURL is the database a migration check opens: in memory,
	// private to the child, and gone when it exits. `migrations status` loads
	// the directory only after it has connected, so it needs one, and this one
	// holds nothing the child could quote.
	scratchDatabaseURL = "sqlite:///:memory:"
)

// reportSchemaProblem asks Ptah what is wrong with the desired schema, in a
// child that holds no database URL. It reports a file that does not load, and a
// fault of the schema as a whole when failed, the failed command's own sentence,
// ends with the same words. It returns false, and changes nothing, when the file
// is sound or the check says anything else, so a failure with another cause
// keeps the code it had.
//
// The failed command's sentence could say the same, and could as well quote what
// the database answered: the command had a connection, and nothing in the
// sentence separates the two. So it is compared and not copied, and every word
// that leaves comes from this child, which has no connection to quote.
//
// One sentence is copied: a row file refused as outside Ptah's working
// directory. The check cannot say it, since validate reads no row file, and
// Ptah's guard writes it from the declaration and the file system alone, so
// dataplane.RowFileOutsideRoot takes it only whole and in the guard's form. It
// names the paths inside this container that the declaration led to, and no
// child runs to explain it.
func reportSchemaProblem(
	ctx context.Context,
	config Config,
	environment []string,
	inputs Inputs,
	failed string,
	result *Result,
	redactor Redactor,
) bool {
	file := environmentMap(environment)[envSchemaFile]
	if file == "" || inputs.ExpectedDatabaseEngine == "" {
		return false
	}
	// The sentence names the row file and not the schema that declares it.
	if reason, outside := dataplane.RowFileOutsideRoot(failed); outside {
		return setDesiredSourceProblem(result, CodeDesiredSourceUnreadable, path.Base(file)+": "+reason, redactor, config.Diagnostics)
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
	code := CodeDesiredSourceUnreadable
	reason, found := dataplane.SchemaSourceProblem(stdout, inputs.ExpectedDatabaseEngine)
	if !found {
		code = CodeDesiredSchemaInvalid
		reason, found = dataplane.SchemaStructuralProblem(stdout, inputs.ExpectedDatabaseEngine, failed)
	}
	if !found {
		return false
	}
	// A SQL parser names the file and a YAML parser or the validator does not,
	// so the name goes in front only where Ptah's words leave it out.
	if name := path.Base(file); !strings.Contains(reason, name) {
		reason = name + ": " + reason
	}
	return setDesiredSourceProblem(result, code, reason, redactor, config.Diagnostics)
}

// reportUnreadableMigrationFiles is reportSchemaProblem for a migration
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
	return setDesiredSourceProblem(result, CodeDesiredSourceUnreadable, reason, redactor, config.Diagnostics)
}

// desiredSourceCheckEnvironment is everything a check's child is told: where
// the files are, and where it may write. It is built up rather than filtered
// down, so a credential added to the Pod later does not reach a check.
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

// setDesiredSourceProblem reports the reason, and reports false, changing
// nothing, when none of it is readable. The redactor knows the credentials of
// the Pod's environment, which the child does not, in case the file quotes one.
func setDesiredSourceProblem(result *Result, code, reason string, redactor Redactor, diagnostics io.Writer) bool {
	text := freeText(reason, redactor, maxDesiredSourceReasonBytes)
	if text == "" {
		return false
	}
	setResultError(result, code, errors.New(text), redactor, diagnostics)
	return true
}
