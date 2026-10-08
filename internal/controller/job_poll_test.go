package controller

import (
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestRunningSchemaReadUsesWatchBeforeFallback(t *testing.T) {
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = api.PhaseResolving
	schema.Status.ActiveOperation = &api.ActiveOperationStatus{Type: api.OperationResolve, ID: "resolve-1", JobName: "resolve-1", Attempt: 1}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	schema.Status.ActiveOperation.JobUID = job.UID
	job.Status = batchv1.JobStatus{Active: 1}
	result := runner.Result{Operation: runner.OperationResolve, OperationID: "resolve-1", ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321}
	r, c := fakeReconciler(t, staticLogs{content: safetyRunnerFrame(t, result)}, schema, job, pod)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	wait, err := r.Reconcile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if wait.RequeueAfter != 30*time.Second {
		t.Errorf("running read polls after %s, want a bounded 30s watch fallback", wait.RequeueAfter)
	}
	// The clock does not advance to the fallback. A terminal watch event must
	// still trigger and consume the result immediately.
	before := job.DeepCopy()
	job.Status = batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}
	if !operationJobEvents().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: job}) {
		t.Fatal("completion event was suppressed")
	}
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	actual := safetyGetSchema(t, c, schema)
	if actual.Status.ActiveOperation != nil || actual.Status.Source.Digest != testDigest || actual.Status.Phase != api.PhaseVerifying {
		t.Fatalf("completion waited for the fallback: phase=%s active=%v digest=%s", actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Source.Digest)
	}
}

func TestRunningMigrationReadUsesWatchBeforeFallback(t *testing.T) {
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = api.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	op := migrationClaim(t, migration, api.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	job.Status = batchv1.JobStatus{Active: 1}
	result := runner.Result{Operation: runner.OperationResolve, OperationID: op.ID, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/migrations@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321}
	r, c := fakeMigrationReconciler(t, staticLogs{content: safetyRunnerFrame(t, result)}, migration, job, pod)
	wait, err := r.Reconcile(t.Context(), migrationRequest(migration))
	if err != nil {
		t.Fatal(err)
	}
	if wait.RequeueAfter != 30*time.Second {
		t.Errorf("running read polls after %s, want a bounded 30s watch fallback", wait.RequeueAfter)
	}
	before := job.DeepCopy()
	job.Status = batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}
	if !operationJobEvents().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: job}) {
		t.Fatal("completion event was suppressed")
	}
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), migrationRequest(migration)); err != nil {
		t.Fatal(err)
	}
	actual := readMigration(t, c, migration)
	if actual.Status.ActiveOperation != nil || actual.Status.Artifact == nil || actual.Status.Artifact.Digest != testDigest || actual.Status.Phase != api.MigrationPhaseVerifying {
		t.Fatalf("completion waited for the fallback: phase=%s active=%v artifact=%v", actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Artifact)
	}
}

func TestSchemaApplyKeepsFrequentLeaseRenewalPoll(t *testing.T) {
	r, c, schema, name := dispatchableApply(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for range 4 {
		if _, err := r.Reconcile(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if applyDispatched(t, c, schema.Namespace, name) {
			break
		}
	}
	job := &batchv1.Job{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: schema.Namespace, Name: name}, job); err != nil {
		t.Fatal(err)
	}
	job.Status = batchv1.JobStatus{Active: 1, StartTime: &metav1.Time{Time: r.now()}}
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	wait, err := r.Reconcile(t.Context(), request)
	if err != nil || wait.RequeueAfter != 5*time.Second {
		t.Fatalf("Apply lost its lease renewal poll: %v, %v", wait, err)
	}
}
