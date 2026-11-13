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
// after the dialect. A fault of one declaration names its kind and the object,
// and indexKind opens the fault of one index.
const (
	wholeSchemaKind = "schema: "
	indexKind       = "index "
)

// SchemaSourceProblem reads what `schema validate` printed about a desired
// schema it could not load: `<dialect>: source: <reason>`, in the dialect the
// engine names. The reason is as Ptah printed it, across lines if it spans them:
// what leaves the runner is the runner's to make, after it has redacted the words.
//
// Only that shape is read here. Validate also reports structural problems, a
// `schema` or an `index` line, which say why a command failed only where that
// command stopped on them; SchemaStructuralProblem reads those. A load failure
// says it of every command: drift and plan load the same file before they
// compare anything.
func SchemaSourceProblem(stdout []byte, engine string) (string, bool) {
	dialect, rest, found := strings.Cut(strings.TrimSpace(string(stdout)), ": ")
	if !found || !dialectMatches(engine, dialect) {
		return "", false
	}
	return strings.CutPrefix(rest, "source: ")
}

// SchemaStructuralProblem reads what `schema validate` printed about the
// schema's structure when that fault is the one the command that failed stopped
// on, in the dialect the engine names. failed is the sentence the failed
// command's own document holds.
//
// Validate prints a line for each index it refuses first, and then a fault of
// the schema as a whole, such as a foreign key into a table it does not
// declare, which runs to the end of its output. Since Ptah v0.26.0 drift and
// plan check every index before they compare anything (stokaro/ptah#5845), and
// the first one they refuse stops them with validate's line, dialect and all.
// So an `<dialect>: index <name>: <fault>` line is read when failed is that
// line and nothing else, and the reason is the line after the dialect. A
// `<dialect>: schema: <fault>` line is read when failed ends with its words:
// drift and plan run the validator that wrote it, and wrap its sentence and
// nothing else around it. The reason is then the fault.
//
// Validate reports a fault of the file whether or not it stopped anything, and
// the command that failed may have stopped on the database or on another fault
// validate does not know. Comparing the words is what ties the two.
//
// failed is compared and never returned. Its command had a connection, so it
// could quote the database. What is returned comes from a run that had none.
func SchemaStructuralProblem(stdout []byte, engine, failed string) (string, bool) {
	text := strings.TrimSpace(string(stdout))
	for offset := 0; offset < len(text); {
		line, _, _ := strings.Cut(text[offset:], "\n")
		dialect, rest, found := strings.Cut(line, ": ")
		switch {
		case !found || !dialectMatches(engine, dialect):
		case strings.HasPrefix(rest, indexKind) && line == failed:
			return rest, true
		case strings.HasPrefix(rest, wholeSchemaKind):
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
