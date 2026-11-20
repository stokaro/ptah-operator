package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// measuredRepeatables is what each document under testdata/repeatables says,
// as Ptah v0.34.0 wrote it against PostgreSQL 17 (pg-) and MySQL 8.4 (my-).
// v0.33.1 wrote the same documents but for the times they record.
// Each scenario ran against a database of its own: s1 one repeatable migration
// pending, applied, changed, failing, refused for want of an approval, and
// refused over its failed record with and without its file; s2 a version and a
// repeatable migration in one run, then the file removed; s3 a run stopped
// inside a repeatable migration; s4 a drift check over an applied repeatable
// migration; s5 a repeatable migration under --tx-mode none.
//
// The runner harness owns internal/dataplane/testdata and removes a file no
// row of it writes, so these documents, taken by hand, live here.
type measuredRepeatables struct {
	// outcome is a run's outcome, empty for a status.
	outcome string
	// planned and applied are the repeatable migrations a run selected and
	// the ones it applied.
	planned, applied []string
	// pending, unfinished and orphaned are what the status says of the
	// record: the run's own for a run, read after it stopped.
	pending, unfinished, orphaned []string
	// replayed is what a drift check replayed of the repeatable migrations.
	replayed []string
}

var measuredRepeatableDocuments = map[string]measuredRepeatables{
	"pg-s1-status-pending-alone.json":          {pending: []string{"item_names"}},
	"pg-s1-up-no-repeatable.json":              {outcome: "failed", planned: []string{"item_names"}, pending: []string{"item_names"}},
	"pg-s1-up-pending-alone.json":              {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s1-status-applied.json":                {},
	"pg-s1-status-changed.json":                {pending: []string{"item_names"}},
	"pg-s1-up-changed.json":                    {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s1-status-failing-changed.json":        {pending: []string{"item_names"}},
	"pg-s1-up-failing.json":                    {outcome: "failed", planned: []string{"item_names"}, unfinished: []string{"item_names"}},
	"pg-s1-status-failed.json":                 {unfinished: []string{"item_names"}},
	"pg-s1-up-over-failed-empty.json":          {outcome: "failed", unfinished: []string{"item_names"}},
	"pg-s1-up-over-failed-named.json":          {outcome: "failed", unfinished: []string{"item_names"}},
	"pg-s1-status-failed-no-file.json":         {unfinished: []string{"item_names"}},
	"pg-s1-up-over-failed-no-file.json":        {outcome: "failed", unfinished: []string{"item_names"}},
	"pg-s1-up-v1.json":                         {outcome: "applied"},
	"pg-s2-status-version-and-repeatable.json": {pending: []string{"item_names"}},
	"pg-s2-up-version-and-repeatable.json":     {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s2-status-orphaned.json":               {orphaned: []string{"item_names"}},
	"pg-s2-up-over-orphaned.json":              {outcome: "up-to-date", orphaned: []string{"item_names"}},
	"pg-s3-up-c.json":                          {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s3-status-running.json":                {unfinished: []string{"zz_slow"}},
	"pg-s3-up-over-running.json":               {outcome: "unknown", unfinished: []string{"zz_slow"}},
	"pg-s4-up-d.json":                          {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s4-status-h.json":                      {pending: []string{"item_names"}},
	"pg-s4-up-h-drift.json": {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"},
		replayed: []string{"item_names"}},
	"pg-s4-status-after.json":                  {},
	"pg-s5-up-txnone.json":                     {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"pg-s5-status-txnone.json":                 {},
	"my-s1-status-pending-alone.json":          {pending: []string{"item_names"}},
	"my-s1-up-no-repeatable.json":              {outcome: "failed", planned: []string{"item_names"}, pending: []string{"item_names"}},
	"my-s1-up-pending-alone.json":              {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s1-status-applied.json":                {},
	"my-s1-status-changed.json":                {pending: []string{"item_names"}},
	"my-s1-up-changed.json":                    {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s1-status-failing-changed.json":        {pending: []string{"item_names"}},
	"my-s1-up-failing.json":                    {outcome: "unknown", planned: []string{"item_names"}, unfinished: []string{"item_names"}},
	"my-s1-status-failed.json":                 {unfinished: []string{"item_names"}},
	"my-s1-up-over-failed-empty.json":          {outcome: "unknown", unfinished: []string{"item_names"}},
	"my-s1-up-over-failed-named.json":          {outcome: "unknown", unfinished: []string{"item_names"}},
	"my-s1-status-failed-no-file.json":         {unfinished: []string{"item_names"}},
	"my-s1-up-over-failed-no-file.json":        {outcome: "unknown", unfinished: []string{"item_names"}},
	"my-s1-up-v1.json":                         {outcome: "applied"},
	"my-s2-status-version-and-repeatable.json": {pending: []string{"item_names"}},
	"my-s2-up-version-and-repeatable.json":     {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s2-status-orphaned.json":               {orphaned: []string{"item_names"}},
	"my-s2-up-over-orphaned.json":              {outcome: "up-to-date", orphaned: []string{"item_names"}},
	"my-s3-up-c.json":                          {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s3-status-running.json":                {unfinished: []string{"zz_slow"}},
	"my-s3-up-over-running.json":               {outcome: "unknown", unfinished: []string{"zz_slow"}},
	"my-s4-up-d.json":                          {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s4-status-h.json":                      {pending: []string{"item_names"}},
	"my-s4-up-h-drift-own-shadow.json": {outcome: "failed", planned: []string{"item_names"}, pending: []string{"item_names"},
		replayed: []string{"item_names"}},
	"my-s4-status-after.json":  {pending: []string{"item_names"}},
	"my-s5-up-txnone.json":     {outcome: "applied", planned: []string{"item_names"}, applied: []string{"item_names"}},
	"my-s5-status-txnone.json": {},
}

// Every document Ptah wrote about repeatable migrations decodes, says what the
// operator acts on, and passes through the runner into a frame: a status as a
// History's, a run as an Apply's, under a drift check where it carries one.
// The frame carries none of the error text a record keeps about a failed
// attempt. Each file has a row and each row a file, so a document added
// without a row, or a row whose file went, fails here.
func TestEveryMeasuredRepeatableDocumentReachesAFrame(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("testdata", "repeatables", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(measuredRepeatableDocuments) {
		t.Fatalf("testdata/repeatables holds %d documents and the table declares %d", len(files), len(measuredRepeatableDocuments))
	}
	for _, file := range files {
		name := filepath.Base(file)
		want, declared := measuredRepeatableDocuments[name]
		if !declared {
			t.Errorf("%s has no row", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var status *dataplane.MigrationStatusReport
			if want.outcome == "" {
				report, err := dataplane.DecodeMigrationStatus(data)
				if err != nil {
					t.Fatalf("DecodeMigrationStatus() error = %v", err)
				}
				status = &report
			} else {
				report, err := dataplane.DecodeMigrationRun(data)
				if err != nil {
					t.Fatalf("DecodeMigrationRun() error = %v", err)
				}
				status = measuredRunSays(t, report, want)
			}
			if status == nil {
				t.Fatal("the document holds no status")
			}
			measuredStatusSays(t, *status, want)
			measuredDocumentReachesAFrame(t, name, data, want)
		})
	}
}

// measuredRunSays holds a run report to its row and returns the status Ptah
// read after it.
func measuredRunSays(t *testing.T, report dataplane.MigrationRunReport, want measuredRepeatables) *dataplane.MigrationStatusReport {
	t.Helper()
	if report.Outcome != want.outcome || report.StoppedBeforeSelecting() {
		t.Fatalf("outcome %s, stopped before selecting %t; want %s after selecting",
			report.Outcome, report.StoppedBeforeSelecting(), want.outcome)
	}
	if planned := report.PlannedRepeatableIdentities(); !slices.Equal(planned, want.planned) {
		t.Errorf("selected %v, want %v", planned, want.planned)
	}
	if !slices.Equal(report.AppliedRepeatables, want.applied) {
		t.Errorf("applied %v, want %v", report.AppliedRepeatables, want.applied)
	}
	var replayed []string
	if report.DriftCheck != nil {
		replayed = report.DriftCheck.ReplayedRepeatables
		if summary := report.DriftCheck.Summary(); summary.ReplayedRepeatables != len(replayed) {
			t.Errorf("the summary counts %d replayed, the check names %v", summary.ReplayedRepeatables, replayed)
		}
	}
	if !slices.Equal(replayed, want.replayed) {
		t.Errorf("the drift check replayed %v, want %v", replayed, want.replayed)
	}
	return report.Status
}

func measuredStatusSays(t *testing.T, status dataplane.MigrationStatusReport, want measuredRepeatables) {
	t.Helper()
	if got := repeatableIdentities(status.PendingRepeatables()); !slices.Equal(got, want.pending) {
		t.Errorf("pending %v, want %v", got, want.pending)
	}
	if got := repeatableIdentities(status.UnfinishedRepeatables()); !slices.Equal(got, want.unfinished) {
		t.Errorf("unfinished %v, want %v", got, want.unfinished)
	}
	if got := repeatableIdentities(status.OrphanedRepeatables()); !slices.Equal(got, want.orphaned) {
		t.Errorf("orphaned %v, want %v", got, want.orphaned)
	}
}

// measuredDocumentReachesAFrame runs the document through the runner as the
// operation that reads it and holds the frame to what the row says.
func measuredDocumentReachesAFrame(t *testing.T, name string, data []byte, want measuredRepeatables) {
	t.Helper()
	operationID := "measured-" + name
	response := scriptedResponse{stdout: string(data)}
	operation, environment := OperationMigrationHistory, withRunnerProtocol(migrationEnvironment(operationID))
	if want.outcome != "" {
		response.stdout = string(referencedRun(t, data, operationID))
		operation = OperationMigrationApply
		environment = withRunnerProtocol(migrationApplyEnvironment(t, operationID))
		if want.replayed != nil {
			environment = driftApplyEnvironment(t, "measured-"+name, testShadowURL, shadowDigest(t, testShadowURL))
		}
		if want.outcome != "applied" && want.outcome != "up-to-date" {
			response.exitCode = 1
		}
	}
	result := Run(context.Background(), Config{
		Operation:   operation,
		Environment: environment,
		Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{response}},
	})
	if result.Error != nil && result.Error.Code != "child_exit" {
		t.Fatalf("Run() error = %#v, want the document read", result.Error)
	}
	var status *dataplane.MigrationStatusReport
	switch {
	case want.outcome == "":
		status = result.MigrationHistory
	case result.MigrationRun != nil:
		status = result.MigrationRun.Status
		if !slices.Equal(result.MigrationRun.AppliedRepeatables, want.applied) {
			t.Errorf("the frame's run applied %v, want %v", result.MigrationRun.AppliedRepeatables, want.applied)
		}
	}
	if status == nil {
		t.Fatalf("Run() = %#v, want the database's account in the frame", result)
	}
	measuredStatusSays(t, *status, want)
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	failed, errors := recordErrors(t, data)
	if failed > 0 && len(errors) == 0 {
		t.Fatal("a failed record kept no error text, so the row would hold nothing out of the frame")
	}
	for _, text := range errors {
		if bytes.Contains(frame, []byte(text)) {
			t.Errorf("the frame carries what the record said of the attempt: %q", text)
		}
	}
}

// referencedRun is a run document with the reference the runner hands
// Ptah as --run-reference on its log entries. The measurement ran Ptah by
// hand, without one, and the runner refuses a run that counts written entries
// under none.
func referencedRun(t *testing.T, data []byte, operationID string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	log, ok := document["migration_log"].(map[string]any)
	if !ok {
		return data
	}
	if written, _ := log["written"].(float64); written > 0 {
		log["run_reference"] = operationID
	}
	referenced, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return referenced
}

// recordErrors counts the failed repeatable records of a document, wherever
// its status sits, and returns the error texts the records keep.
func recordErrors(t *testing.T, data []byte) (int, []string) {
	t.Helper()
	type record struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	var document struct {
		Repeatables []record `json:"repeatables"`
		Status      *struct {
			Repeatables []record `json:"repeatables"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	records := document.Repeatables
	if document.Status != nil {
		records = document.Status.Repeatables
	}
	failed := 0
	var texts []string
	for _, record := range records {
		if record.State == dataplane.RepeatableStateFailed {
			failed++
		}
		if record.Error != "" {
			texts = append(texts, record.Error)
		}
	}
	return failed, texts
}

func repeatableIdentities(repeatables []dataplane.MigrationRepeatable) []string {
	var names []string
	for _, repeatable := range repeatables {
		names = append(names, repeatable.Identity)
	}
	return names
}
