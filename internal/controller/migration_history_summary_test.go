package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// status.history.appliedCount is what an operator reads to know how much of
// the sequence the database already holds, and a checkpoint is the reason that
// is not the same as how many migrations ran. A checkpoint covers the versions
// before it: they are applied, and none of them was ever executed on this
// database.
//
// The summary counts both states in one condition, so counting either marks
// the branch measured. No test named the covered state at all, and dropping it
// leaves a database whose sequence is complete reading as though work remains.
func TestAHistorySummaryCountsWhatTheDatabaseHolds(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name        string
		records     []dataplane.MigrationRecord
		wantApplied int32
		wantPending int32
	}{
		{
			name: "migrations that ran",
			records: []dataplane.MigrationRecord{
				{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateApplied},
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
			},
			wantApplied: 2,
			wantPending: 1,
		},
		{
			// Never executed here, and present all the same.
			name: "migrations a checkpoint covers",
			records: []dataplane.MigrationRecord{
				{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateCheckpointCovered},
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateCheckpointCovered},
				{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
			},
			wantApplied: 2,
			wantPending: 1,
		},
		{
			name: "some of each",
			records: []dataplane.MigrationRecord{
				{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateCheckpointCovered},
				{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
				{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
			},
			wantApplied: 2,
			wantPending: 1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration := migrationFixture()
			migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := dataplane.MigrationStatusReport{
				ContractVersion:   dataplane.SupportedMigrationStatusContract,
				CurrentVersion:    2,
				TotalMigrations:   3,
				HasPendingChanges: true,
				Migrations:        row.records,
			}
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame},
				migration, job, pod, verificationPolicyConfigMap())

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			history := readMigration(t, api, migration).Status.History
			if history == nil {
				t.Fatal("the reading left no history to summarize")
			}
			if history.AppliedCount != row.wantApplied {
				t.Fatalf("appliedCount = %d, want %d: the summary does not say what the "+
					"database holds", history.AppliedCount, row.wantApplied)
			}
			if history.PendingCount != row.wantPending {
				t.Fatalf("pendingCount = %d, want %d", history.PendingCount, row.wantPending)
			}
		})
	}
}
