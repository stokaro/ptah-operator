package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A read that fails, or that its claim lets go of at its Job's stop bound, runs
// again under the claim's next attempt (retryOperationReporting). The write
// that moves the claim hands the attempt's Lease back and leaves the claim
// expecting no epoch, and the next attempt has dispatched nothing when it takes
// the realm again. The epoch it takes is its own, not a lost one (#1007). Taken
// as lost, it discarded the claim, the plan waiting for approval with it, and
// started the read cycle over with a LeaseContinuityLost Warning.

const leaseContinuityLostEvent = corev1.EventTypeWarning + " LeaseContinuityLost "

// waitingPlan is a plan a Plan published before this one ran, waiting for an
// approval: a scheduled re-read plans again while it waits.
func waitingPlan(schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.CurrentPlanStatus {
	binding := schema.Status.ExecutionBinding
	return &operatorv1alpha1.CurrentPlanStatus{
		Name: "waiting-plan", UID: "waiting-plan-uid", ExecutionBindingID: binding.Epoch,
		ControllerStateVersion: binding.ControllerStateVersion, PtahVersion: binding.PtahVersion,
		ExecutorImage: binding.ExecutorImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
	}
}

// failReadJob ends run's Job the way the Job controller ends one whose Pod
// failed: the Pod stopped, and the Job Failed. It returns when the Job failed.
func failReadJob(run dispatchedSchemaRead) time.Time {
	failed := run.job.Status.Conditions[0].LastTransitionTime
	run.job.Status.Terminating = nil
	run.job.Status.Failed = 1
	run.job.Status.Conditions = append(run.job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, LastTransitionTime: failed,
	})
	// The kubelet reported the Pod stopped, so it is not left terminating.
	run.pod.DeletionTimestamp = nil
	run.pod.Finalizers = slices.DeleteFunc(run.pod.Finalizers, func(finalizer string) bool {
		return finalizer == "example.invalid/kubelet-confirmation"
	})
	run.pod.Spec.NodeName = "node-a"
	run.pod.Status.Phase = corev1.PodFailed
	run.pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: executorContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	return failed.Time
}

// retriedPlan is a Plan claim whose attempt ended and whose next attempt the
// claim then dispatched, with what it saw on the way.
type retriedPlan struct {
	run        dispatchedSchemaRead
	reconciler *SchemaReconciler
	api        client.WithWatch
	recorder   *record.FakeRecorder
	clock      *safetyClock
	// dispatched is the claim as the first attempt left it, and claim the
	// claim once its next attempt was dispatched.
	dispatched, claim *operatorv1alpha1.ActiveOperationStatus
	// events is every Event recorded on the way.
	events []string
}

// redispatchPlan runs a Plan on a node that is gone, with a plan waiting for
// approval, to the end of its attempt: its Job's stop bound, or the Job
// failing first. It reconciles until the claim's next attempt is dispatched,
// and fails the test unless the claim moved to that attempt, under the same
// operation, with nothing held, and then dispatched it.
func redispatchPlan(t *testing.T, failed bool) retriedPlan {
	t.Helper()
	ctx := context.Background()
	run := failingSchemaRead(t, operatorv1alpha1.OperationPlan, false)
	run.schema.Status.Plan = waitingPlan(run.schema)
	at := jobStopBound(run.job)
	if failed {
		// The Job failed with no result frame, and the window for one to
		// arrive has closed.
		at = failReadJob(run).Add(frameArrivalWindow)
	}
	reconciler, stored := fakeReconciler(t, staticLogs{}, run.schema, run.job, run.pod)
	api := stored.(client.WithWatch)
	// The API server stamps a UID on the Job the next attempt creates.
	reconciler.Client = &durableTestAPI{Client: reconciler.Client}
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	clock := &safetyClock{now: at}
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)
	plan := retriedPlan{run: run, reconciler: reconciler, api: api, recorder: recorder, clock: clock,
		dispatched: run.schema.Status.ActiveOperation.DeepCopy()}
	if plan.dispatched.LeaseEpoch == "" || leaseHolder(testDatabaseLease(t, api, reconciler.LockNamespace)) == "" {
		t.Fatal("the first attempt holds no Lease, so its retry hands nothing back")
	}

	if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
		t.Fatalf("Reconcile() at the end of the attempt: %v", err)
	}
	plan.events = append(plan.events, eventsWithPrefix(recorder, "")...)
	moved := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
	if moved == nil || moved.ID != plan.dispatched.ID || moved.Attempt != plan.dispatched.Attempt+1 ||
		moved.JobUID != "" || moved.DispatchStarted || moved.LeaseEpoch != "" {
		t.Fatalf("the attempt's end did not move the claim to its next attempt with nothing dispatched or held: claim %#v, events %q",
			moved, plan.events)
	}
	if holder := leaseHolder(testDatabaseLease(t, api, reconciler.LockNamespace)); holder != "" {
		t.Fatalf("the retry kept the Lease for %q", holder)
	}

	clock.now = clock.now.Add(failureRetry(safetyGetSchema(t, api, run.schema)))
	claim := moved
	for pass := 0; pass < 4 && claim.JobUID == ""; pass++ {
		if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
			t.Fatalf("Reconcile() %d after the backoff: %v", pass, err)
		}
		plan.events = append(plan.events, eventsWithPrefix(recorder, "")...)
		if claim = safetyGetSchema(t, api, run.schema).Status.ActiveOperation; claim == nil {
			t.Fatalf("the claim went before its next attempt was dispatched: events %q", plan.events)
		}
	}
	if claim.ID != moved.ID || claim.Attempt != moved.Attempt || claim.JobName != moved.JobName || claim.JobUID == "" {
		t.Fatalf("the claim did not dispatch the attempt it moved to: moved %#v, claim %#v, events %q", moved, claim, plan.events)
	}
	plan.claim = claim
	return plan
}

// TestARetriedPlanTakesItsNextAttemptsEpochAsItsOwn is a Plan whose attempt
// ended, past its Job's stop bound or with its Job failed, while a plan waited
// for approval. The claim moves to its next attempt and hands its Lease back.
// When the attempt takes the realm again under a new epoch, it records that
// epoch as its own and dispatches: the claim keeps its operation, nothing
// reports a lost epoch, and the waiting plan stays.
func TestARetriedPlanTakesItsNextAttemptsEpochAsItsOwn(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		failed bool
	}{
		{name: "past its Job's stop bound"},
		{name: "after its Job failed", failed: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			plan := redispatchPlan(t, row.failed)
			claim := plan.claim
			lease := testDatabaseLease(t, plan.api, plan.reconciler.LockNamespace)
			switch {
			case claim.LeaseContinuityLost:
				t.Fatalf("the next attempt's own epoch was taken as lost: claim %#v, events %q", claim, plan.events)
			case claim.LeaseEpoch == "" || claim.LeaseEpoch == plan.dispatched.LeaseEpoch:
				t.Fatalf("the next attempt recorded epoch %q, want a new one: the first attempt's was %q",
					claim.LeaseEpoch, plan.dispatched.LeaseEpoch)
			case leaseHolder(lease) == "" || lease.Annotations[leaseEpochAnnotation] != claim.LeaseEpoch:
				t.Fatalf("the next attempt does not hold the Lease under the epoch it recorded %q: holder %q, epoch %q",
					claim.LeaseEpoch, leaseHolder(lease), lease.Annotations[leaseEpochAnnotation])
			case !claim.DispatchStarted:
				t.Fatalf("the next attempt was created without its dispatch marker: %#v", claim)
			}
			if lost := slices.DeleteFunc(slices.Clone(plan.events), func(event string) bool {
				return !strings.HasPrefix(event, leaseContinuityLostEvent)
			}); len(lost) != 0 {
				t.Fatalf("the retry reported a lost epoch: %q", lost)
			}
			schema := safetyGetSchema(t, plan.api, plan.run.schema)
			if schema.Status.Plan == nil || schema.Status.Plan.Name != "waiting-plan" || schema.Status.Plan.UID != "waiting-plan-uid" {
				t.Fatalf("the retry took the plan waiting for approval: %#v", schema.Status.Plan)
			}
			for _, condition := range schema.Status.Conditions {
				if condition.Reason == string(operatorv1alpha1.ReasonLeaseContinuityLost) {
					t.Fatalf("condition %s reads %s: %s", condition.Type, condition.Reason, condition.Message)
				}
			}
			job := &batchv1.Job{}
			if err := plan.api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: claim.JobName}, job); err != nil ||
				job.UID != claim.JobUID {
				t.Fatalf("the Job the claim recorded, %s with UID %s, is not stored: %v", claim.JobName, claim.JobUID, err)
			}
		})
	}
}

// TestAClaimThatLostItsEpochStillSaysSo holds the other side of #1007: a claim
// whose hold on the realm broke reports it, though it expects no epoch or was
// retried. A retried Plan that dispatched its next attempt and then found its
// Lease gone has a Job that read under an epoch that ended. A claim that
// dispatched expects an epoch, so one that expects none can show no unbroken
// hold. A proof reads under the epoch of the Apply it accounts for, which
// started long before it, so a proof with no epoch to compare has lost it
// though it dispatched nothing. Each control row is the same claim with its
// epoch, which keeps it.
func TestAClaimThatLostItsEpochStillSaysSo(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// stage returns the run and the reconciler a pass is made with, at the
		// instant it is made.
		stage func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder)
		lost  bool
	}{
		{name: "a retried Plan whose Lease went after it dispatched", lost: true, stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			plan := redispatchPlan(t, false)
			if err := plan.api.Delete(context.Background(), testDatabaseLease(t, plan.api, plan.reconciler.LockNamespace)); err != nil {
				t.Fatal(err)
			}
			plan.clock.now = plan.clock.now.Add(time.Second)
			return plan.run, plan.reconciler, plan.api, plan.recorder
		}},
		{name: "a retried Plan that dispatched and keeps its Lease", stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			plan := redispatchPlan(t, false)
			plan.clock.now = plan.clock.now.Add(time.Second)
			return plan.run, plan.reconciler, plan.api, plan.recorder
		}},
		{name: "a Plan that dispatched and expects no epoch", lost: true, stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			return dispatchedPlanExpecting(t, false)
		}},
		{name: "a Plan that dispatched and expects its epoch", stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			return dispatchedPlanExpecting(t, true)
		}},
		{name: "a proof that dispatched nothing and has no epoch to compare", lost: true, stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			return undispatchedProofExpecting(t, false)
		}},
		{name: "a proof that dispatched nothing and has the Apply's epoch", stage: func(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
			return undispatchedProofExpecting(t, true)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := row.stage(t)
			eventsWithPrefix(recorder, "")
			before := safetyGetSchema(t, api, run.schema).Status.ActiveOperation.DeepCopy()
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			events := eventsWithPrefix(recorder, leaseContinuityLostEvent)
			claim := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if claim == nil || claim.ID != before.ID || claim.Attempt != before.Attempt {
				t.Fatalf("the pass did not keep the claim to report on: before %#v, after %#v", before, claim)
			}
			if row.lost && (!claim.LeaseContinuityLost || len(events) != 1) {
				t.Fatalf("a claim whose hold broke did not report it: lost %v, LeaseContinuityLost Events %q", claim.LeaseContinuityLost, events)
			}
			if !row.lost && (claim.LeaseContinuityLost || len(events) != 0) {
				t.Fatalf("a claim whose hold held reported it lost: lost %v, LeaseContinuityLost Events %q", claim.LeaseContinuityLost, events)
			}
		})
	}
}

// dispatchedPlanExpecting is a Plan whose Job is still running, a second
// before its stop bound. With expects it holds the Lease the fixtures seeded
// under the epoch its claim expects. Without, the claim expects no epoch and no
// Lease is seeded for it.
func dispatchedPlanExpecting(t *testing.T, expects bool) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	run := failingSchemaRead(t, operatorv1alpha1.OperationPlan, false)
	if !schemaMayHaveDispatched(run.schema.Status.ActiveOperation) {
		t.Fatal("the Plan fixture has not dispatched, so the row measures another claim")
	}
	if !expects {
		run.schema.Status.ActiveOperation.LeaseEpoch = ""
	}
	reconciler, stored := fakeReconciler(t, staticLogs{}, run.schema, run.job, run.pod)
	api := stored.(client.WithWatch)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	clock := &safetyClock{now: jobStopBound(run.job).Add(-time.Second)}
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)
	return run, reconciler, api, recorder
}

// undispatchedProofExpecting is the Observe that proves an Apply, claimed and
// not yet dispatched. With expects the pending observation carries the
// Apply's epoch, under which the fixtures seeded the Lease. Without, neither
// the pending observation nor the claim carries one, and no Lease is seeded.
func undispatchedProofExpecting(t *testing.T, expects bool) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	schema := safetyPostApplyObserveSchema(t)
	operation := schema.Status.ActiveOperation
	operation.JobUID = ""
	operation.DispatchStarted = false
	operation.StartedAt = metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	if !expects {
		schema.Status.PendingObservation.LeaseEpoch = ""
	}
	bindActiveInput(t, schema)
	if schemaMayHaveDispatched(operation) || operation.LeaseEpoch != schema.Status.PendingObservation.LeaseEpoch {
		t.Fatalf("the proof fixture dispatched, or expects an epoch its pending observation does not carry: %#v", operation)
	}
	run := dispatchedSchemaRead{schema: schema}
	reconciler, stored := fakeReconciler(t, staticLogs{}, schema)
	api := stored.(client.WithWatch)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	clock := &safetyClock{now: operation.StartedAt.Add(time.Second)}
	reconciler.Clock = clock.Now
	reconciler.Locks = targetlock.New(api, api, clock)
	return run, reconciler, api, recorder
}
