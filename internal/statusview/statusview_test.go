package statusview_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Leading is a selection, and each row below is a reading the operator wrote:
// it has to pick Ready and the holds that are True, and leave out a hold whose
// False status says nothing holds -- the inactive Suspended a reader would
// otherwise take for a suspension.
func TestLeadingPicksReadyAndTheHoldsThatAreTrue(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		reading []metav1.Condition
		want    []string
	}{
		{
			// Waiting for an approval: Suspended=False Active and the other
			// verdicts stay in the table, and ApprovalRequired is the plan's.
			name:    "schema waiting for an approval",
			reading: schemaConditions(t, "schema-approval-waiting-conditions.json"),
			want:    []string{"Ready=False"},
		},
		{
			name:    "suspended schema whose Apply outcome is unknown",
			reading: objectConditions(t, "alert-schema-recovery-wait.json"),
			want:    []string{"Ready=False", "ReconciliationFailed=True", "Suspended=True"},
		},
		{
			name:    "migration held by a run nobody accounted for",
			reading: migrationConditions(t, "mysql"),
			want:    []string{"Ready=False", "Blocked=True"},
		},
		{
			// Blocked=False HistoryMatched is a history that continues.
			name:    "migration in sync",
			reading: objectConditions(t, "isolated-node-durable-migration.json"),
			want:    []string{"Ready=True"},
		},
		{
			// Close to a hold and not one: a status other than True, and a
			// type spelled otherwise.
			name: "near misses",
			reading: []metav1.Condition{
				{Type: "Blocked", Status: metav1.ConditionUnknown},
				{Type: "blocked", Status: metav1.ConditionTrue},
				{Type: "Suspended", Status: metav1.ConditionFalse},
				{Type: "DriftDetected", Status: metav1.ConditionTrue},
				{Type: "Applying", Status: metav1.ConditionTrue},
				{Type: "Progressing", Status: metav1.ConditionTrue},
			},
		},
		{
			// Ready is read first wherever the status holds it.
			name: "ready listed last",
			reading: []metav1.Condition{
				{Type: "Blocked", Status: metav1.ConditionTrue},
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			want: []string{"Ready=False", "Blocked=True"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if len(test.reading) == 0 {
				t.Fatal("the reading holds no condition, so this row would pass over nothing")
			}
			var got []string
			for _, condition := range statusview.Leading(statusview.FromConditions(test.reading)) {
				got = append(got, condition.Type+"="+condition.Status)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("Leading() = %v, want %v", got, test.want)
			}
		})
	}
}

// Every row of the table is one condition, message included, and a row
// written for another generation says so.
func TestWriteTablePrintsEveryConditionWithItsMessage(t *testing.T) {
	t.Parallel()

	reading := objectConditions(t, "alert-schema-recovery-wait.json")
	conditions := statusview.FromConditions(reading)
	var text strings.Builder
	statusview.WriteTable(&text, "  ", conditions, 3)
	rows := strings.Split(strings.TrimSuffix(text.String(), "\n"), "\n")
	if len(rows) != len(reading)+1 {
		t.Fatalf("the table has %d rows for %d conditions:\n%s", len(rows), len(reading), text.String())
	}
	for index, condition := range reading {
		row := rows[index+1]
		for _, want := range []string{condition.Type, string(condition.Status), condition.Reason, condition.Message} {
			if !strings.Contains(row, want) {
				t.Fatalf("row %q does not carry %q", row, want)
			}
		}
		written := strings.Contains(row, "(written for generation")
		if stale := condition.ObservedGeneration != 3; written != stale {
			t.Fatalf("row %q, written for generation %d of 3, says so: %t", row, condition.ObservedGeneration, written)
		}
		if strings.HasSuffix(row, " ") {
			t.Fatalf("row %q ends in padding", row)
		}
	}
	if !strings.Contains(rows[0], "TYPE") || !strings.Contains(rows[0], "MESSAGE") {
		t.Fatalf("the table has no heading: %q", rows[0])
	}

	// Without a generation nothing is called stale.
	text.Reset()
	statusview.WriteTable(&text, "", conditions, 0)
	if strings.Contains(text.String(), "written for generation") {
		t.Fatalf("a table without a generation called a row stale:\n%s", text.String())
	}
}

// Clean keeps what a reader can read and defuses what a terminal would act on.
func TestCleanKeepsTextAndEscapesWhatATerminalActsOn(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, text, want string
	}{
		{name: "plain", text: "Plan is waiting for approval", want: "Plan is waiting for approval"},
		{name: "letters beyond ASCII", text: "таблица café 表", want: "таблица café 表"},
		{name: "line break", text: "first\nsecond\r\nthird", want: "first second  third"},
		{name: "tab", text: "a\tb", want: "a b"},
		{name: "escape sequence", text: "\x1b[2Jcleared", want: `\x1b[2Jcleared`},
		{name: "C1 control", text: "a\u009bb", want: `a\x9bb`},
		{name: "bidirectional override", text: "user\u202eevil", want: `user\u202eevil`},
		{name: "byte that is not UTF-8", text: "a\xffb", want: "a\ufffdb"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := statusview.Clean(test.text); got != test.want {
				t.Fatalf("Clean(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestNewestOrdersByCreationAndKeepsTheBound(t *testing.T) {
	t.Parallel()

	type decision struct {
		name    string
		created time.Time
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	var decisions []decision
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		decisions = append(decisions, decision{name: name, created: base.Add(time.Duration(len(decisions)) * time.Minute)})
	}
	// Two made in the same second: the name decides, so the order is stable.
	decisions = append(decisions, decision{name: "aa", created: decisions[6].created})

	newest := statusview.Newest(decisions,
		func(d decision) metav1.Time { return metav1.NewTime(d.created) },
		func(d decision) string { return d.name })

	if newest.Total != len(decisions) {
		t.Fatalf("Total = %d, want %d", newest.Total, len(decisions))
	}
	var got []string
	for _, item := range newest.Items {
		got = append(got, item.name)
	}
	if want := []string{"aa", "g", "f", "e", "d"}; !slices.Equal(got, want) {
		t.Fatalf("Items = %v, want %v", got, want)
	}

	none := statusview.Newest([]decision(nil),
		func(d decision) metav1.Time { return metav1.NewTime(d.created) },
		func(d decision) string { return d.name })
	encoded, err := json.Marshal(none)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"total":0,"items":[]}` {
		t.Fatalf("an empty list encodes as %s; a script reading items has to find an empty array", encoded)
	}
}

// Forbidden is the answer a reader with get alone receives, and it is told
// apart from every other failure.
func TestUnreadableFromSeparatesAForbiddenList(t *testing.T) {
	t.Parallel()

	resource := schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}
	for _, test := range []struct {
		name      string
		err       error
		forbidden bool
	}{
		{name: "forbidden", err: apierrors.NewForbidden(resource, "", errors.New(`User "reader" cannot list resource`)), forbidden: true},
		{name: "unauthorized", err: apierrors.NewUnauthorized("token expired")},
		{name: "not found", err: apierrors.NewNotFound(resource, "")},
		{name: "timeout", err: apierrors.NewTimeoutError("the list took too long", 1)},
		{name: "not from the API server", err: errors.New("connection refused")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			unreadable := statusview.UnreadableFrom(test.err)
			if unreadable.Forbidden != test.forbidden || unreadable.Message != test.err.Error() {
				t.Fatalf("UnreadableFrom(%v) = %+v", test.err, unreadable)
			}
		})
	}
}

func TestWriteListHeadingSaysWhatFollows(t *testing.T) {
	t.Parallel()

	forbidden := statusview.UnreadableFrom(apierrors.NewForbidden(
		schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}, "",
		errors.New(`User "reader" cannot list resource "ptahschemaapprovals"`)))
	two := &statusview.Decisions[string]{Total: 2, Items: []string{"b", "a"}}
	cut := &statusview.Decisions[string]{Total: 9, Items: []string{"e", "d", "c", "b", "a"}}
	for _, test := range []struct {
		name       string
		decisions  *statusview.Decisions[string]
		unreadable *statusview.Unreadable
		follows    bool
		want       string
	}{
		{
			name: "forbidden", unreadable: forbidden,
			want: "\nApprovals: not shown; listing ptahschemaapprovals is forbidden to you (RBAC): " + forbidden.Message + "\n",
		},
		{
			name: "failed", unreadable: &statusview.Unreadable{Message: "connection refused"},
			want: "\nApprovals: not shown; listing ptahschemaapprovals failed: connection refused\n",
		},
		{name: "none", decisions: &statusview.Decisions[string]{Items: []string{}}, want: "\nApprovals: none\n"},
		{name: "every one", decisions: two, follows: true, want: "\nApprovals, newest first:\n"},
		{
			name: "cut", decisions: cut, follows: true,
			want: "\nApprovals, the newest 5 of 9 (kubectl get ptahschemaapprovals -n application lists every one):\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var text strings.Builder
			follows := statusview.WriteListHeading(&text, "Approvals", "ptahschemaapprovals", "application",
				test.decisions, test.unreadable)
			if follows != test.follows || text.String() != test.want {
				t.Fatalf("WriteListHeading() = %t, %q; want %t, %q", follows, text.String(), test.follows, test.want)
			}
		})
	}
}

func readReading(t *testing.T, name string, into any) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// schemaConditions reads a reading that is a bare list of conditions.
func schemaConditions(t *testing.T, name string) []metav1.Condition {
	t.Helper()
	var conditions []metav1.Condition
	readReading(t, name, &conditions)
	return conditions
}

// objectConditions reads the conditions of a reading that is one object.
func objectConditions(t *testing.T, name string) []metav1.Condition {
	t.Helper()
	var object struct {
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	readReading(t, name, &object)
	return object.Status.Conditions
}

// migrationConditions reads one engine's migration from the unresolved-run
// reading, which holds one per engine.
func migrationConditions(t *testing.T, engine string) []metav1.Condition {
	t.Helper()
	var objects map[string]struct {
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	readReading(t, "uncertain-migration-retained-claim.json", &objects)
	return objects[engine].Status.Conditions
}
