package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recorder is what the fault run does with every watch alike.
type recorder interface {
	stem() string
	alive() error
	requestStop()
	await(timeout time.Duration) error
	abort()
	history() ([]byte, int, error)
	gaps() []watchGap
}

// watchRecorder keeps one collection's resourceVersion watch for the whole
// fault injection. The API server ends each watch request after
// watchSegmentSeconds, and the recorder continues from the last
// resourceVersion it read, so the history has no gap. An API server whose
// watch cache restarted no longer holds history that old, and the recorder
// resumes where the cache's history starts only once bridge has proven that
// nothing in the collection changed in between. A segment that records
// nothing while the heartbeat writes to every watched kind is a stopped
// watch, and ends the recorder with a failure. A quiet collection resumes
// from the same version and owes explicit barriers. Every other error event,
// and every event that is not an object of the kind watched, fails either
// mode.
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

	// live is what the collection holds at the resourceVersion the recorder
	// resumes from: the list the recorder started from with every event
	// recorded since applied to it. Only the goroutine running the watch
	// reads or writes it.
	live map[types.UID]recordedObject

	mu           sync.Mutex
	events       []watchEvent[T]
	recordedGaps []watchGap
	failure      error
	stopping     atomic.Bool
	done         chan struct{}

	// heartbeat is the heartbeat writing to this collection, once one runs.
	// Every write it had acknowledged is owed by the watch within
	// watchDeliveryBound.
	heartbeat atomic.Pointer[heartbeat]
	// segmentStarted is when the current watch request was sent, in Unix
	// nanoseconds, and zero between requests.
	segmentStarted atomic.Int64
}

// newWatchRecorder is the recorder of a collection that initial, read straight
// from the API server, lists. The objects it lists are where the history
// starts, so a gap bridge proves empty is held to them as well as to every
// event recorded after them.
func newWatchRecorder[T client.Object](name, namespace string, quiet bool, watcher client.WithWatch,
	newList func() client.ObjectList, initial client.ObjectList,
) (*watchRecorder[T], error) {
	live, err := listedObjects(initial)
	if err != nil {
		return nil, fmt.Errorf("read the %s list the watch starts from: %w", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &watchRecorder[T]{
		name: name, namespace: namespace, newList: newList, watcher: watcher, quiet: quiet, live: live,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}, nil
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
		var expired *expiredHistoryError
		bridged := false
		if errors.As(err, &expired) {
			if err = r.bridge(resourceVersion, expired.start); err == nil {
				resourceVersion, bridged = expired.start, true
			}
		}
		if err == nil {
			err = r.withheld(count, resourceVersion)
		}
		switch {
		case err != nil:
			r.fail(err)
			return
		case bridged:
			// The gap is proven empty, and the next segment starts where the
			// API server's history does. A refused start is not a stopped
			// watch.
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
// yields a partial event, and a drop before any event is a segment that made
// no progress. A watch cache that restarts ends every watch it serves the
// same way, and its history then starts at the list it restarted from: a
// request older than that is refused with a 410, which segment returns as an
// expiredHistoryError for run to bridge. A bookmark is recorded like any
// event, and the next segment starts from its resourceVersion.
func (r *watchRecorder[T]) segment(resourceVersion string) (count int, last string, err error) {
	r.segmentStarted.Store(time.Now().UnixNano())
	defer r.segmentStarted.Store(0)
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
			refusal := apierrors.FromObject(event.Object)
			err := fmt.Errorf("the %s watch segment emitted an error event: %w", r.name, refusal)
			if start, ok := historyStart(refusal, resourceVersion); ok {
				return count, last, &expiredHistoryError{start: start, err: err}
			}
			return count, last, err
		}
		object, ok := event.Object.(T)
		if !ok || object.GetResourceVersion() == "" {
			return count, last, fmt.Errorf("the %s watch segment emitted an invalid %T event", r.name, event.Object)
		}
		r.mu.Lock()
		r.events = append(r.events, watchEvent[T]{Type: event.Type, Object: object})
		r.mu.Unlock()
		switch event.Type {
		case watch.Added, watch.Modified:
			r.live[object.GetUID()] = recordedObjectOf(object)
		case watch.Deleted:
			delete(r.live, object.GetUID())
		}
		last = object.GetResourceVersion()
		count++
	}
	return count, last, nil
}

// withheld is an error when a segment ended without a write the heartbeat
// had acknowledged more than watchDeliveryBound earlier. The API server
// serving this watch then stopped delivering events while the cluster went
// on accepting writes, and the history can never cross a barrier written
// after them.
func (r *watchRecorder[T]) withheld(count int, resourceVersion string) error {
	h := r.heartbeat.Load()
	if h == nil {
		return nil
	}
	owed, withheld := withheldAck(r.snapshot(), h.acknowledged(r.name), time.Now(), watchDeliveryBound)
	if !withheld {
		return nil
	}
	return &withheldWriteError{fmt.Sprintf("the API server serving the %s watch has not delivered the heartbeat write it acknowledged %s ago at resourceVersion %s; the segment that ended delivered %d events and left the watch at resourceVersion %s",
		r.name, time.Since(owed.At).Round(time.Second), owed.ResourceVersion, count, resourceVersion)}
}

// withheldWriteError is a watch that stopped delivering writes the cluster
// went on acknowledging. What it does not say is which API server stopped,
// and logWatchCaches is what reads that.
type withheldWriteError struct{ message string }

func (e *withheldWriteError) Error() string { return e.message }

// watchCacheWindow is the API server's answer to a watch that asks for
// history its watch cache does not hold: the resourceVersion asked for, and
// the one the cache's history starts after: the list the cache last restarted
// from, or, once it has dropped an event since, the revision before its oldest
// one. k8s.io/apiserver writes it in pkg/storage/cacher, watch_cache.go in
// 1.35 and watch_cache_history.go in 1.37.
var watchCacheWindow = regexp.MustCompile(`^too old resource version: ([0-9]+) \(([0-9]+)\)$`)

// historyStart is where the API server's history starts when refusal is its
// watch cache refusing to resume at requested. Any other refusal, or one that
// names another resourceVersion, is not a window the recorder can bridge.
func historyStart(refusal error, requested string) (string, bool) {
	var status apierrors.APIStatus
	if !apierrors.IsResourceExpired(refusal) || !errors.As(refusal, &status) {
		return "", false
	}
	match := watchCacheWindow.FindStringSubmatch(status.Status().Message)
	if match == nil || match[1] != requested {
		return "", false
	}
	return match[2], true
}

// expiredHistoryError is a watch the API server refused to start, because its
// watch cache's history starts after the resourceVersion asked for.
type expiredHistoryError struct {
	start string
	err   error
}

func (e *expiredHistoryError) Error() string { return e.err.Error() }

func (e *expiredHistoryError) Unwrap() error { return e.err }

// watchGap is a stretch of history the watch never delivered, proven to hold
// no event for the collection: the resourceVersion the recorder had reached,
// the one the API server's history started after, how many revisions were
// listed in between, and how many objects each of them held.
type watchGap struct {
	From, To  string
	Revisions int
	Objects   int
}

// gaps is every gap the recorder resumed across.
func (r *watchRecorder[T]) gaps() []watchGap {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recordedGaps)
}

// bridge proves that no object in the collection changed between from, the
// resourceVersion the history reached, and to, the one the API server's
// history starts after, and records the gap. A list at the end alone would
// miss an object created and deleted in between, so the collection is listed
// at every revision of the gap, each read exactly at that revision, and every
// list has to hold exactly the objects the history holds at the
// resourceVersions it recorded: one created, changed or deleted in between
// shows in the list of the revision that wrote it. This reads
// resourceVersions as the etcd revisions every API server backed by etcd
// gives them, which the window refusal itself compares as numbers.
func (r *watchRecorder[T]) bridge(from, to string) error {
	gap := fmt.Sprintf("the %s watch cannot resume at resourceVersion %s, since the API server's history starts after %s", r.name, from, to)
	low, lowErr := strconv.ParseUint(from, 10, 64)
	high, highErr := strconv.ParseUint(to, 10, 64)
	switch {
	case lowErr != nil || highErr != nil:
		return fmt.Errorf("%s, and the two cannot be read as revisions: %w", gap, errors.Join(lowErr, highErr))
	case high <= low:
		return fmt.Errorf("%s, which leaves no gap to bridge", gap)
	case high-low > watchGapRevisionLimit:
		return fmt.Errorf("%s, and the %d revisions in between are more than the %d the recorder lists to prove a gap empty",
			gap, high-low, watchGapRevisionLimit)
	}
	for revision := low + 1; revision <= high; revision++ {
		exact := strconv.FormatUint(revision, 10)
		list := r.newList()
		if err := r.watcher.List(r.ctx, list, &client.ListOptions{Namespace: r.namespace, Raw: &metav1.ListOptions{
			ResourceVersion: exact, ResourceVersionMatch: metav1.ResourceVersionMatchExact,
		}}); err != nil {
			return fmt.Errorf("%s, and the %s collection could not be listed at resourceVersion %s: %w", gap, r.name, exact, err)
		}
		if answered := list.GetResourceVersion(); answered != exact {
			return fmt.Errorf("%s, and the list asked for at resourceVersion %s answered at %q", gap, exact, answered)
		}
		listed, err := listedObjects(list)
		if err != nil {
			return fmt.Errorf("%s, and the %s list at resourceVersion %s cannot be read: %w", gap, r.name, exact, err)
		}
		if differences := historyDifferences(r.live, listed); len(differences) > 0 {
			return fmt.Errorf("%s, and at resourceVersion %s the collection holds what the watch never delivered: %s",
				gap, exact, strings.Join(differences, "; "))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordedGaps = append(r.recordedGaps, watchGap{From: from, To: to, Revisions: int(high - low), Objects: len(r.live)})
	return nil
}

// recordedObject is one object of the collection as a list or the history
// holds it.
type recordedObject struct {
	namespace, name, resourceVersion string
}

func recordedObjectOf(object metav1.Object) recordedObject {
	return recordedObject{namespace: object.GetNamespace(), name: object.GetName(), resourceVersion: object.GetResourceVersion()}
}

func (o recordedObject) String() string { return o.namespace + "/" + o.name }

// listedObjects is every object list holds, by UID.
func listedObjects(list client.ObjectList) (map[types.UID]recordedObject, error) {
	objects := map[types.UID]recordedObject{}
	err := meta.EachListItem(list, func(item runtime.Object) error {
		object, err := meta.Accessor(item)
		if err != nil {
			return err
		}
		objects[object.GetUID()] = recordedObjectOf(object)
		return nil
	})
	return objects, err
}

// historyDifferences is every way listed is not what the history recorded:
// an object it never recorded, one at another resourceVersion, and one it
// recorded that is gone.
func historyDifferences(recorded, listed map[types.UID]recordedObject) []string {
	var differences []string
	for uid, object := range listed {
		was, ok := recorded[uid]
		switch {
		case !ok:
			differences = append(differences, fmt.Sprintf("%s (uid %s) at resourceVersion %s was never recorded", object, uid, object.resourceVersion))
		case was.resourceVersion != object.resourceVersion:
			differences = append(differences, fmt.Sprintf("%s (uid %s) is at resourceVersion %s, recorded at %s",
				object, uid, object.resourceVersion, was.resourceVersion))
		}
	}
	for uid, was := range recorded {
		if _, ok := listed[uid]; !ok {
			differences = append(differences, fmt.Sprintf("%s (uid %s), recorded at resourceVersion %s, is gone", was, uid, was.resourceVersion))
		}
	}
	slices.Sort(differences)
	return differences
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
	if started := r.segmentStarted.Load(); started != 0 {
		if running := time.Since(time.Unix(0, started)); running > watchSegmentOverrun {
			return fmt.Errorf("the %s watch request has run %s, past the %ds the API server was asked to end it at",
				r.name, running.Round(time.Second), watchSegmentSeconds)
		}
	}
	return nil
}

// follow holds the watch to the writes the heartbeat has acknowledged.
func (r *watchRecorder[T]) follow(h *heartbeat) { r.heartbeat.Store(h) }

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

// heartbeat writes to one object of every watched kind every
// heartbeatInterval, so each watch segment has something to record.
type heartbeat struct {
	stopping atomic.Bool
	cancel   context.CancelFunc
	done     chan struct{}
	err      error

	mu sync.Mutex
	// acks is the newest writes the API server acknowledged, by the name of
	// the watch that owes them, oldest first and at most
	// heartbeatAckRetention of them.
	acks map[string][]watchAck
}

// record keeps the write the API server returned object from.
func (h *heartbeat) record(watch string, object client.Object, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	acks := append(h.acks[watch], watchAck{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(), At: at})
	if len(acks) > heartbeatAckRetention {
		acks = slices.Clone(acks[len(acks)-heartbeatAckRetention:])
	}
	h.acks[watch] = acks
}

// acknowledged is the writes the watch named owes, oldest first.
func (h *heartbeat) acknowledged(watch string) []watchAck {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.acks[watch])
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
