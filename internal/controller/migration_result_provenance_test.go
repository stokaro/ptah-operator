package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A migration run's report becomes the resource's history: which versions are
// applied, and therefore what the next plan will be computed against. So the
// controller takes it only from a run that can account for itself and that ran
// against the database the claim named.
//
// The refusal is a disjunction and neither part was measured. A report
// produced against another database would be written into this migration's
// history as though it said something about it.
//
// One neighbouring operand is worth naming rather than testing: the run's own
// Uncertain flag sits in the condition above, and the branch it selects
// consumes the report whenever there is one. It changes nothing a report
// cannot already say, so there is no row for it here.
func TestAMigrationRunsReportIsTakenOnlyFromTheRunItClaims(t *testing.T) {
	t.Parallel()

	succeeded := func() *dataplane.MigrationRunReport {
		return &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomeApplied,
			Planned:         []int64{3},
			Applied:         []int64{3},
		}
	}

	for _, row := range []struct {
		name        string
		change      func(*runner.Result, *operatorv1alpha1.PtahMigration)
		wantOutcome operatorv1alpha1.MigrationRunOutcome
		wantPhase   operatorv1alpha1.MigrationPhase
	}{
		{
			// The control. Without it every row below would pass against a
			// path that consumes nothing.
			name:        "a run that accounts for itself is taken",
			change:      func(*runner.Result, *operatorv1alpha1.PtahMigration) {},
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeApplied,
			wantPhase:   operatorv1alpha1.MigrationPhaseVerifyingHistory,
		},
		{
			// Another database realm. Its history is not this migration's.
			name: "the run reports another coordination realm",
			change: func(result *runner.Result, _ *operatorv1alpha1.PtahMigration) {
				result.CoordinationDigest = "sha256:" + strings.Repeat("e", 64)
			},
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			wantPhase:   operatorv1alpha1.MigrationPhaseBlocked,
		},
		{
			// The same realm and a different database inside it.
			name: "the run reports another database identity",
			change: func(result *runner.Result, _ *operatorv1alpha1.PtahMigration) {
				result.TargetIdentityDigest = "sha256:" + strings.Repeat("e", 64)
			},
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
			wantPhase:   operatorv1alpha1.MigrationPhaseBlocked,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			result := runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun:         succeeded(),
			}
			row.change(&result, migration)

			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: migrationFrame(t, result)},
				migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			holdMigrationApplyLease(t, reconciler, api, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.LastRun == nil {
				t.Fatalf("the run left no evidence in status: phase=%q", actual.Status.Phase)
			}
			if actual.Status.LastRun.Outcome != row.wantOutcome {
				t.Fatalf("outcome = %q, want %q", actual.Status.LastRun.Outcome, row.wantOutcome)
			}
			if actual.Status.Phase != row.wantPhase {
				t.Fatalf("phase = %q, want %q", actual.Status.Phase, row.wantPhase)
			}
			if row.wantPhase == operatorv1alpha1.MigrationPhaseBlocked &&
				!meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked) {
				t.Fatal("a run the controller could not take was not blocked")
			}
		})
	}
}
