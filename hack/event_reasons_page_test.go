package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Events are what a person reads first: `kubectl describe` prints them under
// the status, before anybody thinks about Conditions. Twenty reasons were
// recorded and twelve of them appeared nowhere in the documentation, including
// every one that asks a person to do something -- ProtectedTableRefused,
// MigrationRunUncertain, TargetLockReleaseOwed, ResultReadTimedOut.
//
// So the page lists them, and these hold it to what the controllers record.

const (
	eventsPage = "docs/site/src/content/docs/troubleshoot/events.md"
	// computedEventType is what a row says when the controller decides the
	// type from the outcome it is reporting rather than fixing one.
	computedEventType = "Normal or Warning"
)

var eventRow = regexp.MustCompile("(?m)^\\| `([A-Za-z]+)` \\| (Normal or Warning|Normal|Warning) \\| ([^|]+) \\| (.*) \\|$")

// recordedEvent is one Event the controllers record.
type recordedEvent struct {
	eventType string
	families  map[string]bool
}

// Every reason a controller records has a row.
func TestEveryRecordedEventIsOnThePage(t *testing.T) {
	t.Parallel()
	rows := eventRows(t)
	for reason := range recordedEvents(t) {
		if _, ok := rows[reason]; !ok {
			t.Errorf("the controllers record %s and the page does not list it, so a reader who sees it has nowhere to look", reason)
		}
	}
}

// And every row is a reason something records. A row for an Event nothing
// records sends a reader looking for a state that cannot happen.
func TestThePageListsNoEventNothingRecords(t *testing.T) {
	t.Parallel()
	recorded := recordedEvents(t)
	for reason := range eventRows(t) {
		if _, ok := recorded[reason]; !ok {
			t.Errorf("the page lists %s and nothing records it", reason)
		}
	}
}

// The type decides how a reader treats the line, so the page carries the one
// the controller uses.
func TestEveryEventRowCarriesTheTypeTheControllerUses(t *testing.T) {
	t.Parallel()
	rows := eventRows(t)
	for reason, event := range recordedEvents(t) {
		row, ok := rows[reason]
		if !ok {
			continue // the row above reports this
		}
		if row.eventType != event.eventType {
			t.Errorf("%s is recorded as %s and the page says %s", reason, event.eventType, row.eventType)
		}
		for family := range event.families {
			if !strings.Contains(row.recordedOn, family) {
				t.Errorf("%s is recorded by the %s family and its row does not name that resource: %q",
					reason, family, strings.TrimSpace(row.recordedOn))
			}
		}
	}
}

// The table is read by looking a reason up, which is alphabetical order or
// nothing, and a row with no meaning answers nothing.
func TestTheEventRowsReadInOrderAndSayWhatTheyMean(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, eventsPage)
	matches := eventRow.FindAllStringSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatalf("%s has no Event rows, so this check reads the page and holds none of it", eventsPage)
	}
	var order []string
	for _, match := range matches {
		order = append(order, match[1])
		if strings.TrimSpace(match[4]) == "" {
			t.Errorf("%s has a row and no meaning", match[1])
		}
	}
	sorted := append([]string(nil), order...)
	sort.Slice(sorted, func(i, j int) bool { return strings.ToLower(sorted[i]) < strings.ToLower(sorted[j]) })
	for index := range order {
		if order[index] != sorted[index] {
			t.Fatalf("the reasons are listed %v...; alphabetically they are %v...", order[index:], sorted[index:])
		}
	}
}

// eventRowOnPage is one row of the table.
type eventRowOnPage struct {
	eventType  string
	recordedOn string
}

func eventRows(t *testing.T) map[string]eventRowOnPage {
	t.Helper()
	rows := map[string]eventRowOnPage{}
	for _, match := range eventRow.FindAllStringSubmatch(readDocumentationPage(t, eventsPage), -1) {
		rows[match[1]] = eventRowOnPage{eventType: match[2], recordedOn: match[3]}
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no Event rows", eventsPage)
	}
	return rows
}

// recordedEvents reads what the controllers record, by reason.
func recordedEvents(t *testing.T) map[string]recordedEvent {
	t.Helper()
	events := map[string]recordedEvent{}
	var unresolved []string
	root := repositoryFile(t, "internal")
	fileSet := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isEventCall(call) {
					return true
				}
				if function.Name.Name == "event" {
					// The helper records whatever its caller named, and every
					// caller is a site this already read.
					return true
				}
				eventType, reason, ok := eventArguments(call)
				if !ok {
					unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
					return true
				}
				family, ok := familyOf(path)
				if !ok {
					unresolved = append(unresolved, fileSet.Position(call.Pos()).String()+" is in a file this cannot attribute to a family")
					return true
				}
				entry, seen := events[reason]
				if !seen {
					entry = recordedEvent{eventType: eventType, families: map[string]bool{}}
				}
				if entry.eventType != eventType {
					t.Errorf("%s is recorded as both %s and %s", reason, entry.eventType, eventType)
				}
				entry.families[family] = true
				events[reason] = entry
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Fatalf("these Event records do not name a type and reason where this can read them, so the set of recorded Events is incomplete: %v", unresolved)
	}
	if len(events) == 0 {
		t.Fatal("no Event record was found, so this check would call the page complete")
	}
	return events
}

// isEventCall reports whether the call records an Event.
func isEventCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && (selector.Sel.Name == "event" || selector.Sel.Name == "Event" || selector.Sel.Name == "Eventf")
}

// eventArguments reads the type and the reason a record names.
func eventArguments(call *ast.CallExpr) (string, string, bool) {
	if len(call.Args) < 3 {
		return "", "", false
	}
	eventType := ""
	switch argument := call.Args[1].(type) {
	case *ast.SelectorExpr:
		if !strings.HasPrefix(argument.Sel.Name, "EventType") {
			return "", "", false
		}
		eventType = strings.TrimPrefix(argument.Sel.Name, "EventType")
	case *ast.CallExpr:
		// A type the controller decides from the result it is reporting. The
		// row says so rather than claiming one of the two.
		eventType = computedEventType
	default:
		return "", "", false
	}
	literal, ok := call.Args[2].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", "", false
	}
	reason, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", "", false
	}
	return eventType, reason, true
}

// familyOf attributes a file to the resource family whose reconciler it is.
func familyOf(path string) (string, bool) {
	name := filepath.Base(path)
	switch {
	case strings.Contains(name, "migration"):
		return "PtahMigration", true
	case strings.Contains(name, "schema"):
		return "PtahSchema", true
	}
	return "", false
}
