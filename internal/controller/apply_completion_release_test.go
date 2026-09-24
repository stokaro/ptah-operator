package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// An Apply that completed does not hand the database back. It hands it to the
// post-Apply observation, which is recorded in the same write and carries the
// Apply's own lease epoch so the proof runs under the same lock.
//
// This is the case a state-shaped reading gets wrong. `RealmHeldBy` reports a
// mutating claim with a proof outstanding as the realm's owner, which is what
// the retirement paths need and the opposite of what this one does, so a
// condition here written in terms of realm ownership releases the lease the
// proof was just given. That mistake reaches no unit assertion -- the status
// this pass settles on looks right either way -- and costs three acceptance
// jobs to find.
func TestACompletedApplyHandsTheRealmToItsProofRatherThanBack(t *testing.T) {
	t.Parallel()

	schema := safetyApplySchema(t)
	schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	schema.Status.ActiveOperation.JobUID = "apply-job-uid"
	operation := schema.Status.ActiveOperation.DeepCopy()

	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	writes := &[]retiredClaim{}
	reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
	reconciler.Locks = targetlock.New(api, api, nil)

	stored := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName,
		UID: "apply-job-uid", ResourceVersion: "1",
	}}
	job.Spec.TTLSecondsAfterFinished = ptr(int32(jobCleanupTTLSeconds))

	if _, err := reconciler.consumeResult(context.Background(), stored, job, runner.Result{
		Operation:            runner.OperationApply,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: operation.TargetIdentityDigest,
	}, nil, 0); err != nil {
		t.Fatalf("the Apply result was not consumed: %v", err)
	}

	after := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), after); err != nil {
		t.Fatal(err)
	}
	// The control: without a proof recorded there is no handover to measure,
	// and the rows below would pass against a pass that did something else.
	if after.Status.PendingObservation == nil {
		t.Fatalf("the Apply recorded no post-Apply proof, so nothing here was measured: %#v", after.Status)
	}
	if after.Status.PendingObservation.LeaseEpoch == "" {
		t.Fatal("the proof carries no lease epoch, so there was no realm to hand over")
	}
	for index, write := range *writes {
		if write.owed {
			t.Fatalf("write %d staged a database release while the proof it just recorded still needs the realm: %#v",
				index, *writes)
		}
	}
	if after.Status.PendingLockRelease != nil {
		t.Fatalf("a completed Apply left a release owed: %#v", after.Status.PendingLockRelease)
	}
}
