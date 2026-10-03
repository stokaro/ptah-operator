// Package resultconsumer loads durable evidence outside reconciliation workers.
// Persistence does not authorize SQL or replace the consumer's live epoch checks.
package resultconsumer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"k8s.io/apimachinery/pkg/types"
)

var (
	ErrPending = errors.New("durable result loading is pending")
	ErrStopped = errors.New("durable result reader is not running")
	ErrBinding = errors.New("durable result does not match the operation claim")
)

// Request is the persisted claim and engine expected by a consumer. Pod identity
// is deliberately absent: it comes from the authenticated publication, which
// remains readable after the producing Pod is removed.
type Request struct {
	Namespace, Kind, Name                                                 string
	UID                                                                   types.UID
	Generation                                                            int64
	ExecutionBindingID, InputFingerprint, Operation, OperationID, JobName string
	JobUID                                                                types.UID
	Engine                                                                string
}

func (r Request) Matches(b resultstore.Binding) bool {
	return r.Namespace == b.Namespace && r.Kind == b.Kind && r.Name == b.Name && r.UID == b.UID && r.Generation == b.Generation && r.ExecutionBindingID == b.ExecutionBindingID && r.InputFingerprint == b.InputFingerprint && r.Operation == b.Operation && r.OperationID == b.OperationID && r.JobName == b.JobName && r.JobUID == b.JobUID
}

type Result struct {
	Binding resultstore.Binding
	Receipt resultstore.Receipt
	Value   runner.Result
}

type Loader interface {
	Load(context.Context, Request) (Result, error)
}
type StoreLoader struct{ Store resultstore.Store }

func (s StoreLoader) Load(ctx context.Context, request Request) (Result, error) {
	b, payload, receipt, err := s.Store.LoadAttempt(ctx, request.Namespace, request.UID, request.OperationID, request.JobName)
	if err != nil {
		return Result{}, err
	}
	if !request.Matches(b) {
		return Result{}, ErrBinding
	}
	value, err := resultdelivery.Decode(resultdelivery.Identity{Binding: b, Engine: request.Engine}, payload)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Binding: b, Receipt: receipt, Value: value}, nil
}

type Options struct {
	Workers, Entries   int
	Timeout, Retention time.Duration
}
type entry struct {
	done      bool
	completed time.Time
	value     Result
	err       error
}

// Reader bounds both in-flight loads and retained decoded results. Poll never
// performs API I/O. Completed entries are consumed once and may be reloaded
// after a failed status write. Expiration discards only memory, never evidence.
type Reader struct {
	loader  Loader
	options Options
	mu      sync.Mutex
	ctx     context.Context
	stopped bool
	entries map[Request]*entry
	running int
	workers sync.WaitGroup
	now     func() time.Time
}

func New(loader Loader, options Options) (*Reader, error) {
	if loader == nil || options.Workers < 1 || options.Workers > 4 || options.Entries < options.Workers || options.Entries > 16 || options.Timeout <= 0 || options.Timeout > time.Minute || options.Retention <= 0 || options.Retention > 5*time.Minute {
		return nil, errors.New("invalid durable result reader configuration")
	}
	return &Reader{loader: loader, options: options, entries: map[Request]*entry{}, now: time.Now}, nil
}

func (*Reader) NeedLeaderElection() bool { return false }
func (r *Reader) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.ctx != nil || r.stopped {
		r.mu.Unlock()
		return errors.New("durable result reader cannot be restarted")
	}
	r.ctx = ctx
	r.mu.Unlock()
	ticker := time.NewTicker(min(r.options.Retention, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.stopped = true
			r.mu.Unlock()
			r.workers.Wait()
			r.mu.Lock()
			clear(r.entries)
			r.mu.Unlock()
			return nil
		case <-ticker.C:
			r.mu.Lock()
			r.expire()
			r.mu.Unlock()
		}
	}
}

func (r *Reader) expire() {
	now := r.now()
	for key, e := range r.entries {
		if e.done && now.Sub(e.completed) >= r.options.Retention {
			delete(r.entries, key)
		}
	}
}

func (r *Reader) Poll(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if _, err := resultstore.AttemptName(request.Namespace, request.UID, request.OperationID, request.JobName); err != nil || request.JobUID == "" || request.Generation < 1 {
		return Result{}, ErrBinding
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil || r.stopped || r.ctx.Err() != nil {
		return Result{}, ErrStopped
	}
	r.expire()
	if e, ok := r.entries[request]; ok {
		if !e.done {
			return Result{}, ErrPending
		}
		delete(r.entries, request)
		return e.value, e.err
	}
	if r.running >= r.options.Workers || len(r.entries) >= r.options.Entries {
		return Result{}, ErrPending
	}
	e := &entry{}
	r.entries[request] = e
	r.running++
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		loadCtx, cancel := context.WithTimeout(r.ctx, r.options.Timeout)
		value, err := r.loader.Load(loadCtx, request)
		if err != nil {
			value = Result{}
		}
		if loadCtx.Err() != nil {
			value = Result{}
			err = loadCtx.Err()
		}
		cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.running--
		e.value = value
		e.err = err
		e.completed = r.now()
		e.done = true
	}()
	return Result{}, ErrPending
}
