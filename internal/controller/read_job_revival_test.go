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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	// A read that opens no database session, such as a Resolve, names no
	// engine, as its claim binds no target.
	engine := ""
	if operation.Target != nil {
		engine = strings.ToLower(string(operation.Target.Engine))
	}
	return resultdelivery.Identity{
		Engine: engine,
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

// startedReadPod stores the Pod the Job controller makes from job's template,
// admitted the way admittedReadPod gives every fixture Pod, and running.
func startedReadPod(t *testing.T, api client.Client, job *batchv1.Job) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: job.Name + "-pod", UID: types.UID(job.Name + "-pod-uid"),
		Labels:          maps.Clone(job.Spec.Template.Labels),
		Annotations:     maps.Clone(job.Spec.Template.Annotations),
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}}
	pod.Spec = *job.Spec.Template.Spec.DeepCopy()
	admittedReadPod(pod)
	runningExecutorPod(pod)
	if err := api.Create(context.Background(), pod.DeepCopy()); err != nil {
		t.Fatalf("start the Pod of Job %s: %v", job.Name, err)
	}
	return pod
}

// TestARedispatchedReadIgnoresWhatItsOldPodDelivers is a live read whose Job the
// Job controller marked to fail while its Pod sat on a node that is gone, or
// never marked, as while the Job controller is down, with its Pod running on a
// node that answers (#1006). At the Job's stop bound the claim runs the read
// again under a new Job (#997), and the next passes dispatch that Job. The old Pod then comes back, delivers, and
// its Job finishes. The authority every delivery path asks admitted that Pod
// while the claim named its Job, and refuses it once the claim has moved, under
// the same operation ID, input fingerprint and execution binding: the claim
// names another Job. With both Jobs finished and a record stored for each, the
// controller asks for the new Job's record and never for the old one's.
//
// A Resolve holds no Lease. A Plan does: its retry hands the Lease back, and
// its next attempt takes the realm again under an epoch of its own (#1007).
func TestARedispatchedReadIgnoresWhatItsOldPodDelivers(t *testing.T) {
	t.Parallel()

	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationPlan,
	} {
		for _, marked := range []bool{true, false} {
			name := string(operationType)
			if !marked {
				name += " the Job controller never marked"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				redispatchedReadIgnoresItsOldPod(t, operationType, marked)
			})
		}
	}
}

func redispatchedReadIgnoresItsOldPod(t *testing.T, operationType operatorv1alpha1.OperationType, marked bool) {
	t.Helper()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	current.ResultEndpoint = "https://receiver.operator.svc:9444"
	// A Plan Job carries the key its plan is sealed to, and its claim the
	// digest of that key, as a dispatch records them.
	current.PlanSealPublicKey = testSchemaSealKey.PublicKey()
	run := schemaOperationDispatchedBy(t, operationType, runShape{dispatcher: current, recorded: true})
	ensureTestPlanSealKeyDigest(run.schema)
	readOnALostNode(run)
	if !marked {
		theJobControllerNeverMarked(run)
	}
	admittedReadPod(run.pod)
	reconciler, api, recorder := run.reconciler(t, current)
	// The API server stamps a UID on the Job the next attempt creates.
	reconciler.Client = &durableTestAPI{Client: reconciler.Client}
	results := &countingResults{reader: controllerConsumer(t, api)}
	reconciler.Results = results
	reconciler.Logs = forbiddenResultLogs{t}
	clock := &safetyClock{now: jobStopBound(run.job)}
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)
	if !durableDeliveryRequested(run.job) {
		t.Fatal("the read's Job delivers no durable result, so nothing below measures one")
	}
	old := readIdentity(t, api, run)
	authority := resultauthority.Authorizer{Reader: api}
	if err := authority.Check(ctx, old); err != nil {
		t.Fatalf("the read's Pod could not deliver while its claim stood, so the refusal below proves nothing: %v", err)
	}
	dispatched := safetyGetSchema(t, api, run.schema).Status.ActiveOperation.DeepCopy()

	// At the bound the claim moves to its next attempt.
	if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
		t.Fatalf("Reconcile() at the bound: %v", err)
	}
	moved := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
	if moved == nil || moved.Attempt != dispatched.Attempt+1 || moved.JobName == dispatched.JobName || moved.ID != dispatched.ID ||
		moved.InputFingerprint != dispatched.InputFingerprint || moved.ExecutionBindingID != dispatched.ExecutionBindingID {
		t.Fatalf("at the Job's stop bound the claim did not move to a new Job under the same operation: %#v, events %q",
			moved, eventsWithPrefix(recorder, ""))
	}
	if events := readJobOverdueEvents(recorder); len(events) != 1 {
		t.Fatalf("the claim moved without one ReadJobOverdue Warning, so it did not move at the bound: %q", events)
	}
	if err := authority.Check(ctx, old); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("the old Pod could still deliver once the claim moved: Check() = %v, want %v", err, resultdelivery.ErrAuthority)
	}

	// The retry dispatches the new attempt once its backoff has passed.
	clock.now = clock.now.Add(failureRetry(safetyGetSchema(t, api, run.schema)))
	claim := moved
	for pass := 0; pass < 4 && claim.JobUID == ""; pass++ {
		if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
			t.Fatalf("Reconcile() %d after the backoff: %v", pass, err)
		}
		if claim = safetyGetSchema(t, api, run.schema).Status.ActiveOperation; claim == nil {
			t.Fatalf("the claim went before its next attempt was dispatched: events %q", eventsWithPrefix(recorder, ""))
		}
	}
	if claim.JobName != moved.JobName || claim.Attempt != moved.Attempt || claim.JobUID == "" {
		t.Fatalf("the next attempt was not dispatched under the name the claim moved to %s: claim %#v, events %q",
			moved.JobName, claim, eventsWithPrefix(recorder, ""))
	}
	if schemaClaimHoldsLock(run.schema) {
		switch {
		case dispatched.LeaseEpoch == "":
			t.Fatal("the first attempt held no Lease, so its next attempt took nothing back")
		case claim.LeaseContinuityLost || claim.LeaseEpoch == "" || claim.LeaseEpoch == dispatched.LeaseEpoch:
			t.Fatalf("the next attempt did not take an epoch of its own: lost %v, epoch %q, the first attempt's %q",
				claim.LeaseContinuityLost, claim.LeaseEpoch, dispatched.LeaseEpoch)
		}
	}
	newJob := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: run.schema.Namespace, Name: claim.JobName}, newJob); err != nil || newJob.UID != claim.JobUID {
		t.Fatalf("the Job the claim recorded, %s with UID %s, is not stored: %v", claim.JobName, claim.JobUID, err)
	}
	// The API server gives a created Job the selector and template labels of its
	// own UID, which the fake client does not.
	newJob.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{batchv1.ControllerUidLabel: string(newJob.UID)}}
	newJob.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(newJob.UID)
	newJob.Spec.Template.Labels[batchv1.JobNameLabel] = newJob.Name
	if err := api.Update(ctx, newJob); err != nil {
		t.Fatal(err)
	}
	newRun := dispatchedSchemaRead{schema: safetyGetSchema(t, api, run.schema), job: newJob, pod: startedReadPod(t, api, newJob)}
	fresh := readIdentity(t, api, newRun)
	if err := authority.Check(ctx, fresh); err != nil {
		t.Fatalf("the new attempt's Pod could not deliver, so the old one's refusal is not about its Job: %v", err)
	}

	// The old Pod comes back and delivers, and its Job finishes; so does the
	// new attempt's.
	if err := authority.Check(ctx, old); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("the old Pod could deliver beside the new attempt: Check() = %v, want %v", err, resultdelivery.ErrAuthority)
	}
	plantReadResult(t, api, old)
	finishSchemaRead(t, api, run)
	plantReadResult(t, api, fresh)
	finishSchemaRead(t, api, newRun)
	results.requests = nil
	for pass := 0; pass < 3; pass++ {
		if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
			t.Fatalf("Reconcile() %d after both Jobs finished: %v", pass, err)
		}
	}
	names := func(identity resultdelivery.Identity) func(resultconsumer.Request) bool {
		return func(request resultconsumer.Request) bool {
			return request.JobName == identity.Binding.JobName || request.JobUID == identity.Binding.JobUID
		}
	}
	if !slices.ContainsFunc(results.requests, names(fresh)) {
		t.Fatalf("the controller did not read the new attempt's record, so finding no read of the old one proves nothing: %#v",
			results.requests)
	}
	if slices.ContainsFunc(results.requests, names(old)) {
		t.Fatalf("the controller read the record of the Job the claim moved off: %#v", results.requests)
	}
	if lost := slices.DeleteFunc(eventsWithPrefix(recorder, ""), func(event string) bool {
		return !strings.HasPrefix(event, leaseContinuityLostEvent)
	}); len(lost) != 0 {
		t.Fatalf("the next attempt's own epoch was reported lost: %q", lost)
	}
}
