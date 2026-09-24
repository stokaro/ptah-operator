package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// One obligation, two families, and an order rather than a state.
//
// The claim is the only stored thing that names the epoch an operation took
// its Lease with. A pass that persists the claim's removal and releases
// afterwards leaves a window in which the database is held by an epoch nothing
// names: the manager can stop there, and every other claimant on that database
// then waits out the whole lease duration -- sixteen minutes by default -- for
// a run that has already finished.
//
// Coverage cannot see this and neither can a status assertion. The state each
// pass settles on is correct either way, because the release does happen, one
// statement later and only in memory. The window is between two writes, so the
// writes are what these rows read.

// One retirement is deliberately not in this table. An Apply that finished
// uncertainly releases only where a live read proves the dispatched Job can no
// longer write, and that read answers "may still be writing" to anything it
// cannot settle, a transient error included. Recording the obligation ahead of
// that answer would write down a release that must not happen, and the next
// pass would perform it. There the claim outliving the write is the safe
// failure: the Lease stays held until it expires, which is what the lock is
// for.

// retiredClaim is what one status write said about the claim and about the
// obligation that has to outlive it.
type retiredClaim struct {
	claimed bool
	owed    bool
}

// claimWriteRecorder reads every status write on its way to the API server.
type claimWriteRecorder struct {
	client.Client
	writes *[]retiredClaim
}

func (c *claimWriteRecorder) Status() client.SubResourceWriter {
	return &claimWriteWriter{SubResourceWriter: c.Client.Status(), writes: c.writes}
}

type claimWriteWriter struct {
	client.SubResourceWriter
	writes *[]retiredClaim
}

func (w *claimWriteWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	switch resource := object.(type) {
	case *operatorv1alpha1.PtahMigration:
		*w.writes = append(*w.writes, retiredClaim{
			claimed: resource.Status.ActiveOperation != nil,
			owed:    resource.Status.PendingLockRelease != nil,
		})
	case *operatorv1alpha1.PtahSchema:
		*w.writes = append(*w.writes, retiredClaim{
			claimed: resource.Status.ActiveOperation != nil,
			owed:    resource.Status.PendingLockRelease != nil,
		})
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

// firstRetirement returns the write that gave the claim up, and whether there
// was one at all. A row whose fixture never reaches it measured nothing.
func firstRetirement(writes []retiredClaim) (retiredClaim, bool) {
	for _, write := range writes {
		if !write.claimed {
			return write, true
		}
	}
	return retiredClaim{}, false
}

// claimRetirement is one family's way into the question. Each returns the
// status writes its retirement produced, in order.
//
// The plumbing is family-local because the fixtures and the reconcilers are.
// The contract is not: every row below is asserted the same way, so a family
// that retires a claim its own way still has to hand the database back in the
// same write.
type claimRetirement struct {
	family string
	name   string
	// leased says whether the claim took a Lease. A claim that took none owes
	// nothing, and these rows are the control: an implementation that recorded
	// an obligation unconditionally would name an empty epoch, which is a
	// release no later pass can perform.
	leased bool
	retire func(t *testing.T, leased bool) []retiredClaim
}

func migrationClaimRetirements() []claimRetirement {
	retire := func(
		drive func(*MigrationReconciler, *operatorv1alpha1.PtahMigration) error,
	) func(*testing.T, bool) []retiredClaim {
		return func(t *testing.T, leased bool) []retiredClaim {
			t.Helper()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			if !leased {
				operation.LeaseEpoch = ""
				operation.CoordinationDigest = ""
			}
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
			writes := &[]retiredClaim{}
			reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
			// Releasing succeeds here: a Lease that is not there is not a
			// failure to release, so nothing falls into the best-effort record
			// and the only way an obligation reaches the wire is a retirement
			// staging it.
			reconciler.Locks = targetlock.New(api, api, nil)

			if err := drive(reconciler, migration); err != nil {
				t.Fatal(err)
			}
			return *writes
		}
	}

	cannotDispatch := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		_, err := r.failUndispatchedMigrationOperation(
			context.Background(), migration, errors.New("the Job could not be created"))
		return err
	})
	wentStale := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		_, err := r.discardUndispatchedMigrationOperation(
			context.Background(), migration, errors.New("the dispatch deadline passed"))
		return err
	})

	runFinished := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		operation := migration.Status.ActiveOperation
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: operation.JobName, UID: "apply-job-uid",
			// Already carrying a deadline, so harvesting it writes nothing and
			// the only writes recorded below are the resource's own.
			ResourceVersion: "1",
		}}
		job.Spec.TTLSecondsAfterFinished = ptr(int32(jobCleanupTTLSeconds))
		_, err := r.consumeMigrationRun(context.Background(), migration, job, runner.Result{
			Operation:            runner.OperationMigrationApply,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: testDigest,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
				Planned: []int64{3}, Applied: []int64{3},
			},
		})
		return err
	})

	return []claimRetirement{
		{family: "PtahMigration", name: "a run that finished", leased: true, retire: runFinished},
		{family: "PtahMigration", name: "a run that finished, holding no Lease", leased: false, retire: runFinished},
		{family: "PtahMigration", name: "a claim that cannot dispatch", leased: true, retire: cannotDispatch},
		{family: "PtahMigration", name: "a claim that cannot dispatch, holding no Lease", leased: false, retire: cannotDispatch},
		{family: "PtahMigration", name: "a claim whose dispatch deadline passed", leased: true, retire: wentStale},
	}
}

func schemaClaimRetirements() []claimRetirement {
	policyChanged := func(t *testing.T, leased bool) []retiredClaim {
		t.Helper()

		schema := safetyLockedOperationSchema(operatorv1alpha1.OperationApply)
		if !leased {
			schema.Status.ActiveOperation.LeaseEpoch = ""
			schema.Status.ActiveOperation.CoordinationDigest = ""
		}
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		writes := &[]retiredClaim{}
		reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
		reconciler.Locks = targetlock.New(api, api, nil)

		// Read the stored object back: the fixture pointer carries the
		// resource version it had before the fake client accepted it, and the
		// status patch below takes the optimistic lock.
		stored := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
			t.Fatal(err)
		}

		if _, err := reconciler.verificationPolicyChanged(
			context.Background(), stored, errors.New("the verification policy bytes changed")); err != nil {
			t.Fatal(err)
		}
		return *writes
	}

	return []claimRetirement{
		{family: "PtahSchema", name: "a verification policy that changed under the claim", leased: true, retire: policyChanged},
		{family: "PtahSchema", name: "the same, holding no Lease", leased: false, retire: policyChanged},
	}
}

// TestRetiringAClaimRecordsTheReleaseItOwes holds both families to one order:
// the write that drops a claim carries the release that claim owes, and the
// release itself happens after.
func TestRetiringAClaimRecordsTheReleaseItOwes(t *testing.T) {
	t.Parallel()

	rows := append(migrationClaimRetirements(), schemaClaimRetirements()...)
	for _, row := range rows {
		t.Run(row.family+"/"+row.name, func(t *testing.T) {
			t.Parallel()

			writes := row.retire(t, row.leased)

			retirement, reached := firstRetirement(writes)
			if !reached {
				t.Fatalf("the fixture never gave the claim up, so nothing here was measured: %#v", writes)
			}
			if retirement.owed != row.leased {
				t.Fatalf("the write that dropped the claim recorded owed=%t, want %t; writes: %#v",
					retirement.owed, row.leased, writes)
			}
		})
	}
}
