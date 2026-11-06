package runner

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// freeText is the one way words a child wrote leave the runner for a place a
// person reads them: a frame, a condition, an Event or the Pod log.
//
// It redacts first, while the words are still as the child wrote them, because
// a credential whose whitespace has changed no longer matches. It then replaces
// control and format characters, so a file name cannot move a terminal's cursor
// or reverse the text around it, along with any byte that is not a character,
// joins the words onto one line, takes the frame's marker out, and cuts them at a
// character boundary. The result is empty when nothing readable is left.
//
// The marker goes because every caller ends this line with a line break, and the
// manager reads a merged log with an unanchored search. `PTAH_RUNNER_RESULT_V1 12
// <digest>` in a row or a file name is then a complete frame header, and a header
// naming another digest makes the manager refuse the termination summary that
// stands in for a lost frame.
//
// Redaction here cannot be complete. A parser that quotes a fragment of a value,
// `Hunter2...` for Hunter2Hunter2, leaves nothing a whole credential matches.
func freeText(text string, redactor Redactor, limit int) string {
	readable := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return ' '
		}
		return character
	}, redactor.redactFolded(text))
	line := strings.ReplaceAll(strings.Join(strings.Fields(readable), " "), strings.TrimSpace(frameHeader), "[frame marker]")
	if len(line) <= limit {
		return line
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return strings.TrimRight(line[:cut], " ")
}
