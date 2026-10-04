package resultconsumer

import (
	"context"
	"errors"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"sync/atomic"
	"testing"
	"time"
)

type loadFunc func(context.Context, Request) (Result, error)

func (f loadFunc) Load(ctx context.Context, r Request) (Result, error) { return f(ctx, r) }
func request() Request {
	return Request{Namespace: "tenant", UID: "resource-uid", Generation: 1, JobName: "attempt", JobUID: "job-uid", OperationID: "op-1"}
}
func startReader(t *testing.T, loader Loader, options Options) *Reader {
	t.Helper()
	r, err := New(loader, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("reader did not stop")
		}
	})
	until(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.ctx != nil })
	return r
}
func until(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func take(t *testing.T, r *Reader, key Request) (Result, error) {
	t.Helper()
	var value Result
	var err error
	until(t, func() bool { value, err = r.Poll(t.Context(), key); return !errors.Is(err, ErrPending) })
	return value, err
}

func TestPollDoesNotWaitAndDeduplicates(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	loader := loadFunc(func(ctx context.Context, _ Request) (Result, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	})
	r := startReader(t, loader, Options{Workers: 1, Entries: 2, Timeout: time.Second, Retention: time.Second})
	for range 20 {
		if _, err := r.Poll(t.Context(), request()); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("load not started")
	}
	other := request()
	other.OperationID = "op-2"
	for range 20 {
		if _, err := r.Poll(t.Context(), other); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("poll duplicated work or exceeded worker limit")
	}
	close(release)
	if _, err := take(t, r, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := take(t, r, other); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("independent work did not progress: %d", calls.Load())
	}
}

func TestLoadTimeoutCancellationAndRetry(t *testing.T) {
	var calls atomic.Int32
	r := startReader(t, loadFunc(func(ctx context.Context, _ Request) (Result, error) {
		calls.Add(1)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}), Options{Workers: 1, Entries: 1, Timeout: 20 * time.Millisecond, Retention: time.Second})
	for range 2 {
		if _, err := take(t, r, request()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded or misreported load: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("failed load could not be retried")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Poll(canceled, request()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("canceled poll scheduled a load")
	}
}

func TestCompletedEntryBoundAndExpiration(t *testing.T) {
	var calls atomic.Int32
	r := startReader(t, loadFunc(func(context.Context, Request) (Result, error) { calls.Add(1); return Result{}, nil }), Options{Workers: 1, Entries: 1, Timeout: time.Second, Retention: time.Second})
	now := time.Now()
	r.mu.Lock()
	r.now = func() time.Time { return now }
	r.mu.Unlock()
	if _, err := r.Poll(t.Context(), request()); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	until(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.entries[request()].done })
	other := request()
	other.OperationID = "other"
	if _, err := r.Poll(t.Context(), other); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("ready entry limit was exceeded")
	}
	r.mu.Lock()
	now = now.Add(2 * time.Second)
	r.mu.Unlock()
	if _, err := take(t, r, other); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("expired memory prevented independent progress")
	}
}

func TestReaderRequiresLifecycleAndDropsFailedValues(t *testing.T) {
	failure := errors.New("storage unavailable")
	options := Options{Workers: 1, Entries: 1, Timeout: time.Second, Retention: time.Second}
	loader := loadFunc(func(context.Context, Request) (Result, error) {
		return Result{Binding: resultstore.Binding{UID: "partial"}}, failure
	})
	r, err := New(loader, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Poll(t.Context(), request()); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	active := startReader(t, loader, options)
	value, err := take(t, active, request())
	if !errors.Is(err, failure) || value.Binding.UID != "" {
		t.Fatalf("failed load leaked partial result: %v", err)
	}
}
