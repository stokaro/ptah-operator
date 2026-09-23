package controller

import (
	"context"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// TestSuspendingAnApplyHandsBackTheDatabaseItHolds covers the last exit an
// undispatched Apply claim has.
//
// The database Lease is taken on the pass that reaches dispatch, before any
// Job exists, so a claim suspended there holds a realm nothing is using.
// Clearing it without handing the Lease back leaves every claimant on that
// database waiting out the full lease duration for a run that never started --
// and the claim that owned it is gone from the status, so nothing says who to
// wait for.
//
// suspendActiveOperation stages the release for an Apply and for a Plan that
// owes no proof. Removing the Apply arm leaves the Plan arm, which an Apply
// never satisfies, and nothing in the package noticed.
func TestSuspendingAnApplyHandsBackTheDatabaseItHolds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	schema, plan, policyConfig := safetyReadyToApplyFixture(t)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policyConfig)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.claim(ctx, safetyGetSchema(t, api, schema), operatorv1alpha1.OperationApply); err != nil {
		t.Fatalf("claim(Apply) error = %v", err)
	}
	suspending := safetyGetSchema(t, api, schema)
	suspending.Spec.Suspend = true
	if err := api.Update(ctx, suspending); err != nil {
		t.Fatalf("suspend the schema: %v", err)
	}

	// The first pass persists the lease epoch and acquires nothing; the claim
	// takes the Lease once its own epoch is durable.
	acquired := false
	for range 3 {
		var err error
		acquired, _, err = reconciler.acquireApplyLock(ctx, safetyGetSchema(t, api, schema))
		if err != nil {
			t.Fatalf("acquireApplyLock() error = %v", err)
		}
		if acquired {
			break
		}
	}
	if !acquired {
		t.Fatal("the Apply claim did not acquire the database, so handing it back proves nothing")
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder == "" {
		t.Fatal("no holder stands on the database Lease, so handing it back proves nothing")
	}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	suspended := safetyGetSchema(t, api, schema)
	if suspended.Status.Phase != operatorv1alpha1.PhaseSuspended {
		t.Fatalf("phase = %q, want %q", suspended.Status.Phase, operatorv1alpha1.PhaseSuspended)
	}
	if holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); holder != "" {
		t.Fatalf("a suspended Apply kept the database: holder %q, status %#v", holder, suspended.Status)
	}
}
