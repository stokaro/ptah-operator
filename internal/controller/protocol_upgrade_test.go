package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestTheProtocolFencesAnOperationThePreviousOneDispatched upgrades a
// manager across one runner protocol version, with a read-only operation the
// previous protocol dispatched still standing. The binding rotates and the
// operation is fenced before its log is read, because its frame is written in
// a protocol this manager does not read.
func TestTheProtocolFencesAnOperationThePreviousOneDispatched(t *testing.T) {
	t.Parallel()

	priorProtocolVersion := int32(runner.ProtocolVersion) - 1

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.ExecutionBinding.RunnerProtocolVersion = priorProtocolVersion
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:      operatorv1alpha1.OperationResolve,
		ID:        "previous-protocol-resolve",
		JobName:   "previous-protocol-resolve-job",
		JobUID:    "job-uid",
		StartedAt: metav1.Now(),
		Attempt:   1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
	oldEpoch := schema.Status.ExecutionBinding.Epoch
	oldOperation := schema.Status.ActiveOperation.DeepCopy()

	logs := &safetyCountingLogs{content: []byte("a previous-protocol result must not be read")}
	reconciler, api := fakeReconciler(t, logs, schema, job, pod)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() protocol upgrade fence error = %v", err)
	}

	fenced := safetyGetSchema(t, api, schema)
	if fenced.Status.ExecutionBinding == nil ||
		fenced.Status.ExecutionBinding.RunnerProtocolVersion != int32(runner.ProtocolVersion) ||
		fenced.Status.ExecutionBinding.Epoch == oldEpoch {
		t.Fatalf("protocol upgrade did not rotate the execution binding: %#v", fenced.Status.ExecutionBinding)
	}
	if fenced.Status.ActiveOperation == nil ||
		fenced.Status.ActiveOperation.ID != oldOperation.ID ||
		fenced.Status.ActiveOperation.JobName != oldOperation.JobName ||
		fenced.Status.ActiveOperation.JobUID != oldOperation.JobUID ||
		fenced.Status.ActiveOperation.ExecutionBindingID != oldEpoch {
		t.Fatalf("protocol upgrade lost the retired operation identity: %#v", fenced.Status.ActiveOperation)
	}
	if logs.reads != 0 {
		t.Fatalf("protocol upgrade read retired runner logs %d times", logs.reads)
	}
	storedJob := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), storedJob); err != nil {
		t.Fatalf("Get(retired Job) error = %v", err)
	}
	if storedJob.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("protocol upgrade cleaned the retired Job before its durable fence: %#v", storedJob.Spec.TTLSecondsAfterFinished)
	}
}
