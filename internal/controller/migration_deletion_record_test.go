package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Deleting a migration that carries an unresolved run destroys the record, and
// the record is the only thing that says a database may hold a change nobody
// accounted for.
//
// The operator does not refuse the deletion. Only a person clears that record,
// so a refusal would be one the operator can never lift, and a resource nobody
// can remove is worse than a record that ends in the event stream. What it
// must not do is lose it quietly: whoever later finds an unaccounted-for change
// in that database has the Events and the log and nothing else.
func TestDeletingAMigrationNamesTheUnresolvedRunItDiscards(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	if err := api.Delete(ctx, migration); err != nil {
		t.Fatal(err)
	}

	// The settling pass records the run nobody accounted for.
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	settled := readMigration(t, api, migration)
	if settled.Status.UnresolvedRun == nil {
		t.Fatalf("the deletion settled no unresolved run, so this proof measured nothing: %#v", settled.Status)
	}

	// The pass that removes the finalizer is the one that loses it.
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() after the outcome was recorded error = %v", err)
	}

	warning := findEvent(recorder, corev1.EventTypeWarning+" UnresolvedRunDiscarded")
	if warning == "" {
		t.Fatal("the resource was deleted and the unresolved run was never named")
	}
	for _, want := range []string{
		string(settled.Status.UnresolvedRun.Outcome),
		settled.Status.UnresolvedRun.JobName,
		settled.Status.UnresolvedRun.PlanRef.Name,
	} {
		if want == "" {
			t.Fatal("the record names nothing the event could carry")
		}
		if !strings.Contains(warning, want) {
			t.Fatalf("the warning %q does not name %q", warning, want)
		}
	}

	// And the deletion still completes. A record that ends in the event stream
	// is the cost; a resource nobody can remove is not.
	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("naming the record held the deletion: error %v, finalizers %#v", err, gone.Finalizers)
	}
}

// A migration with nothing unresolved is deleted without the warning. An
// operator who sees it on every delete stops reading it.
func TestDeletingAnAccountedForMigrationWarnsAboutNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration := migrationFixture()
	migration.Finalizers = []string{migrationOperationFinalizer}
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseInSync
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	if err := api.Delete(ctx, migration); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if warning := findEvent(recorder, corev1.EventTypeWarning+" UnresolvedRunDiscarded"); warning != "" {
		t.Fatalf("a settled migration was deleted with a warning: %q", warning)
	}
	gone := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
		t.Fatalf("a settled migration survived its deletion: error %v, finalizers %#v", err, gone.Finalizers)
	}
}

// findEvent drains what the recorder holds and returns the first event with
// the prefix, or the empty string.
func findEvent(recorder *record.FakeRecorder, prefix string) string {
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, prefix) {
				return event
			}
		default:
			return ""
		}
	}
}

// The finalizer is what holds a deleting resource while a claim is live, so
// removing it under one hands the resource to the garbage collector with an
// Apply still dispatched.
//
// Both callers clear the claim first, which makes this a contract rather than
// a reachable bug today. The schema family states the same contract where it
// can be enforced, and a third caller is exactly the kind of thing that
// arrives without anybody rereading the two that came before.
func TestTheMigrationFinalizerWillNotLeaveWhileAClaimIsLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())

	err := reconciler.removeMigrationFinalizer(ctx, migration)
	if err == nil {
		t.Fatal("the finalizer left while an Apply claim was live")
	}
	if !strings.Contains(err.Error(), string(operation.Type)) {
		t.Fatalf("refusal = %v, want it to name the %s claim it protects", err, operation.Type)
	}
	kept := readMigration(t, api, migration)
	if !contains(kept.Finalizers, migrationOperationFinalizer) {
		t.Fatal("the refusal removed the finalizer anyway")
	}

	// And it leaves once the claim is gone.
	migration.Status.ActiveOperation = nil
	if err := reconciler.removeMigrationFinalizer(ctx, migration); err != nil {
		t.Fatalf("the finalizer would not leave with nothing to protect: %v", err)
	}
}
