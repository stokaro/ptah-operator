package main

import (
	"regexp"
	"strings"
)

// The event kinds the player renders. They are the vocabulary ptah.run's own
// recorded runs already use, so one player reads both: `cmd` and `cont` are
// typed a character at a time, `note` is the demonstration's narration, `sync`
// moves the phase pill, `wait` is a beat, and the rest arrive whole.
const (
	kindCommand     = "cmd"
	kindContinued   = "cont"
	kindNote        = "note"
	kindOutput      = "out"
	kindSQL         = "sql"
	kindHighlighted = "new"
	kindError       = "err"
	kindMuted       = "mute"
	kindBlank       = "blank"
	kindSync        = "sync"
)

// event is one line of the transcript, as [kind, text].
type event [2]any

// commandEvents splits a shell command the way a terminal shows it: the first
// line typed after the prompt, and each continuation indented under it.
//
// The split is on the line breaks the scenario file already wrote, because the
// author chose where the command wraps and a reader copying it needs the same
// shape back.
func commandEvents(run string) []event {
	lines := strings.Split(strings.TrimRight(run, "\n"), "\n")
	events := make([]event, 0, len(lines))
	for index, line := range lines {
		kind := kindContinued
		if index == 0 {
			kind = kindCommand
		}
		events = append(events, event{kind, line})
	}
	return events
}

// outputEvents renders captured output, one event per line.
//
// A statement the operator plans or applies is the subject of this
// demonstration, so it is marked wherever it appears -- and a statement spans
// lines, so the marking continues to the end of it rather than colouring only
// the keyword line and leaving the columns under it as ordinary output.
func outputEvents(text string, kind string) []event {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	events := make([]event, 0, len(lines))
	statement := false
	for _, line := range lines {
		// A blank line inside output is a spacer, and the player draws one
		// rather than an empty output row whose color says something was
		// printed there. It also ends whatever statement was being printed.
		if strings.TrimSpace(line) == "" {
			statement = false
			events = append(events, event{kindBlank})
			continue
		}
		if sqlLine.MatchString(line) {
			statement = true
		}
		if statement {
			events = append(events, event{kindSQL, line})
			if statementEnds(line) {
				statement = false
			}
			continue
		}
		events = append(events, event{kind, line})
	}
	return events
}

// statementEnds reports whether a line closes the statement it is part of.
//
// The terminator, or the closing parenthesis of a CREATE TABLE the renderer
// wrote without one. Anything else is another line of the same statement.
func statementEnds(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasSuffix(trimmed, ";") || trimmed == ")" || trimmed == ");"
}

var sqlLine = regexp.MustCompile(`^\s*(CREATE|ALTER|DROP|INSERT|UPDATE|DELETE|SELECT|BEGIN|COMMIT)\b`)
