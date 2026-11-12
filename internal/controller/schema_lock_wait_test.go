package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A schema claim that lost the realm Lease asked for it every five seconds and
// said nothing while it did: Ready read OperationInProgress, as it does over a
// Job at work, and no Job, Event or condition told a reader it waited for a
// sibling (#873). A PtahMigration's Apply says so on Progressing (#822); a
// schema says so on Ready, where it reports a Pod that cannot run.

// takeRealmForASibling hands the realm Lease of digest to a sibling's
// operation, after removing a Lease a fixture seeded for the claim itself. It
// returns what releases it.
func takeRealmForASibling(t *testing.T, reconciler *SchemaReconciler, api client.Client, digest string) func() {
	t.Helper()
	ctx := context.Background()
	name, err := targetlock.LeaseName(digest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: reconciler.LockNamespace, Name: name}}
	if err := api.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("remove the claim's own Lease: %v", err)
	}
	request := targetlock.Request{
		CoordinationNamespace: reconciler.LockNamespace,
		CoordinationDigest:    digest,
		Holder:                targetlock.Holder{SchemaUID: "sibling-uid", OperationID: "sibling-plan"},
		Duration:              5 * time.Minute,
	}
	result, err := reconciler.Locks.Acquire(ctx, request)
	if err != nil || !result.Acquired {
		t.Fatalf("the sibling could not take the Lease: %#v, %v", result, err)
	}
	request.ExpectedEpoch = result.Epoch
	return func() {
		t.Helper()
		if err := reconciler.Locks.Release(ctx, request); err != nil {
			t.Fatalf("the sibling could not release the Lease: %v", err)
		}
	}
}

// heldUnchanged holds a waiting pass to what the wait must leave alone: the
// claim as it stood, the finalizer, and the sibling on the Lease.
func heldUnchanged(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	stored *operatorv1alpha1.PtahSchema,
	claim *operatorv1alpha1.ActiveOperationStatus,
	holder string,
) {
	t.Helper()
	if !reflect.DeepEqual(stored.Status.ActiveOperation, claim) {
		t.Fatalf("the wait moved the claim:\n got %#v\nwant %#v", stored.Status.ActiveOperation, claim)
	}
	if !controllerutil.ContainsFinalizer(stored, activeOperationFinalizer) {
		t.Fatal("the wait dropped the active-operation finalizer")
	}
	if got := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest); got != holder {
		t.Fatalf("the Lease is held by %q during the wait, want the sibling %q", got, holder)
	}
}

func lockWaitLease(t *testing.T, digest string) string {
	t.Helper()
	lease, err := targetlock.LeaseName(digest)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestASchemaApplyThatLostTheLockSaysSoUntilItTakesIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, jobName := dispatchableApply(t, false)
	claim := safetyGetSchema(t, api, schema).Status.ActiveOperation
	release := takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease held = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	waiting := safetyGetSchema(t, api, schema)
	ready := findCondition(waiting.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		t.Fatalf("Ready with the Lease held = %#v, want False/WaitingForDatabaseLock", ready)
	}
	for _, want := range []string{"Apply is waiting for the database lock:", lockWaitLease(t, claim.CoordinationDigest), maxLockContentionPoll.String()} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("Ready message %q does not say %q", ready.Message, want)
		}
	}
	if strings.Contains(ready.Message, "may still be running") {
		t.Errorf("Ready message %q names a Job for a claim that dispatched nothing", ready.Message)
	}
	heldUnchanged(t, reconciler, api, waiting, claim, holder)
	if applyDispatched(t, api, schema.Namespace, jobName) {
		t.Fatal("an Apply Job exists with the Lease held by another operation")
	}

	// The next look with nothing changed writes nothing.
	again, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if again.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue on the next look = %s, want %s", again.RequeueAfter, maxLockContentionPoll)
	}
	stored := safetyGetSchema(t, api, schema)
	if stored.ResourceVersion != waiting.ResourceVersion {
		t.Fatalf("a look that found the same Lease wrote status: %s to %s", waiting.ResourceVersion, stored.ResourceVersion)
	}
	heldUnchanged(t, reconciler, api, stored, claim, holder)

	// The Lease comes free, and Ready stops saying the claim waits on the pass
	// that takes it.
	release()
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	taken := safetyGetSchema(t, api, schema)
	ready = findCondition(taken.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) || ready.Message != "Apply operation is in progress" {
		t.Fatalf("Ready with the Lease taken = %#v, want the words of a claim in flight", ready)
	}
	if taken.Status.ActiveOperation == nil || taken.Status.ActiveOperation.ID != claim.ID || taken.Status.ActiveOperation.LeaseEpoch == "" {
		t.Fatalf("the pass that took the Lease did not record its epoch on the same claim: %#v", taken.Status.ActiveOperation)
	}
	for range 6 {
		if applyDispatched(t, api, schema.Namespace, jobName) {
			return
		}
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	t.Fatalf("the Apply did not dispatch once it held the Lease: %#v", safetyGetSchema(t, api, schema).Status)
}

// runningApply is an Apply whose Job is running and whose claim holds no Lease
// now: the manager was down for longer than the Lease lasts.
func runningApply(t *testing.T) (*SchemaReconciler, client.Client, *operatorv1alpha1.PtahSchema) {
	t.Helper()
	schema, plan, policy := dispatchedApplyWithPlan(t)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationInProgress, "Apply operation is in progress")
	job, _ := terminalWorkload(schema, batchv1.JobFailed)
	job.Status.Conditions = nil
	job.Status.Active = 1
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, policy, job)
	reconciler.Locks = targetlock.New(api, api, nil)
	return reconciler, api, schema
}

// A claim whose Job runs renews the Lease before it reads the Job. A reader told
// only that the Apply waits for the lock could take it for an Apply that has not
// started, and act on the database while its SQL runs.
func TestASchemaApplyWithAJobThatLostTheLockSaysTheJobMayRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema := runningApply(t)
	claim := safetyGetSchema(t, api, schema).Status.ActiveOperation
	takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease lost = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	stored := safetyGetSchema(t, api, schema)
	ready := findCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) {
		t.Fatalf("Ready with a Job and the Lease lost = %#v, want False/WaitingForDatabaseLock", ready)
	}
	for _, want := range []string{"Job " + claim.JobName + " may still be running", lockWaitLease(t, claim.CoordinationDigest)} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("Ready message %q does not say %q", ready.Message, want)
		}
	}
	heldUnchanged(t, reconciler, api, stored, claim, holder)
}

// A report about the claim's Pod is what an Apply's ending carries from Ready,
// and it says more than the wait does about a Pod that cannot start. The wait
// leaves it standing.
func TestALockWaitLeavesAPodReportStanding(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema := runningApply(t)
	stored := safetyGetSchema(t, api, schema)
	report := "Job " + stored.Status.ActiveOperation.JobName + ` cannot start its Pod: secret "application-database" not found`
	setCondition(stored, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, report)
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	claim := safetyGetSchema(t, api, schema).Status.ActiveOperation
	takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest)
	written := safetyGetSchema(t, api, schema).ResourceVersion

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease lost = %s, want %s; this row is about a claim that waits", result.RequeueAfter, maxLockContentionPoll)
	}
	after := safetyGetSchema(t, api, schema)
	ready := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) || ready.Message != report {
		t.Fatalf("Ready after a wait over a Pod report = %#v, want the report", ready)
	}
	if after.ResourceVersion != written {
		t.Fatalf("a wait over a Pod report wrote status: %s to %s", written, after.ResourceVersion)
	}
	heldUnchanged(t, reconciler, api, after, claim, holder)
}

// The Observe that proves an Apply holds the Apply's Lease, and asks for it
// before anything else it does.
func TestAProofThatLostTheLockSaysSo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	run, reconciler, api, _ := undispatchedProofExpecting(t, true)
	stored := safetyGetSchema(t, api, run.schema)
	setCondition(stored, operatorv1alpha1.ConditionReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationInProgress, "Observe operation is in progress")
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	claim := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
	takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease held = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	after := safetyGetSchema(t, api, run.schema)
	ready := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != string(operatorv1alpha1.ReasonWaitingForDatabaseLock) ||
		!strings.HasPrefix(ready.Message, "Observe is waiting for the database lock:") {
		t.Fatalf("Ready with the Lease held = %#v, want the proof's wait", ready)
	}
	heldUnchanged(t, reconciler, api, after, claim, holder)
}

// A deleted schema renews the Lease only to keep it while a Pod may still write,
// and deletion has its own Warnings for that wait. Its Ready is not rewritten.
func TestADeletionThatLostTheLockLeavesReadyAlone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema := runningApply(t)
	if err := api.Delete(ctx, safetyGetSchema(t, api, schema)); err != nil {
		t.Fatal(err)
	}
	deleting := safetyGetSchema(t, api, schema)
	if deleting.DeletionTimestamp == nil {
		t.Fatal("the schema is not being deleted, so this row measures a live one")
	}
	claim := deleting.Status.ActiveOperation
	takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, claim.CoordinationDigest)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the Lease lost = %s, want %s; this row is about a deletion that waits", result.RequeueAfter, maxLockContentionPoll)
	}
	after := safetyGetSchema(t, api, schema)
	if after.ResourceVersion != deleting.ResourceVersion {
		t.Fatalf("a deletion that lost the Lease wrote status: %#v", findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReady))
	}
	heldUnchanged(t, reconciler, api, after, claim, holder)
}

// The report is not what the claim waits on. A write the API server refuses
// leaves the pass asking again at the same pace, where an error would back off.
func TestAFailedLockWaitReportDoesNotSlowTheClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, _ := dispatchableApply(t, false)
	claim := safetyGetSchema(t, api, schema).Status.ActiveOperation
	takeRealmForASibling(t, reconciler, api, claim.CoordinationDigest)
	refused := errors.New("status writes are refused")
	watching, ok := api.(client.WithWatch)
	if !ok {
		t.Fatalf("the fixture's client %T cannot be intercepted", api)
	}
	reconciler.Client = interceptor.NewClient(watching, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return refused
		},
	})
	written := safetyGetSchema(t, api, schema).ResourceVersion

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want the refused report logged and the wait kept", err)
	}
	if result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("requeue with the report refused = %s, want %s", result.RequeueAfter, maxLockContentionPoll)
	}
	if stored := safetyGetSchema(t, api, schema); stored.ResourceVersion != written {
		t.Fatal("the interceptor let a status write through, so this row proves nothing about a refused one")
	}
}

// What the message says is a statement about the claim, and each side of it has
// to refuse the other's words.
func TestSchemaLockWaitMessageSaysWhetherAJobMayBeRunning(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.ActiveOperationStatus
		wantJob   bool
	}{
		{name: "no dispatch yet", operation: operatorv1alpha1.ActiveOperationStatus{JobName: "apply-job"}},
		{name: "a dispatch begun", operation: operatorv1alpha1.ActiveOperationStatus{JobName: "apply-job", DispatchStarted: true}, wantJob: true},
		{name: "a Job on record", operation: operatorv1alpha1.ActiveOperationStatus{JobName: "apply-job", JobUID: "job-uid"}, wantJob: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.operation.Type = operatorv1alpha1.OperationApply
			row.operation.CoordinationDigest = testCoordinationDigest
			message := schemaLockWaitMessage(&row.operation)
			if got := strings.Contains(message, "Job apply-job may still be running"); got != row.wantJob {
				t.Fatalf("message %q names a Job that may be running: %t, want %t", message, got, row.wantJob)
			}
			if !strings.HasPrefix(message, "Apply is waiting for the database lock") ||
				!strings.Contains(message, lockWaitLease(t, testCoordinationDigest)) {
				t.Fatalf("message %q does not say what it waits for", message)
			}
			// A message that read as a report about the claim's Pod would be
			// carried into the Apply's ending as one.
			report := schemaPodReport(&operatorv1alpha1.PtahSchema{Status: operatorv1alpha1.PtahSchemaStatus{
				ActiveOperation: &row.operation,
				Conditions: []metav1.Condition{{
					Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
					Reason: string(operatorv1alpha1.ReasonWaitingForDatabaseLock), Message: message,
				}},
			}}, nil)
			if report.standing() {
				t.Fatalf("message %q reads as a standing Pod report", message)
			}
		})
	}
}
