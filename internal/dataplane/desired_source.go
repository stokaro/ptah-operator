package dataplane

import "strings"

// migrationRegistrationPrefix is how `migrations status` opens a failure to
// load the directory. The loader reads files and nothing else, and everything
// after the prefix is its sentence.
const migrationRegistrationPrefix = "error registering migrations: "

// SchemaSourceProblem reads what `schema validate` printed about a desired
// schema it could not load: `<dialect>: source: <reason>`, in the dialect the
// engine names. The reason keeps Ptah's words on one line.
//
// Only that shape is read. Validate also reports structural problems, a
// `schema` or an `index` line, and `schema drift` and `schema plan` do not
// refuse all of those, so such a line does not say why a command failed. A
// load failure does: both commands load the same file before they compare
// anything.
func SchemaSourceProblem(stdout []byte, engine string) (string, bool) {
	dialect, rest, found := strings.Cut(strings.TrimSpace(string(stdout)), ": ")
	if !found || !dialectMatches(engine, dialect) {
		return "", false
	}
	reason, isSource := strings.CutPrefix(rest, "source: ")
	if !isSource {
		return "", false
	}
	return oneLine(reason)
}

// MigrationFilesProblem reads the failure document `migrations status --json`
// printed about a migration directory it could not load. Any other failure,
// including one that names a target failure kind, is not about the files.
func MigrationFilesProblem(stdout []byte) (string, bool) {
	failure, err := DecodeMigrationStatusFailure(stdout)
	if err != nil || failure.Kind != "" {
		return "", false
	}
	reason, loading := strings.CutPrefix(failure.Message, migrationRegistrationPrefix)
	if !loading {
		return "", false
	}
	return oneLine(reason)
}

// oneLine joins a reason that spans lines, as a YAML parser's does, so that it
// reads as one sentence in a condition. An empty reason is no reason.
func oneLine(reason string) (string, bool) {
	joined := strings.Join(strings.Fields(reason), " ")
	return joined, joined != ""
}
