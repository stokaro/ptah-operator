package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// TestMigrationRunRefusedBeforeItsFirstFile runs the document Ptah v0.29.0
// printed for a run it refused before applying any file (stokaro/ptah#6052),
// as the runner harness captured it. The second file builds an index
// CONCURRENTLY, which the transaction it runs in refuses. Ptah's sentence
// names that file and ends with the statement: the Pod log keeps the sentence
// without the statement, and the frame carries neither, only a failed run
// that applied nothing and has no failure position.
func TestMigrationRunRefusedBeforeItsFirstFile(t *testing.T) {
	t.Parallel()

	document := ptahV023Document(t, "migrations-up-refused-postgres.json")
	const (
		sentence  = "0000000002_index_items.up.sql cannot run inside a transaction"
		statement = "CREATE INDEX CONCURRENTLY items_name ON items (name)"
	)
	if !strings.Contains(document, sentence) || !strings.Contains(document, statement) {
		t.Fatal("the captured document no longer carries the file's name and its statement, so this row proves nothing about either")
	}
	var log bytes.Buffer
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "sha256:"+strings.Repeat("a", 64))),
		Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: 2}}},
		Diagnostics: &log,
	})
	if !strings.Contains(log.String(), "ptah-runner: ptah reported: "+sentence) {
		t.Fatalf("the Pod log reads %q, want Ptah's sentence naming the file", log.String())
	}
	if strings.Contains(log.String(), statement) {
		t.Fatalf("the Pod log carries the statement: %q", log.String())
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	for _, text := range []string{sentence, statement, "index_items"} {
		if bytes.Contains(frame, []byte(text)) {
			t.Fatalf("the framed result carries %q: %s", text, frame)
		}
	}
	parsed, err := ParseResult(frame)
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	run := parsed.MigrationRun
	if run == nil || run.Outcome != dataplane.MigrationOutcomeFailed || len(run.Applied) != 0 || run.Failure != nil {
		t.Fatalf("the controller parsed migration run %#v, want a failed run that applied nothing and names no failure", run)
	}
	if run.Status == nil || run.Status.DirtyRevision != nil || len(run.Status.Pending()) != 2 {
		t.Fatalf("the controller parsed history %#v, want both migrations pending and no dirty row", run.Status)
	}
	if parsed.Uncertain {
		t.Fatal("the run is marked uncertain, and Ptah applied nothing")
	}
}
