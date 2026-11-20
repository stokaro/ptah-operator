package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// An Apply of a plan with repeatable migrations is read per repeatable
// migration, from the run's document: which ones it applied, a selection under
// the lock that is not the approved one, and one it left unfinished, which the
// history read after it stops on. Each row holds the outcome, the message a
// person reads, the list the run records, whether the run stops the resource,
// and whether its Job keeps its log for the person who has to repair it.
func TestAMigrationApplyReadsEachRepeatableMigration(t *testing.T) {
	t.Parallel()

	approved := []operatorv1alpha1.PlannedRepeatableMigration{{Identity: "names", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA}}
	selected := func(identity, checksum string) dataplane.MigrationRepeatableWork {
		return dataplane.MigrationRepeatableWork{Identity: identity, Path: "R__" + identity + ".sql", Checksum: checksum,
			AppliedChecksum: repeatableContentA, Reason: dataplane.RepeatableReasonChanged}
	}
	afterward := func(record dataplane.MigrationRepeatable) *dataplane.MigrationStatusReport {
		report := syncedHistoryWith(record)
		report.CurrentVersion = 3
		report.Migrations = append(report.Migrations,
			dataplane.MigrationRecord{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied})
		return &report
	}
	record := func(state string, noTransaction bool) dataplane.MigrationRepeatable {
		return dataplane.MigrationRepeatable{Identity: "names", Path: "R__names.sql", Checksum: repeatableContentB,
			AppliedChecksum: repeatableContentA, State: state, NoTransaction: noTransaction}
	}
	for _, test := range []struct {
		name               string
		planned            []operatorv1alpha1.PlannedMigration
		report             dataplane.MigrationRunReport
		outcome            operatorv1alpha1.MigrationRunOutcome
		message            string
		appliedVersions    []int64
		appliedRepeatables []string
		// stops is a run the resource does not plan past: one that left a
		// record a person has to repair, or one nobody accounted for.
		stops bool
	}{
		{
			name:    "a version, then the repeatable migration",
			planned: []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}},
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeApplied, Planned: []int64{3}, Applied: []int64{3},
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				AppliedRepeatables: []string{"names"}, Status: afterward(record(dataplane.RepeatableStateApplied, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, appliedVersions: []int64{3}, appliedRepeatables: []string{"names"},
		},
		{
			name: "the repeatable migration alone",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeApplied,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				AppliedRepeatables: []string{"names"}, Status: afterward(record(dataplane.RepeatableStateApplied, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, appliedRepeatables: []string{"names"},
		},
		{
			name: "other content selected under the lock",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentC)}},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed,
			message: "Ptah selected other content for the repeatable migrations under the migration lock than the plan " +
				"approved, so it ran nothing: the history moved after the approval",
		},
		{
			name: "another repeatable migration selected under the lock",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB), selected("totals", repeatableContentC)}},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed,
			message: "Ptah selected repeatable migrations [names totals] under the migration lock and the plan approved [names], " +
				"so it ran nothing: the history moved after the approval",
		},
		{
			name: "failed inside a transaction, alone",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				Status:             afterward(record(dataplane.RepeatableStateFailed, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed,
			message: "Repeatable migration names failed inside a transaction, which took no effect; 0 migrations before it are recorded applied",
			stops:   true,
		},
		{
			// Ptah reads a clean failure after other work as partial, so the
			// run is one nobody accounted for even though the repeatable
			// migration took no effect.
			name:    "failed inside a transaction, after a version",
			planned: []operatorv1alpha1.PlannedMigration{{Version: 3, Checksum: "checksum-3"}},
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{3}, Applied: []int64{3},
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				Status:             afterward(record(dataplane.RepeatableStateFailed, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomePartial, appliedVersions: []int64{3},
			message: "Repeatable migration names failed inside a transaction, which took no effect; 1 migration before it is recorded applied",
			stops:   true,
		},
		{
			name: "failed outside a transaction",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				Status:             afterward(record(dataplane.RepeatableStateFailed, true))},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			message: "Repeatable migration names failed outside a transaction and may have left part of its work behind",
			stops:   true,
		},
		{
			name: "stopped inside it",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeUnknown,
				PlannedRepeatables: []dataplane.MigrationRepeatableWork{selected("names", repeatableContentB)},
				Status:             afterward(record(dataplane.RepeatableStateRunning, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			message: "Repeatable migration names recorded its start and no outcome, so whether its statements took effect is not known",
			stops:   true,
		},
		{
			// Another run left the record between the history read and this
			// Apply. Ptah refuses every run over it and reports the run by the
			// record's state.
			name: "refused over a record another run left",
			report: dataplane.MigrationRunReport{Outcome: dataplane.MigrationOutcomeFailed,
				Status: afterward(record(dataplane.RepeatableStateFailed, false))},
			outcome: operatorv1alpha1.MigrationRunOutcomeFailed,
			message: "Ptah ran nothing, because repeatable migration names did not finish in an earlier run",
			stops:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harvest := harvestRepeatableApply(t, test.planned, approved, test.report)
			actual, api, job := harvest.migration, harvest.api, harvest.job
			run := actual.Status.LastRun
			if run == nil {
				t.Fatalf("the run left no record: phase %s, conditions %#v", actual.Status.Phase, actual.Status.Conditions)
			}
			if run.Outcome != test.outcome {
				t.Fatalf("outcome = %s, want %s (message %q)", run.Outcome, test.outcome, run.Message)
			}
			if test.message != "" && !strings.HasPrefix(run.Message, test.message) {
				t.Fatalf("message = %q, want it to start %q", run.Message, test.message)
			}
			if !slices.Equal(run.AppliedVersions, test.appliedVersions) || !slices.Equal(run.AppliedRepeatables, test.appliedRepeatables) {
				t.Fatalf("the run records versions %v and repeatable migrations %v, want %v and %v",
					run.AppliedVersions, run.AppliedRepeatables, test.appliedVersions, test.appliedRepeatables)
			}
			blocked := meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			unresolved := actual.Status.UnresolvedRun != nil
			if jobclaim.MigrationRunUnresolved(test.outcome) != unresolved || (unresolved && !blocked) {
				t.Fatalf("unresolved run %t and Blocked %t for outcome %s", unresolved, blocked, test.outcome)
			}
			// Without an approval, only a run that stops keeps its Job past
			// the ordinary cleanup, so a refused selection, which a reading
			// plans past, does not hold one for a day.
			ttl := storedTTL(t, api, job)
			want := int32(jobclaim.CleanupTTLSeconds)
			if test.stops {
				want = jobclaim.FailedApplyRetentionTTLSeconds
			}
			if ttl == nil || *ttl != want {
				t.Fatalf("the Job's TTL = %s, want %d", ttlText(ttl), want)
			}
			assertDatabaseHandedBack(t, harvest.reconciler, api)
		})
	}
}

// repeatableHarvest is what one pass over a finished Apply left: the
// migration as stored, and what a row needs to read the rest.
type repeatableHarvest struct {
	migration  *operatorv1alpha1.PtahMigration
	reconciler *MigrationReconciler
	api        client.Client
	job        *batchv1.Job
}

// harvestRepeatableApply runs one pass over a completed Apply of a plan that
// approved the versions and repeatable migrations given, whose runner reported
// the run given.
func harvestRepeatableApply(
	t *testing.T,
	planned []operatorv1alpha1.PlannedMigration,
	approved []operatorv1alpha1.PlannedRepeatableMigration,
	report dataplane.MigrationRunReport,
) repeatableHarvest {
	t.Helper()
	return harvestRepeatableApplyFrame(t, planned, approved, report, nil)
}

// harvestRepeatableApplyFrame is harvestRepeatableApply with the frame
// changed by frame before the runner's log holds it. The frame starts as the
// runner writes it, uncertain for every run Ptah reported partial or unknown.
func harvestRepeatableApplyFrame(
	t *testing.T,
	planned []operatorv1alpha1.PlannedMigration,
	approved []operatorv1alpha1.PlannedRepeatableMigration,
	report dataplane.MigrationRunReport,
	frame func(*runner.Result),
) repeatableHarvest {
	t.Helper()
	migration, _ := awaitingApprovalFixture(t)
	plan := publishedPlanWith(t, migration, planned, approved)
	migration.Status.Plan.Name, migration.Status.Plan.UID = plan.Name, plan.UID
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	report.ContractVersion, report.Direction = dataplane.SupportedMigrationRunContract, "up"
	result := runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, CoordinationDigest: operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MutationStarted:      true, MigrationRun: &report,
		Uncertain: report.Outcome == dataplane.MigrationOutcomePartial || report.Outcome == dataplane.MigrationOutcomeUnknown,
	}
	if frame != nil {
		frame(&result)
	}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: migrationFrame(t, result)},
		migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return repeatableHarvest{migration: readMigration(t, api, migration), reconciler: reconciler, api: api, job: job}
}
