package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
		operatorv1alpha1.ReasonApprovedPlan, "Applying 1 planned migration")
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
// there is a wait as well: the pass stops, and says why. A manager down past the
// Lease's duration gets here with SQL still running, so a reader told only that
// the Apply waits for the lock could take it for an Apply that has not started.
func TestAMigrationApplyWithAJobThatLostTheLockSaysSo(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, _ := terminalMigrationWorkload(migration, batchv1.JobComplete)
	job.Status.Conditions = nil
	job.Status.Active = 1
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, verificationPolicyConfigMap())
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
	lease, err := targetlock.LeaseName(operation.CoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Job " + operation.JobName + " may still be running", lease, maxLockContentionPoll.String()} {
		if !strings.Contains(progressing.Message, want) {
			t.Fatalf("Progressing with a Job and the Lease lost says %q, want it to name %q", progressing.Message, want)
		}
	}
}

// What the message says is a statement about the claim, and each side of it has
// to refuse the other's words.
func TestMigrationLockWaitMessageSaysWhetherAJobMayBeRunning(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.MigrationOperationStatus
		wantJob   bool
	}{
		{name: "no dispatch yet", operation: operatorv1alpha1.MigrationOperationStatus{JobName: "apply-job"}},
		{name: "a dispatch begun", operation: operatorv1alpha1.MigrationOperationStatus{JobName: "apply-job", DispatchStarted: true}, wantJob: true},
		{name: "a Job on record", operation: operatorv1alpha1.MigrationOperationStatus{JobName: "apply-job", JobUID: "job-uid"}, wantJob: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.operation.Type = operatorv1alpha1.MigrationOperationApply
			row.operation.CoordinationDigest = testCoordinationDigest
			message := migrationLockWaitMessage(&row.operation)
			if got := strings.Contains(message, "Job apply-job may still be running"); got != row.wantJob {
				t.Fatalf("message %q names a Job that may be running: %t, want %t", message, got, row.wantJob)
			}
			if !strings.Contains(message, "the database lock") {
				t.Fatalf("message %q does not say what it waits for", message)
			}
		})
	}
}

// lockContentionRequeue's comment says what a look costs. A new read there changes
// the sum a person sizes a cluster by, so these rows name each one.
func TestALookOfAWaitingClaimCostsTheseDirectReads(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// counting records what a client given to the reconciler reads from the API
	// server: the manager's cached client serves everything else.
	counting := func(api client.WithWatch, reads *[]string) client.WithWatch {
		return interceptor.NewClient(api, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				*reads = append(*reads, fmt.Sprintf("GET %T", object))
				return c.Get(ctx, key, object, options...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				*reads = append(*reads, fmt.Sprintf("LIST %T", list))
				return c.List(ctx, list, options...)
			},
		})
	}
	hold := func(t *testing.T, reads []string, want ...string) {
		t.Helper()
		slices.Sort(reads)
		if !slices.Equal(reads, want) {
			t.Fatalf("one look read %q from the API server, want %q; update the comment on lockContentionRequeue with this row", reads, want)
		}
	}

	t.Run("a migration Apply", func(t *testing.T) {
		t.Parallel()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		operation.DispatchStarted = false
		operation.AdmissionSnapshot = nil
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
		holdRealmLeaseForASibling(t, reconciler, operation)
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("the first look: %v", err)
		}

		var reads []string
		reader := counting(api, &reads)
		reconciler.APIReader = reader
		reconciler.Locks = targetlock.New(reader, api, nil)
		result, err := reconciler.Reconcile(ctx, migrationRequest(migration))
		if err != nil {
			t.Fatalf("the next look: %v", err)
		}
		if result.RequeueAfter != maxLockContentionPoll {
			t.Fatalf("the next look requeues after %s, want %s; this row is about a claim still waiting", result.RequeueAfter, maxLockContentionPoll)
		}
		hold(t, reads, "GET *v1.ConfigMap", "GET *v1.Job", "GET *v1.Lease", "GET *v1alpha1.PtahMigration", "GET *v1alpha1.PtahMigrationPlan")
	})

	t.Run("a schema Apply whose Job may be running", func(t *testing.T) {
		t.Parallel()
		schema, plan, policy := dispatchedApplyWithPlan(t)
		job, _ := terminalWorkload(schema, batchv1.JobFailed)
		job.Status.Conditions = nil
		job.Status.Active = 1
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policy, job)
		reconciler.Locks = targetlock.New(api, api, nil)
		watching, ok := api.(client.WithWatch)
		if !ok {
			t.Fatalf("the fixture's client %T cannot be intercepted", api)
		}
		// The fixture seeds the claim's own Lease, and a sibling takes one only
		// once it is gone.
		digest := schema.Status.ActiveOperation.CoordinationDigest
		name, err := targetlock.LeaseName(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := api.Delete(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: reconciler.LockNamespace, Name: name}}); err != nil {
			t.Fatalf("remove the claim's Lease: %v", err)
		}
		held, err := reconciler.Locks.Acquire(ctx, targetlock.Request{
			CoordinationNamespace: reconciler.LockNamespace, CoordinationDigest: digest,
			Holder:   targetlock.Holder{SchemaUID: "sibling-uid", OperationID: "sibling-plan"},
			Duration: 5 * time.Minute,
		})
		if err != nil || !held.Acquired {
			t.Fatalf("the sibling could not take the Lease: %#v, %v", held, err)
		}

		var reads []string
		reader := counting(watching, &reads)
		reconciler.APIReader = reader
		reconciler.Locks = targetlock.New(reader, api, nil)
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if err != nil {
			t.Fatalf("the look: %v", err)
		}
		if result.RequeueAfter != maxLockContentionPoll {
			t.Fatalf("the look requeues after %s, want %s; this row is about a claim still waiting", result.RequeueAfter, maxLockContentionPoll)
		}
		if stored := safetyGetSchema(t, api, schema); stored.Status.ActiveOperation == nil {
			t.Fatal("the look gave up the claim; this row is about a claim still waiting")
		}
		hold(t, reads, "GET *v1.Job", "GET *v1.Lease", "GET *v1alpha1.PtahSchema", "GET *v1alpha1.PtahSchemaPlan")
	})
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
		operatorv1alpha1.ReasonApprovedPlan, "Applying 1 planned migration")
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
