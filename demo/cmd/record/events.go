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
// SQL is marked so the player can colour it, because the plan is the thing a
// reader of this demonstration came for. Everything else is ordinary output;
// a stream captured from stderr is marked as such only when the step failed on
// purpose, which the caller decides.
func outputEvents(text string, kind string) []event {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	events := make([]event, 0, len(lines))
	for _, line := range lines {
		// A blank line inside output is a spacer, and the player draws one
		// rather than an empty output row whose colour says something was
		// printed there.
		if strings.TrimSpace(line) == "" {
			events = append(events, event{kindBlank})
			continue
		}
		events = append(events, event{lineKind(line, kind), line})
	}
	return events
}

var sqlLine = regexp.MustCompile(`^\s*(CREATE|ALTER|DROP|INSERT|UPDATE|DELETE|SELECT|BEGIN|COMMIT)\b`)

// lineKind marks a line of output. A statement the operator plans or applies is
// the subject of this demonstration, so it is marked wherever it appears rather
// than only inside a block somebody remembered to label.
func lineKind(line, fallback string) string {
	if sqlLine.MatchString(line) {
		return kindSQL
	}
	return fallback
}
