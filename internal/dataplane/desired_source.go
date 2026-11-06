package dataplane

import "strings"

// migrationRegistrationPrefix is how `migrations status` opens a failure to
// load the directory. The loader reads files and nothing else, and everything
// after the prefix is its sentence.
const migrationRegistrationPrefix = "error registering migrations: "

// SchemaSourceProblem reads what `schema validate` printed about a desired
// schema it could not load: `<dialect>: source: <reason>`, in the dialect the
// engine names. The reason is as Ptah printed it, across lines if it spans them:
// what leaves the runner is the runner's to make, after it has redacted the words.
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
	return strings.CutPrefix(rest, "source: ")
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
	reason = strings.TrimSpace(reason)
	return reason, reason != ""
}
