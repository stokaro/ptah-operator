package main

import (
	"strings"
	"testing"
)

func TestCommandEventsKeepTheShapeTheAuthorWrote(t *testing.T) {
	t.Parallel()
	events := commandEvents("kubectl -n \"$NAMESPACE\" get ptahschema storefront \\\n  -o json | jq -r .status.phase\n")
	if len(events) != 2 {
		t.Fatalf("commandEvents made %d events", len(events))
	}
	if events[0][0] != kindCommand {
		t.Fatalf("the first line is %v", events[0][0])
	}
	if events[1][0] != kindContinued {
		t.Fatalf("the continuation is %v", events[1][0])
	}
}

func TestOutputEventsMarkWhatTheyAre(t *testing.T) {
	t.Parallel()
	events := outputEvents("Name        storefront\n\nCREATE TABLE \"customers\" (\n", kindOutput)
	want := []string{kindOutput, kindBlank, kindSQL}
	if len(events) != len(want) {
		t.Fatalf("outputEvents made %d events, expected %d", len(events), len(want))
	}
	for index, kind := range want {
		if events[index][0] != kind {
			t.Fatalf("event %d is %v, expected %s", index, events[index][0], kind)
		}
	}
}

func TestOutputEventsPublishNothingForNothing(t *testing.T) {
	t.Parallel()
	if events := outputEvents("   \n\n", kindOutput); events != nil {
		t.Fatalf("outputEvents made %d events out of whitespace", len(events))
	}
}

// A step that shows stderr is showing a refusal, and the refusal is colored as
// one. The SQL rule still wins where a statement appears, because a plan the
// operator refused is the thing the reader came for.
func TestOutputEventsRespectTheStreamTheStepNamed(t *testing.T) {
	t.Parallel()
	events := outputEvents("error: the policy refused this plan\nDROP COLUMN \"email\"\n", kindError)
	if events[0][0] != kindError {
		t.Fatalf("the diagnostic is %v", events[0][0])
	}
	if events[1][0] != kindSQL {
		t.Fatalf("the statement is %v", events[1][0])
	}
}

// A statement spans lines. Marking only the keyword line leaves the columns
// under it reading as ordinary output, which is most of the plan.
func TestOutputEventsMarkAStatementToItsEnd(t *testing.T) {
	t.Parallel()
	plan := strings.Join([]string{
		"safe\t-- POSTGRES TABLE: customers --",
		`CREATE TABLE "customers" (`,
		`  "id" bigint PRIMARY KEY NOT NULL,`,
		`  "email" text NOT NULL`,
		")",
		"Schema apply completed",
	}, "\n")
	events := outputEvents(plan, kindOutput)
	want := []string{kindOutput, kindSQL, kindSQL, kindSQL, kindSQL, kindOutput}
	if len(events) != len(want) {
		t.Fatalf("outputEvents made %d events, expected %d", len(events), len(want))
	}
	for index, kind := range want {
		if events[index][0] != kind {
			t.Fatalf("event %d is %v, expected %s (%q)", index, events[index][0], kind, events[index][1])
		}
	}
}

func TestOutputEventsEndAStatementOnItsTerminator(t *testing.T) {
	t.Parallel()
	// psql echoes the command tag after the statement, and a tag reads as a
	// statement to anything matching on keywords -- so the line after the one
	// tested here is one whose first word is nobody's SQL.
	printed := "ALTER TABLE \"customers\" ADD COLUMN \"signed_up_at\" timestamptz;\nSchema apply completed\n"
	events := outputEvents(printed, kindOutput)
	if events[0][0] != kindSQL {
		t.Fatalf("the statement is %v", events[0][0])
	}
	if events[1][0] != kindOutput {
		t.Fatalf("the line after the terminator is %v", events[1][0])
	}
}
