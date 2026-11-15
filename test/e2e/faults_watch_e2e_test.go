//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// startRecorder reads the collection straight from the API server, which
// answers with the typed list and the resourceVersion the watch continues
// from, and starts the watch there. Starting from an empty resourceVersion
// would replay every existing object as ADDED, as if the phase had created
// it. The Jobs listed enter the observed ledger, since the list and the watch
// after it are one boundary with no gap, and every object listed is part of
// the history a gap is held to.
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
	r, err := newWatchRecorder[T](name, namespace, quiet, f.watcher, newList, list)
	f.check(err, "start the %s watch", name)
	go r.run(resourceVersion)
	f.recorders = append(f.recorders, r)
	return r
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
	f.assertHeartbeatAlive()
	for _, r := range f.recorders {
		if err := r.alive(); err != nil {
			f.watchFailed(err)
		}
	}
}

// assertWatchAlive is assertWatchesAlive for the one watch a wait reads.
func (f *faultRun) assertWatchAlive(r recorder) {
	f.t.Helper()
	f.assertHeartbeatAlive()
	if err := r.alive(); err != nil {
		f.watchFailed(err)
	}
}

// watchFailed ends the scenario with a watch's failure, and first logs every
// API server's watch caches when the watch was withheld writes.
func (f *faultRun) watchFailed(err error) {
	f.t.Helper()
	var withheld *withheldWriteError
	if errors.As(err, &withheld) {
		f.logWatchCaches()
	}
	f.fatalf("%v", err)
}

// watchCacheResources are the collections the fault watches read, as the API
// server's watch cache metrics label them.
var watchCacheResources = []string{
	`group="batch",resource="jobs"`, `group="",resource="pods"`, `group="coordination.k8s.io",resource="leases"`,
	`group="operator.ptah.run",resource="ptahschemas"`, `group="operator.ptah.run",resource="ptahschemaapprovals"`,
}

// logWatchCaches logs the resourceVersion each API server's watch cache has
// reached for every watched collection. Each API server keeps its own cache,
// and a watch is served by the one its connection reached, so the one whose
// cache stopped reads behind the others.
func (f *faultRun) logWatchCaches() {
	f.t.Helper()
	pods := &corev1.PodList{}
	if err := f.cluster.Client.List(f.ctx, pods, client.InNamespace(metav1.NamespaceSystem),
		client.MatchingLabels{"component": "kube-apiserver"}); err != nil {
		f.logf("list the API server Pods to read their watch caches: %v", err)
		return
	}
	if len(pods.Items) == 0 {
		f.logf("found no API server Pod to read the watch caches of")
	}
	for _, pod := range pods.Items {
		metrics, err := f.cluster.Raw(f.ctx, "/api/v1/namespaces/kube-system/pods/https:"+pod.Name+":6443/proxy/metrics")
		if err != nil {
			f.logf("read the watch caches of %s: %v", pod.Name, err)
			continue
		}
		for line := range strings.SplitSeq(string(metrics), "\n") {
			if rest, ok := strings.CutPrefix(line, "apiserver_watch_cache_resource_version{"); ok &&
				slices.ContainsFunc(watchCacheResources, func(labels string) bool { return strings.HasPrefix(rest, labels+"}") }) {
				f.logf("%s %s", pod.Name, line)
			}
		}
	}
}

func (f *faultRun) assertHeartbeatAlive() {
	f.t.Helper()
	if f.heartbeat != nil && !f.heartbeatStopped && f.heartbeat.exited() {
		f.fatalf("the Kubernetes watch heartbeat exited before the assertions completed: %v", f.heartbeat.failure())
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
	poll(fmt.Sprintf("the %s watch to cross its exact API resourceVersion barrier at %s", r.name, resourceVersion), func() bool {
		crossed := slices.ContainsFunc(r.snapshot(), func(event watchEvent[T]) bool {
			return uidIs(event.Object, uid) && event.Object.GetResourceVersion() == resourceVersion &&
				annotationIs(event.Object, annotationWatchBarrier, marker)
		})
		// A watch that stopped can never cross the barrier, so the wait ends
		// with why it stopped rather than at its bound.
		if !crossed {
			f.assertWatchAlive(r)
		}
		return crossed
	})
}

// jobBarrier is the barrier the Job watch is checkpointed at: a write to the
// fault publisher Job, which no proof counts.
func (f *faultRun) jobBarrier() {
	f.t.Helper()
	establishBarrier(f, f.jobs, &batchv1.Job{}, f.in.TestNamespace, f.heartbeatJobName())
}

// heartbeatTarget is one object the heartbeat writes to, named by the watch
// that owes the heartbeat the event of every write.
type heartbeatTarget struct {
	watch  string
	object func() client.Object
}

// heartbeatFollower is a watch the heartbeat can hold to its writes.
type heartbeatFollower interface {
	stem() string
	follow(*heartbeat)
}

// createHeartbeatLease creates the Lease the heartbeat writes to in the
// release namespace, the one watched namespace the phase owns no object in.
func (f *faultRun) createHeartbeatLease() {
	f.t.Helper()
	lease := &coordinationv1.Lease{}
	lease.Namespace, lease.Name = f.in.OperatorNamespace, f.heartbeatLeaseName()
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
	targets := []heartbeatTarget{
		{"jobs", func() client.Object { return objectNamed(&batchv1.Job{}, test, f.heartbeatJobName()) }},
		{"pods", func() client.Object { return objectNamed(&corev1.Pod{}, test, pod) }},
		{"schemas", func() client.Object { return objectNamed(&ptahv1alpha1.PtahSchema{}, test, heartbeatSchema) }},
		{"approvals", func() client.Object {
			return objectNamed(&ptahv1alpha1.PtahSchemaApproval{}, test, heartbeatApproval)
		}},
		{"leases", func() client.Object {
			return objectNamed(&coordinationv1.Lease{}, f.in.OperatorNamespace, f.heartbeatLeaseName())
		}},
	}
	for _, target := range targets {
		object := target.object()
		f.check(f.cluster.Client.Get(f.ctx, client.ObjectKeyFromObject(object), object),
			"read %T %s the heartbeat writes to", object, object.GetName())
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &heartbeat{cancel: cancel, done: make(chan struct{}), acks: map[string][]watchAck{}}
	f.heartbeat = h
	for _, target := range targets {
		followed := false
		for _, r := range f.recorders {
			if follower, ok := r.(heartbeatFollower); ok && follower.stem() == target.watch {
				follower.follow(h)
				followed = true
			}
		}
		if !followed {
			f.fatalf("the heartbeat writes to the %s collection and no watch of it follows the heartbeat", target.watch)
		}
	}
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
					object := target.object()
					if errs[index] = f.annotate(write, object, annotationWatchHeartbeat, marker); errs[index] == nil {
						h.record(target.watch, object, time.Now())
					}
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
// to carrying no credential, and logs every gap a watch resumed across.
func (f *faultRun) validateAndScanWatches() {
	f.t.Helper()
	for _, r := range f.recorders {
		for _, gap := range r.gaps() {
			f.logf("the %s watch resumed at resourceVersion %s after an API server's watch cache dropped its history from %s; the %d revisions between held the %d objects the history records, unchanged",
				r.stem(), gap.To, gap.From, gap.Revisions, gap.Objects)
		}
		content, count, err := r.history()
		f.check(err, "encode the %s watch history", r.stem())
		if count == 0 {
			f.fatalf("Kubernetes watch produced no events: %s", r.stem())
		}
		f.scan(content, "fault-test Kubernetes watch history")
	}
}
