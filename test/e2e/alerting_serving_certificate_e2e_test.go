//go:build e2e

package e2e

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *alertingRun) probeServingPod(ctx context.Context, pod corev1.Pod, authority []byte, expected *x509.Certificate) error {
	if pod.Name == "" || pod.Namespace != a.in.OperatorNamespace || pod.UID == "" {
		return errors.New("the serving probe needs the exact manager Pod identity")
	}
	current := &corev1.Pod{}
	if a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(&pod), current) != nil || current.UID != pod.UID || !noRestarts(current) {
		return errors.New("the manager Pod changed before the serving probe")
	}
	var port int32
	for _, container := range pod.Spec.Containers {
		for _, candidate := range container.Ports {
			if candidate.Name == "webhook" {
				if port != 0 || candidate.ContainerPort <= 0 || candidate.ContainerPort > 65535 || candidate.Protocol != corev1.ProtocolTCP {
					return errors.New("the manager Pod has no unique webhook port")
				}
				port = candidate.ContainerPort
			}
		}
	}
	if port == 0 {
		return errors.New("the manager Pod has no webhook port")
	}
	transport, upgrader, err := spdy.RoundTripperFor(a.cluster.Config)
	if err != nil {
		return errors.New("could not prepare the exact manager port forward")
	}
	url := a.cluster.Clientset.CoreV1().RESTClient().Post().Namespace(pod.Namespace).
		Resource("pods").Name(pod.Name).SubResource("portforward").URL()
	// The SPDY dialer creates its own request. Bound that initial upgrade as
	// well as the forwarding context, so cancellation cannot strand its worker.
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: 10 * time.Second}, http.MethodPost, url)
	forwardCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ready := make(chan struct{})
	forward, err := portforward.NewOnAddressesWithContext(forwardCtx, dialer, []string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", port)}, ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return errors.New("could not configure the exact manager port forward")
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = forward.ForwardPorts() }()
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case <-done:
		return errors.New("the exact manager port forward stopped before the serving probe")
	case <-forwardCtx.Done():
		return errors.New("the exact manager port forward exceeded its bound")
	}
	ports, err := forward.GetPorts()
	if err != nil || len(ports) != 1 || ports[0].Remote != uint16(port) || ports[0].Local == 0 {
		return errors.New("the manager port forward did not expose the exact webhook port")
	}
	if err := alProbeServingCertificate(forwardCtx, fmt.Sprintf("127.0.0.1:%d", ports[0].Local), authority, expected); err != nil {
		return err
	}
	if a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(&pod), current) != nil || current.UID != pod.UID || !noRestarts(current) {
		return errors.New("the manager Pod changed during the serving probe")
	}
	return nil
}
