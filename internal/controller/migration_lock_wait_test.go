package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A migration Apply that lost the Lease slept for all the time the Lease had
// left, and a sibling's Plan that took seconds left about five minutes on it:
// 292 seconds with no Job, no Event and no condition, with the Lease free for
// nearly all of them (#822). A schema's Apply in the same place asks every five
// seconds, and nothing about a migration asks for longer.

// holdRealmLeaseForASibling is the Lease a sibling's operation took on the realm
// migration's claim addresses. It returns what releases it.
func holdRealmLeaseForASibling(
	t *testing.T,
	reconciler *MigrationReconciler,
	operation *operatorv1alpha1.MigrationOperationStatus,
) func() {
	t.Helper()
	request := targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    operation.CoordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: "sibling-uid", OperationID: "sibling-plan"},
		Duration:              5 * time.Minute,
	}
	result, err := reconciler.Locks.Acquire(context.Background(), request)
	if err != nil || !result.Acquired {
		t.Fatalf("the sibling could not take the Lease: acquired=%t err=%v", result.Acquired, err)
	}
	request.ExpectedEpoch = result.Epoch
	return func() {
		t.Helper()
		if err := reconciler.Locks.Release(context.Background(), request); err != nil {
			t.Fatalf("the sibling could not release the Lease: %v", err)
		}
	}
}

func TestAMigrationApplyThatLostTheLockAsksAgainAndSaysSo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = false
	operation.AdmissionSnapshot = nil
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApprovedPlan, "Applying 1 planned migrations")
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	release := holdRealmLeaseForASibling(t, reconciler, operation)

	result, err := reconciler.Reconcile(ctx, migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease held = %s, want %s rather than what the Lease has left", result.RequeueAfter, maxLockContentionPoll)
	}
	waiting := readMigration(t, api, migration)
	progressing := meta.FindStatusCondition(waiting.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		t.Fatalf("Progressing with the Lease held = %#v, want True/WaitingForDatabaseLock", progressing)
	}
	lease, err := targetlock.LeaseName(operation.CoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Apply is waiting for the database lock", lease, maxLockContentionPoll.String()} {
		if !strings.Contains(progressing.Message, want) {
			t.Errorf("Progressing message %q does not say %q", progressing.Message, want)
		}
	}
	if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != operation.ID {
		t.Fatalf("waiting retired the claim: %#v", waiting.Status.ActiveOperation)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a Job exists with the Lease held by another operation: %v", jobs.Items[0].Name)
	}

	// The next look with nothing changed writes nothing.
	again, err := reconciler.Reconcile(ctx, migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if again.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue on the next look = %s, want %s", again.RequeueAfter, maxLockContentionPoll)
	}
	if stored := readMigration(t, api, migration); stored.ResourceVersion != waiting.ResourceVersion {
		t.Fatalf("a look that found the same Lease wrote status: %s to %s", waiting.ResourceVersion, stored.ResourceVersion)
	}

	// The Lease comes free, and the condition stops saying it is waiting on the
	// pass that takes it.
	release()
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	taken := readMigration(t, api, migration)
	progressing = meta.FindStatusCondition(taken.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Progressing with the Lease taken = %#v, want True/OperationInProgress", progressing)
	}
}

// A claim whose Job exists renews the Lease before it reads the Job. Losing it
// there is a wait as well: the pass stops, and says why.
func TestAMigrationApplyWithAJobThatLostTheLockSaysSo(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdRealmLeaseForASibling(t, reconciler, operation)

	result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease lost = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	progressing := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		t.Fatalf("Progressing with the Lease lost = %#v, want WaitingForDatabaseLock", progressing)
	}
}

// The other way a pass can end without the Lease in hand is the write that
// records the epoch it just took. That says nothing about anybody waiting.
func TestRecordingALeaseEpochIsNotWaitingForTheLock(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = false
	operation.LeaseEpoch = ""
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonApprovedPlan, "Applying 1 planned migrations")
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	stored := readMigration(t, api, migration)
	acquired, requeue, err := reconciler.waitForMigrationApplyLock(context.Background(), stored)
	if err != nil {
		t.Fatalf("waitForMigrationApplyLock() error = %v", err)
	}
	if acquired || requeue != statusPatchRequeue {
		t.Fatalf("waitForMigrationApplyLock() = %t, %s, want the pass to end at the epoch write", acquired, requeue)
	}
	progressing := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonApprovedPlan) {
		t.Fatalf("Progressing = %#v, want the claim's own reason", progressing)
	}
}

// A deleted or unaccounted claim renews its Lease only to keep a Pod that may
// still write inside the realm, and what it reports is about that Pod. Losing
// the Lease there changes neither what it does nor what it says.
func TestARenewalThatLostTheLockLeavesTheConditionAlone(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOutcomeUnknown, "the Pod may still be running")
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdRealmLeaseForASibling(t, reconciler, operation)

	stored := readMigration(t, api, migration)
	written := stored.ResourceVersion
	acquired, requeue, err := reconciler.acquireMigrationApplyLock(context.Background(), stored)
	if err != nil {
		t.Fatalf("acquireMigrationApplyLock() error = %v", err)
	}
	if acquired || requeue != maxLockContentionPoll {
		t.Fatalf("acquireMigrationApplyLock() = %t, %s, want contention at %s", acquired, requeue, maxLockContentionPoll)
	}
	if after := readMigration(t, api, migration); after.ResourceVersion != written {
		t.Fatalf("a renewal that lost the Lease wrote status: %#v", after.Status.Conditions)
	}
}

func TestLockContentionRequeueIsBoundEitherWay(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		contention *targetlock.Contention
		want       time.Duration
	}{
		{name: "no answer asks again at the cap", want: maxLockContentionPoll},
		{name: "a Lease with minutes left", contention: &targetlock.Contention{RequeueAfter: 292 * time.Second}, want: maxLockContentionPoll},
		{name: "a Lease with seconds left", contention: &targetlock.Contention{RequeueAfter: 3 * time.Second}, want: 3 * time.Second},
		{name: "a Lease about to lapse", contention: &targetlock.Contention{RequeueAfter: 10 * time.Millisecond}, want: time.Second},
		{name: "a Lease with nothing left", contention: &targetlock.Contention{}, want: time.Second},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := lockContentionRequeue(row.contention); got != row.want {
				t.Fatalf("lockContentionRequeue() = %s, want %s", got, row.want)
			}
		})
	}
}
