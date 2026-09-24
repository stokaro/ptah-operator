package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// planPublishingMigration is a migration whose History run has just come back
// with pending work, which is the pass that publishes a plan.
func planPublishingMigration(t *testing.T) (*operatorv1alpha1.PtahMigration, []byte, []client.Object) {
	t.Helper()

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
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
			{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending},
		},
	}
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: testDigest,
		MigrationHistory:     &report,
	})
	return migration, frame, []client.Object{job, pod, verificationPolicyConfigMap()}
}

// The plan's object name is derived from what the plan is, so two publications
// of one decision are one object and a controller that restarted mid-write
// cannot leave a second copy. The cost of deriving it is that something else
// can already hold the name, and adopting whatever stands there would point
// status.plan at a decision this migration never made.
//
// Neither half of the check that the standing object is this plan was
// measured: not the fingerprint, and not the migration it belongs to.
func TestAMigrationAdoptsNoPlanButItsOwnUnderItsDerivedName(t *testing.T) {
	t.Parallel()

	// What the pass publishes when nothing holds the name. This is also the
	// control: a fixture that stopped publishing would fail here.
	migration, frame, extra := planPublishingMigration(t)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame},
		append([]client.Object{migration}, extra...)...)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	published := readMigration(t, api, migration).Status.Plan
	if published == nil {
		t.Fatal("the pass published no plan, so holding its name proves nothing")
	}
	// The published object, so a standing plan can differ from it in exactly
	// one respect and the rest of it stays the plan this pass would compute.
	publishedPlan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := api.Get(context.Background(),
		client.ObjectKey{Namespace: migration.Namespace, Name: published.Name}, publishedPlan); err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		name     string
		standing func(*operatorv1alpha1.PtahMigrationPlan)
	}{
		{
			// Another migration computed a plan that happens to derive the
			// same name.
			name: "the name is held by another migration's plan",
			standing: func(plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Spec.MigrationRef = operatorv1alpha1.ImmutableObjectReference{
					Name: "another-migration", UID: types.UID("another-migration-uid"),
				}
			},
		},
		{
			// The same migration, and not this decision.
			name: "the name is held by a plan of another fingerprint",
			standing: func(plan *operatorv1alpha1.PtahMigrationPlan) {
				plan.Spec.Fingerprint = "sha256:" + strings.Repeat("e", 64)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, frame, extra := planPublishingMigration(t)
			standing := publishedPlan.DeepCopy()
			standing.ResourceVersion = ""
			standing.UID = "a-plan-that-was-here-first"
			row.standing(standing)
			objects := append([]client.Object{migration, standing}, extra...)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, objects...)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Plan != nil {
				t.Fatalf("the migration adopted a plan standing under its derived name: %#v",
					actual.Status.Plan)
			}
			var named bool
			for _, condition := range actual.Status.Conditions {
				if strings.Contains(condition.Message, "deterministic name") {
					named = true
				}
			}
			if !named {
				t.Fatalf("no condition names the colliding plan name: phase=%q conditions=%#v",
					actual.Status.Phase, actual.Status.Conditions)
			}
		})
	}
}
