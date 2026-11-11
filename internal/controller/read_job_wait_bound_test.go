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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A read Job whose Pod sits on a node that is gone never finishes: since
// Kubernetes 1.31 the Job carries only FailureTarget until every Pod has
// terminated, and the Pod stays Terminating until the node object goes or the
// Pod is force-deleted (#997). A read-only claim waits for such a Job, or for
// the Pods of a Job that is gone, until their stop bound, and no longer: its
// deletion, its supervision and its retry past a lost Job each stop there. The
// fake client runs no Job controller and no kubelet, so each row writes what a
// supported cluster shows: a Job past its deadline with FailureTarget, and its
// Pod deleted and still Running on a node that does not answer.

const readJobOverdueEvent = corev1.EventTypeWarning + " ReadJobOverdue "

// readJobOverdueEvents is every ReadJobOverdue Event recorded since the last
// call.
func readJobOverdueEvents(recorder *record.FakeRecorder) []string {
	return eventsWithPrefix(recorder, readJobOverdueEvent)
}

// eventsWithPrefix drains recorder and returns the Events that start with
// prefix.
func eventsWithPrefix(recorder *record.FakeRecorder, prefix string) []string {
	var found []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, prefix) {
				found = append(found, event)
			}
		default:
			return found
		}
	}
}

// leaseWrites records the holder every Lease write a Locker makes names: a
// renewal names its holder, and a release names nobody.
type leaseWrites struct{ holders []string }

// writer is api with every Lease create and update recorded once it is stored.
func (w *leaseWrites) writer(api client.WithWatch) client.WithWatch {
	record := func(object client.Object) {
		if lease, ok := object.(*coordinationv1.Lease); ok {
			holder := ""
			if lease.Spec.HolderIdentity != nil {
				holder = *lease.Spec.HolderIdentity
			}
			w.holders = append(w.holders, holder)
		}
	}
	return interceptor.NewClient(api, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := c.Create(ctx, object, options...); err != nil {
				return err
			}
			record(object)
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if err := c.Update(ctx, object, options...); err != nil {
				return err
			}
			record(object)
			return nil
		},
	})
}

// take returns the holders written since the last call.
func (w *leaseWrites) take() []string {
	taken := w.holders
	w.holders = nil
	return taken
}

// schemaStatusWrites records every PtahSchema status a client stores.
type schemaStatusWrites struct {
	written []*operatorv1alpha1.PtahSchema
}

func (w *schemaStatusWrites) client(api client.WithWatch) client.WithWatch {
	return interceptor.NewClient(api, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption) error {
			if err := c.SubResource(subResource).Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			if schema, ok := object.(*operatorv1alpha1.PtahSchema); ok {
				w.written = append(w.written, schema.DeepCopy())
			}
			return nil
		},
	})
}

// testDatabaseLease is the realm Lease the fixtures' claims hold.
func testDatabaseLease(t *testing.T, api client.Client, namespace string) *coordinationv1.Lease {
	t.Helper()
	name, err := targetlock.LeaseName(testCoordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, lease); err != nil {
		t.Fatalf("Get(Lease): %v", err)
	}
	return lease
}

// leaseHolder is who holds lease, or empty.
func leaseHolder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// readOnALostNode leaves run's Pod deleted and still Running on a node that
// does not answer, and its Job past its deadline: FailureTarget, with the Pod
// counted as terminating. The Pod is stored already deleted, which the fake
// client keeps, so its grace ends on the fixture's clock.
func readOnALostNode(run dispatchedSchemaRead) {
	failed := run.job.Status.StartTime.Add(time.Duration(*run.job.Spec.ActiveDeadlineSeconds) * time.Second)
	goneBy := metav1.NewTime(failed.Add(time.Duration(*run.job.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second))
	run.pod.DeletionTimestamp = &goneBy
	run.pod.Finalizers = append(run.pod.Finalizers, "example.invalid/kubelet-confirmation")
	run.pod.Spec.NodeName = "node-gone"
	terminating := int32(1)
	run.job.Status = batchv1.JobStatus{StartTime: run.job.Status.StartTime, Terminating: &terminating, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
		LastTransitionTime: metav1.NewTime(failed),
	}}}
}

// TestADeletedSchemaStopsWaitingForAReadAtItsStopBound is the deletion of a
// schema whose Plan, or whose read that proves an Apply, runs on a node that is
// gone. A read writes nothing to the database, so deletion waits for its Job
// only until the Job's stop bound (#997). Before it, the claim stays and renews
// the Lease, as it did. At it, the claim is discarded without another renewal,
// a ReadJobOverdue Warning names the Job and the Pod that may still read, the
// Lease goes back, a proof that was never read is recorded unknown on the write
// that discards its claim, and the resource goes.
func TestADeletedSchemaStopsWaitingForAReadAtItsStopBound(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.OperationType
		proof     bool
	}{
		{name: "a Plan", operation: operatorv1alpha1.OperationPlan},
		{name: "the Observe that proves an Apply", operation: operatorv1alpha1.OperationObserve, proof: true},
		{name: "the Plan that proves an Apply", operation: operatorv1alpha1.OperationPlan, proof: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, row.operation, runShape{dispatcher: current, recorded: true, proof: row.proof})
			readOnALostNode(run)
			reconciler, api, recorder := run.reconciler(t, current)
			statuses := &schemaStatusWrites{}
			reconciler.Client = statuses.client(reconciler.Client.(client.WithWatch))
			writes := &leaseWrites{}
			clock := &safetyClock{}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, writes.writer(api), clock)
			if err := api.Delete(ctx, run.schema.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			holder := leaseHolder(testDatabaseLease(t, api, reconciler.LockNamespace))
			if holder == "" {
				t.Fatal("the read's claim holds no Lease, so nothing below measures one")
			}
			pod := &corev1.Pod{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}
			bound := jobStopBound(run.job)
			if !bound.After(pod.DeletionTimestamp.Time) {
				t.Fatalf("the Job's stop bound %s is not after the Pod was to be gone, %s, so the Pod is not stuck by then", bound, pod.DeletionTimestamp)
			}

			// A second before the bound the deletion waits and renews.
			clock.now = bound.Add(-time.Second)
			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile() before the bound: %v", err)
			}
			waiting := safetyGetSchema(t, api, run.schema)
			if waiting.Status.ActiveOperation == nil || result.RequeueAfter != 5*time.Second {
				t.Fatalf("a second before the Job's stop bound the deletion did not wait: result %#v, claim %#v",
					result, waiting.Status.ActiveOperation)
			}
			if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) != holder ||
				lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(clock.now) {
				t.Fatalf("the waiting deletion did not renew the Lease at %s: holder %q, renewed %v",
					clock.now, leaseHolder(lease), lease.Spec.RenewTime)
			}
			if events := readJobOverdueEvents(recorder); len(events) != 0 {
				t.Fatalf("a second before the Job's stop bound the deletion recorded %q", events)
			}
			writes.take()
			statuses.written = nil

			// At the bound it stops.
			clock.now = bound
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() at the bound: %v", err)
			}
			want := fmt.Sprintf("%sStopped waiting for %s Job %s: it did not stop by %s, "+
				"its own deadline plus the grace its Pods are given and 30 seconds. Deletion discards the claim and goes on. "+
				"Pod %s on node node-gone has not stopped and may still read the database; nothing uses what it reads. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				readJobOverdueEvent, row.operation, run.job.Name, bound.UTC().Format(time.RFC3339),
				pod.Name, pod.Namespace, pod.Name)
			events := eventsWithPrefix(recorder, "")
			if !slices.Contains(events, want) {
				t.Fatalf("Events at the Job's stop bound = %q, want %q", events, want)
			}
			if slices.ContainsFunc(events, func(event string) bool { return strings.HasPrefix(event, stuckPodEvent) }) {
				t.Fatalf("the deletion that stopped waiting still says it waits for the Pod: %q", events)
			}
			gone := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.schema), gone); !apierrors.IsNotFound(err) {
				t.Fatalf("the resource stayed past the Job's stop bound: error %v, claim %#v", err, gone.Status.ActiveOperation)
			}
			if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) != "" {
				t.Fatalf("the discarded claim's Lease is still held by %q", leaseHolder(lease))
			}
			held := writes.take()
			if !slices.Contains(held, "") {
				t.Fatalf("the pass at the bound wrote the Lease %q times and never released it", held)
			}
			// A proof's realm is the pending observation's, and the deletion
			// renews it once for the Apply it names before it lets it go.
			// The claim's own wait renews nothing past the bound.
			if renewals := slices.DeleteFunc(slices.Clone(held), func(h string) bool { return h == "" }); len(renewals) > 0 && !row.proof {
				t.Fatalf("the Plan's Lease was renewed for %q at its Job's stop bound", renewals)
			}
			discard := slices.IndexFunc(statuses.written, func(schema *operatorv1alpha1.PtahSchema) bool {
				return schema.Status.ActiveOperation == nil
			})
			if discard < 0 {
				t.Fatalf("no status write discarded the claim: %d writes", len(statuses.written))
			}
			pending := statuses.written[discard].Status.PendingObservation
			switch {
			case !row.proof && pending != nil:
				t.Fatalf("a Plan that proves nothing left a pending observation: %#v", pending)
			case row.proof && (pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.PlanRequired):
				t.Fatalf("the write that discarded the proof's claim recorded %#v, want the Apply's outcome unknown and no plan required", pending)
			}
		})
	}
}

// failingSchemaRead is a running read of operationType, with the Job and the
// Pod fakeReconciler's builder makes for it, on a node that is gone. The Job
// started when the fixtures seeded the realm Lease and carries no deadline of its
// own, the Job controller marked it to fail ten seconds in, and the Pod it
// deleted then still runs. A proof read serves an Apply's pending observation,
// which holds the realm.
func failingSchemaRead(t *testing.T, operationType operatorv1alpha1.OperationType, proof bool) dispatchedSchemaRead {
	t.Helper()
	started := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	var schema *operatorv1alpha1.PtahSchema
	if proof {
		if operationType != operatorv1alpha1.OperationObserve {
			t.Fatalf("no proof fixture for a %s", operationType)
		}
		schema = safetyPostApplyObserveSchema(t)
		schema.Status.ActiveOperation.StartedAt = started
	} else {
		schema = schemaFixture()
		schema.Finalizers = []string{activeOperationFinalizer}
		schema.Status.Phase = mutationlifecycle.SchemaOperation(operationType).Phase
		id := strings.ToLower(string(operationType)) + "-running"
		schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operationType, ID: id, JobName: id, StartedAt: started, Attempt: 1,
		}
		if operationType == operatorv1alpha1.OperationPlan {
			schema.Status.Target = operatorv1alpha1.TargetStatus{
				Engine: schema.Spec.Target.Engine, CoordinationDigest: testCoordinationDigest,
				IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
			}
		}
		bindActiveInput(t, schema)
		schema.Status.ActiveOperation.DispatchStarted = schemaClaimHoldsLock(schema)
	}
	job, pod := terminalWorkload(schema, batchv1.JobFailureTarget)
	schema.Status.ActiveOperation.JobUID = job.UID
	failed := metav1.NewTime(started.Add(10 * time.Second))
	terminating := int32(1)
	job.Status = batchv1.JobStatus{StartTime: &started, Terminating: &terminating, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
		LastTransitionTime: failed,
	}}}
	runningExecutorPod(pod)
	pod.Status.StartTime = &started
	goneBy := metav1.NewTime(failed.Add(time.Duration(corev1.DefaultTerminationGracePeriodSeconds) * time.Second))
	pod.DeletionTimestamp = &goneBy
	pod.Finalizers = append(pod.Finalizers, "example.invalid/kubelet-confirmation")
	pod.Spec.NodeName = "node-gone"
	return dispatchedSchemaRead{schema: schema, job: job, pod: pod}
}

// TestASchemaReadStopsSupervisingAFailingJobAtItsStopBound is a read whose Job
// the Job controller marked to fail past its deadline while the Pod sits on a
// node that is gone. The claim supervises the Job until its stop bound (#997).
// Before it, the claim waits as it did, and a Plan's claim renews the Lease. At
// it, a ReadJobOverdue Warning names the Job and the Pod, and the claim moves to
// its next attempt under a new Job name without another renewal: a Plan hands
// its Lease back, and a read that proves an Apply leaves the realm with the
// proof, which goes on. A Job with no FailureTarget is still running as far as
// the Job controller is concerned, and is supervised past the same instant.
func TestASchemaReadStopsSupervisingAFailingJobAtItsStopBound(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.OperationType
		proof     bool
		// unmarked leaves the Job with no condition at all: the Job controller
		// has not marked it, though its bound has passed.
		unmarked bool
	}{
		{name: "a Resolve", operation: operatorv1alpha1.OperationResolve},
		{name: "a Plan", operation: operatorv1alpha1.OperationPlan},
		{name: "the Observe that proves an Apply", operation: operatorv1alpha1.OperationObserve, proof: true},
		{name: "a Plan the Job controller has not marked", operation: operatorv1alpha1.OperationPlan, unmarked: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := failingSchemaRead(t, row.operation, row.proof)
			bound := jobStopBound(run.job)
			if row.unmarked {
				run.job.Status.Conditions = nil
				if unmarked := jobStopBound(run.job); !unmarked.Equal(bound) {
					t.Fatalf("the unmarked Job's stop bound %s is not the marked one's %s, so the row compares two instants", unmarked, bound)
				}
			}
			reconciler, stored := fakeReconciler(t, staticLogs{}, run.schema, run.job, run.pod)
			api := stored.(client.WithWatch)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			writes := &leaseWrites{}
			clock := &safetyClock{now: bound.Add(-time.Second)}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, writes.writer(api), clock)
			holdsLock := schemaClaimHoldsLock(run.schema)
			dispatched := run.schema.Status.ActiveOperation.DeepCopy()
			pod := &corev1.Pod{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(run.pod), pod); err != nil {
				t.Fatal(err)
			}

			// A second before the bound the claim supervises the Job.
			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile() before the bound: %v", err)
			}
			if operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; operation == nil ||
				operation.Attempt != dispatched.Attempt || operation.JobName != dispatched.JobName ||
				result.RequeueAfter != activeJobPollInterval(holdsLock) {
				t.Fatalf("a second before the Job's stop bound the claim did not supervise it: result %#v, claim %#v, events %q",
					result, operation, eventsWithPrefix(recorder, ""))
			}
			if holdsLock {
				if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) == "" ||
					lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(clock.now) {
					t.Fatalf("the supervising claim did not renew the Lease at %s: holder %q, renewed %v",
						clock.now, leaseHolder(lease), lease.Spec.RenewTime)
				}
			}
			if events := readJobOverdueEvents(recorder); len(events) != 0 {
				t.Fatalf("a second before the Job's stop bound the claim recorded %q", events)
			}
			writes.take()

			// At the bound it stops, once the Job controller has said so.
			clock.now = bound
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() at the bound: %v", err)
			}
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			all := eventsWithPrefix(recorder, "")
			events := slices.DeleteFunc(slices.Clone(all), func(event string) bool { return !strings.HasPrefix(event, readJobOverdueEvent) })
			held := writes.take()
			if row.unmarked {
				if operation == nil || operation.Attempt != dispatched.Attempt || operation.JobName != dispatched.JobName || len(events) != 0 {
					t.Fatalf("the claim stopped supervising a Job the Job controller has not marked: claim %#v, events %q", operation, all)
				}
				return
			}
			if operation == nil || operation.Attempt != dispatched.Attempt+1 || operation.JobName == dispatched.JobName || operation.JobUID != "" {
				t.Fatalf("at the Job's stop bound the claim did not move to its next attempt: claim %#v, events %q", operation, all)
			}
			want := fmt.Sprintf("%sStopped waiting for %s Job %s: it did not stop by %s, "+
				"its own deadline plus the grace its Pods are given and 30 seconds. "+
				"The %s runs again under a new Job, and Job %s stays until it is deleted by hand or with its owner. "+
				"Pod %s on node node-gone has not stopped and may still read the database; nothing uses what it reads. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				readJobOverdueEvent, row.operation, run.job.Name, bound.UTC().Format(time.RFC3339),
				row.operation, run.job.Name, pod.Name, pod.Namespace, pod.Name)
			if len(events) != 1 || events[0] != want {
				t.Fatalf("Events at the Job's stop bound = %q, want one %q", all, want)
			}
			if holdsLock {
				lease := testDatabaseLease(t, api, reconciler.LockNamespace)
				switch {
				case row.proof && leaseHolder(lease) == "":
					t.Fatal("the proof's realm was handed back with the read that serves it")
				case !row.proof && leaseHolder(lease) != "":
					t.Fatalf("the Plan that moved on still holds the Lease as %q", leaseHolder(lease))
				case !row.proof && !slices.Contains(held, ""):
					t.Fatalf("the pass at the bound wrote the Lease %q and never released it", held)
				}
			}
			// The proof's realm is the pending observation's, renewed by it at
			// the top of every pass; no claim renews a Lease for the read past
			// the bound.
			if !row.proof && slices.ContainsFunc(held, func(holder string) bool { return holder != "" }) {
				t.Fatalf("the Lease was renewed for %q at the Job's stop bound", held)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job that has not stopped got a cleanup TTL of %d", *got)
			}
		})
	}
}

// TestAMigrationReadStopsSupervisingAFailingJobAtItsStopBound is the same for a
// PtahMigration read, which holds no Lease.
func TestAMigrationReadStopsSupervisingAFailingJobAtItsStopBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
	failed := run.job.Status.StartTime.Add(time.Duration(*run.job.Spec.ActiveDeadlineSeconds) * time.Second)
	goneBy := metav1.NewTime(failed.Add(time.Duration(*run.job.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second))
	run.pod.DeletionTimestamp = &goneBy
	run.pod.Finalizers = append(run.pod.Finalizers, "example.invalid/kubelet-confirmation")
	run.pod.Spec.NodeName = "node-gone"
	terminating := int32(1)
	run.job.Status = batchv1.JobStatus{StartTime: run.job.Status.StartTime, Terminating: &terminating, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
		LastTransitionTime: metav1.NewTime(failed),
	}}}
	reconciler, api := run.reconciler(t, current)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	bound := jobStopBound(run.job)
	clock := &fixedClock{now: bound.Add(-time.Second)}
	reconciler.Clock = clock.Now
	dispatched := run.migration.Status.ActiveOperation.DeepCopy()
	request := migrationRequest(run.migration)

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() before the bound: %v", err)
	}
	if operation := readMigration(t, api, run.migration).Status.ActiveOperation; operation == nil ||
		operation.Attempt != dispatched.Attempt || operation.JobName != dispatched.JobName || result.RequeueAfter != activeJobPollInterval(false) {
		t.Fatalf("a second before the Job's stop bound the claim did not supervise it: result %#v, claim %#v", result, operation)
	}
	if events := readJobOverdueEvents(recorder); len(events) != 0 {
		t.Fatalf("a second before the Job's stop bound the claim recorded %q", events)
	}

	clock.now = bound
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() at the bound: %v", err)
	}
	operation := readMigration(t, api, run.migration).Status.ActiveOperation
	if operation == nil || operation.Attempt != dispatched.Attempt+1 || operation.JobName == dispatched.JobName || operation.JobUID != "" {
		t.Fatalf("at the Job's stop bound the claim did not move to its next attempt: %#v", operation)
	}
	want := fmt.Sprintf("%sStopped waiting for History Job %s: it did not stop by %s, "+
		"its own deadline plus the grace its Pods are given and 30 seconds. "+
		"The History runs again under a new Job, and Job %s stays until it is deleted by hand or with its owner. "+
		"Pod %s on node node-gone has not stopped and may still read the database; nothing uses what it reads. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		readJobOverdueEvent, run.job.Name, bound.UTC().Format(time.RFC3339), run.job.Name,
		run.pod.Name, run.pod.Namespace, run.pod.Name)
	if events := readJobOverdueEvents(recorder); len(events) != 1 || events[0] != want {
		t.Fatalf("Events at the Job's stop bound = %q, want one %q", events, want)
	}
}

// lostJobPod leaves pod deleted with its Job, still Running on a node that does
// not answer: the garbage collector deleted it at deleted, so its grace ends
// there plus its termination grace. It started at started.
func lostJobPod(pod *corev1.Pod, started, deleted time.Time) {
	start := metav1.NewTime(started)
	pod.Status.StartTime = &start
	goneBy := metav1.NewTime(deleted.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second))
	pod.DeletionTimestamp = &goneBy
	pod.Finalizers = append(pod.Finalizers, "example.invalid/kubelet-confirmation")
	pod.Spec.NodeName = "node-gone"
}

// TestAReadWhoseJobIsGoneWaitsForItsPodsOnlyUntilTheirStopBound is a read whose
// recorded Job was deleted while its Pod sat on a node that is gone. Deleting
// a Job deletes its Pods, and that one stays Terminating. The claim waits for
// the Pods the Job owned until their stop bound (#997), and a Plan's claim
// renews the Lease while it waits, as it did. At the bound a ReadJobOverdue
// Warning names the Job and the Pod, and the claim moves to its next attempt
// without another renewal, handing a Plan's Lease back.
func TestAReadWhoseJobIsGoneWaitsForItsPodsOnlyUntilTheirStopBound(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, operationType := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := schemaOperationDispatchedBy(t, operationType, runShape{dispatcher: current, recorded: true})
			deleted := run.job.Status.StartTime.Add(2 * time.Minute)
			lostJobPod(run.pod, run.job.Status.StartTime.Time, deleted)
			reconciler, stored := fakeReconciler(t, staticLogs{}, run.schema, run.pod)
			api := stored.(client.WithWatch)
			reconciler.Jobs = current
			reconciler.Client = admittedJobWrites(api, current)
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			bound := podStopBound(run.pod)
			if want := run.pod.DeletionTimestamp.Add(jobStopReportSlack); !bound.Equal(want) {
				t.Fatalf("the Pod's stop bound is %s, want %s, the end of its grace plus the slack", bound, want)
			}
			writes := &leaseWrites{}
			clock := &safetyClock{now: bound.Add(-time.Second)}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, writes.writer(api), clock)
			holdsLock := schemaClaimHoldsLock(run.schema)
			dispatched := run.schema.Status.ActiveOperation.DeepCopy()

			result, err := reconciler.Reconcile(ctx, run.request())
			if err != nil {
				t.Fatalf("Reconcile() before the bound: %v", err)
			}
			if operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation; operation == nil ||
				operation.Attempt != dispatched.Attempt || operation.JobUID != dispatched.JobUID || result.RequeueAfter != maxLockContentionPoll {
				t.Fatalf("a second before the Pod's stop bound the claim did not wait for it: result %#v, claim %#v", result, operation)
			}
			if held := writes.take(); holdsLock && (len(held) == 0 || slices.Contains(held, "")) {
				t.Fatalf("the waiting Plan wrote the Lease %q, want it renewed", held)
			}
			if events := readJobOverdueEvents(recorder); len(events) != 0 {
				t.Fatalf("a second before the Pod's stop bound the claim recorded %q", events)
			}

			clock.now = bound
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() at the bound: %v", err)
			}
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			if operation == nil || operation.Attempt != dispatched.Attempt+1 || operation.JobUID != "" || operation.JobName == dispatched.JobName {
				t.Fatalf("at the Pod's stop bound the claim did not move to its next attempt: %#v", operation)
			}
			want := fmt.Sprintf("%sStopped waiting for the Pods of %s Job %s, which is gone: they did not stop by %s, "+
				"their own deadline plus the grace they are given and 30 seconds. The %s runs again under a new Job. "+
				"Pod %s on node node-gone has not stopped and may still read the database; nothing uses what it reads. "+
				"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
				readJobOverdueEvent, operationType, dispatched.JobName, bound.UTC().Format(time.RFC3339), operationType,
				run.pod.Name, run.pod.Namespace, run.pod.Name)
			if events := readJobOverdueEvents(recorder); len(events) != 1 || events[0] != want {
				t.Fatalf("Events at the Pod's stop bound = %q, want one %q", events, want)
			}
			held := writes.take()
			if slices.ContainsFunc(held, func(holder string) bool { return holder != "" }) {
				t.Fatalf("the Lease was renewed for %q at the Pod's stop bound", held)
			}
			if holdsLock {
				if lease := testDatabaseLease(t, api, reconciler.LockNamespace); leaseHolder(lease) != "" || !slices.Contains(held, "") {
					t.Fatalf("the Plan that moved on did not hand its Lease back: holder %q, writes %q", leaseHolder(lease), held)
				}
			}
		})
	}
}

// TestAMigrationReadWhoseJobIsGoneWaitsForItsPodsOnlyUntilTheirStopBound is the
// same for a PtahMigration read.
func TestAMigrationReadWhoseJobIsGoneWaitsForItsPodsOnlyUntilTheirStopBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationHistory, runShape{dispatcher: current, recorded: true})
	deleted := run.job.Status.StartTime.Add(2 * time.Minute)
	lostJobPod(run.pod, run.job.Status.StartTime.Time, deleted)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, run.migration, run.pod, verificationPolicyConfigMap())
	reconciler.Jobs = current
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	bound := podStopBound(run.pod)
	clock := &fixedClock{now: bound.Add(-time.Second)}
	reconciler.Clock = clock.Now
	dispatched := run.migration.Status.ActiveOperation.DeepCopy()
	request := migrationRequest(run.migration)

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("Reconcile() before the bound: %v", err)
	}
	if operation := readMigration(t, api, run.migration).Status.ActiveOperation; operation == nil ||
		operation.Attempt != dispatched.Attempt || operation.JobUID != dispatched.JobUID || result.RequeueAfter != maxLockContentionPoll {
		t.Fatalf("a second before the Pod's stop bound the claim did not wait for it: result %#v, claim %#v", result, operation)
	}
	if events := readJobOverdueEvents(recorder); len(events) != 0 {
		t.Fatalf("a second before the Pod's stop bound the claim recorded %q", events)
	}

	clock.now = bound
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() at the bound: %v", err)
	}
	operation := readMigration(t, api, run.migration).Status.ActiveOperation
	if operation == nil || operation.Attempt != dispatched.Attempt+1 || operation.JobUID != "" || operation.JobName == dispatched.JobName {
		t.Fatalf("at the Pod's stop bound the claim did not move to its next attempt: %#v", operation)
	}
	want := fmt.Sprintf("%sStopped waiting for the Pods of History Job %s, which is gone: they did not stop by %s, "+
		"their own deadline plus the grace they are given and 30 seconds. The History runs again under a new Job. "+
		"Pod %s on node node-gone has not stopped and may still read the database; nothing uses what it reads. "+
		"Once the node is known to be gone, delete the Pod: kubectl delete pod -n %s %s --force --grace-period=0",
		readJobOverdueEvent, dispatched.JobName, bound.UTC().Format(time.RFC3339),
		run.pod.Name, run.pod.Namespace, run.pod.Name)
	if events := readJobOverdueEvents(recorder); len(events) != 1 || events[0] != want {
		t.Fatalf("Events at the Pod's stop bound = %q, want one %q", events, want)
	}
}

// TestAPodStopBoundIsItsDeadlineOrItsDeletion holds podStopBound to the two
// instants a Pod has had every chance to stop by: its deadline from its start
// plus its grace and the slack, or the end of the grace of its deletion plus the
// slack, whichever comes first. A Pod that never started counts from its
// creation, and one with no grace of its own has the default.
func TestAPodStopBoundIsItsDeadlineOrItsDeletion(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	started := created.Add(time.Minute)
	defaultGrace := time.Duration(corev1.DefaultTerminationGracePeriodSeconds) * time.Second
	for _, row := range []struct {
		name string
		pod  func(*corev1.Pod)
		want time.Time
	}{
		{name: "a running Pod", want: started.Add(900*time.Second + 20*time.Second + jobStopReportSlack)},
		{name: "a Pod that never started", pod: func(pod *corev1.Pod) { pod.Status.StartTime = nil },
			want: created.Add(900*time.Second + 20*time.Second + jobStopReportSlack)},
		{name: "a Pod with no grace of its own", pod: func(pod *corev1.Pod) { pod.Spec.TerminationGracePeriodSeconds = nil },
			want: started.Add(900*time.Second + defaultGrace + jobStopReportSlack)},
		{name: "a Pod deleted before its deadline", pod: func(pod *corev1.Pod) {
			goneBy := metav1.NewTime(started.Add(5 * time.Minute))
			pod.DeletionTimestamp = &goneBy
		}, want: started.Add(5*time.Minute + jobStopReportSlack)},
		{name: "a Pod deleted after its deadline", pod: func(pod *corev1.Pod) {
			goneBy := metav1.NewTime(started.Add(time.Hour))
			pod.DeletionTimestamp = &goneBy
		}, want: started.Add(900*time.Second + 20*time.Second + jobStopReportSlack)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			start := metav1.NewTime(started)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Spec:       corev1.PodSpec{ActiveDeadlineSeconds: ptr(int64(900)), TerminationGracePeriodSeconds: ptr(int64(20))},
				Status:     corev1.PodStatus{StartTime: &start},
			}
			if row.pod != nil {
				row.pod(pod)
			}
			if got := podStopBound(pod); !got.Equal(row.want) {
				t.Fatalf("podStopBound() = %s, want %s", got, row.want)
			}
		})
	}
}

// TestPodsStillStoppingCountOnlyThePodsThatHaveNotStopped holds the wait for
// the Pods of a Job that is gone to the Pods that may still run: one that
// Succeeded or Failed waits for nothing, and the wait lasts until the latest
// bound among the rest.
func TestPodsStillStoppingCountOnlyThePodsThatHaveNotStopped(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	pod := func(name string, phase corev1.PodPhase, deadline int64) *corev1.Pod {
		started := metav1.NewTime(start)
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.PodSpec{ActiveDeadlineSeconds: ptr(deadline), TerminationGracePeriodSeconds: ptr(int64(0))},
			Status:     corev1.PodStatus{Phase: phase, StartTime: &started},
		}
	}
	running, stopBy := podsStillStopping([]*corev1.Pod{
		pod("succeeded", corev1.PodSucceeded, 3600),
		pod("early", corev1.PodRunning, 60),
		pod("failed", corev1.PodFailed, 7200),
		pod("late", corev1.PodPending, 600),
	})
	var names []string
	for _, pod := range running {
		names = append(names, pod.Name)
	}
	if !slices.Equal(names, []string{"early", "late"}) {
		t.Fatalf("Pods still stopping = %q, want the two that have not stopped", names)
	}
	if want := start.Add(600*time.Second + jobStopReportSlack); !stopBy.Equal(want) {
		t.Fatalf("the wait lasts until %s, want %s, the later of the two bounds", stopBy, want)
	}
	if running, stopBy := podsStillStopping(nil); len(running) != 0 || !stopBy.IsZero() {
		t.Fatalf("no Pods left %d still stopping until %s", len(running), stopBy)
	}
}
