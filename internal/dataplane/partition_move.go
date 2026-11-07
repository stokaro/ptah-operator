package dataplane

import "strings"

// partitionDetachmentsLeavingParent returns the statements that detach a
// PostgreSQL partition and do not put it back under the same parent: those the
// operator rates destructive whatever Ptah wrote.
//
// A DETACH PARTITION keeps the partition's rows in the table and takes them out
// of what a query on the parent returns, so a reader of the parent sees them
// disappear. Ptah v0.23.0 rates every statement of a partition move a warning,
// which lets apply: Always run a bare detachment, or a move to another parent,
// with nobody looking. Ptah rates such a detachment destructive after
// stokaro/ptah#5739 (fixed by stokaro/ptah#5741), and this rule agrees with
// that rating; until a release with it is pinned, this rule is what holds the
// detachment for an approval.
//
// A detachment stays as Ptah rated it only when the next statement that moves
// the same table attaches it to the same parent, with nothing renamed in
// between. That is how Ptah writes a bound change. The two statements run in
// the one transaction the all and file modes open, so after the commit the
// parent returns the same rows or more: ATTACH refuses a partition that holds
// a row outside its bound, and the refusal rolls the detachment back too.
//
// The statements are read only in the shape Ptah renders. A detachment in any
// other shape, DETACH CONCURRENTLY and FINALIZE included, has no pair and is
// destructive, and so is every detachment on MySQL, which has no such
// statement. Names are compared as PostgreSQL resolves them, and a parent named
// with a schema on one side and without one on the other is not the same
// parent here, although Ptah reads it as the same: the operator may raise a
// rating and never lowers one.
func partitionDetachmentsLeavingParent(statements []PlanStatement, dialect string) map[int]bool {
	var raised map[int]bool
	for index, statement := range statements {
		if !hasSQLTokenSequence(sqlKeywordTokens(statement.SQL, dialect), "DETACH", "PARTITION") {
			continue
		}
		if partitionReattached(statements, index, dialect) {
			continue
		}
		if raised == nil {
			raised = make(map[int]bool)
		}
		raised[index] = true
	}
	return raised
}

// partitionReattached reports whether the detachment at index is followed by
// an attachment of the same table to the same parent, before any other move of
// that table and before any rename.
func partitionReattached(statements []PlanStatement, index int, dialect string) bool {
	if mysqlDialect(dialect) {
		return false
	}
	detached, ok := readPartitionMove(statements[index].SQL)
	if !ok || detached.attach {
		return false
	}
	for _, statement := range statements[index+1:] {
		move, ok := readPartitionMove(statement.SQL)
		if ok && move.table == detached.table {
			return move.attach && move.parent == detached.parent
		}
		if hasSQLToken(sqlKeywordTokens(statement.SQL, dialect), "RENAME") {
			return false
		}
	}
	return false
}

// partitionMove is one statement of a PostgreSQL partition move. The names are
// as PostgreSQL resolves them: an unquoted part folded to lower case, a quoted
// one as written, and the parts joined with a dot.
type partitionMove struct {
	attach        bool
	parent, table string
}

// readPartitionMove reads a statement in the shape Ptah renders a partition
// move:
//
//	ALTER TABLE <parent> DETACH PARTITION <table>
//	ALTER TABLE <parent> ATTACH PARTITION <table> <bound>
//
// with one optional terminating semicolon. A detachment ends at the table's
// name. What follows an attachment's name is its bound, which decides nothing
// here; the statement is rated on its own as well.
func readPartitionMove(statement string) (partitionMove, bool) {
	rest := strings.TrimSpace(statement)
	rest = strings.TrimSpace(strings.TrimSuffix(rest, ";"))
	var move partitionMove
	var ok bool
	if rest, ok = readSQLKeyword(rest, "ALTER"); !ok {
		return partitionMove{}, false
	}
	if rest, ok = readSQLKeyword(rest, "TABLE"); !ok {
		return partitionMove{}, false
	}
	if move.parent, rest, ok = readQualifiedName(rest); !ok {
		return partitionMove{}, false
	}
	if detach, ok := readSQLKeyword(rest, "DETACH"); ok {
		rest = detach
	} else if rest, ok = readSQLKeyword(rest, "ATTACH"); ok {
		move.attach = true
	} else {
		return partitionMove{}, false
	}
	if rest, ok = readSQLKeyword(rest, "PARTITION"); !ok {
		return partitionMove{}, false
	}
	if move.table, rest, ok = readQualifiedName(rest); !ok {
		return partitionMove{}, false
	}
	if move.attach {
		// The bound is separated from the name.
		return move, rest != "" && rest != strings.TrimLeft(rest, " \t\r\n")
	}
	return move, rest == ""
}

// readSQLKeyword consumes leading whitespace and then keyword, in any case,
// where a word boundary follows it.
func readSQLKeyword(statement, keyword string) (string, bool) {
	statement = strings.TrimLeft(statement, " \t\r\n")
	if len(statement) < len(keyword) || !strings.EqualFold(statement[:len(keyword)], keyword) {
		return statement, false
	}
	if len(statement) > len(keyword) && identifierCharacter(statement[len(keyword)]) {
		return statement, false
	}
	return statement[len(keyword):], true
}

// readQualifiedName consumes leading whitespace and then a name of one or two
// parts. A part is a double-quoted identifier, kept as written, or an unquoted
// ASCII identifier, kept as the quoted identifier of its lower-case spelling,
// which is the table PostgreSQL resolves it to. A doubled quote is the only way
// to write a quote inside an identifier, so two parts are the same name exactly
// when their quoted spellings are equal. Anything else, a U& identifier or a
// comment between the parts included, is no name.
func readQualifiedName(statement string) (string, string, bool) {
	rest := strings.TrimLeft(statement, " \t\r\n")
	var parts []string
	for {
		part, after, ok := readIdentifierPart(rest)
		if !ok {
			return "", statement, false
		}
		parts = append(parts, part)
		rest = after
		if len(parts) == 2 || !strings.HasPrefix(rest, ".") {
			break
		}
		rest = rest[1:]
	}
	return strings.Join(parts, "."), rest, true
}

func readIdentifierPart(statement string) (string, string, bool) {
	if statement == "" {
		return "", statement, false
	}
	if statement[0] == '"' {
		for index := 1; index < len(statement); index++ {
			if statement[index] != '"' {
				continue
			}
			if index+1 < len(statement) && statement[index+1] == '"' {
				index++
				continue
			}
			if index == 1 {
				// PostgreSQL refuses a zero-length identifier.
				return "", statement, false
			}
			return statement[:index+1], statement[index+1:], true
		}
		return "", statement, false
	}
	if !asciiKeywordCharacter(statement[0]) {
		return "", statement, false
	}
	end := 1
	for end < len(statement) && (asciiKeywordCharacter(statement[end]) ||
		statement[end] >= '0' && statement[end] <= '9' || statement[end] == '$') {
		end++
	}
	if end < len(statement) && statement[end] >= 0x80 {
		return "", statement, false
	}
	return `"` + strings.ToLower(statement[:end]) + `"`, statement[end:], true
}
