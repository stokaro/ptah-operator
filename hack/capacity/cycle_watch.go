package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

const maxCycleReadings = 200000
const maxCycleWatchRetries = 3

type cycleRecorder struct {
	mu      sync.Mutex
	history cycleHistory
	client  dynamic.ResourceInterface
	seen    map[string]bool
	ready   chan error
	done    chan struct{}
}

func newCycleRecorder(client dynamic.ResourceInterface, family, namespace, selector string) *cycleRecorder {
	return &cycleRecorder{client: client, history: cycleHistory{Family: family, Namespace: namespace, Selector: selector, StartedAt: time.Now().UTC()}, seen: map[string]bool{}, ready: make(chan error, 1), done: make(chan struct{})}
}

func (r *cycleRecorder) snapshot() cycleHistory {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.history
	h.Readings = append([]cycleReading(nil), h.Readings...)
	h.Retries = append([]cycleWatchRetry(nil), h.Retries...)
	return h
}

func (r *cycleRecorder) cursor() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.history.Cursor
}

func (r *cycleRecorder) run(ctx context.Context) {
	defer close(r.done)
	err := r.collect(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history.EndedAt = time.Now().UTC()
	if err != nil {
		r.history.Error = err.Error()
	}
}

func (r *cycleRecorder) collect(ctx context.Context) error {
	h := r.snapshot()
	initialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	initial, err := r.client.List(initialCtx, metav1.ListOptions{LabelSelector: h.Selector})
	cancel()
	if err == nil && initial.GetResourceVersion() == "" {
		err = errors.New("cycle list has no resource version")
	}
	if err != nil {
		r.ready <- err
		return err
	}
	for i := range initial.Items {
		if err := r.record("INITIAL", &initial.Items[i]); err != nil {
			r.ready <- err
			return err
		}
	}
	r.mu.Lock()
	r.history.Cursor = initial.GetResourceVersion()
	r.mu.Unlock()
	// The cursor closes the list/watch boundary even if creation starts before
	// the HTTP watch upgrade completes. A lost cursor ends collection.
	r.ready <- nil
	var retryErr error
	retries := 0
	for ctx.Err() == nil {
		segment, cancel := context.WithTimeout(ctx, 45*time.Second)
		seconds := int64(30)
		stream, err := r.client.Watch(segment, metav1.ListOptions{LabelSelector: h.Selector, ResourceVersion: r.cursor(), AllowWatchBookmarks: true, TimeoutSeconds: &seconds})
		if err == nil {
			retryErr = nil
			err = r.segment(segment, stream)
			stream.Stop()
			// A connected watch's normal segment timeout is not a failed
			// connection attempt. The next segment resumes its cursor.
			if errors.Is(err, context.DeadlineExceeded) {
				err = nil
			}
		}
		cancel()
		if ctx.Err() != nil {
			return retryErr
		}
		if err != nil {
			retryErr = fmt.Errorf("cycle watch cannot establish continuous history: %w", err)
			if !retryableCycleWatchError(err) || retries == maxCycleWatchRetries {
				return retryErr
			}
			retries++
			r.mu.Lock()
			r.history.Retries = append(r.history.Retries, cycleWatchRetry{At: time.Now().UTC(), Cursor: r.history.Cursor, Error: err.Error()})
			r.mu.Unlock()
		} else {
			retries = 0
			retryErr = nil
		}
		select {
		case <-ctx.Done():
			return retryErr
		case <-time.After(100 * time.Millisecond):
		}
	}
	return retryErr
}

func retryableCycleWatchError(err error) bool {
	// Never relist over an expired cursor, an authorization refusal, or an
	// invalid event. A transport retry requests exactly the last recorded RV.
	return utilnet.IsProbableEOF(err) || utilnet.IsHTTP2ConnectionLost(err) ||
		utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) || utilnet.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err)
}

func (r *cycleRecorder) segment(ctx context.Context, stream watch.Interface) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-stream.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Error {
				return apierrors.FromObject(event.Object)
			}
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok || object.GetResourceVersion() == "" {
				return errors.New("cycle watch event has no object cursor")
			}
			if event.Type != watch.Bookmark {
				if event.Type != watch.Added && event.Type != watch.Modified && event.Type != watch.Deleted {
					return errors.New("unexpected cycle watch event")
				}
				if err := r.record(string(event.Type), object); err != nil {
					return err
				}
			}
			r.mu.Lock()
			r.history.Cursor = object.GetResourceVersion()
			r.mu.Unlock()
		}
	}
}

func (r *cycleRecorder) record(event string, object *unstructured.Unstructured) error {
	r.mu.Lock()
	family, namespace := r.history.Family, r.history.Namespace
	r.mu.Unlock()
	if object.GetNamespace() != namespace {
		return errors.New("cycle watch crossed its namespace")
	}
	reading, err := readCycle(family, event, object, time.Now().UTC())
	if err != nil {
		return err
	}
	key := event + "/" + reading.UID + "/" + reading.ResourceVersion
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[key] {
		return nil
	}
	if len(r.history.Readings) >= maxCycleReadings {
		return errors.New("cycle history limit reached; refusing to drop events")
	}
	r.seen[key] = true
	r.history.Readings = append(r.history.Readings, reading)
	return nil
}

func collectCycleEvidence(recorders []*cycleRecorder) cycleEvidence {
	var out cycleEvidence
	for _, recorder := range recorders {
		h := recorder.snapshot()
		out.Histories = append(out.Histories, h)
		completed, lives := completedCycles(h)
		out.Completed = append(out.Completed, completed...)
		out.Lifetimes = append(out.Lifetimes, lives...)
	}
	return out
}
