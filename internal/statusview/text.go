package statusview

import (
	"fmt"
	"strings"
	"time"
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

// Duration prints a length of time the way the plugin's help writes one: 5m
// rather than 5m0s, 1h rather than 1h0m0s, and 90s as 1m30s.
func Duration(d time.Duration) string {
	text := d.String()
	if trimmed, ok := strings.CutSuffix(text, "m0s"); ok {
		text = trimmed + "m"
	}
	if trimmed, ok := strings.CutSuffix(text, "h0m"); ok {
		text = trimmed + "h"
	}
	return text
}

// Clean makes free text safe to print on one terminal line.
//
// A message, a reason or a user name reaches the screen from whoever wrote
// it: the operator, Ptah's report, an identity provider. A line break would
// split one row of a table into two that read as separate rows, and a control
// sequence or a bidirectional override can rewrite what the terminal shows.
// Line breaks and tabs become spaces; every other control character and
// every bidirectional control is printed as its escape, so a reader sees that
// it was there. Format characters -- zero-width spaces and joiners, a byte
// order mark, a soft hyphen -- are escaped too: they print as nothing, so a
// name that holds one reads exactly like a name that does not.
func Clean(text string) string {
	if !needsCleaning(text) {
		return text
	}
	var out strings.Builder
	for _, character := range text {
		switch {
		case character == '\n' || character == '\r' || character == '\t':
			out.WriteByte(' ')
		case invisible(character):
			switch {
			case character <= 0xff:
				fmt.Fprintf(&out, `\x%02x`, character)
			case character <= 0xffff:
				fmt.Fprintf(&out, `\u%04x`, character)
			default:
				fmt.Fprintf(&out, `\U%08x`, character)
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
		if invisible(character) {
			return true
		}
	}
	return false
}

// invisible reports a character a terminal acts on or prints as nothing.
func invisible(character rune) bool {
	return unicode.IsControl(character) ||
		unicode.Is(unicode.Bidi_Control, character) ||
		unicode.Is(unicode.Cf, character)
}
