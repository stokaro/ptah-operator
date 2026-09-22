package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// owedReleaseFor moves the claim's lock binding into the record a failed
// release leaves behind, and retires the claim the way the outcome patch does.
func owedReleaseFor(
	t *testing.T,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) {
	t.Helper()

	stored := readMigration(t, api, migration)
	release, err := targetLockReleaseForMigrationOperation(stored.Status.ActiveOperation)
	if err != nil {
		t.Fatalf("build the owed release: %v", err)
	}
	before := stored.DeepCopy()
	stored.Status.PendingLockRelease = release
	stored.Status.ActiveOperation = nil
	stored.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	if err := api.Status().Patch(context.Background(), stored, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
}

// A database handed back by a release that failed is handed back on the next
// pass instead of at the end of the lease.
//
// The failure this covers is one API error at one instant. Before the record
// existed it cost every other claimant of that database the whole lease
// duration -- sixteen minutes by default, and as much as a day where the claim
// asked for one -- for a run that had already finished and already reported.
func TestAnOwedDatabaseLockIsHandedBackOnTheNextPass(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	owedReleaseFor(t, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	assertDatabaseHandedBack(t, reconciler, api)
	actual := readMigration(t, api, migration)
	if actual.Status.PendingLockRelease != nil {
		t.Fatalf("the record survived a release that succeeded: %#v", actual.Status.PendingLockRelease)
	}
}

// The pass that owes a database does nothing else. Every other claimant of
// that database is waiting on it, and a pass that claims new work first makes
// them wait for that too.
func TestAPassThatOwesADatabaseDoesNothingElse(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	owedReleaseFor(t, api, migration)

	result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatal("the pass that released the database did not ask to look again")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("the pass that owed a database claimed %d Job(s) as well", len(jobs.Items))
	}
	if actual := readMigration(t, api, migration); actual.Status.ActiveOperation != nil {
		t.Fatalf("the pass that owed a database took a new claim: %#v", actual.Status.ActiveOperation)
	}
}

// A release that fails is written down. Without the record the realm stays
// claimed and nothing in status says so.
func TestAFailedReleaseIsRecordedAsOwed(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)

	stored := readMigration(t, api, migration)
	claim := stored.Status.ActiveOperation.DeepCopy()
	reconciler.recordOwedMigrationLockRelease(context.Background(), stored, claim,
		errors.New("the API server refused the release"))

	actual := readMigration(t, api, migration)
	owed := actual.Status.PendingLockRelease
	if owed == nil {
		t.Fatal("a release that failed left nothing saying the database is still claimed")
	}
	if owed.CoordinationDigest != claim.CoordinationDigest || owed.OperationID != claim.ID ||
		owed.LeaseEpoch != claim.LeaseEpoch || owed.LeaseDurationSeconds != claim.LeaseDurationSeconds {
		t.Fatalf("the record does not reproduce the claim that took the Lease: %#v vs %#v", owed, claim)
	}
}

// Two obligations on one resource would mean one of them was dropped, so the
// second is refused rather than overwriting the first.
func TestASecondOwedReleaseIsRefused(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.LeaseEpoch = "v1-" + "0123456789abcdef0123456789abcdef"
	if err := stageMigrationLockRelease(migration, operation); err != nil {
		t.Fatalf("stage the first release: %v", err)
	}
	first := migration.Status.PendingLockRelease.DeepCopy()
	if err := stageMigrationLockRelease(migration, operation); err == nil {
		t.Fatal("a second owed release replaced the first")
	}
	if got := migration.Status.PendingLockRelease; got.LeaseEpoch != first.LeaseEpoch {
		t.Fatalf("the refused staging still changed the record: %#v", got)
	}
}

// A claim that took no Lease owes nothing.
func TestAClaimWithNoLeaseStagesNothing(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.LeaseEpoch = ""
	if err := stageMigrationLockRelease(migration, operation); err != nil {
		t.Fatalf("stage a claim that took no Lease: %v", err)
	}
	if migration.Status.PendingLockRelease != nil {
		t.Fatalf("a claim that took no Lease recorded one as owed: %#v", migration.Status.PendingLockRelease)
	}
}
