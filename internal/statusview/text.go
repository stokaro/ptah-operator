package statusview

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TimeLayout is how every instant is printed: UTC, to the second.
const TimeLayout = "2006-01-02T15:04:05Z"

// Time prints one instant the way every other line of the plugin does.
func Time(instant metav1.Time) string {
	return instant.UTC().Format(TimeLayout)
}

// Clean makes free text safe to print on one terminal line.
//
// A message, a reason or a user name reaches the screen from whoever wrote
// it: the operator, Ptah's report, an identity provider. A line break would
// split one row of a table into two that read as separate rows, and a control
// sequence or a bidirectional override can rewrite what the terminal shows.
// Line breaks and tabs become spaces; every other control character and
// every bidirectional control is printed as its escape, so a reader sees that
// it was there.
func Clean(text string) string {
	if !needsCleaning(text) {
		return text
	}
	var out strings.Builder
	for _, character := range text {
		switch {
		case character == '\n' || character == '\r' || character == '\t':
			out.WriteByte(' ')
		case unicode.IsControl(character) || unicode.Is(unicode.Bidi_Control, character):
			if character <= 0xff {
				fmt.Fprintf(&out, `\x%02x`, character)
			} else {
				fmt.Fprintf(&out, `\u%04x`, character)
			}
		default:
			out.WriteRune(character)
		}
	}
	return out.String()
}

// needsCleaning also reports a string that is not UTF-8: ranging over it
// yields U+FFFD for each stray byte, so the cleaned copy carries no byte a
// terminal could read as the start of a sequence.
func needsCleaning(text string) bool {
	if !utf8.ValidString(text) {
		return true
	}
	for _, character := range text {
		if unicode.IsControl(character) || unicode.Is(unicode.Bidi_Control, character) {
			return true
		}
	}
	return false
}
