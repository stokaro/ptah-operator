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
// under it reading as ordinary output, which is most of the plan. The lines
// are what kubectl ptah approve printed in run 41458488586, with Ptah v0.17.0,
// whose plans carry no comment line ahead of a CREATE TABLE: the statement is
// found by its keyword at the start of the line.
func TestOutputEventsMarkAStatementToItsEnd(t *testing.T) {
	t.Parallel()
	plan := strings.Join([]string{
		"Statement 1 of 1: safe (does not remove data or tighten constraints)",
		`CREATE TABLE "customers" (`,
		`  "id" bigint PRIMARY KEY NOT NULL,`,
		`  "email" text NOT NULL`,
		");",
		"Approve plan ptah-plan-64e304c38b3a185acc873186 of schema/storefront in demo? Type yes to approve: yes",
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

// A statement the renderer wrote without a terminator runs on into the
// comment Ptah appends to it, which ends it: the destructive plan in
// destructive-block, as run 41458488586 printed it. The comment ahead of the
// statement is not part of it.
func TestOutputEventsCarryAStatementThroughItsTrailingComment(t *testing.T) {
	t.Parallel()
	plan := strings.Join([]string{
		"Statement 1 of 1: destructive (DROP COLUMN removes existing column data)",
		"-- Remove columns from table: customers",
		`ALTER TABLE "customers" DROP COLUMN "email" CASCADE`,
		"-- WARNING: Dropping column customers.email with CASCADE - This will delete data and dependent objects!;",
		"Stored:         <time>",
	}, "\n")
	events := outputEvents(plan, kindOutput)
	want := []string{kindOutput, kindOutput, kindSQL, kindSQL, kindOutput}
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
