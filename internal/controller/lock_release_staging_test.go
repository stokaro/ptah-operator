package controller

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A claim that took no Lease owes no release, and asking for one is not a
// smaller mistake than skipping one: the record refuses a binding with no
// epoch, so the pass that tried to write it fails instead of recording what it
// came to record.
//
// `stageOperationLockRelease` already answers this -- an operation with no
// epoch stages nothing and returns no error. Five sites built the record
// themselves and reached the same question without it.
//
// The protected-table refusal is the one measured here because its cost is the
// plainest: Ptah refused to plan a change to a protected table, that refusal is
// the whole result of the run, and a pass that cannot persist it leaves the
// resource saying nothing about why it stopped.
func TestARefusalIsRecordedByAClaimThatTookNoLease(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// leased says whether the Plan claim holds a Lease epoch. Only a
		// leased claim owes a release; the unleased row is the one that used
		// to fail the pass.
		leased bool
		// proof says whether a post-Apply observation is outstanding. It owns
		// the realm, so the claim retiring under it owes nothing.
		proof bool
		owes  bool
	}{
		{name: "holding a Lease, no proof outstanding", leased: true, proof: false, owes: true},
		{name: "holding no Lease, no proof outstanding", leased: false, proof: false, owes: false},
		{name: "holding a Lease under an outstanding proof", leased: true, proof: true, owes: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyPostApplyObserveSchema(t)
			epoch := schema.Status.PendingObservation.LeaseEpoch
			digest := schema.Status.PendingObservation.CoordinationDigest
			if !row.proof {
				schema.Status.PendingObservation = nil
			}
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationPlan, ID: "plan-operation",
				JobName: "plan-job", JobUID: "plan-job-uid", Attempt: 1,
				CoordinationDigest: digest, TargetIdentityDigest: testDigest,
				LeaseDurationSeconds: 960,
			}
			if row.leased {
				schema.Status.ActiveOperation.LeaseEpoch = epoch
			}

			reconciler, api := fakeReconciler(t, staticLogs{}, schema)
			writes := &[]retiredClaim{}
			reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
			reconciler.Locks = targetlock.New(api, api, nil)

			stored := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
				t.Fatal(err)
			}

			if _, err := reconciler.refuseProtectedTable(
				context.Background(), stored, nil, `table "orders" is protected`); err != nil {
				t.Fatalf("the refusal was not recorded: %v", err)
			}

			retirement, reached := firstRetirement(*writes)
			if !reached {
				t.Fatalf("the refusal never reached the API server, so nothing here was measured: %#v", *writes)
			}
			if retirement.owed != row.owes {
				t.Fatalf("the write that recorded the refusal carried owed=%t, want %t; writes: %#v",
					retirement.owed, row.owes, *writes)
			}
		})
	}
}
