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

// measuredFetchReason is what the manager writes for the fixture, from what
// `ptah schema pull` printed on the pinned image: the harness row
// oci-schema-fetch/observe-unparsable holds the sentence there.
const measuredFetchReason = runner.CodeDesiredSourceUnreadable + ": parse schema artifact: parse HCL schema: schema.hcl:3,31-32: " +
	"Invalid single-argument block definition; A single-line block definition must end with a closing brace immediately after its single argument definition."

// TestFetchReasonSaysHoldsTheClaim reads what failedInitBoundary writes, and
// refuses what the row is there to catch: the sentence that names only the step,
// another step, a message that says nothing of the run, half of the reason, and a
// sentence of the fetch's that carries the registry's words.
func TestFetchReasonSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const host = "e2e-registry.ptah-test.svc:5000"
	if err := fetchReasonSays(measuredFetchReason, host); err != nil {
		t.Fatalf("fetchReasonSays(%q) = %v", measuredFetchReason, err)
	}
	const named = "the fetch-schema step exited 2, so the run never started"
	code := runner.CodeDesiredSourceUnreadable + ": "
	for name, message := range map[string]string{
		"only the step":      named,
		"another step":       "the fetch-migrations step exited 2, so the run never started",
		"a generic Job":      "observe Job failed",
		"no word of the run": "the fetch-schema step exited 2",
		"the step and the reason": named + ": parse schema artifact: parse HCL schema: schema.hcl:3,31-32: " +
			"Invalid single-argument block definition",
		"the reason with no code": "parse schema artifact: parse HCL schema: schema.hcl:3,31-32: Invalid single-argument block definition",
		"the code with no reason": code + "parse schema artifact: parse HCL schema: schema.hcl:3,31-32:",
		"no place in the file":    code + "parse schema artifact: parse HCL schema: Invalid single-argument block definition",
		"another file's parser": code + "error parsing schema file: read SQL schema file /source/schema/schema.hcl:3,31-32: " +
			"Invalid single-argument block definition",
		"the reason and the step": measuredFetchReason + " the fetch-schema step exited 2",
		"the registry by host":    measuredFetchReason + " from " + host,
		"the reference":           measuredFetchReason + " oci://elsewhere/a@sha256:0",
		"a registry refusal":      measuredFetchReason + " fetch OCI manifest",
		"an authorization":        measuredFetchReason + " unauthorized",
		"a credential's file":     measuredFetchReason + " /credentials/docker/config.json",
		"the scratch space":       measuredFetchReason + " /fetch-work/schema.hcl",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchReasonSays(message, host); err == nil {
				t.Fatalf("fetchReasonSays(%q) accepted it", message)
			}
		})
	}
}

// TestFetchStatusSaysReadsTheTerminationMessage holds the status check to what the
// runner writes, and to the messages a Pod could hold in its place.
func TestFetchStatusSaysReadsTheTerminationMessage(t *testing.T) {
	t.Parallel()

	status := func(message string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: unreadableFetchStep, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: message},
		}}
	}
	const marker = "PTAH_RUNNER_FETCH_REASON_V1 "
	sentence := strings.TrimPrefix(measuredFetchReason, runner.CodeDesiredSourceUnreadable+": ")
	if err := fetchStatusSays(status(marker + sentence + "\n")); err != nil {
		t.Fatalf("fetchStatusSays() = %v", err)
	}
	for name, candidate := range map[string]corev1.ContainerStatus{
		"no message":           status(""),
		"the sentence bare":    status(sentence),
		"another parse":        status(marker + "parse schema artifact: read schema.hcl\n"),
		"a registry refusal":   status(marker + "pull oci://registry: unauthorized\n"),
		"a step still running": {Name: unreadableFetchStep},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchStatusSays(candidate); err == nil {
				t.Fatalf("fetchStatusSays(%+v) accepted it", candidate)
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

// TestFetchReasonEventFindsTheEventOfTheResource keeps the Event search to the
// resource and the reason it was asked about, and says which way it came up empty.
func TestFetchReasonEventFindsTheEventOfTheResource(t *testing.T) {
	t.Parallel()

	const host = "e2e-registry.ptah-test.svc:5000"
	event := func(owner types.UID, reason, eventType, message string) corev1.Event {
		return corev1.Event{
			InvolvedObject: corev1.ObjectReference{UID: owner}, Reason: reason, Type: eventType, Message: message,
		}
	}
	if err := fetchReasonEvent([]corev1.Event{event("a", "OperationFailed", corev1.EventTypeWarning, measuredFetchReason)}, "a", "OperationFailed", host); err != nil {
		t.Fatalf("fetchReasonEvent() = %v", err)
	}
	for name, events := range map[string][]corev1.Event{
		"no Event":         nil,
		"another resource": {event("b", "OperationFailed", corev1.EventTypeWarning, measuredFetchReason)},
		"another reason":   {event("a", "OperationRetried", corev1.EventTypeWarning, measuredFetchReason)},
		"a Normal Event":   {event("a", "OperationFailed", corev1.EventTypeNormal, measuredFetchReason)},
		"an Event naming only the step": {
			event("a", "OperationFailed", corev1.EventTypeWarning, "the fetch-schema step exited 2, so the run never started"),
		},
		"an Event with the registry's words": {event("a", "OperationFailed", corev1.EventTypeWarning, measuredFetchReason+" "+host)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchReasonEvent(events, "a", "OperationFailed", host); err == nil {
				t.Fatalf("fetchReasonEvent() accepted %+v", events)
			}
		})
	}
}
