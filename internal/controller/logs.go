package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// PodLogReader isolates the pod/log subresource from the reconciliation logic.
type PodLogReader interface {
	Read(ctx context.Context, namespace, podName, containerName string) ([]byte, error)
}

// defaultResultReadTimeout bounds the one call a reconcile makes that streams
// a body of the executor's choosing across two hops.
//
// Every other external call a reconcile makes is a cached read or a small
// write against the API server. This one is a pod/log request the API server
// proxies to a kubelet, and neither client-go's zero rest.Config.Timeout nor
// controller-runtime's opt-in reconcile timeout bounds it. A controller runs
// one reconcile worker per resource family, and leader election admits one
// manager, so a response that never arrives holds the only worker that family
// has -- including the passes that would renew the Lease of an Apply that is
// executing SQL somewhere else.
//
// The number is two thirds of the shortest Apply Lease the API can produce --
// sixty seconds of the ninety that activeDeadlineSeconds at its minimum of 30
// plus the grace produces. The worker blocked here is the worker that renews
// Leases for every other resource of this family, so the read may hold it for
// no longer, and has to give it back with time to spend: a bound equal to the
// whole Lease would let a read that began just after a renewal run until the
// moment that Lease expired, leaving nothing in which to renew it.
//
// It still clears what a legitimate result needs. runner.MaxResultLogBytes is
// a little over 48 MiB, which is the shortest tail of a log that can still
// hold a frame, and at a floor of a mebibyte a second that takes about fifty
// seconds.
//
// The two constraints meet close together, and where they conflict the Lease
// wins. A maximum-size frame on a link slower than that floor times out and is
// retried; a renewal that arrives too late lets another family take a realm
// whose SQL may still be running.
//
// The tail is what the read keeps; this is how long it may spend arriving. A
// log too long for that ends here, and that is the same answer as a log that
// stopped arriving, which is correct for both: nothing was read, so nothing is
// decided.
//
// What this does not remove is that one worker serves the whole family. A read
// holding it for its full budget is a read during which no other resource of
// that family is reconciled, including one whose Apply Lease wants renewing.
// Bounding the read shortens that window and does not close it; closing it
// means taking the read off the reconcile path or giving the family more than
// one worker, and neither belongs in a change about the bound.
//
// A read that times out returns an error, which requeues with backoff. The
// claim, the Lease and status.unresolvedRun are untouched by it: nothing about
// a log this manager could not read says what the database now holds, so an
// Apply stays exactly as uncertain as it was.
const defaultResultReadTimeout = 60 * time.Second

// boundedResultReadTimeout is the bound this read actually gets: the ceiling
// above, or the resource's own execution deadline where that is shorter.
//
// The ceiling alone is out of proportion at the low end. A resource may ask
// for activeDeadlineSeconds as low as 30, and an Apply's Lease is that plus a
// minute; spending two minutes reading the result of thirty seconds of work is
// the worker held for longer than the operation it is reporting on. So the
// read is also held to the Lease the operation took, which is the only other
// number the operation itself supplies.
//
// What this is not is the guard on the realm. A read is read-only, and a read
// that outlives its Lease cannot authorize anything: the epoch is checked
// before the result is used, and a Lease that changed hands retires the
// operation and discards the result rather than acting on it. This bound is
// there so one unanswered request does not hold the family's single worker --
// it makes that read proportionate, and the epoch makes it safe.
//
// Zero or less means no Lease bounds this read -- a read-only operation holds
// none -- and the ceiling applies.
func boundedResultReadTimeout(configured, leaseBudget time.Duration) time.Duration {
	bound := configured
	if bound <= 0 {
		bound = defaultResultReadTimeout
	}
	if leaseBudget > 0 && leaseBudget < bound {
		return leaseBudget
	}
	return bound
}

// leaseReadBudget turns a Lease this operation actually holds into the time a
// read may spend reading its result.
//
// The duration has to come from the claim rather than from the spec. A Lease
// is taken for the duration the claim recorded and renewed at that duration
// afterwards, and spec.execution.activeDeadlineSeconds is mutable: raising it
// after an Apply started grows nothing about the Lease already held, so a
// bound derived from the spec would say more time is available than is.
//
// It is the whole duration and not the duration less its grace. The grace is
// what makes the Lease outlive the Job; it is not time withheld from reading
// the result afterwards, and subtracting it left the shortest configuration
// with thirty seconds to fetch a frame this protocol allows to be 48 MiB --
// about fifty at the floor rate the ceiling is derived from. Every attempt
// would have timed out, and every retry would have been given the same
// thirty seconds. The shortest Lease the API can produce is ninety seconds,
// which clears that with room.
//
// Zero means no Lease.
func leaseReadBudget(leaseDurationSeconds int32) time.Duration {
	if leaseDurationSeconds <= 0 {
		return 0
	}
	return time.Duration(leaseDurationSeconds) * time.Second
}

// readOperationResult reads a terminal Pod's executor log under a deadline of
// its own. The deadline is applied here rather than inside a reader, so it
// bounds every implementation of the seam and not just the one that talks to
// an API server.
func readOperationResult(
	ctx context.Context,
	reader PodLogReader,
	timeout, activeDeadline time.Duration,
	namespace, podName, containerName string,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, boundedResultReadTimeout(timeout, activeDeadline))
	defer cancel()
	return reader.Read(ctx, namespace, podName, containerName)
}

// ClientsetPodLogs reads exact container logs through the Kubernetes API.
type ClientsetPodLogs struct {
	Client kubernetes.Interface
}

func (r ClientsetPodLogs) Read(ctx context.Context, namespace, podName, containerName string) ([]byte, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("Kubernetes clientset is required")
	}
	stream, err := r.Client.CoreV1().Pods(namespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: containerName}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("read executor logs: %w", err)
	}
	defer func() { _ = stream.Close() }()
	logs, err := readLogTail(stream, runner.MaxResultLogBytes)
	if err != nil {
		return nil, fmt.Errorf("read executor logs: %w", err)
	}
	return logs, nil
}

// readLogTail returns the last limit bytes of r, reading all of it and keeping
// none of what it drops.
//
// The runner's own output is bounded; the executor's is not, and a migration
// that prints a large amount of native output produces a log this worker would
// otherwise hold whole and hand whole to the parser. Every bound the parser
// has is expressed against the log it is given, so the log's size is the
// multiplier on each scan it makes.
//
// Taking the tail rather than the head is what makes the bound safe: the frame
// is the last thing the runner writes, and runner.MaxResultLogBytes is the
// shortest tail that still holds any frame the parser could have found in the
// whole log. A bound applied at the front would take a complete run and report
// it unreadable, which costs more than the allocation it saves.
func readLogTail(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("a result log tail needs a positive bound, got %d", limit)
	}
	// Once the log passes the bound the buffer stops growing and is written
	// around, so the oldest byte moves instead of the whole tail being copied
	// down. Copying down would cost the length of the bound for every chunk
	// past it, which on a large log is most of the work this does.
	var (
		tail   []byte
		oldest int
		wrap   bool
	)
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			block := chunk[:n]
			if int64(len(block)) > limit {
				block = block[int64(len(block))-limit:]
			}
			for len(block) > 0 {
				if !wrap {
					room := int(limit) - len(tail)
					take := min(room, len(block))
					tail = growTail(tail, take, limit)
					tail = append(tail, block[:take]...)
					block = block[take:]
					if int64(len(tail)) == limit {
						wrap = true
						oldest = 0
					}
					continue
				}
				take := min(len(tail)-oldest, len(block))
				copy(tail[oldest:], block[:take])
				block = block[take:]
				oldest = (oldest + take) % len(tail)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if !wrap {
		return tail, nil
	}
	ordered := make([]byte, 0, len(tail))
	ordered = append(ordered, tail[oldest:]...)
	ordered = append(ordered, tail[:oldest]...)
	return ordered, nil
}

// growTail makes room for take more bytes without ever reserving more than the
// bound. Letting append choose would double past it on the last growth, which
// is the one allocation this is here to keep inside the bound it advertises.
func growTail(tail []byte, take int, limit int64) []byte {
	if cap(tail) >= len(tail)+take {
		return tail
	}
	next := max(2*cap(tail), len(tail)+take)
	if int64(next) > limit {
		next = int(limit)
	}
	grown := make([]byte, len(tail), next)
	copy(grown, tail)
	return grown
}
