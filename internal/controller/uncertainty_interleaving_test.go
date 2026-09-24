package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// #225 asks for a contract suite run against both families over interleaved
// failures: uncertainty followed by another refusal, suspension and resume,
// a changed generation, and a restart. The record of a mutation nobody could
// account for has to survive every one of them, because each is a way the
// resource's visible state is rewritten while the question the record asks --
// did something change the database -- is untouched.
//
// Both families were covered, and asymmetrically, which is the thing this
// issue is about. A migration had "outlives every other refusal" and no
// suspension row; a schema had suspension and retry and no other-refusal row.
// The case below is the one neither family had.
//
// Two interleavings are deliberately not here.
//
// Restart, because for these records a restarted manager is another pass:
// nothing in memory carries them and the adoption that runs first is
// idempotent, so the test would assert that reconciling twice reconciles
// twice. Where a restart does carry an obligation -- a schema renewing the
// Lease its proof still owes -- it belongs with the Lease.
//
// A refusal arriving while a *schema* owes proof, because there is no such
// interleaving to write. The note here used to say the row needed a fixture
// nobody had built -- a schema owing proof with no claim in flight -- so it
// was built, and the answer is structural rather than missing.
//
// schema_controller.go returns on `schema.Status.PendingObservation != nil`
// well above the realm census and above every refusal decided below it. A
// schema owing proof discharges the proof and never reaches them. Measured
// with the sharpest refusal available: a second resource claiming the same
// database realm without declaring it shared, which the census does count as
// a conflict on exactly this object pair -- two schemas, both undeclared --
// and which the pass never asks about, claiming the read-only operation that
// discharges the proof instead.
//
// So the schema family answers this interleaving by ordering, and the ordering
// is the thing to hold. That belongs with the boundary itself, not with a test
// asserting that a refusal which cannot arrive does not clear a record.
//
// Where a family already answers an interleaving, its existing test does; a
// second one measuring the same thing is not coverage.

// migrationWithUnresolvedRun returns a migration in the state a History
// reading leaves when it refuses to settle a record: blocked, no plan, and the
// record standing.
func migrationWithUnresolvedRun(t *testing.T) *operatorv1alpha1.PtahMigration {
	t.Helper()

	migration, _ := awaitingApprovalFixture(t)
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome:              operatorv1alpha1.MigrationRunOutcomeUnknown,
		RecordedAt:           metav1.Now(),
		JobName:              "ptah-m-apply-orders-abcdef0123456789",
		JobUID:               "apply-job-uid",
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
	}
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApplyOutcomeUnknown, "A run nobody accounted for is recorded")
	return migration
}

// Suspending a migration does not clear the record, and resuming does not
// treat it as answered.
//
// Suspension is the gesture an operator reaches for when a resource is stuck,
// and a record cleared by it would be cleared by the one action somebody
// stuck is most likely to take.
func TestAnUnresolvedMigrationRunSurvivesSuspensionAndResume(t *testing.T) {
	t.Parallel()

	migration := migrationWithUnresolvedRun(t)
	migration.Spec.Suspend = true
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() while suspended: %v", err)
	}
	suspended := readMigration(t, api, migration)
	if suspended.Status.UnresolvedRun == nil {
		t.Fatal("suspending the migration cleared the record of a run nobody accounted for")
	}

	suspended.Spec.Suspend = false
	if err := api.Update(context.Background(), suspended); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() after resume: %v", err)
	}
	resumed := readMigration(t, api, migration)
	if resumed.Status.UnresolvedRun == nil {
		t.Fatal("resuming the migration cleared the record instead of leaving it for a person")
	}
	if resumed.Status.ActiveOperation != nil &&
		resumed.Status.ActiveOperation.Type == operatorv1alpha1.MigrationOperationApply {
		t.Fatalf("resuming claimed an Apply with the record standing: %#v", resumed.Status.ActiveOperation)
	}
}
