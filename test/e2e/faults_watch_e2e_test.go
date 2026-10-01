//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// recorder is what the fault run does with every watch alike.
type recorder interface {
	stem() string
	alive() error
	requestStop()
	await(timeout time.Duration) error
	abort()
	history() ([]byte, int, error)
}

// watchRecorder keeps one collection's resourceVersion watch for the whole
// fault injection. The API server ends each watch request after
// watchSegmentSeconds, and the recorder continues from the last
// resourceVersion it read, so the history has no gap. A segment that records
// nothing while the heartbeat writes to every watched kind is a stopped
// watch, and ends the recorder with a failure. A quiet collection resumes
// from the same version and owes explicit barriers. Every error event or
// event that is not an object of the kind watched fails either mode.
type watchRecorder[T client.Object] struct {
	name      string
	namespace string
	newList   func() client.ObjectList
	watcher   client.WithWatch
	ctx       context.Context
	cancel    context.CancelFunc
	// quiet permits a collection with no heartbeat fixture. An empty segment
	// resumes at the same resourceVersion; explicit watch barriers still have
	// to be observed before any assertion or successful closure.
	quiet bool

	mu       sync.Mutex
	events   []watchEvent[T]
	failure  error
	stopping atomic.Bool
	done     chan struct{}
}

// startRecorder reads the collection straight from the API server, which
// answers with the typed list and the resourceVersion the watch continues
// from, and starts the watch there. Starting from an empty resourceVersion
// would replay every existing object as ADDED, as if the phase had created
// it. The Jobs listed enter the observed ledger, since the list and the watch
// after it are one boundary with no gap.
func startRecorder[T client.Object](f *faultRun, name, namespace string, quiet bool, newList func() client.ObjectList) *watchRecorder[T] {
	f.t.Helper()
	list := newList()
	f.check(f.cluster.Client.List(f.ctx, list, client.InNamespace(namespace)), "list the %s collection to watch from", name)
	resourceVersion := list.GetResourceVersion()
	if resourceVersion == "" {
		f.fatalf("the %s collection list carries no resourceVersion to watch from", name)
	}
	if jobs, ok := list.(*batchv1.JobList); ok {
		records, err := observedJobRecords(jobs.Items)
		f.check(err, "validate the initial fault Job list before updating the observed ledger")
		f.observed.add(records)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &watchRecorder[T]{
		name: name, namespace: namespace, newList: newList, watcher: f.watcher, quiet: quiet,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	go r.run(resourceVersion)
	f.recorders = append(f.recorders, r)
	return r
}

func (r *watchRecorder[T]) stem() string { return r.name }

func (r *watchRecorder[T]) run(resourceVersion string) {
	defer close(r.done)
	defer r.cancel()
	for !r.stopping.Load() {
		count, last, err := r.segment(resourceVersion)
		if last != "" {
			resourceVersion = last
		}
		switch {
		case err != nil:
			r.fail(err)
			return
		case count == 0 && !r.stopping.Load() && !r.quiet:
			r.fail(errors.New("watch segment made no progress while its heartbeat was required"))
			return
		case count == 0 && !r.stopping.Load():
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// segment is one watch request, read to the end the API server gives it. The
// client ends a stream whose connection dropped the way the server ends one at
// its timeout, without an error event, so a dropped connection is resumed
// from the last resourceVersion read like any segment: the decoder never
// yields a partial event, a resourceVersion compacted meanwhile comes back as
// 410 and fails the recorder, and a drop before any event is a segment that
// made no progress.
func (r *watchRecorder[T]) segment(resourceVersion string) (count int, last string, err error) {
	timeout := int64(watchSegmentSeconds)
	stream, err := r.watcher.Watch(r.ctx, r.newList(), &client.ListOptions{
		Namespace: r.namespace,
		Raw: &metav1.ListOptions{
			ResourceVersion: resourceVersion, AllowWatchBookmarks: true, TimeoutSeconds: &timeout,
		},
	})
	if err != nil {
		return 0, "", fmt.Errorf("start the %s watch at resourceVersion %s: %w", r.name, resourceVersion, err)
	}
	defer stream.Stop()
	for event := range stream.ResultChan() {
		if event.Type == watch.Error {
			return count, last, fmt.Errorf("the %s watch segment emitted an error event: %w", r.name, apierrors.FromObject(event.Object))
		}
		object, ok := event.Object.(T)
		if !ok || object.GetResourceVersion() == "" {
			return count, last, fmt.Errorf("the %s watch segment emitted an invalid %T event", r.name, event.Object)
		}
		r.mu.Lock()
		r.events = append(r.events, watchEvent[T]{Type: event.Type, Object: object})
		r.mu.Unlock()
		last = object.GetResourceVersion()
		count++
	}
	return count, last, nil
}

func (r *watchRecorder[T]) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == nil {
		r.failure = err
	}
}

// snapshot is the history recorded so far. The events are never changed
// after they are recorded, so the copy shares them.
func (r *watchRecorder[T]) snapshot() []watchEvent[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *watchRecorder[T]) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// alive is nil while the watch runs and has reported nothing.
func (r *watchRecorder[T]) alive() error {
	r.mu.Lock()
	failure := r.failure
	r.mu.Unlock()
	switch {
	case failure != nil:
		return fmt.Errorf("the %s resourceVersion watch reported an error: %w", r.name, failure)
	case r.exited():
		return fmt.Errorf("the %s resourceVersion watch exited before the assertions completed", r.name)
	}
	return nil
}

// requestStop lets the current segment run to its natural end and starts no
// other.
func (r *watchRecorder[T]) requestStop() { r.stopping.Store(true) }

// await waits for the recorder to end, and is nil when it ended at a segment's
// natural end with nothing reported.
func (r *watchRecorder[T]) await(timeout time.Duration) error {
	// A recorder that already ended is read as ended, whatever time is left:
	// a select on a done channel and an expired timer picks either.
	if !r.exited() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-r.done:
		case <-timer.C:
			return fmt.Errorf("the %s rotating watch did not reach natural segment EOF", r.name)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return fmt.Errorf("the %s rotating watch reported a framing failure: %w", r.name, r.failure)
	}
	return nil
}

// abort ends the watch at once. Only the cleanup does this.
func (r *watchRecorder[T]) abort() {
	r.stopping.Store(true)
	r.cancel()
	<-r.done
}

// history is the recorded history as JSON, for the credential scan, and how
// many events it holds.
func (r *watchRecorder[T]) history() ([]byte, int, error) {
	events := r.snapshot()
	type recorded struct {
		Type   watch.EventType `json:"type"`
		Object T               `json:"object"`
	}
	lines := make([]recorded, 0, len(events))
	for _, event := range events {
		lines = append(lines, recorded(event))
	}
	content, err := json.Marshal(lines)
	return content, len(events), err
}

// startWatches starts the five watches every fault proof reads its history
// from.
func (f *faultRun) startWatches() {
	f.startWatchesWithMode(false)
}

// Quiet watches still resume from the last resourceVersion and refuse every
// watch error. Their caller owes explicit write barriers instead of heartbeat
// writes through a webhook it deliberately removes during Recreate.
func (f *faultRun) startWatchesWithMode(quiet bool) {
	f.t.Helper()
	test, operator := f.in.TestNamespace, f.in.OperatorNamespace
	f.jobs = startRecorder[*batchv1.Job](f, "jobs", test, quiet, func() client.ObjectList { return &batchv1.JobList{} })
	f.pods = startRecorder[*corev1.Pod](f, "pods", test, quiet, func() client.ObjectList { return &corev1.PodList{} })
	f.schemas = startRecorder[*ptahv1alpha1.PtahSchema](f, "schemas", test, quiet,
		func() client.ObjectList { return &ptahv1alpha1.PtahSchemaList{} })
	f.approvals = startRecorder[*ptahv1alpha1.PtahSchemaApproval](f, "approvals", test, quiet,
		func() client.ObjectList { return &ptahv1alpha1.PtahSchemaApprovalList{} })
	f.leases = startRecorder[*coordinationv1.Lease](f, "leases", operator, quiet,
		func() client.ObjectList { return &coordinationv1.LeaseList{} })
}

// assertWatchesAlive ends the scenario when the heartbeat or any watch has
// stopped or reported an error.
func (f *faultRun) assertWatchesAlive() {
	f.t.Helper()
	if f.heartbeat != nil && !f.heartbeatStopped {
		if f.heartbeat.exited() {
			f.fatalf("the Kubernetes watch heartbeat exited before the assertions completed: %v", f.heartbeat.failure())
		}
	}
	for _, r := range f.recorders {
		if err := r.alive(); err != nil {
			f.fatalf("%v", err)
		}
	}
}

// stopWatches ends every watch at its segment's natural end.
func (f *faultRun) stopWatches() {
	f.t.Helper()
	f.assertWatchesAlive()
	for _, r := range f.recorders {
		r.requestStop()
	}
	deadline := time.Now().Add(45 * time.Second)
	for _, r := range f.recorders {
		if err := r.await(time.Until(deadline)); err != nil {
			f.fatalf("%v", err)
		}
	}
}

// establishBarrier annotates one object and waits for the watch to deliver
// the exact resourceVersion the API server returned for that write: every
// event before the write is then in the history a proof reads.
func establishBarrier[T client.Object](f *faultRun, r *watchRecorder[T], object T, namespace, name string) {
	establishBarrierWithPoll(f, r, object, namespace, name, f.poll)
}

// The hung-result row cannot run the credential audit while it waits for a
// watch barrier: that audit would request the same deliberately stalled log.
func establishBarrierWithPoll[T client.Object](f *faultRun, r *watchRecorder[T], object T, namespace, name string, poll func(string, func() bool)) {
	f.t.Helper()
	marker := fmt.Sprintf("fault-%s-%d-%d-%d", r.name, os.Getpid(), f.barrierSequence, time.Now().Unix())
	f.barrierSequence++
	object.SetNamespace(namespace)
	object.SetName(name)
	f.check(f.annotate(f.ctx, object, annotationWatchBarrier, marker), "annotate %s %s for the %s watch barrier", r.name, name, r.name)
	uid, resourceVersion := string(object.GetUID()), object.GetResourceVersion()
	poll(fmt.Sprintf("the %s watch to cross its exact API resourceVersion barrier", r.name), func() bool {
		return slices.ContainsFunc(r.snapshot(), func(event watchEvent[T]) bool {
			return uidIs(event.Object, uid) && event.Object.GetResourceVersion() == resourceVersion &&
				annotationIs(event.Object, annotationWatchBarrier, marker)
		})
	})
}

// jobBarrier is the barrier the Job watch is checkpointed at: a write to the
// fault publisher Job, which no proof counts.
func (f *faultRun) jobBarrier() {
	f.t.Helper()
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, faultHeartbeatJob)
}

// heartbeat writes to one object of every watched kind every
// heartbeatInterval, so each watch segment has something to record.
type heartbeat struct {
	stopping atomic.Bool
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
}

func (h *heartbeat) exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// failure is what the heartbeat ended with, once it ended.
func (h *heartbeat) failure() error {
	if !h.exited() {
		return nil
	}
	return h.err
}

func (h *heartbeat) requestStop() { h.stopping.Store(true) }

// await waits for the heartbeat to end and reports whether it did in time.
func (h *heartbeat) await(timeout time.Duration) bool {
	if h.exited() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-h.done:
		return true
	case <-timer.C:
		h.cancel()
		return false
	}
}

// createHeartbeatLease creates the Lease the heartbeat writes to in the
// release namespace, the one watched namespace the phase owns no object in.
func (f *faultRun) createHeartbeatLease() {
	f.t.Helper()
	lease := &coordinationv1.Lease{}
	lease.Namespace, lease.Name = f.in.OperatorNamespace, watchHeartbeatLease
	lease.Labels = map[string]string{"operator.ptah.run/e2e-purpose": "watch-heartbeat"}
	f.check(f.cluster.Client.Create(f.ctx, lease, client.FieldOwner(harness.FieldOwner)), "create the watch heartbeat Lease")
}

// startHeartbeat starts writing to the fault publisher Job, the PostgreSQL
// database Pod, the suspended schema, the fixture approval and the heartbeat
// Lease. Each round writes through the release's admission webhooks, so one
// round can fail on a webhook call that outran its timeout while the manager
// was under load; that is not evidence that a watch stopped, so a round is
// retried and only heartbeatFailureLimit failed rounds in a row end it.
func (f *faultRun) startHeartbeat() {
	f.t.Helper()
	if f.heartbeatStopped {
		f.fatalf("watch heartbeat cannot be restarted")
	}
	pod := f.databasePod()
	test := f.in.TestNamespace
	targets := []func() client.Object{
		func() client.Object { return objectNamed(&batchv1.Job{}, test, faultHeartbeatJob) },
		func() client.Object { return objectNamed(&corev1.Pod{}, test, pod) },
		func() client.Object { return objectNamed(&ptahv1alpha1.PtahSchema{}, test, heartbeatSchema) },
		func() client.Object { return objectNamed(&ptahv1alpha1.PtahSchemaApproval{}, test, heartbeatApproval) },
		func() client.Object {
			return objectNamed(&coordinationv1.Lease{}, f.in.OperatorNamespace, watchHeartbeatLease)
		},
	}
	for _, target := range targets {
		object := target()
		f.check(f.cluster.Client.Get(f.ctx, client.ObjectKeyFromObject(object), object),
			"read %T %s the heartbeat writes to", object, object.GetName())
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &heartbeat{cancel: cancel, done: make(chan struct{})}
	f.heartbeat = h
	go func() {
		defer close(h.done)
		defer cancel()
		failures := 0
		for sequence := 0; !h.stopping.Load(); {
			marker := fmt.Sprintf("fault-watch-%d-%d-%d", os.Getpid(), sequence, time.Now().Unix())
			errs := make([]error, len(targets))
			var round sync.WaitGroup
			for index, target := range targets {
				round.Go(func() {
					write, done := context.WithTimeout(ctx, 8*time.Second)
					defer done()
					object := target()
					errs[index] = f.annotate(write, object, annotationWatchHeartbeat, marker)
				})
			}
			round.Wait()
			if err := errors.Join(errs...); err != nil {
				failures++
				if failures >= heartbeatFailureLimit {
					h.err = err
					return
				}
				pause(ctx, 2*time.Second)
				continue
			}
			failures = 0
			sequence++
			for second := 0; second < int(heartbeatInterval/time.Second) && !h.stopping.Load(); second++ {
				pause(ctx, time.Second)
			}
		}
	}()
}

// stopHeartbeat ends the heartbeat and holds it to having written every
// round it was asked to.
func (f *faultRun) stopHeartbeat() {
	f.t.Helper()
	if f.heartbeat == nil {
		return
	}
	f.heartbeat.requestStop()
	if !f.heartbeat.await(15 * time.Second) {
		f.fatalf("Kubernetes watch heartbeat did not stop cleanly")
	}
	if err := f.heartbeat.err; err != nil {
		f.fatalf("Kubernetes watch heartbeat reported an update failure: %v", err)
	}
	f.heartbeatStopped = true
}

// named is an object of a kind with a namespace and a name and nothing else.
func objectNamed[T client.Object](object T, namespace, name string) T {
	object.SetNamespace(namespace)
	object.SetName(name)
	return object
}

// pause waits for the duration or until the context ends.
func pause(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// databasePod is the one live PostgreSQL database Pod.
func (f *faultRun) databasePod() string {
	f.t.Helper()
	pods := &corev1.PodList{}
	f.check(f.list(pods, client.MatchingLabels{
		"app.kubernetes.io/name": pgService, "app.kubernetes.io/component": "e2e-database",
	}), "list the PostgreSQL database Pods")
	var live []string
	for index := range pods.Items {
		if pods.Items[index].DeletionTimestamp == nil {
			live = append(live, pods.Items[index].Name)
		}
	}
	if len(live) != 1 {
		f.fatalf("expected one PostgreSQL database Pod, found %d", len(live))
	}
	return live[0]
}

// validateAndScanWatches holds every watch to having recorded something and
// to carrying no credential.
func (f *faultRun) validateAndScanWatches() {
	f.t.Helper()
	for _, r := range f.recorders {
		content, count, err := r.history()
		f.check(err, "encode the %s watch history", r.stem())
		if count == 0 {
			f.fatalf("Kubernetes watch produced no events: %s", r.stem())
		}
		f.scan(content, "fault-test Kubernetes watch history")
	}
}
