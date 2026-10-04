package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// A Pod-specific tunnel supplies routing only. The inner connection still
// verifies the API serving certificate and authenticates with the kubeconfig.
func apiMetricsClient(config *rest.Config, host string) (*http.Client, error) {
	if config == nil || config.Insecure || config.Transport != nil || len(config.CAData) == 0 && config.CAFile == "" {
		return nil, errors.New("API metrics require a verified cluster CA transport")
	}
	direct := rest.CopyConfig(config)
	direct.Host = host
	direct.ServerName = "kubernetes.default.svc"
	direct.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	direct.Dial = nil
	direct.Timeout = 10 * time.Second
	client, err := rest.HTTPClientFor(direct)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("API metrics redirects are refused") }
	return client, nil
}

func scrapeAPIPod(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, pod corev1.Pod) (scrape, error) {
	// Refuse insecure input before opening even the outer tunnel.
	if _, err := apiMetricsClient(config, "https://127.0.0.1"); err != nil {
		return nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, err
	}
	target := clientset.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: 10 * time.Second}, http.MethodPost, target)
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ready := make(chan struct{})
	forward, err := portforward.NewOnAddressesWithContext(bounded, dialer, []string{"127.0.0.1"}, []string{"0:6443"}, ready, io.Discard, io.Discard)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	var forwardErr error
	go func() { forwardErr = forward.ForwardPorts(); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case <-done:
		return nil, fmt.Errorf("API tunnel stopped before becoming ready: %v", forwardErr)
	case <-bounded.Done():
		return nil, bounded.Err()
	}
	ports, err := forward.GetPorts()
	if err != nil || len(ports) != 1 || ports[0].Remote != 6443 || ports[0].Local == 0 {
		return nil, errors.New("API tunnel did not expose port 6443")
	}
	host := fmt.Sprintf("https://127.0.0.1:%d", ports[0].Local)
	client, err := apiMetricsClient(config, host)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, host+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API metrics HTTP %d", response.StatusCode)
	}
	const limit = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("API metrics exceeded the response limit")
	}
	return parseScrape(raw)
}
