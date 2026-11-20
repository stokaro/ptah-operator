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

// The one refusal of the directory `migrations status` makes after it loaded
// the files: an Atlas migration whose version Ptah's revision table, which
// records numbers, cannot keep, such as R__x.sql (stokaro/ptah#6313). The
// migrator makes it while it reads the revision table, so it arrives under
// that read's prefix, which also carries what a database answered. Only the
// sentence that opens with the file and says why the table cannot keep it is
// the directory's.
const (
	migrationRevisionsPrefix = "error getting migration status: failed to get migration revisions: "
	numericVersionLead       = "Atlas migration "
	numericVersionRefusal    = ", which a revision table that records numbers cannot keep; "
)

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
// as well (stokaro/ptah#6219). An index, a column or a constraint keeps the
// name validate prints. So on PostgreSQL a table validate names without a
// schema matches the same table with a schema in front of it, in both kinds of
// line, and the tables one sentence places share one schema (see placedFault).
// On MySQL the words have to be validate's.
//
// The same placement reaches two more names in a schema fault, both measured
// with Ptah v0.34.0 on PostgreSQL 17. A domain is placed with the tables, so
// the refusal of its own relative time default (stokaro/ptah#6386) names
// domain "public.moment" where validate names domain "moment". A column typed
// by a domain the file declares names that type with the domain's schema,
// unquoted, so the refusal of the column's relative time default says on a
// public.moment column where validate says on a moment column. Validate never
// qualifies a column's type, and drift and plan do even where nothing is
// placed, so that schema is the domain's and not the tables'. Nothing else
// validate names in a schema fault on PostgreSQL is one drift and plan place:
// a sequence, an enum, a function or a view appears there only in a view's
// owner refusal, which every schema reader refuses while it parses, as a load
// failure that needs no comparison.
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

// placedLeads are the words before a quoted name Ptah may place in a schema:
// a table's, as in `on table "parent"`, and a domain's, as in
// `domain "moment": DEFAULT 'now'`. Each is a word of its own, at the start of
// the words or after a space.
var placedLeads = []string{"table ", "domain "}

// placedColumnType is a column's type as the refusal of a relative time
// default names it, `on a moment column`, which drift and plan write with the
// schema of the domain the type names. The type is unquoted: one name, with
// the brackets of an array after it. A built-in type is never qualified, and a
// quoted or qualified one comes back as validate prints it.
var placedColumnType = regexp.MustCompile(`(?:\A| )on a ([A-Za-z_][A-Za-z0-9_$]*(?:\[[0-9]*\])*) column(?: |\z)`)

// faultEnds reports whether failed ends with ": " and then fault, with the
// names it holds placed as placedFault allows.
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

// faultIs reports whether failed is line and nothing else, with the names it
// holds placed as placedFault allows.
func faultIs(failed, line, engine string) bool {
	if failed == line {
		return true
	}
	expression, placed := placedFault(line, engine)
	return placed && placedAs(expression, failed)
}

// placedFault is the expression for the whole of fault in which each quoted
// name that follows one of placedLeads and names no schema may name one, the
// schema a plan or a drift placed it in, as a group of its own, and in which a
// column type placedColumnType finds may name the schema of its domain, which
// is no group: it need not be the tables'. A name validate prints with a
// schema has to come back as it is, and nothing else in the words may differ.
// It reports false where Ptah places no name, on every engine but PostgreSQL,
// and where fault names nothing it could place.
func placedFault(fault, engine string) (*regexp.Regexp, bool) {
	if !dialectMatches(engine, "postgres") {
		return nil, false
	}
	var pattern strings.Builder
	pattern.WriteString(`\A`)
	last, placeable := 0, 0
	for _, found := range quotedName.FindAllStringIndex(fault, -1) {
		placeable += writeWords(&pattern, fault[last:found[0]])
		name := fault[found[0]+1 : found[1]-1]
		if placedLead(fault[:found[0]]) && !strings.Contains(name, ".") {
			pattern.WriteString(`"(?:([^"\\.]+)\.)?` + regexp.QuoteMeta(name) + `"`)
			placeable++
		} else {
			pattern.WriteString(regexp.QuoteMeta(fault[found[0]:found[1]]))
		}
		last = found[1]
	}
	placeable += writeWords(&pattern, fault[last:])
	pattern.WriteString(`\z`)
	if placeable == 0 {
		return nil, false
	}
	expression, err := regexp.Compile(pattern.String())
	return expression, err == nil
}

// placedLead reports whether before, the words in front of a quoted name, ends
// with one of placedLeads as a word of its own.
func placedLead(before string) bool {
	for _, lead := range placedLeads {
		if before == lead || strings.HasSuffix(before, " "+lead) {
			return true
		}
	}
	return false
}

// writeWords writes the expression for words that hold no quoted name: the
// words as they are, but for each column type placedColumnType finds, which may
// come back with a schema in front of it. It returns how many it found.
func writeWords(pattern *strings.Builder, words string) int {
	last := 0
	types := placedColumnType.FindAllStringSubmatchIndex(words, -1)
	for _, found := range types {
		pattern.WriteString(regexp.QuoteMeta(words[last:found[2]]))
		pattern.WriteString(`(?:[^\s".\\]+\.)?` + regexp.QuoteMeta(words[found[2]:found[3]]))
		last = found[3]
	}
	pattern.WriteString(regexp.QuoteMeta(words[last:]))
	return len(types)
}

// placedAs reports whether text is the fault expression matches, with every
// table or domain that names a schema naming the same one. RE2 has no
// backreference, so the schemas are compared here.
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
		reason, loading = numericVersionProblem(failure.Message)
	}
	if !loading {
		return "", false
	}
	reason = strings.TrimSpace(reason)
	return reason, reason != ""
}

// numericVersionProblem reads the migrator's refusal of an Atlas migration
// whose version the numeric revision table cannot keep, and returns it without
// the read's prefix:
//
//	Atlas migration R__x.sql has version "R", which a revision table that records numbers cannot keep; use ...
func numericVersionProblem(message string) (string, bool) {
	refusal, found := strings.CutPrefix(message, migrationRevisionsPrefix)
	if !found || !strings.HasPrefix(refusal, numericVersionLead) || !strings.Contains(refusal, numericVersionRefusal) {
		return "", false
	}
	return refusal, true
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
