package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// measuredRepeatableRuns is where internal/runner keeps the documents Ptah
// v0.34.0 wrote about repeatable migrations against PostgreSQL 17 (pg-) and
// MySQL 8.4 (my-). Its measured_repeatables_test.go says what each scenario
// ran; the runs are the files named *-up-*.
const measuredRepeatableRuns = "../runner/testdata/repeatables"

// measuredRun decodes one run document as the runner does, and drops the
// error object the runner keeps out of a frame.
func measuredRun(t *testing.T, name string) dataplane.MigrationRunReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(measuredRepeatableRuns, name))
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeMigrationRun(data)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	report.Error = nil
	return report
}

// What the operator makes of each run Ptah wrote: the outcome it records,
// whether the history read after it stops on a record, and how the message
// opens. A run Ptah refused over an unfinished record selected and applied
// nothing, so it is Failed whatever outcome Ptah gave it, and only a run that
// selected the repeatable migration and failed outside a transaction stays
// Unknown.
var measuredRunVerdicts = map[string]struct {
	outcome operatorv1alpha1.MigrationRunOutcome
	stops   bool
	message string
}{
	"pg-s1-up-no-repeatable.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, message: "A migration failed and committed nothing"},
	"pg-s1-up-pending-alone.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 repeatable migration is recorded applied"},
	"pg-s1-up-changed.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 repeatable migration is recorded applied"},
	"pg-s1-up-failing.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: "Repeatable migration item_names failed inside a transaction, which took no effect"},
	"pg-s1-up-over-failed-empty.json":   {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"pg-s1-up-over-failed-named.json":   {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"pg-s1-up-over-failed-no-file.json": {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"pg-s1-up-v1.json":                  {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration is recorded applied"},
	"pg-s2-up-version-and-repeatable.json": {outcome: operatorv1alpha1.MigrationRunOutcomeApplied,
		message: "1 migration and 1 repeatable migration are recorded applied"},
	"pg-s2-up-over-orphaned.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeUpToDate, message: "The database already had every planned migration"},
	"pg-s3-up-c.json":                   {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration and 1 repeatable migration are recorded applied"},
	"pg-s3-up-over-running.json":        {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("zz_slow")},
	"pg-s4-up-d.json":                   {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "2 migrations and 1 repeatable migration are recorded applied"},
	"pg-s4-up-h-drift.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration and 1 repeatable migration are recorded applied"},
	"pg-s5-up-txnone.json":              {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration and 1 repeatable migration are recorded applied"},
	"my-s1-up-no-repeatable.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, message: "A migration failed and committed nothing"},
	"my-s1-up-pending-alone.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 repeatable migration is recorded applied"},
	"my-s1-up-changed.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 repeatable migration is recorded applied"},
	"my-s1-up-failing.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, stops: true, message: "Repeatable migration item_names failed outside a transaction and may have left part of its work behind"},
	"my-s1-up-over-failed-empty.json":   {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"my-s1-up-over-failed-named.json":   {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"my-s1-up-over-failed-no-file.json": {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("item_names")},
	"my-s1-up-v1.json":                  {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration is recorded applied"},
	"my-s2-up-version-and-repeatable.json": {outcome: operatorv1alpha1.MigrationRunOutcomeApplied,
		message: "1 migration and 1 repeatable migration are recorded applied"},
	"my-s2-up-over-orphaned.json":      {outcome: operatorv1alpha1.MigrationRunOutcomeUpToDate, message: "The database already had every planned migration"},
	"my-s3-up-c.json":                  {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration and 1 repeatable migration are recorded applied"},
	"my-s3-up-over-running.json":       {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true, message: ranNothingOver("zz_slow")},
	"my-s4-up-d.json":                  {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "2 migrations and 1 repeatable migration are recorded applied"},
	"my-s4-up-h-drift-own-shadow.json": {outcome: operatorv1alpha1.MigrationRunOutcomeFailed, message: "Migration 3 (Index Name) failed and committed nothing"},
	"my-s5-up-txnone.json":             {outcome: operatorv1alpha1.MigrationRunOutcomeApplied, message: "1 migration and 1 repeatable migration are recorded applied"},
}

func ranNothingOver(identity string) string {
	return "Ptah ran nothing, because repeatable migration " + identity + " did not finish in an earlier run"
}

// Every run document Ptah wrote has a row, and each row a document, so a
// measurement added without a verdict, or a verdict whose file went, fails
// here; the count is held to the table's, not to more than none.
func TestClassifyMigrationRunReadsEveryMeasuredRun(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join(measuredRepeatableRuns, "*-up-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(measuredRunVerdicts) {
		t.Fatalf("%s holds %d run documents and the table declares %d", measuredRepeatableRuns, len(files), len(measuredRunVerdicts))
	}
	for _, file := range files {
		name := filepath.Base(file)
		want, declared := measuredRunVerdicts[name]
		if !declared {
			t.Errorf("%s has no row", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			verdict := classifyMigrationRun(measuredRun(t, name))
			if verdict.outcome != want.outcome || verdict.stopsAtRecord != want.stops || !strings.HasPrefix(verdict.message, want.message) {
				t.Fatalf("verdict %s, stops %t, %q; want %s, stops %t, opening %q",
					verdict.outcome, verdict.stopsAtRecord, verdict.message, want.outcome, want.stops, want.message)
			}
			// Every measured run read the history after it.
			if verdict.stoppedBeforeSelecting {
				t.Fatal("the verdict says the run stopped before selecting with no history read, and the report holds one")
			}
		})
	}
}

// The rule a run that selected and applied nothing ran nothing holds for a
// report that is not Ptah's refusal over a repeatable record too: a run
// refused over a dirty revision row an earlier run left, and one that names no
// reason at all. A run that selected something and is Unknown stays Unknown,
// and so does a Partial one, and so does an outcome this build does not know,
// whatever its report lists.
func TestARunThatSelectedNothingRanNothing(t *testing.T) {
	t.Parallel()

	dirty := syncedHistoryWith()
	dirty.DirtyRevision = &dataplane.MigrationDirty{Version: 2, Applied: 1, Total: 3}
	for _, test := range []struct {
		name    string
		report  dataplane.MigrationRunReport
		outcome operatorv1alpha1.MigrationRunOutcome
		stops   bool
		message string
		// unread is a run that stopped before it selected and whose report
		// holds no history after it; compares is one whose selection may be
		// held against the plan.
		unread, compares bool
	}{
		{
			name:    "unknown, with no history read after it",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, unread: true,
			message: "Ptah stopped before it selected any migration and could not read the history after it, so it ran nothing",
		},
		{
			name:    "partial, with no history read after it",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomePartial},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, unread: true,
			message: "Ptah stopped before it selected any migration and could not read the history after it, so it ran nothing",
		},
		{
			name:    "failed, with no history read after it",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, unread: true,
			message: "The run stopped before it selected any migration, and ran none",
		},
		{
			name:    "failed, having selected nothing, with the history read after it",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Status: new(syncedHistoryWith())},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, compares: true,
			message: "A migration failed and committed nothing",
		},
		{
			name:    "unknown after selecting a version, with no history read after it",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown, Planned: []int64{3}},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			name:    "unknown over a dirty revision row",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown, Status: &dirty},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true,
			message: "Ptah ran nothing, because revision 2 is recorded dirty by an earlier run",
		},
		{
			name:    "partial with no reason",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomePartial, Status: new(syncedHistoryWith())},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed,
			message: "Ptah selected no migration and ran nothing",
		},
		{
			name: "unknown after selecting a version",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown, Planned: []int64{3},
				Status: new(syncedHistoryWith())},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			message: "The database's own account of the run could not be read",
		},
		{
			name: "partial after applying a version",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{3, 4},
				Applied: []int64{3}, Status: new(syncedHistoryWith())},
			outcome: operatorv1alpha1.MigrationRunOutcomePartial,
			message: "A migration committed some of its statements and not the rest",
		},
		{
			name: "unknown over a record no file name carries",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown,
				Status: new(syncedHistoryWith(orphanedFailed("old names\x1b[2J")))},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, stops: true,
			message: `Ptah ran nothing, because repeatable migration "old names\x1b[2J" did not finish in an earlier run`,
		},
		{
			name:    "an outcome this build does not know, with nothing selected",
			report:  dataplane.MigrationRunReport{Outcome: "abandoned", Status: new(syncedHistoryWith(orphanedFailed("names")))},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			name:    "up to date over an unfinished record",
			report:  dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUpToDate, Status: new(syncedHistoryWith(orphanedFailed("names")))},
			outcome: operatorv1alpha1.MigrationRunOutcomeUpToDate,
			message: "The database already had every planned migration",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			verdict := classifyMigrationRun(test.report)
			if verdict.outcome != test.outcome || verdict.stopsAtRecord != test.stops || !strings.HasPrefix(verdict.message, test.message) {
				t.Fatalf("verdict %s, stops %t, %q; want %s, stops %t, opening %q",
					verdict.outcome, verdict.stopsAtRecord, verdict.message, test.outcome, test.stops, test.message)
			}
			if verdict.stoppedBeforeSelecting != test.unread || verdict.comparesSelection != test.compares {
				t.Fatalf("verdict stopped before selecting %t and compares the selection %t, want %t and %t",
					verdict.stoppedBeforeSelecting, verdict.comparesSelection, test.unread, test.compares)
			}
		})
	}
}

// A run that stopped before it selected and whose report holds no history
// after it ran nothing, and the history is as the plan read it. It waits the
// failure interval in phase Failed rather than reading the history back at
// once, and the plan's list is not held against its empty selection, since
// nothing separates a refused selection from a run that never selected. Ptah
// reports it unknown when the history read after the failure failed too, and
// the runner marks such a frame uncertain. The control is the same run as Ptah
// reports it when that read was not attempted.
func TestARunThatStoppedBeforeSelectingWaitsTheFailureInterval(t *testing.T) {
	t.Parallel()

	approved := []operatorv1alpha1.PlannedRepeatableMigration{{Identity: "names", Checksum: repeatableContentB}}
	for _, test := range []struct {
		name    string
		outcome string
		message string
	}{
		{
			name: "reported unknown", outcome: dataplane.MigrationOutcomeUnknown,
			message: "Ptah stopped before it selected any migration and could not read the history after it, so it ran nothing",
		},
		{
			name: "reported failed", outcome: dataplane.MigrationOutcomeFailed,
			message: "The run stopped before it selected any migration, and ran none",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harvest := harvestRepeatableApply(t, nil, approved, dataplane.MigrationRunReport{Outcome: test.outcome})
			actual, run := harvest.migration, harvest.migration.Status.LastRun
			if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed || actual.Status.UnresolvedRun != nil {
				t.Fatalf("last run %+v with unresolved run %+v, want Failed and nothing to acknowledge", run, actual.Status.UnresolvedRun)
			}
			if !strings.HasPrefix(run.Message, test.message) || strings.Contains(run.Message, "history moved") {
				t.Fatalf("message = %q, want it to start %q", run.Message, test.message)
			}
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed || actual.Status.NextReconciliationTime == nil {
				t.Fatalf("phase %s with next reading %v, want Failed and a next reading", actual.Status.Phase, actual.Status.NextReconciliationTime)
			}
			wait := actual.Status.NextReconciliationTime.Sub(run.FinishedAt.Time)
			if retry := migrationFailureRetry(actual); wait < retry-time.Second || wait > retry+time.Second {
				t.Fatalf("the next reading is %s after the run, want the failure interval %s", wait, retry)
			}
		})
	}
}

// A run is taken only from the database its claim named unless its verdict
// still reads Unknown or Partial. The runner marks every run Ptah reported
// unknown as uncertain, a refused one included; once the verdict makes that
// run Failed, taking it from another database would put that database's
// record on this resource, so it is the wrong-database Unknown instead. A run
// that stays Unknown is taken whatever database it names, since it blocks the
// resource either way and only a reading settles it.
func TestARunIsTakenFromAnotherDatabaseOnlyWhileItStaysUnknown(t *testing.T) {
	t.Parallel()

	approved := []operatorv1alpha1.PlannedRepeatableMigration{{Identity: "item_names", Checksum: repeatableContentB}}
	other := "sha256:" + strings.Repeat("e", 64)
	for _, test := range []struct {
		name    string
		file    string
		target  string
		outcome operatorv1alpha1.MigrationRunOutcome
		message string
	}{
		{
			name: "refused over a running record, from another database", file: "pg-s3-up-over-running.json", target: other,
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, message: "a database other than the one it was planned for",
		},
		{
			name: "refused over a running record, from the planned database", file: "pg-s3-up-over-running.json",
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed, message: "Ptah ran nothing, because repeatable migration ",
		},
		{
			name: "failed outside a transaction, from another database", file: "my-s1-up-failing.json", target: other,
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, message: "Repeatable migration item_names failed outside a transaction",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harvest := harvestRepeatableApplyFrame(t, nil, approved, measuredRun(t, test.file), func(result *runner.Result) {
				if !result.Uncertain {
					t.Fatal("the runner marks a run Ptah reported unknown as uncertain, and this frame is not")
				}
				if test.target != "" {
					result.TargetIdentityDigest = test.target
				}
			})
			run := harvest.migration.Status.LastRun
			if run == nil || run.Outcome != test.outcome || !strings.Contains(run.Message, test.message) {
				t.Fatalf("last run %+v, want %s saying %q", run, test.outcome, test.message)
			}
		})
	}
}

// orphanedFailed is a failed record of identity that no file carries.
func orphanedFailed(identity string) dataplane.MigrationRepeatable {
	return dataplane.MigrationRepeatable{Identity: identity, Orphaned: true, AppliedChecksum: repeatableContentA,
		State: dataplane.RepeatableStateFailed, NoTransaction: true}
}

// A run that selected nothing over a dirty revision row an earlier run left
// was refused over the row, not over the plan, so it says Ptah ran nothing
// because of the row rather than that the history moved after the approval.
// The control is the same run with no row, which is the sequence refusal.
func TestARunRefusedOverADirtyRevisionNamesTheRow(t *testing.T) {
	t.Parallel()

	approved := []operatorv1alpha1.PlannedRepeatableMigration{{Identity: "names", Checksum: repeatableContentB}}
	for _, test := range []struct {
		name    string
		dirty   *dataplane.MigrationDirty
		message string
		stops   bool
	}{
		{
			name:    "over a dirty revision row",
			dirty:   &dataplane.MigrationDirty{Version: 2, Applied: 1, Total: 3},
			message: "Ptah ran nothing, because revision 2 is recorded dirty by an earlier run",
			stops:   true,
		},
		{
			name: "with no row",
			message: "Ptah selected repeatable migrations [] under the migration lock and the plan approved [names], " +
				"so it ran nothing: the history moved after the approval",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			history := syncedHistoryWith()
			history.DirtyRevision = test.dirty
			harvest := harvestRepeatableApply(t, nil, approved,
				dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed, Status: &history})
			actual, run := harvest.migration, harvest.migration.Status.LastRun
			if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed || actual.Status.UnresolvedRun != nil {
				t.Fatalf("last run %+v with unresolved run %+v, want Failed and nothing to acknowledge", run, actual.Status.UnresolvedRun)
			}
			if !strings.HasPrefix(run.Message, test.message) {
				t.Fatalf("message = %q, want it to start %q", run.Message, test.message)
			}
			want := int32(jobclaim.CleanupTTLSeconds)
			if test.stops {
				want = jobclaim.FailedApplyRetentionTTLSeconds
			}
			if ttl := storedTTL(t, harvest.api, harvest.job); ttl == nil || *ttl != want {
				t.Fatalf("the Job's TTL = %s, want %d", ttlText(ttl), want)
			}
		})
	}
}

// A run Ptah refused over a record an earlier run left unfinished, as Ptah
// wrote it, is recorded as a Failed run that ran nothing: no unresolved run,
// no ApplyOutcomeUnknown, a history read next, which stops on the record, and
// a Job kept for whoever repairs it. Ptah reported each of these unknown. The
// control is the run that selected the repeatable migration and failed outside
// a transaction on MySQL, which may have left work behind and stays Unknown
// until a person accounts for it.
func TestARunRefusedOverAnUnfinishedRecordIsFailed(t *testing.T) {
	t.Parallel()

	approved := []operatorv1alpha1.PlannedRepeatableMigration{{Identity: "item_names", Checksum: repeatableContentB}}
	for _, name := range []string{
		"pg-s3-up-over-running.json",
		"my-s3-up-over-running.json",
		"my-s1-up-over-failed-empty.json",
		"my-s1-up-over-failed-named.json",
		"my-s1-up-over-failed-no-file.json",
		"my-s1-up-failing.json",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report := measuredRun(t, name)
			if name != "my-s1-up-failing.json" && report.Outcome != dataplane.MigrationOutcomeUnknown {
				t.Fatalf("Ptah reported %s, and the row is about a refusal it reported unknown", report.Outcome)
			}
			harvest := harvestRepeatableApply(t, nil, approved, report)
			actual, run := harvest.migration, harvest.migration.Status.LastRun
			if run == nil {
				t.Fatalf("the run left no record: phase %s", actual.Status.Phase)
			}
			blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			outcomeUnknown := blocked != nil && blocked.Reason == string(operatorv1alpha1.ReasonApplyOutcomeUnknown)
			if name == "my-s1-up-failing.json" {
				if run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || actual.Status.UnresolvedRun == nil || !outcomeUnknown {
					t.Fatalf("the control read %s with unresolved run %+v and Blocked %+v, want Unknown and unresolved",
						run.Outcome, actual.Status.UnresolvedRun, blocked)
				}
				return
			}
			if run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed || actual.Status.UnresolvedRun != nil || outcomeUnknown {
				t.Fatalf("the refused run read %s with unresolved run %+v and Blocked %+v, want Failed and nothing to acknowledge",
					run.Outcome, actual.Status.UnresolvedRun, blocked)
			}
			if !strings.HasPrefix(run.Message, "Ptah ran nothing, because repeatable migration ") ||
				len(run.AppliedVersions) != 0 || len(run.AppliedRepeatables) != 0 {
				t.Fatalf("the refused run says %q and records %v and %v applied", run.Message, run.AppliedVersions, run.AppliedRepeatables)
			}
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseVerifyingHistory {
				t.Fatalf("phase %s, want the history read that stops on the record", actual.Status.Phase)
			}
			if ttl := storedTTL(t, harvest.api, harvest.job); ttl == nil || *ttl != jobclaim.FailedApplyRetentionTTLSeconds {
				t.Fatalf("the Job's TTL = %s, want the retention a run that stops its resource keeps", ttlText(ttl))
			}
		})
	}
}
