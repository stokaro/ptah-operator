package dataplane

import (
	"regexp"
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
// line and nothing else, apart from the schema a name may carry (below), and
// the reason is validate's line after the dialect. A
// `<dialect>: schema: <fault>` line is read when failed ends with its words:
// drift and plan run the validator that wrote it, and wrap its sentence and
// nothing else around it. The reason is then the fault.
//
// Validate reports a fault of the file whether or not it stopped anything, and
// the command that failed may have stopped on the database or on another fault
// validate does not know. Comparing the words is what ties the two. On a
// PostgreSQL URL that pins no search_path, plan since Ptah v0.30.0
// (stokaro/ptah#6132) and drift since v0.30.1 (stokaro/ptah#6204) place every
// name the file writes without a schema in the schema the database resolves it
// to, and a sentence can name a table that way, as "public.parent", where
// validate, which has no database, prints "parent". Drift does in both kinds of
// line, and plan does in a schema fault and, since v0.32.0, in an index line
// as well (stokaro/ptah#6219). Both place a table and nothing else: an index, a
// column or a constraint keeps the name validate prints. So on PostgreSQL a
// table validate names without a schema matches the same table with a schema
// in front of it, in both kinds of line, and the tables one sentence places
// share one schema (see placedFault). On MySQL the words have to be
// validate's.
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
		case strings.HasPrefix(rest, indexKind) && faultIs(failed, line, engine):
			return rest, true
		case strings.HasPrefix(rest, wholeSchemaKind):
			fault := text[offset+len(dialect)+len(": ")+len(wholeSchemaKind):]
			if !faultEnds(failed, fault, engine) {
				return "", false
			}
			return fault, true
		}
		offset += len(line) + len("\n")
	}
	return "", false
}

// quotedName is a name Ptah prints with %q, escapes and all.
var quotedName = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// placedTableLead is the word before a quoted name Ptah may place in a
// schema, a table's, as in `on table "parent"`. It is a word of its own, at
// the start of the words or after a space.
const placedTableLead = "table "

// faultEnds reports whether failed ends with ": " and then fault, with the
// tables it names placed as placedFault allows.
func faultEnds(failed, fault, engine string) bool {
	if strings.HasSuffix(failed, ": "+fault) {
		return true
	}
	expression, placed := placedFault(fault, engine)
	if !placed {
		return false
	}
	for offset := strings.Index(failed, ": "); offset >= 0; {
		if placedAs(expression, failed[offset+len(": "):]) {
			return true
		}
		next := strings.Index(failed[offset+1:], ": ")
		if next < 0 {
			break
		}
		offset += 1 + next
	}
	return false
}

// faultIs reports whether failed is line and nothing else, with the tables it
// names placed as placedFault allows.
func faultIs(failed, line, engine string) bool {
	if failed == line {
		return true
	}
	expression, placed := placedFault(line, engine)
	return placed && placedAs(expression, failed)
}

// placedFault is the expression for the whole of fault in which each quoted
// name that follows placedTableLead and names no schema may name one, the
// schema a plan or a drift placed the table in, as a group of its own. A name
// validate prints with a schema has to come back as it is, and nothing else in
// the words may differ. It reports false where Ptah places no name, on every
// engine but PostgreSQL, and where fault names no table it could place.
func placedFault(fault, engine string) (*regexp.Regexp, bool) {
	if !dialectMatches(engine, "postgres") {
		return nil, false
	}
	var pattern strings.Builder
	pattern.WriteString(`\A`)
	last, tables := 0, 0
	for _, found := range quotedName.FindAllStringIndex(fault, -1) {
		pattern.WriteString(regexp.QuoteMeta(fault[last:found[0]]))
		name := fault[found[0]+1 : found[1]-1]
		before := fault[:found[0]]
		names := before == placedTableLead || strings.HasSuffix(before, " "+placedTableLead)
		if names && !strings.Contains(name, ".") {
			pattern.WriteString(`"(?:([^"\\.]+)\.)?` + regexp.QuoteMeta(name) + `"`)
			tables++
		} else {
			pattern.WriteString(regexp.QuoteMeta(fault[found[0]:found[1]]))
		}
		last = found[1]
	}
	pattern.WriteString(regexp.QuoteMeta(fault[last:]) + `\z`)
	if tables == 0 {
		return nil, false
	}
	expression, err := regexp.Compile(pattern.String())
	return expression, err == nil
}

// placedAs reports whether text is the fault expression matches, with every
// table that names a schema naming the same one. RE2 has no backreference, so
// the schemas are compared here.
func placedAs(expression *regexp.Regexp, text string) bool {
	match := expression.FindStringSubmatch(text)
	if match == nil {
		return false
	}
	schema := ""
	for _, placed := range match[1:] {
		switch {
		case placed == "":
		case schema == "":
			schema = placed
		case placed != schema:
			return false
		}
	}
	return true
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
