package controller

import (
	"context"
	"errors"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// TestARecordOfAnOwedReleaseThatCannotBeWrittenChangesNothing covers the two
// rollbacks in recordOwedMigrationLockRelease.
//
// Writing down a release that did not happen is best effort on purpose: the
// evidence of what the run did is already durable, and failing the pass that
// carries it would lose more than the record is worth. Where the record cannot
// be written, the Lease expires on its own as it did before.
//
// Best effort is not the same as leaving the resource half-written. The caller
// goes on using this object and patches it afterwards, so a staged release
// that never reached the API server must not survive in memory -- above all
// where the staging was refused because another release was already owed.
// Carrying that overwrite into the caller's own patch would replace one
// obligation with another and lose the first.
func TestARecordOfAnOwedReleaseThatCannotBeWrittenChangesNothing(t *testing.T) {
	t.Parallel()

	t.Run("another release is already owed", func(t *testing.T) {
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

		reconciler.releaseMigrationApplyLock(ctx, migration, operation)

		if migration.Status.PendingLockRelease == nil ||
			migration.Status.PendingLockRelease.OperationID != "an-earlier-apply" {
			t.Fatalf("a refused record replaced the obligation already owed: %#v",
				migration.Status.PendingLockRelease)
		}
	})

	t.Run("the record cannot be persisted", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		if migration.Status.PendingLockRelease != nil {
			t.Fatal("the fixture already owes a release, so this row proves nothing")
		}

		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		reconciler.Locks = targetlock.New(
			&leaseReadFaultReader{Reader: api, failure: errors.New("etcdserver: request timed out")}, api, nil)
		// The resource is gone, so the record has nowhere to land.
		if err := api.Delete(ctx, migration); err != nil {
			t.Fatal(err)
		}

		reconciler.releaseMigrationApplyLock(ctx, migration, operation)

		if migration.Status.PendingLockRelease != nil {
			t.Fatalf("a record that never reached the API server survived in memory: %#v",
				migration.Status.PendingLockRelease)
		}
	})

	t.Run("a record that lands is kept", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)

		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		reconciler.Locks = targetlock.New(
			&leaseReadFaultReader{Reader: api, failure: errors.New("etcdserver: request timed out")}, api, nil)

		reconciler.releaseMigrationApplyLock(ctx, migration, operation)

		if migration.Status.PendingLockRelease == nil ||
			migration.Status.PendingLockRelease.OperationID != operation.ID {
			t.Fatalf("a release that failed left no obligation behind: %#v",
				migration.Status.PendingLockRelease)
		}
		stored := readMigration(t, api, migration)
		if stored.Status.PendingLockRelease == nil ||
			stored.Status.PendingLockRelease.OperationID != operation.ID {
			t.Fatalf("the obligation was never made durable: %#v", stored.Status.PendingLockRelease)
		}
	})
}
