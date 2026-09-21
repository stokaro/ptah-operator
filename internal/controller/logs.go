package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
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
// The bound is not a round number. runner.DefaultMaxFrameBytes is a little
// over 48 MiB, which is the largest result frame the protocol admits, and the
// parser tolerates interleaved output around it; at a floor of a mebibyte a
// second that frame takes about fifty seconds to arrive. Two minutes leaves
// the API server room to open the stream to the kubelet and the log room to
// carry the executor's own output ahead of the frame, and is still short
// enough that a stalled read costs one operation a reconcile rather than
// costing the family its worker.
//
// A read that times out returns an error, which requeues with backoff. The
// claim, the Lease and status.unresolvedRun are untouched by it: nothing about
// a log this manager could not read says what the database now holds, so an
// Apply stays exactly as uncertain as it was.
const defaultResultReadTimeout = 2 * time.Minute

// readOperationResult reads a terminal Pod's executor log under a deadline of
// its own. The deadline is applied here rather than inside a reader, so it
// bounds every implementation of the seam and not just the one that talks to
// an API server.
func readOperationResult(
	ctx context.Context,
	reader PodLogReader,
	timeout time.Duration,
	namespace, podName, containerName string,
) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultResultReadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	logs, err := r.Client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: containerName}).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("read executor logs: %w", err)
	}
	return logs, nil
}
