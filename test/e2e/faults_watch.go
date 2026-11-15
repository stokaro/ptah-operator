package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// heartbeat is the heartbeat writing to this collection, once one runs.
	// Every write it had acknowledged is owed by the watch within
	// watchDeliveryBound.
	heartbeat atomic.Pointer[heartbeat]
	// segmentStarted is when the current watch request was sent, in Unix
	// nanoseconds, and zero between requests.
	segmentStarted atomic.Int64
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
		if err == nil {
			err = r.withheld(count, resourceVersion)
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
