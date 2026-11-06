package runner

import (
	"io"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const (
	// maxReportedErrorBytes bounds the line a failed run leaves in the Pod log.
	maxReportedErrorBytes = 1 << 10
	// statementSeparator is how Ptah ends its sentence with the statement.
	statementSeparator = "\nSQL: "
	// statementTextMarker stands in for quoted text the statement also holds.
	statementTextMarker = "[in the statement]"
	// maxQuotesCompared bounds the work of comparing quoted parts with the
	// statement. A line with more than this keeps only what comes before them.
	maxQuotesCompared = 64

	codeApplyFailed         = "apply_failed"
	codeApplyOutcomeUnknown = "apply_outcome_unknown"
)

// LeavesReportedError says whether a schema Apply that ended with this code wrote
// Ptah's sentence to the Pod log. The two are decided here so that what a resource
// promises of the log cannot drift from what finishSchemaApply writes.
func LeavesReportedError(code string) bool {
	return code == codeApplyFailed || code == codeApplyOutcomeUnknown
}

// logReportedError writes what Ptah said about a run that failed to the Pod log,
// and nowhere else. It never reaches the result frame, the termination summary,
// status or an Event.
//
// The line can carry what the database read: a role, a host, a database name and
// a value from a row. `Duplicate entry 'alice@example.com' for key` is the
// database's own sentence, and nothing tells a row from any other text. It is kept
// because that sentence is the reason a person needs.
//
// What the plan declared is another matter. A database often quotes the statement
// it refused, `near 'CREATE USER 'app' IDENTIFIED BY 'a-password”`, and a reader
// of this log may not read plan SQL. Ptah ends its sentence with that statement, so
// the runner holds what to compare with: quoted text the statement also holds is
// replaced before anything else happens to the line, and the statement itself is
// dropped. A name the statement declares goes with it, because a comparison cannot
// tell a name from a literal.
//
// A failure with a target failure kind is left out: it has a sentence of the
// runner's own, and Ptah's names the host and the user it tried.
func logReportedError(failure *dataplane.CommandError, redactor Redactor, diagnostics io.Writer) {
	if failure == nil || diagnostics == nil {
		return
	}
	if _, known := failure.TargetFailure(); known {
		return
	}
	words, statement, _ := strings.Cut(failure.Message, statementSeparator)
	first, _, _ := strings.Cut(words, "\n")
	line := freeText(hideStatementText(first, statement), redactor, maxReportedErrorBytes)
	if line == "" {
		return
	}
	_, _ = io.WriteString(diagnostics, "ptah-runner: ptah reported: "+line+"\n")
}

// hideStatementText replaces each quoted part of the line that the statement also
// holds. Quotes nest in what a database echoes, so every quote is tried as an
// opening one and takes the longest part that matches, and a part the line break
// cut short runs to the end. That hides more than the quoted parts when the text
// between two of them is in the statement too, which is the side to err on.
func hideStatementText(line, statement string) string {
	if statement == "" {
		return line
	}
	const quotes = "'\"`"
	first := strings.IndexAny(line, quotes)
	if first < 0 {
		return line
	}
	count := 0
	for _, quote := range quotes {
		count += strings.Count(line, string(quote))
	}
	if count > maxQuotesCompared {
		return line[:first] + statementTextMarker
	}
	var kept strings.Builder
	for position := 0; position < len(line); {
		character := line[position]
		kept.WriteByte(character)
		position++
		if strings.IndexByte(quotes, character) < 0 {
			continue
		}
		end := quotedStatementTextEnd(line, position, statement)
		if end < 0 {
			continue
		}
		kept.WriteString(statementTextMarker)
		position = end
		if position < len(line) {
			// The closing quote stays, and it opens nothing.
			kept.WriteByte(line[position])
			position++
		}
	}
	return kept.String()
}

// quotedStatementTextEnd is where the quoted part whose text starts at start ends,
// when the statement holds it: at its closing quote, or at the end of the line when
// no closing quote gives a match. It is -1 when the statement holds no such part.
func quotedStatementTextEnd(line string, start int, statement string) int {
	quote := line[start-1]
	if rest := line[start:]; rest != "" && strings.Contains(statement, rest) {
		if strings.IndexByte(rest, quote) == len(rest)-1 {
			return len(line) - 1
		}
		return len(line)
	}
	for end := len(line) - 1; end > start; end-- {
		if line[end] == quote && strings.Contains(statement, line[start:end]) {
			return end
		}
	}
	return -1
}
