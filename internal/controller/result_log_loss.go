package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// A result log can be gone while the Pod that wrote it is not. Container
// garbage collection removes the log of a terminated container, a node that is
// deleted takes every log on it, and a node that stops answering takes them for
// as long as it stays away. The Pod object outlives all of that, and its status
// still carries the runner's termination summary, so a read that keeps failing
// is a question with an answer: is this log coming back?
//
// A failure the API server states plainly is answered at once. Anything else
// is given resultLogLossWindow to recover, and a log still unreadable after it
// is treated as gone. Either way a gone log is read as a log that holds no
// frame, and each family does with it what it does with one: a read-only
// operation is retried, a schema Apply is unknown, and a migration Apply is
// settled from the termination summary if one stands in for the frame.
//
// A Pod that is gone is not this case. Its status went with it, the next pass
// finds no Pod, and the operation is judged as it always was.

// resultLogLossWindow is how long a result log may keep failing to read, as
// this process has watched it fail, before it is treated as gone.
//
// Two whole read budgets. A failure can be a blip -- a kubelet restarting, the
// API server's connection to it dropped once -- and a single retry after one
// can land inside the same blip. Two reads that each had the full budget
// failing back to back, with the retries between them, is a node that has not
// come back in the time a read is allowed to take, twice over. It is not
// longer because of what waiting costs: a migration Apply holds its realm's
// Lease until its run is accounted for, so every minute spent on a log that is
// not coming back is a minute no other resource may touch that database.
//
// It is measured from the first failure this process saw, not from anything
// stored, so a manager restart starts it again: a restart can only make it
// wait longer for a log, never give one up sooner.
const resultLogLossWindow = 2 * defaultResultReadTimeout

// errResultReadRetry marks a read that failed in a way that may pass, inside
// resultLogLossWindow. The pass is requeued and nothing is decided.
var errResultReadRetry = errors.New("the result log could not be read yet")

// resultLogLost is a result log this manager will not read again. It is a
// missing frame, so errors.Is reports runner.ErrFrameNotFound, and it keeps the
// read error that decided it.
type resultLogLost struct {
	cause error
	// failingFor is how long the log had been failing, and zero where the API
	// server said it is gone.
	failingFor time.Duration
}

func (e *resultLogLost) Error() string {
	if e.failingFor == 0 {
		return "the result log is gone: " + bounded(e.cause.Error(), 256)
	}
	return fmt.Sprintf("the result log could not be read for %s: %s",
		e.failingFor.Round(time.Second), bounded(e.cause.Error(), 256))
}

func (e *resultLogLost) Unwrap() []error { return []error{runner.ErrFrameNotFound, e.cause} }

// resultLogFailure is what a failed result log read says about the log.
type resultLogFailure int

const (
	// resultLogTransient may pass: the log may be read on a later pass.
	resultLogTransient resultLogFailure = iota
	// resultLogGone will not pass: the API server says the log cannot be served.
	resultLogGone
	// resultLogPodGone is the Pod itself gone. The next pass finds no Pod.
	resultLogPodGone
	// resultLogDenied is this manager not allowed to read logs, which says
	// nothing about the log and everything about the installation.
	resultLogDenied
)

// classifyResultLogError reads what the API server said about a failed read.
//
// Only an answer that cannot change while the Pod exists is gone:
//
//   - NotFound for the Node the Pod ran on. The API server looks the node up to
//     reach its kubelet, and a node that has been deleted will not be back
//     under that Pod.
//   - BadRequest. The kubelet answers this request, which is the same fixed one
//     every time, with 400 when it cannot serve that container's log: the
//     container status it would read no longer names a container.
//
// A NotFound for the Pod is the Pod gone. A NotFound the kubelet produced for
// the Pod or the container is a kubelet that does not know them yet, which a
// restart causes and a resync mends, so it waits like the rest. So do a server
// error, a kubelet that cannot be reached, a read that ran out of time, and
// anything the client could not even send.
//
// The kubelet has one more way to say a log is gone, and it is not an error:
// once it has started answering, a container that was garbage collected or a
// log file that was removed ends the stream with the reason as its body, under
// a 200. That arrives here as a log that holds no frame, and is judged as one.
func classifyResultLogError(err error) resultLogFailure {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return resultLogDenied
	case apierrors.IsBadRequest(err):
		return resultLogGone
	case apierrors.IsNotFound(err) && errors.As(err, &status):
		details := status.Status().Details
		switch {
		case details == nil:
			return resultLogTransient
		case details.Kind == "nodes":
			return resultLogGone
		case details.Kind == "pods":
			return resultLogPodGone
		}
	}
	return resultLogTransient
}

// resultLogFailures remembers, per Pod, when this process first failed to read
// that Pod's executor log. It is the only clock resultLogLossWindow is measured
// on, and it is kept in memory on purpose; see resultLogLossWindow.
type resultLogFailures struct {
	mu    sync.Mutex
	first map[types.UID]time.Time
}

// resultLogFailuresMade guards making a reconciler's resultLogFailures on first
// use. A reconciler is a struct literal in the manager and in every test, and
// a test that restarts one copies it, so the memory is a pointer made here
// rather than a value that would be copied with its lock.
var resultLogFailuresMade sync.Mutex

func resultLogFailuresOf(field **resultLogFailures) *resultLogFailures {
	resultLogFailuresMade.Lock()
	defer resultLogFailuresMade.Unlock()
	if *field == nil {
		*field = &resultLogFailures{}
	}
	return *field
}

// failingFor records a failed read and reports how long reads of this Pod's log
// have been failing.
func (f *resultLogFailures) failingFor(pod types.UID, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.first == nil {
		f.first = map[types.UID]time.Time{}
	}
	// A Pod whose log stopped being read without being settled here -- its
	// operation was retired some other way -- would otherwise stay forever.
	for uid, first := range f.first {
		if now.Sub(first) > 10*resultLogLossWindow {
			delete(f.first, uid)
		}
	}
	first, seen := f.first[pod]
	if !seen {
		f.first[pod] = now
		return 0
	}
	return now.Sub(first)
}

func (f *resultLogFailures) forget(pod types.UID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.first, pod)
}

// readResultLog reads a terminal executor's log and decides what a failure to
// read it means.
//
// It returns the log; or a *resultLogLost, when the log is gone or has failed
// past resultLogLossWindow; or an error wrapping errResultReadRetry, when the
// pass should be tried again; or the read error itself, when it is about
// neither the log nor its loss.
func readResultLog(
	ctx context.Context,
	reader PodLogReader,
	failures *resultLogFailures,
	now time.Time,
	timeout, budget time.Duration,
	pod *corev1.Pod,
) ([]byte, error) {
	logs, err := readOperationResult(ctx, reader, timeout, budget, pod.Namespace, pod.Name, executorContainerName)
	if err == nil {
		failures.forget(pod.UID)
		return logs, nil
	}
	if ctx.Err() != nil {
		// This process is stopping. Nothing was learned about the log.
		return nil, err
	}
	switch classifyResultLogError(err) {
	case resultLogGone:
		failures.forget(pod.UID)
		return nil, &resultLogLost{cause: err}
	case resultLogTransient:
		if elapsed := failures.failingFor(pod.UID, now); elapsed >= resultLogLossWindow {
			failures.forget(pod.UID)
			return nil, &resultLogLost{cause: err, failingFor: elapsed}
		}
		return nil, fmt.Errorf("%w: %w", errResultReadRetry, err)
	default:
		return nil, err
	}
}
