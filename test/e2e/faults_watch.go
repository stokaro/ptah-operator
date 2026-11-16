package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
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
// watch cache no longer holds history that old, because it restarted or
// evicted it, refuses the resume, and the recorder replays the gap from exact
// lists before it resumes where the cache's history starts. A segment that
// records nothing while the heartbeat writes to every watched kind is a
// stopped watch, and ends the recorder with a failure. A quiet collection resumes
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
	// replayStarted is when the replay of the current gap started, in Unix
	// nanoseconds, and zero while no gap is replayed.
	replayStarted atomic.Int64
}

// newWatchRecorder is the recorder of one namespace's collection. watcher is
// what it watches and lists with, and newRecorderClient builds the one every
// recorder in the suite uses.
func newWatchRecorder[T client.Object](name, namespace string, quiet bool, watcher client.WithWatch,
	newList func() client.ObjectList,
) *watchRecorder[T] {
	ctx, cancel := context.WithCancel(context.Background())
	return &watchRecorder[T]{
		name: name, namespace: namespace, newList: newList, watcher: watcher, quiet: quiet,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
}

// newRecorderClient is the client a watch recorder runs on: config without
// its client-side rate limit. A replay lists the namespace once for every
// revision of a gap, back to back, and the 50 requests a second the harness
// allows would make a gap of watchGapRevisionLimit revisions take about 20
// seconds, past the bound a replay is given. client-go uses the limiter a
// config carries in place of building one from QPS (rest/config.go).
func newRecorderClient(config *rest.Config, scheme *runtime.Scheme) (client.WithWatch, error) {
	config = rest.CopyConfig(config)
	config.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	return client.NewWithWatch(config, client.Options{Scheme: scheme})
}

func (r *watchRecorder[T]) stem() string { return r.name }

// run records the collection from initial, a list read straight from the
// API server: its objects are what the collection held at its
// resourceVersion, and the watch starts there.
func (r *watchRecorder[T]) run(initial client.ObjectList) {
	defer close(r.done)
	defer r.cancel()
	resourceVersion := initial.GetResourceVersion()
	// live is what the collection holds at resourceVersion: the list the
	// recorder started from with every event recorded since applied to it.
	// Only this goroutine has it.
	live, err := listedObjects[T](initial)
	switch {
	case err != nil:
		r.fail(fmt.Errorf("read the %s list the watch starts from: %w", r.name, err))
		return
	case resourceVersion == "":
		r.fail(fmt.Errorf("the %s list the watch starts from carries no resourceVersion", r.name))
		return
	}
	for !r.stopping.Load() {
		count, last, err := r.segment(resourceVersion, live)
		if last != "" {
			resourceVersion = last
		}
		var expired *expiredHistoryError
		replayed := false
		if errors.As(err, &expired) {
			var state map[types.UID]T
			if state, err = r.replay(resourceVersion, expired.start, live); err == nil {
				resourceVersion, live, replayed = expired.start, state, true
			}
		}
		if err == nil {
			err = r.withheld(count, resourceVersion)
		}
		switch {
		case err != nil:
			r.fail(err)
			return
		case replayed:
			// The gap is in the history, and the next segment starts where
			// the API server's history does. A refused start is not a
			// stopped watch.
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
// request older than that, or older than what a cache that evicted events
// still holds, is refused with a 410, which segment returns as an
// expiredHistoryError for run to replay. A bookmark is recorded like any
// event, and the next segment starts from its resourceVersion. segment
// applies every object event to live.
func (r *watchRecorder[T]) segment(resourceVersion string, live map[types.UID]T) (count int, last string, err error) {
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
			live[object.GetUID()] = object
		case watch.Deleted:
			delete(live, object.GetUID())
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
// from, or, once it has evicted an event since, the revision before its oldest
// one. k8s.io/apiserver writes it in pkg/storage/cacher, watch_cache.go in
// 1.35 and watch_cache_history.go in 1.37.
var watchCacheWindow = regexp.MustCompile(`^too old resource version: ([0-9]+) \(([0-9]+)\)$`)

// historyStart is where the API server's history starts when refusal is its
// watch cache refusing to resume at requested. Any other refusal, or one that
// names another resourceVersion, is not a window the recorder can replay.
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

// watchGap is a stretch of history the watch never delivered and the
// recorder replayed: the resourceVersion the history had reached, the one the
// API server's history started after, how many revisions were listed in
// between, how many events their lists replayed, and how long that took.
type watchGap struct {
	From, To  string
	Revisions int
	Events    int
	Took      time.Duration
}

// describeWatchGap is the log line for a gap the watch named crossed.
func describeWatchGap(name string, gap watchGap) string {
	return fmt.Sprintf("the %s watch replayed %d events from exact lists of the %d revisions between resourceVersion %s, where its history stopped, and %s, where the API server's history starts, in %s",
		name, gap.Events, gap.Revisions, gap.From, gap.To, gap.Took.Round(time.Millisecond))
}

// gaps is every gap the recorder replayed.
func (r *watchRecorder[T]) gaps() []watchGap {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recordedGaps)
}

// replay records the events the watch never delivered between from, the
// resourceVersion the history reached and live describes, and to, the one the
// API server's history starts after, and is what the collection holds at to.
//
// The collection is listed at every revision from from to to, each list read
// exactly at its revision. The list at from has to hold what live does, which
// proves the history the replay continues. Every revision after it is one
// write the API server made to one key, so the list at it differs from the
// one before in at most one object, and that difference is the event the
// watch would have delivered: the listed object, added or modified, which
// the revision wrote and so carries its resourceVersion; or the object the
// previous list held, deleted, carrying the revision as its resourceVersion,
// as k8s.io/apiserver's cacher sends a delete (cache_watcher.go,
// convertToWatchEvent). A revision that changed no object of the collection
// wrote something else. This reads resourceVersions as the etcd revisions
// every API server backed by etcd gives them, which the window refusal itself
// compares as numbers.
func (r *watchRecorder[T]) replay(from, to string, live map[types.UID]T) (map[types.UID]T, error) {
	gap := fmt.Sprintf("the %s watch cannot resume at resourceVersion %s, since the API server's history starts after %s", r.name, from, to)
	low, lowErr := strconv.ParseUint(from, 10, 64)
	high, highErr := strconv.ParseUint(to, 10, 64)
	switch {
	case lowErr != nil || highErr != nil:
		return nil, fmt.Errorf("%s, and the two cannot be read as revisions: %w", gap, errors.Join(lowErr, highErr))
	case high <= low:
		return nil, fmt.Errorf("%s, which leaves no gap to replay", gap)
	case high-low > watchGapRevisionLimit:
		return nil, fmt.Errorf("%s, and the %d revisions in between are more than the %d the recorder lists to replay a gap",
			gap, high-low, watchGapRevisionLimit)
	}
	started := time.Now()
	r.replayStarted.Store(started.UnixNano())
	defer r.replayStarted.Store(0)
	ctx, cancel := context.WithTimeout(r.ctx, watchGapReplayBound)
	defer cancel()
	state := maps.Clone(live)
	var events []watchEvent[T]
	for revision := low; revision <= high; revision++ {
		exact := strconv.FormatUint(revision, 10)
		listed, err := r.listExactly(ctx, exact)
		if err != nil {
			return nil, fmt.Errorf("%s, and %w", gap, err)
		}
		if revision == low {
			if differences := historyDifferences(state, listed); len(differences) > 0 {
				return nil, fmt.Errorf("%s, and at resourceVersion %s, where the history stopped, the collection is not what the history holds: %s",
					gap, exact, strings.Join(differences, "; "))
			}
			continue
		}
		event, changed, err := replayRevision(state, listed, exact)
		if err != nil {
			return nil, fmt.Errorf("%s, and at resourceVersion %s %w", gap, exact, err)
		}
		if !changed {
			continue
		}
		events = append(events, event)
		if event.Type == watch.Deleted {
			delete(state, event.Object.GetUID())
		} else {
			state[event.Object.GetUID()] = event.Object
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	r.recordedGaps = append(r.recordedGaps, watchGap{
		From: from, To: to, Revisions: int(high - low), Events: len(events), Took: time.Since(started),
	})
	return state, nil
}

// listExactly is the collection at revision, read exactly there.
func (r *watchRecorder[T]) listExactly(ctx context.Context, revision string) (map[types.UID]T, error) {
	ctx, cancel := context.WithTimeout(ctx, watchGapListTimeout)
	defer cancel()
	list := r.newList()
	if err := r.watcher.List(ctx, list, &client.ListOptions{Namespace: r.namespace, Raw: &metav1.ListOptions{
		ResourceVersion: revision, ResourceVersionMatch: metav1.ResourceVersionMatchExact,
	}}); err != nil {
		return nil, fmt.Errorf("the %s collection could not be listed at resourceVersion %s: %w", r.name, revision, err)
	}
	if answered := list.GetResourceVersion(); answered != revision {
		return nil, fmt.Errorf("the list asked for at resourceVersion %s answered at %q", revision, answered)
	}
	listed, err := listedObjects[T](list)
	if err != nil {
		return nil, fmt.Errorf("the %s list at resourceVersion %s cannot be read: %w", r.name, revision, err)
	}
	return listed, nil
}

// replayRevision is the event revision wrote, from held, what the collection
// held before it, and listed, what it holds at it, and whether it wrote one.
// A revision writes one key, so a second change, or an added or modified
// object at another resourceVersion, is a list that is not this revision's.
func replayRevision[T client.Object](held, listed map[types.UID]T, revision string) (watchEvent[T], bool, error) {
	var changes []watchEvent[T]
	for uid, object := range listed {
		previous, ok := held[uid]
		switch {
		case !ok:
			changes = append(changes, watchEvent[T]{Type: watch.Added, Object: object})
		case previous.GetResourceVersion() != object.GetResourceVersion():
			changes = append(changes, watchEvent[T]{Type: watch.Modified, Object: object})
		}
	}
	for uid, previous := range held {
		if _, ok := listed[uid]; !ok {
			deleted, ok := previous.DeepCopyObject().(T)
			if !ok {
				return watchEvent[T]{}, false, fmt.Errorf("the copy of %s is a %T", describeObject(previous), previous.DeepCopyObject())
			}
			deleted.SetResourceVersion(revision)
			changes = append(changes, watchEvent[T]{Type: watch.Deleted, Object: deleted})
		}
	}
	switch len(changes) {
	case 0:
		return watchEvent[T]{}, false, nil
	case 1:
	default:
		described := make([]string, 0, len(changes))
		for _, change := range changes {
			described = append(described, fmt.Sprintf("%s %s", change.Type, describeObject(change.Object)))
		}
		slices.Sort(described)
		return watchEvent[T]{}, false, fmt.Errorf("the collection changed %d objects, which one revision cannot write: %s",
			len(changes), strings.Join(described, "; "))
	}
	change := changes[0]
	if change.Type != watch.Deleted && change.Object.GetResourceVersion() != revision {
		return watchEvent[T]{}, false, fmt.Errorf("the collection holds %s, %s at another revision than the one that wrote it",
			describeObject(change.Object), strings.ToLower(string(change.Type)))
	}
	return change, true, nil
}

// describeObject names an object of the collection and the resourceVersion it
// is at.
func describeObject(object client.Object) string {
	return fmt.Sprintf("%s/%s (uid %s) at resourceVersion %s",
		object.GetNamespace(), object.GetName(), object.GetUID(), object.GetResourceVersion())
}

// listedObjects is every object list holds, by UID.
func listedObjects[T client.Object](list client.ObjectList) (map[types.UID]T, error) {
	objects := map[types.UID]T{}
	err := meta.EachListItem(list, func(item runtime.Object) error {
		object, ok := item.(T)
		if !ok {
			return fmt.Errorf("the list holds a %T, which is not the kind watched", item)
		}
		objects[object.GetUID()] = object
		return nil
	})
	return objects, err
}

// historyDifferences is every way listed is not what the history recorded:
// an object it never recorded, one at another resourceVersion, and one it
// recorded that is gone.
func historyDifferences[T client.Object](recorded, listed map[types.UID]T) []string {
	var differences []string
	for uid, object := range listed {
		was, ok := recorded[uid]
		switch {
		case !ok:
			differences = append(differences, describeObject(object)+" was never recorded")
		case was.GetResourceVersion() != object.GetResourceVersion():
			differences = append(differences, fmt.Sprintf("%s, recorded at %s", describeObject(object), was.GetResourceVersion()))
		}
	}
	for uid, was := range recorded {
		if _, ok := listed[uid]; !ok {
			differences = append(differences, describeObject(was)+", recorded there, is gone")
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
	if started := r.replayStarted.Load(); started != 0 {
		if running := time.Since(time.Unix(0, started)); running > watchGapReplayOverrun {
			return fmt.Errorf("the %s watch has replayed a gap from exact lists for %s, past the %s a replay is bounded to",
				r.name, running.Round(time.Second), watchGapReplayBound)
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
