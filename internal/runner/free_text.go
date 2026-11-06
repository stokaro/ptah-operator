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
// joins the words onto one line, and cuts them at a character boundary. The result
// is empty when nothing readable is left.
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
	line := strings.Join(strings.Fields(readable), " ")
	if len(line) <= limit {
		return line
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return strings.TrimRight(line[:cut], " ")
}
