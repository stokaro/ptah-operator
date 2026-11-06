package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

// capturedReason is the reason the pinned Ptah image gave for the file, from
// the output internal/dataplane/testdata/desired-source holds.
func capturedReason(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "dataplane", "testdata", "desired-source", fixture))
	if err != nil {
		t.Fatal(err)
	}
	reason, found := dataplane.SchemaSourceProblem(data, "PostgreSQL")
	if !found {
		reason, found = dataplane.MigrationFilesProblem(data)
	}
	if !found {
		t.Fatalf("%s holds no reason", fixture)
	}
	return reason
}

func TestUnreadableSourceSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const code = runner.CodeDesiredSourceUnreadable + ": "
	partition := code + capturedReason(t, "validate-sql.txt")
	migration := code + capturedReason(t, "scratch-directive.json")
	if err := unreadableSourceSays(partition, unreadablePartitionFile, unreadablePartitionReason); err != nil {
		t.Errorf("the schema reason: %v", err)
	}
	if err := unreadableSourceSays("OperationRetried history attempt 1: "+partition, unreadablePartitionFile, unreadablePartitionReason); err != nil {
		t.Errorf("the reason inside an Event's sentence: %v", err)
	}
	// The fixture's migration file is named by the directory the pinned image
	// was measured on, and the row's by what it delivers.
	if err := unreadableSourceSays(migration, "0000000001_init.up.sql", unreadableMigrationReason); err != nil {
		t.Errorf("the migration reason: %v", err)
	}

	for name, message := range map[string]string{
		"the exit status it replaced": "child_exit: ptah exited with code 2",
		"the Plan code it replaced":   "invalid_plan_output: plan command did not complete successfully",
		"the code with no reason":     code + "the file",
		"another file":                code + "read SQL schema file other.sql: " + unreadablePartitionReason,
		"another reason":              code + "read SQL schema file " + unreadablePartitionFile + ": unsupported SQL statement",
		"another code":                "invalid_input: read SQL schema file " + unreadablePartitionFile + ": " + unreadablePartitionReason,
		"a kind":                      "database_unreachable: " + unreadablePartitionFile + " " + unreadablePartitionReason,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := unreadableSourceSays(message, unreadablePartitionFile, unreadablePartitionReason); err == nil {
				t.Fatalf("unreadableSourceSays(%q) accepted it", message)
			}
		})
	}
}

func TestUnreadableSourceEventFindsTheEventOfTheResource(t *testing.T) {
	t.Parallel()

	const owner = types.UID("schema-uid")
	said := runner.CodeDesiredSourceUnreadable + ": " + capturedReason(t, "validate-sql.txt")
	event := func(uid types.UID, kind, reason, message string) corev1.Event {
		return corev1.Event{InvolvedObject: corev1.ObjectReference{UID: uid}, Type: kind, Reason: reason, Message: message}
	}
	check := func(events ...corev1.Event) error {
		return unreadableSourceEvent(events, owner, "OperationFailed", unreadablePartitionFile, unreadablePartitionReason)
	}

	if err := check(event(owner, corev1.EventTypeWarning, "OperationFailed", "child_exit: ptah exited with code 2"),
		event(owner, corev1.EventTypeWarning, "OperationFailed", said)); err != nil {
		t.Errorf("the Event among others: %v", err)
	}
	for name, test := range map[string]struct {
		events []corev1.Event
		want   string
	}{
		"no Events": {nil, "no Warning OperationFailed"},
		"another resource's Event": {
			[]corev1.Event{event("other-uid", corev1.EventTypeWarning, "OperationFailed", said)}, "no Warning OperationFailed",
		},
		"another reason": {
			[]corev1.Event{event(owner, corev1.EventTypeWarning, "OperationRetried", said)}, "no Warning OperationFailed",
		},
		"a Normal Event": {
			[]corev1.Event{event(owner, corev1.EventTypeNormal, "OperationFailed", said)}, "no Warning OperationFailed",
		},
		"an Event that does not say why": {
			[]corev1.Event{event(owner, corev1.EventTypeWarning, "OperationFailed", "child_exit: ptah exited with code 2")},
			"1 Warning OperationFailed Events, none says why",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := check(test.events...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unreadableSourceEvent() = %v, want %q", err, test.want)
			}
		})
	}
}

// TestTheUnreadableGitCommitLeavesTheCapturedOnes holds the repository the Git row
// serves to the history the captures name: one commit more, on top of the
// rewritten main, with the file at the path the row selects.
func TestTheUnreadableGitCommitLeavesTheCapturedOnes(t *testing.T) {
	t.Parallel()

	spec := gitUnreadableSpec()
	built, err := gitrepo.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := gitrepo.Build(gitRewrittenSpec())
	if err != nil {
		t.Fatal(err)
	}
	captured := len(gitRewrittenSpec().Commits)
	if len(spec.Commits) != captured+1 {
		t.Fatalf("the spec has %d commits, want the %d of the rewritten history and one more", len(spec.Commits), captured)
	}
	for index := range captured {
		if built.Commit(index) != rewritten.Commit(index) {
			t.Errorf("commit %d moved from %s to %s", index, rewritten.Commit(index), built.Commit(index))
		}
	}
	if spec.Branches["main"] != captured {
		t.Errorf("main names commit %d, want the added one", spec.Branches["main"])
	}
	if spec.Commits[captured].Files[gitUnreadablePath] != unreadablePartitionSchema {
		t.Errorf("the added commit does not hold the file at %s", gitUnreadablePath)
	}
	migrationFile := gitUnreadableMigrationPath + "/" + unreadableMigrationFile
	if spec.Commits[captured].Files[migrationFile] != unreadableMigrationFiles[0].body {
		t.Errorf("the added commit does not hold the file at %s", migrationFile)
	}
	for index := range captured {
		for _, added := range []string{gitUnreadablePath, migrationFile} {
			if _, present := spec.Commits[index].Files[added]; present {
				t.Errorf("commit %d holds the file %s the row adds", index, added)
			}
		}
	}
}

// TestTheUnreadableFilesAreTheOnesThePinnedImageRefused keeps the row's input
// tied to what was measured: the delivered schema is the file the capture read,
// so the reason the row waits for is the reason that file gives.
func TestTheUnreadableFilesAreTheOnesThePinnedImageRefused(t *testing.T) {
	t.Parallel()

	if !strings.Contains(capturedReason(t, "validate-sql.txt"), unreadablePartitionReason) {
		t.Errorf("the capture does not say %q", unreadablePartitionReason)
	}
	if !strings.Contains(capturedReason(t, "scratch-directive.json"), unreadableMigrationReason) {
		t.Errorf("the capture does not say %q", unreadableMigrationReason)
	}
	if !strings.Contains(unreadablePartitionSchema, "PARTITION OF events") {
		t.Errorf("the row's file lost the statement the parser stops at")
	}
	up := unreadableMigrationFiles[0]
	if up.path != unreadableMigrationFile || !strings.Contains(up.body, "no_transaction=sometimes") {
		t.Errorf("the row's up migration is %+v", up)
	}
}

// TestFetchStepSaysHoldsTheClaim reads what failedInitBoundary writes, and refuses
// what the row is there to catch: another step, a message that says nothing of the
// run, and one that carries the words the step's container wrote.
func TestFetchStepSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const named = "the fetch-schema step exited 2, so the run never started"
	if err := fetchStepSays(named, unreadableFetchStep); err != nil {
		t.Fatalf("fetchStepSays(%q) = %v", named, err)
	}
	for name, message := range map[string]string{
		"another step":       "the fetch-migrations step exited 2, so the run never started",
		"a generic Job":      "observe Job failed",
		"no word of the run": "the fetch-schema step exited 2",
		"the reason":         named + ": parse schema artifact: parse HCL schema",
		"the file":           named + ": schema.hcl:3,31-32",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchStepSays(message, unreadableFetchStep); err == nil {
				t.Fatalf("fetchStepSays(%q) accepted it", message)
			}
		})
	}
}

// TestFetchStepLogSaysReadsTheMeasuredLine holds the log check to what `ptah schema
// pull` printed for the fixture on the pinned image.
func TestFetchStepLogSaysReadsTheMeasuredLine(t *testing.T) {
	t.Parallel()

	measured := []byte("error: parse schema artifact: parse HCL schema: schema.hcl:3,31-32: Invalid single-argument block definition; " +
		"A single-line block definition must end with a closing brace immediately after its single argument definition.\n")
	if err := fetchStepLogSays(measured); err != nil {
		t.Fatalf("fetchStepLogSays() = %v", err)
	}
	for name, log := range map[string][]byte{
		"an empty log":          nil,
		"a registry refusal":    []byte("error: unauthorized: authentication required\n"),
		"another layer's parse": []byte("error: parse schema artifact: read managed data layer\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchStepLogSays(log); err == nil {
				t.Fatalf("fetchStepLogSays(%q) accepted it", log)
			}
		})
	}
}

// TestFetchStepEventFindsTheEventOfTheResource keeps the Event search to the
// resource and the reason it was asked about, and says which way it came up empty.
func TestFetchStepEventFindsTheEventOfTheResource(t *testing.T) {
	t.Parallel()

	const named = "the fetch-schema step exited 2, so the run never started"
	event := func(owner types.UID, reason, eventType, message string) corev1.Event {
		return corev1.Event{
			InvolvedObject: corev1.ObjectReference{UID: owner}, Reason: reason, Type: eventType, Message: message,
		}
	}
	if err := fetchStepEvent([]corev1.Event{event("a", "OperationFailed", corev1.EventTypeWarning, named)}, "a", "OperationFailed", unreadableFetchStep); err != nil {
		t.Fatalf("fetchStepEvent() = %v", err)
	}
	for name, events := range map[string][]corev1.Event{
		"no Event":            nil,
		"another resource":    {event("b", "OperationFailed", corev1.EventTypeWarning, named)},
		"another reason":      {event("a", "OperationRetried", corev1.EventTypeWarning, named)},
		"a Normal Event":      {event("a", "OperationFailed", corev1.EventTypeNormal, named)},
		"an Event with words": {event("a", "OperationFailed", corev1.EventTypeWarning, named+": parse schema artifact")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchStepEvent(events, "a", "OperationFailed", unreadableFetchStep); err == nil {
				t.Fatalf("fetchStepEvent() accepted %+v", events)
			}
		})
	}
}
