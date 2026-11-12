package dataplane

import (
	"strconv"
	"strings"
)

// migrationRegistrationPrefix is how `migrations status` opens a failure to
// load the directory. The loader reads files and nothing else, and everything
// after the prefix is its sentence.
const migrationRegistrationPrefix = "error registering migrations: "

// wholeSchemaKind opens what `schema validate` says of a schema as a whole,
// after the dialect. A fault of one declaration names its kind and the object.
const wholeSchemaKind = "schema: "

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

// SchemaStructuralProblem reads what `schema validate` printed about a schema
// as a whole, such as a foreign key into a table it does not declare, when that
// fault is the one the command that failed stopped on: a `<dialect>: schema:
// <fault>` line, in the dialect the engine names, whose words end failed, the
// sentence the failed command's own document holds. Validate prints an index
// line first for each index it refuses, and the fault runs to the end of its
// output.
//
// Validate reports a fault of the file whether or not it stopped anything, and
// the command that failed may have stopped on the database or on another fault
// validate does not know. Comparing the words is what ties the two: drift and
// plan run the validator that wrote this line, and wrap its sentence and nothing
// else around it. An index line has no such tie, so it is not read: drift
// accepts an index on a column no table declares, and on a table none declares it
// fails in other words.
//
// failed is compared and never returned. Its command had a connection, so it
// could quote the database. What is returned comes from a run that had none.
func SchemaStructuralProblem(stdout []byte, engine, failed string) (string, bool) {
	text := strings.TrimSpace(string(stdout))
	for offset := 0; offset < len(text); {
		line, _, _ := strings.Cut(text[offset:], "\n")
		dialect, rest, found := strings.Cut(line, ": ")
		if found && dialectMatches(engine, dialect) && strings.HasPrefix(rest, wholeSchemaKind) {
			fault := text[offset+len(dialect)+len(": ")+len(wholeSchemaKind):]
			if !strings.HasSuffix(failed, ": "+fault) {
				return "", false
			}
			return fault, true
		}
		offset += len(line) + len("\n")
	}
	return "", false
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

// The words Ptah's row-file guard writes (stokaro/ptah#5560), and the one
// wrapper around them: drift reads declared rows after it compared the schema,
// and wraps the guard's sentence, where plan returns it as it is.
const (
	rowComparisonPrefix = "error comparing declared rows: "
	rowFilePrefix       = "managed data file "
	rowFileTable        = " for table "
	rowFileOutside      = " is outside allowed root "
	rowFileRooted       = "a path starting at a root"
)

// RowFileOutsideRoot reads the sentence a failed `schema drift` or `schema plan`
// holds when a `data` block names a row file outside the directory Ptah bounds
// row files by, which is its working directory. It returns the guard's sentence
// without drift's wrapper, so both commands give the same reason:
//
//	managed data file "../x.yaml" for table public.t: "/x.yaml" is outside allowed root "/work"
//	managed data file "/x.yaml" for table public.t: a path starting at a root is outside allowed root "/work"
//
// Every other sentence a validate run explains is copied from that child, which
// has no connection. This one is copied from the failed command, which had one,
// and so only the whole of it, in the guard's own form: the declared path, the
// resolved path and the root are each one quoted string, the resolved path and
// the root are absolute, and nothing follows the root. Ptah writes it from the
// declaration and the file system before it opens the file, so none of it is
// something the database answered, and a database's answer reaches a failed
// sentence in another form: another prefix, or a line of SQL after it.
func RowFileOutsideRoot(failed string) (string, bool) {
	if strings.ContainsAny(failed, "\r\n") {
		return "", false
	}
	sentence := strings.TrimPrefix(failed, rowComparisonPrefix)
	rest, found := strings.CutPrefix(sentence, rowFilePrefix)
	if !found {
		return "", false
	}
	declared, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return "", false
	}
	if rest, found = strings.CutPrefix(rest[len(declared):], rowFileTable); !found {
		return "", false
	}
	// The root is the last quoted string, and the sentence ends with it.
	outside := strings.LastIndex(rest, rowFileOutside)
	if outside < 0 || !absoluteQuoted(rest[outside+len(rowFileOutside):]) {
		return "", false
	}
	// Before it is the table, which Ptah does not quote, and either the path the
	// declaration resolved to, quoted, or the words for a path that starts at a
	// root. A quoted string holds no unescaped quote, so the resolved path starts
	// at the last ": \"" before the root.
	refused := rest[:outside]
	if table, found := strings.CutSuffix(refused, ": "+rowFileRooted); found && table != "" {
		return sentence, true
	}
	separator := strings.LastIndex(refused, `: "`)
	if separator <= 0 || !absoluteQuoted(refused[separator+len(": "):]) {
		return "", false
	}
	return sentence, true
}

// absoluteQuoted reports whether text is exactly one Go-quoted absolute path.
func absoluteQuoted(text string) bool {
	unquoted, err := strconv.Unquote(text)
	return err == nil && text[0] == '"' && strings.HasPrefix(unquoted, "/")
}
