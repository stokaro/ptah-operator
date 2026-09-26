package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

// serviceTestWait bounds every wait in this file for something that should
// already have happened: a listener stopping, a runner starting, a connection
// closing. Each one fires only when that thing never happens, so the bound
// costs nothing on the passing path and everything on a slow one.
//
// It has to be generous rather than tight. The race detector and a two-core
// hosted runner stretch these waits well past what they take on a developer
// machine, and a budget that a slower machine cannot meet reports a timing
// accident as a defect, which is the one thing a test must not do.
const serviceTestWait = 20 * time.Second

func TestRunServiceServesHealthAndStopsItsListener(t *testing.T) {
	t.Parallel()

	healthListener := newTestConnectionListener()
	runnerStarted := make(chan struct{})
	runner := rotationRunnerFunc(func(ctx context.Context) (certrotation.Result, error) {
		close(runnerStarted)
		<-ctx.Done()
		return certrotation.Result{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runServiceOnListener(ctx, testServiceRuntimeConfig(runner), healthListener)
	}()
	select {
	case <-runnerStarted:
	case <-time.After(serviceTestWait):
		t.Fatal("supervisor did not start its first reconciliation")
	}

	connection := healthListener.Dial(t)
	t.Cleanup(func() { _ = connection.Close() })
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://health/healthz", nil)
	if err != nil {
		t.Fatalf("create health request: %v", err)
	}
	if err := request.Write(connection); err != nil {
		t.Fatalf("write health request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		t.Fatalf("read health response: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("liveness status while the supervisor runs = %d, want %d", response.StatusCode, http.StatusOK)
	}
	_ = connection.Close()

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServiceOnListener() error = %v", err)
		}
	case <-time.After(serviceTestWait):
		t.Fatal("runServiceOnListener() did not stop after cancellation")
	}
	if !healthListener.IsClosed() {
		t.Fatal("health listener stayed open after shutdown")
	}
}

func TestRunServiceStopsSupervisorAfterHealthServerFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("health listener failed")
	runnerStarted := make(chan struct{})
	healthListener := &testErrorListener{err: wantErr, waitFor: runnerStarted}
	runnerStopped := make(chan struct{})
	runner := rotationRunnerFunc(func(ctx context.Context) (certrotation.Result, error) {
		close(runnerStarted)
		<-ctx.Done()
		close(runnerStopped)
		return certrotation.Result{}, ctx.Err()
	})

	err := runServiceOnListener(context.Background(), testServiceRuntimeConfig(runner), healthListener)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "serve health requests") {
		t.Fatalf("runServiceOnListener() error = %v, want health listener error", err)
	}
	select {
	case <-runnerStopped:
	default:
		t.Fatal("supervisor did not stop after the health listener failed")
	}
	if !healthListener.IsClosed() {
		t.Fatal("health listener stayed open after its failure")
	}
}

func TestServiceRuntimeErrorRejectsUnexpectedSupervisorStop(t *testing.T) {
	t.Parallel()

	err := serviceRuntimeError(
		supervisorServiceComponent,
		false,
		[]serviceRuntimeResult{
			{component: supervisorServiceComponent},
			{component: healthServiceComponent, err: http.ErrServerClosed},
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "supervisor stopped unexpectedly") {
		t.Fatalf("serviceRuntimeError() error = %v, want unexpected supervisor stop", err)
	}
}

func TestServiceRuntimeConfigValidation(t *testing.T) {
	t.Parallel()

	valid := testServiceRuntimeConfig(rotationRunnerFunc(func(context.Context) (certrotation.Result, error) {
		return certrotation.Result{}, nil
	}))
	tests := []struct {
		name   string
		mutate func(*serviceRuntimeConfig)
		want   string
	}{
		{name: "health address", mutate: func(config *serviceRuntimeConfig) { config.HealthBindAddress = " " }, want: "health bind address"},
		{name: "health handler", mutate: func(config *serviceRuntimeConfig) { config.HealthHandler = nil }, want: "health handler"},
		{name: "supervisor", mutate: func(config *serviceRuntimeConfig) { config.Supervisor = nil }, want: "supervisor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if err := config.validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config error = %v", err)
	}
}

func testServiceRuntimeConfig(runner rotationRunner) serviceRuntimeConfig {
	probes := &probeState{}
	return serviceRuntimeConfig{
		HealthBindAddress: "health-listener",
		HealthHandler:     probes.handler(),
		Supervisor: newSupervisor(runner, supervisorConfig{
			RunInterval:      time.Hour,
			OperationTimeout: time.Hour,
			RetryInitial:     time.Second,
			RetryMax:         time.Minute,
		}, probes, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

type testConnectionListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func newTestConnectionListener() *testConnectionListener {
	return &testConnectionListener{
		connections: make(chan net.Conn),
		closed:      make(chan struct{}),
	}
}

func (l *testConnectionListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *testConnectionListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *testConnectionListener) Addr() net.Addr {
	return testNetworkAddress("listener")
}

func (l *testConnectionListener) Dial(t *testing.T) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serviceTestWait)
	defer cancel()
	server, client := net.Pipe()
	select {
	case l.connections <- server:
		return client
	case <-l.closed:
	case <-ctx.Done():
	}
	_ = server.Close()
	_ = client.Close()
	t.Fatal("dial test listener: the listener did not accept")
	return nil
}

func (l *testConnectionListener) IsClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

type testErrorListener struct {
	err       error
	waitFor   <-chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
	mu        sync.Mutex
}

func (l *testErrorListener) Accept() (net.Conn, error) {
	if l.waitFor != nil {
		<-l.waitFor
	}
	return nil, l.err
}

func (l *testErrorListener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.closed == nil {
			l.closed = make(chan struct{})
		}
		close(l.closed)
	})
	return nil
}

func (l *testErrorListener) Addr() net.Addr {
	return testNetworkAddress("error-listener")
}

func (l *testErrorListener) IsClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed == nil {
		return false
	}
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

type testNetworkAddress string

func (a testNetworkAddress) Network() string { return "test" }
func (a testNetworkAddress) String() string  { return string(a) }
