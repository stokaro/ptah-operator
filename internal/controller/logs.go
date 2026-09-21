package controller

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// PodLogReader isolates the pod/log subresource from the reconciliation logic.
type PodLogReader interface {
	Read(ctx context.Context, namespace, podName, containerName string) ([]byte, error)
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
