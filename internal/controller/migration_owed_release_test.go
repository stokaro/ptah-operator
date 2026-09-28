package controller

import (
	"context"
	"errors"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// leaseReadFaultReader makes releasing the database fail the only way it can:
// an epoch that does not match is not an error, and a Lease that is gone is
// not one either, so the failure has to come from the read itself.
type leaseReadFaultReader struct {
	client.Reader
	failure error
}

func (r *leaseReadFaultReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*coordinationv1.Lease); ok {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// A migration run retired as uncertain stages the release it owes in the
// write that drops its claim, and only then tries the release. So a release
// that fails leaves the record behind rather than a Lease nothing names.
//
// Staging is best effort, because the write carries the record of a run
// nobody accounted for, which must not be lost to bookkeeping. Best effort is
// not the same as losing an obligation already owed, nor as handing the
// database back under a claim the API server still holds.
func TestARetiredRunKeepsTheReleaseItOwes(t *testing.T) {
	t.Parallel()

	t.Run("a release that fails stays recorded", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		migration, plan := awaitingApprovalFixture(t)
		applyClaimFor(t, migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		reconciler.Locks = targetlock.New(
			&leaseReadFaultReader{Reader: api, failure: errors.New("etcdserver: request timed out")}, api, nil)

		stored := readMigration(t, api, migration)
		claim := stored.Status.ActiveOperation.DeepCopy()
		// Nothing stands under the name the claim reserved and it recorded no
		// UID, so nothing it dispatched can still write.
		if _, err := reconciler.finishUncertainMigrationApply(ctx, stored, nil,
			errors.New("the Apply Job create result is uncertain"), ""); err != nil {
			t.Fatal(err)
		}

		actual := readMigration(t, api, migration)
		if actual.Status.ActiveOperation != nil || actual.Status.UnresolvedRun == nil {
			t.Fatalf("the run was not retired as unresolved: claim %#v, record %#v",
				actual.Status.ActiveOperation, actual.Status.UnresolvedRun)
		}
		owed := actual.Status.PendingLockRelease
		if owed == nil {
			t.Fatal("a release that failed left nothing saying the database is still claimed")
		}
		if owed.CoordinationDigest != claim.CoordinationDigest || owed.OperationID != claim.ID ||
			owed.LeaseEpoch != claim.LeaseEpoch || owed.LeaseDurationSeconds != claim.LeaseDurationSeconds {
			t.Fatalf("the record does not reproduce the claim that took the Lease: %#v vs %#v", owed, claim)
		}
	})

	t.Run("another release already owed is not replaced", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		owed, err := targetLockReleaseForMigrationOperation(operation)
		if err != nil {
			t.Fatal(err)
		}
		// An obligation from an earlier operation, still unpaid.
		earlier := owed.DeepCopy()
		earlier.OperationID = "an-earlier-apply"
		migration.Status.PendingLockRelease = earlier

		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		reconciler.Locks = targetlock.New(
			&leaseReadFaultReader{Reader: api, failure: errors.New("etcdserver: request timed out")}, api, nil)

		stored := readMigration(t, api, migration)
		if _, err := reconciler.finishUncertainMigrationApply(ctx, stored, nil,
			errors.New("the Apply Job create result is uncertain"), ""); err != nil {
			t.Fatal(err)
		}

		actual := readMigration(t, api, migration)
		if actual.Status.PendingLockRelease == nil ||
			actual.Status.PendingLockRelease.OperationID != "an-earlier-apply" {
			t.Fatalf("a refused staging replaced the obligation already owed: %#v",
				actual.Status.PendingLockRelease)
		}
	})

	t.Run("a write that fails hands nothing back", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		migration, plan := awaitingApprovalFixture(t)
		applyClaimFor(t, migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		holdMigrationApplyLease(t, reconciler, api, migration)
		stored := readMigration(t, api, migration)
		// The claim stays in the API server, so the realm stays held by it.
		reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return errors.New("etcdserver: request timed out")
			},
		})

		if _, err := reconciler.finishUncertainMigrationApply(ctx, stored, nil,
			errors.New("the Apply Job create result is uncertain"), ""); err == nil {
			t.Fatal("a retirement whose write failed reported success")
		}

		assertDatabaseStillHeld(t, reconciler, api)
		if actual := readMigration(t, api, migration); actual.Status.ActiveOperation == nil {
			t.Fatal("the claim was dropped although its write failed")
		}
	})
}
