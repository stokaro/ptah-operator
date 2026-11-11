package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A deletion that stops waiting for a read at its stop bound has stopped
// waiting, and the read may not be over: the node can come back, and the Pod
// on it with it, finish its read and deliver what it read (#997). These rows
// hold that the Pod reaches nothing. Every path a result takes into the cluster
// (the receiver, the credential issuer, both record admissions) asks
// resultauthority.Authorizer first, and it answers only for the live claim that
// names the Pod's Job; and the controller reads a result only for the Job its
// live claim names. The Pod mounts no ServiceAccount token, so it cannot write
// to the API server itself.

// countingResults is the controller's durable result reader with every request
// it is asked recorded.
type countingResults struct {
	reader   OperationResults
	requests []resultconsumer.Request
}

func (c *countingResults) Poll(ctx context.Context, request resultconsumer.Request) (resultconsumer.Result, error) {
	c.requests = append(c.requests, request)
	return c.reader.Poll(ctx, request)
}

// readIdentity is the identity the receiver authenticates for the run's Pod: the
// one its client certificate, or its projected token, names.
func readIdentity(t *testing.T, api client.Client, run dispatchedSchemaRead) resultdelivery.Identity {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	operation := run.schema.Status.ActiveOperation
	return resultdelivery.Identity{
		Engine: strings.ToLower(string(operation.Target.Engine)),
		Binding: resultstore.Binding{
			Namespace: run.schema.Namespace, Kind: "PtahSchema", Name: run.schema.Name, UID: run.schema.UID,
			Generation:         run.schema.Generation,
			ExecutionBindingID: job.Annotations[workload.AnnotationExecutionBindingID],
			InputFingerprint:   job.Annotations[workload.AnnotationInputFingerprint],
			Operation:          string(mutationlifecycle.SchemaOperation(operation.Type).Runner),
			OperationID:        job.Annotations[workload.AnnotationOperationID],
			JobName:            job.Name, JobUID: job.UID, PodName: pod.Name, PodUID: pod.UID,
		},
	}
}

// admittedReadPod gives the run's Pod what the Job controller and admission give
// every Pod the fixtures' admission snapshot covers: a name generated from its
// Job's, the priority admission resolves, and the default not-ready and
// unreachable tolerations.
func admittedReadPod(pod *corev1.Pod) {
	pod.GenerateName = pod.OwnerReferences[0].Name + "-"
	pod.Name = generatedTerminalPodName(pod.OwnerReferences[0].Name, "abc12")
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	pod.Spec.Priority = &priority
	pod.Spec.PreemptionPolicy = &preemption
	pod.Spec.Tolerations = append(pod.Spec.Tolerations,
		corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
	)
}

// plantReadResult stores the result record identity's Pod would deliver, as the
// test administrator: past the receiver and its authority.
func plantReadResult(t *testing.T, api client.Client, identity resultdelivery.Identity) {
	t.Helper()
	result := runner.Result{ProtocolVersion: runner.ProtocolVersion, Operation: runner.Operation(identity.Binding.Operation),
		OperationID: identity.Binding.OperationID, ChildExitCode: -1,
		Error: &runner.ResultError{Code: "refused", Message: "a result nothing may use"}}
	payload, err := resultdelivery.Encode(identity, result)
	if err != nil {
		t.Fatal(err)
	}
	store := resultstore.Store{Client: &durableTestAPI{Client: api}, Reader: api}
	if _, err := store.Publish(context.Background(), identity.Binding, payload, fingerprint.DigestBytes(payload)); err != nil {
		t.Fatalf("plant the Pod's result record: %v", err)
	}
}

// rebindProofJob gives the run's stored Job and Pod the input fingerprint its
// proof claim holds. A proof's inputs include the epoch of the Lease it reads
// under, and the fixtures take that Lease after the Job was built, so the claim
// is rebound to an epoch the Job never saw. A real proof takes the Lease first.
// The claim's admission snapshot follows the template.
func rebindProofJob(t *testing.T, api client.Client, run dispatchedSchemaRead) {
	t.Helper()
	ctx := context.Background()
	stored := safetyGetSchema(t, api, run.schema)
	operation := stored.Status.ActiveOperation
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
		t.Fatal(err)
	}
	if job.Annotations[workload.AnnotationInputFingerprint] == operation.InputFingerprint {
		t.Fatal("the proof's Job already carries its claim's fingerprint, so this rebinding is not needed")
	}
	job.Annotations[workload.AnnotationInputFingerprint] = operation.InputFingerprint
	job.Spec.Template.Annotations[workload.AnnotationInputFingerprint] = operation.InputFingerprint
	operation.AdmissionSnapshot = testAdmissionSnapshotOf(&job.Spec.Template)
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		annotations[workload.AnnotationAdmissionSnapshotDigest] = operation.AdmissionSnapshot.Digest
	}
	pod.Annotations = maps.Clone(job.Spec.Template.Annotations)
	for _, object := range []client.Object{job, pod} {
		if err := api.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	run.schema.Status.ActiveOperation = operation.DeepCopy()
}

// TestARevivedReadPodPastItsDiscardDeliversNothingAnythingUses is a Plan, and a
// read that proves an Apply, whose Pod comes back after deletion stopped
// waiting for it at its Job's stop bound. Something else holds the resource
// past its deletion, so the row can read what the discard left. The Pod's
// result is refused by the authority every delivery path asks, though the same
// Pod was authorized while its claim stood. A record of that result planted by
// an administrator past the authority, and the Pod and the Job then finishing,
// leave the resource as the discard wrote it: no status write, no Lease write,
// no Event, and no read of the record or of the Pod's logs. The same record and
// the same finish are what the controller's result read takes for a claim that
// still stands, so the record is one it would use.
func TestARevivedReadPodPastItsDiscardDeliversNothingAnythingUses(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	current.ResultEndpoint = "https://receiver.operator.svc:9444"
	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.OperationType
		proof     bool
		// standing leaves the resource undeleted, so the claim still stands
		// when the Pod finishes.
		standing bool
	}{
		{name: "a Plan", operation: operatorv1alpha1.OperationPlan},
		{name: "the Observe that proves an Apply", operation: operatorv1alpha1.OperationObserve, proof: true},
		{name: "a Plan whose claim still stands", operation: operatorv1alpha1.OperationPlan, standing: true},
		{name: "a proof whose claim still stands", operation: operatorv1alpha1.OperationObserve, proof: true, standing: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, row.operation, runShape{dispatcher: current, recorded: true, proof: row.proof})
			readOnALostNode(run)
			admittedReadPod(run.pod)
			run.schema.Finalizers = append(run.schema.Finalizers, "example.invalid/hold")
			reconciler, api, recorder := run.reconciler(t, current)
			if row.proof {
				rebindProofJob(t, api, run)
			}
			results := &countingResults{reader: controllerConsumer(t, api)}
			reconciler.Results = results
			reconciler.Logs = forbiddenResultLogs{t}
			writes := &leaseWrites{}
			clock := &safetyClock{now: jobStopBound(run.job)}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, writes.writer(api), clock)
			if !durableDeliveryRequested(run.job) {
				t.Fatal("the read's Job delivers no durable result, so nothing below measures one")
			}
			identity := readIdentity(t, api, run)
			authority := resultauthority.Authorizer{Reader: api}
			if err := authority.Check(ctx, identity); err != nil {
				t.Fatalf("the read's Pod could not deliver while its claim stood, so the refusal below proves nothing: %v", err)
			}
			if row.standing {
				// The controller's own read of a finished Job's result, for the
				// claim that names it.
				plantReadResult(t, api, identity)
				finishSchemaRead(t, api, run)
				job := &batchv1.Job{}
				if err := api.Get(ctx, client.ObjectKeyFromObject(run.job), job); err != nil {
					t.Fatal(err)
				}
				evidence, err := pollEvidence(t, func() (terminalEvidence, error) {
					return reconciler.terminalLogs(ctx, safetyGetSchema(t, api, run.schema), job)
				})
				if err != nil || evidence.Result == nil || evidence.Result.OperationID != identity.Binding.OperationID ||
					!slices.ContainsFunc(results.requests, func(request resultconsumer.Request) bool {
						return request.JobName == identity.Binding.JobName && request.JobUID == identity.Binding.JobUID
					}) {
					t.Fatalf("a claim that still stands did not read its finished Job's record, so the rows that read nothing prove nothing: "+
						"error %v, result %#v, requests %#v", err, evidence.Result, results.requests)
				}
				return
			}

			if err := api.Delete(ctx, run.schema.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() at the bound: %v", err)
			}
			discarded := safetyGetSchema(t, api, run.schema)
			if discarded.Status.ActiveOperation != nil || slices.Contains(discarded.Finalizers, activeOperationFinalizer) {
				t.Fatalf("the deletion did not discard the claim at the Job's stop bound: claim %#v, finalizers %q",
					discarded.Status.ActiveOperation, discarded.Finalizers)
			}
			if events := readJobOverdueEvents(recorder); len(events) != 1 {
				t.Fatalf("the discard recorded %q, want one ReadJobOverdue Warning", events)
			}
			writes.take()
			if len(results.requests) != 0 {
				t.Fatalf("the deletion read a result for the Job it stopped waiting for: %#v", results.requests)
			}

			// The Pod comes back and delivers.
			if err := authority.Check(ctx, identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("the receiver would take the revived Pod's result: Check() = %v, want %v", err, resultdelivery.ErrAuthority)
			}
			plantReadResult(t, api, identity)
			finishSchemaRead(t, api, run)

			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() after the Pod finished: %v", err)
			}
			after := safetyGetSchema(t, api, run.schema)
			if after.ResourceVersion != discarded.ResourceVersion {
				t.Fatalf("the finished Pod's result reached the deleted resource: resourceVersion %s, then %s; status %#v",
					discarded.ResourceVersion, after.ResourceVersion, after.Status)
			}
			if held := writes.take(); len(held) != 0 {
				t.Fatalf("the finished Pod's Job took the Lease again: writes %q", held)
			}
			if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) != "" {
				t.Fatalf("the Lease is held by %q after the Pod finished", leaseHolder(lease))
			}
			if events := eventsWithPrefix(recorder, ""); len(events) != 0 {
				t.Fatalf("the finished Pod's Job was reported: %q", events)
			}
			if len(results.requests) != 0 {
				t.Fatalf("the controller read the revived Pod's record: %#v", results.requests)
			}
			if err := authority.Check(ctx, identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("the finished Pod's identity regained authority: Check() = %v", err)
			}
		})
	}
}
