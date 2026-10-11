package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A migration's Resolve claim sets the next refresh one interval after the
// claim starts, through the real controller rather than a hand-built claim.
func TestMigrationResolveClaimSetsTheNextRefreshFromItsStart(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("active operation = %#v, want a Resolve claim", operation)
	}
	want := operation.StartedAt.Add(migrationInterval(actual))
	if next := actual.Status.NextReconciliationTime; next == nil || !next.Time.Equal(want) {
		t.Fatalf("Resolve claim persisted next=%v, want %v", next, want)
	}
}

// The end of a migration's read-only cycle keeps the deadline its Resolve set
// when the history settles in sync or blocked, and gives a plan that waits for
// approval a full interval from now.
func TestMigrationHistoryKeepsTheCadenceItsResolveSet(t *testing.T) {
	t.Parallel()
	inSync := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract, CurrentVersion: 3, TotalMigrations: 2,
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStateApplied},
		},
	}
	ahead := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract, CurrentVersion: 3, TotalMigrations: 1,
		Migrations: []dataplane.MigrationRecord{{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied}},
	}
	pending := dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract, CurrentVersion: 2, TotalMigrations: 3, HasPendingChanges: true,
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
		},
	}
	carriedIn := 50 * time.Second
	tests := []struct {
		name      string
		report    dataplane.MigrationStatusReport
		carried   bool
		wantPhase operatorv1alpha1.MigrationPhase
		// wantCarried: the next refresh is the carried deadline; otherwise one
		// interval from the harvest.
		wantCarried bool
	}{
		{"in sync keeps the carried deadline", inSync, true, operatorv1alpha1.MigrationPhaseInSync, true},
		{"in sync with nothing carried waits an interval", inSync, false, operatorv1alpha1.MigrationPhaseInSync, false},
		{"blocked keeps the carried deadline", ahead, true, operatorv1alpha1.MigrationPhaseBlocked, true},
		{"a plan for approval gets a full interval", pending, true, operatorv1alpha1.MigrationPhaseAwaitingApproval, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := test.report
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)
			carried := metav1.NewTime(reconciler.now().Add(carriedIn))
			if test.carried {
				stored := readMigration(t, api, migration)
				stored.Status.NextReconciliationTime = &carried
				if err := api.Status().Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", actual.Status.Phase, test.wantPhase)
			}
			want := reconciler.now().Add(migrationInterval(actual))
			if test.wantCarried {
				want = carried.Time
			}
			if next := actual.Status.NextReconciliationTime; next == nil || !next.Time.Equal(want) {
				t.Fatalf("next refresh = %v, want %v", next, want)
			}
		})
	}
}
