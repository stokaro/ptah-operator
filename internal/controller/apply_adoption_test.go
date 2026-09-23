package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// The complement of the dispatch-boundary tables: there the create failed, and
// here it succeeded while the claim never learned so. The pass that finds a
// Job standing under the name it reserved, owned by this resource and matching
// the claim it was built from, adopts it -- it records the UID and marks the
// dispatch as started -- rather than creating a second executor beside it.
//
// Neither family had a test for adopting an Apply. The schema family adopts
// read-only Jobs in passing; the migration family's adoption did not run once
// in the package.

func TestASchemaAdoptsTheApplyJobItsLostAnswerLeft(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, jobName := dispatchableApply(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	dispatched := false
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if applyDispatched(t, api, schema.Namespace, jobName) {
			dispatched = true
			break
		}
	}
	if !dispatched {
		t.Fatal("the harness never dispatched, so adopting the Job proves nothing")
	}

	// Roll the claim back to what a lost create answer leaves: the Job stands,
	// and the claim records neither its UID nor that anything was dispatched.
	stored := safetyGetSchema(t, api, schema)
	stored.Status.ActiveOperation.JobUID = ""
	stored.Status.ActiveOperation.DispatchStarted = false
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() adoption error = %v", err)
	}

	adopted := safetyGetSchema(t, api, schema).Status.ActiveOperation
	if adopted == nil {
		t.Fatal("the claim was retired rather than adopting the Job it had created")
	}
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: jobName}, job); err != nil {
		t.Fatal(err)
	}
	if adopted.JobUID != job.UID {
		t.Fatalf("adopted job UID = %q, want the standing Job's %q", adopted.JobUID, job.UID)
	}
	if !adopted.DispatchStarted {
		t.Fatal("an adopted Apply was left recorded as never dispatched")
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs, client.InNamespace(schema.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("adoption left %d Jobs, want the one that was already running", len(jobs.Items))
	}
}

func TestAMigrationAdoptsTheApplyJobItsLostAnswerLeft(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := undispatchedApplyClaim(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	// The Job the lost create left behind is still running.
	job.Status.Conditions = nil
	job.Status.Active = 1
	runningExecutorPod(pod)
	// Building the workload stamps the claim, so the lost answer is restored
	// afterwards: the Job stands and the claim knows neither its UID nor that
	// anything was dispatched.
	operation.JobUID = ""
	operation.DispatchStarted = false

	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan,
		verificationPolicyConfigMap(), job, pod)
	observed := &telemetryObservation{}
	reconciler.Telemetry = observed
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	adopted := readMigration(t, api, migration).Status.ActiveOperation
	if adopted == nil {
		t.Fatal("the claim was retired rather than adopting the Job it had created")
	}
	if adopted.JobUID != job.UID {
		t.Fatalf("adopted job UID = %q, want the standing Job's %q", adopted.JobUID, job.UID)
	}
	if !adopted.DispatchStarted {
		t.Fatal("an adopted Apply was left recorded as never dispatched")
	}
	// The run is reported as started once, by the pass that adopted it.
	wantApplies := []telemetry.ApplyOutcome{telemetry.ApplyStarted}
	if len(observed.applies) != 1 || observed.applies[0] != wantApplies[0] {
		t.Fatalf("apply observations = %#v, want %#v", observed.applies, wantApplies)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs, client.InNamespace(migration.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("adoption left %d Jobs, want the one that was already running", len(jobs.Items))
	}
	if adopted.Type != operatorv1alpha1.MigrationOperationApply {
		t.Fatalf("adopted operation = %q, want Apply", adopted.Type)
	}
}
