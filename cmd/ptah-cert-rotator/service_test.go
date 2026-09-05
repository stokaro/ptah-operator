package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serviceTestWait bounds every wait in this file for something that should
// already have happened: a listener stopping, a runner starting, a connection
// closing. Each one fires only when that thing never happens, so the bound
// costs nothing on the passing path and everything on a slow one.
//
// It has to be generous rather than tight. The race detector and a two-core
// hosted runner stretch these waits well past what they take on a developer
// machine: TestCandidateHTTPRequestsNeverReuseConnections failed in CI at one
// second while passing locally, three times in a row, in under two seconds
// total. A budget that a slower machine cannot meet reports a timing accident
// as a defect, which is the one thing a test must not do.
const serviceTestWait = 20 * time.Second

func TestRunServiceServesCandidateTLSAndStopsBothListeners(t *testing.T) {
	t.Parallel()

	candidateConfig := testCandidateAdmissionConfig()
	candidateHandler, err := newCandidateAdmissionHandler(candidateConfig)
	if err != nil {
		t.Fatalf("newCandidateAdmissionHandler() error = %v", err)
	}
	certificatePEM, privateKeyPEM := testTLSKeyPair(t, "candidate-a")
	certificateStore := &candidateCertificateStore{}
	if err := certificateStore.StoreCandidateCertificate(certificatePEM, privateKeyPEM); err != nil {
		t.Fatalf("StoreCandidateCertificate() error = %v", err)
	}

	healthListener := newTestConnectionListener()
	candidateListener := newTestConnectionListener()
	runnerStarted := make(chan struct{})
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		close(runnerStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runServiceOnListeners(ctx, testServiceRuntimeConfig(
			runner,
			candidateHandler,
			certificateStore.tlsConfig(),
		), healthListener, candidateListener)
	}()
	<-runnerStarted

	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("candidate certificate was not added to the test root pool")
	}
	plainConnection := candidateListener.Dial(t)
	tlsConnection := tls.Client(plainConnection, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootPool,
		ServerName: "candidate-a",
		NextProtos: []string{"h2", "http/1.1"},
	})
	t.Cleanup(func() { _ = plainConnection.Close() })
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		t.Fatalf("candidate TLS handshake: %v", err)
	}
	if protocol := tlsConnection.ConnectionState().NegotiatedProtocol; protocol != "http/1.1" {
		t.Fatalf("negotiated protocol = %q, want http/1.1", protocol)
	}

	requestBody := mustCandidateJSON(t, testCandidateAdmissionReview(t, candidateConfig, candidateConfig.MutatingFieldManager))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://candidate-a"+candidateMutatingCanaryPath, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create candidate admission request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if err := request.Write(tlsConnection); err != nil {
		t.Fatalf("write candidate admission request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConnection), request)
	if err != nil {
		t.Fatalf("read candidate admission response: %v", err)
	}
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read candidate admission body: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !response.Close ||
		!bytes.Contains(responseBody, []byte(candidateMutatingDenialMessage)) {
		t.Fatalf("candidate response = status:%d close:%t body:%q", response.StatusCode, response.Close, responseBody)
	}
	_ = plainConnection.Close()

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServiceOnListeners() error = %v", err)
		}
	case <-time.After(serviceTestWait):
		t.Fatal("runServiceOnListeners() did not stop after cancellation")
	}
	if !healthListener.IsClosed() || !candidateListener.IsClosed() {
		t.Fatalf("listeners after shutdown = health:%t candidate:%t, want both closed", healthListener.IsClosed(), candidateListener.IsClosed())
	}
}

func TestRunServiceStopsPeerAndSupervisorAfterCandidateServerFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("candidate listener failed")
	healthListener := newTestConnectionListener()
	runnerStarted := make(chan struct{})
	candidateListener := &testErrorListener{err: wantErr, waitFor: runnerStarted}
	runnerStopped := make(chan struct{})
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		close(runnerStarted)
		<-ctx.Done()
		close(runnerStopped)
		return ctx.Err()
	})
	certificateStore := &candidateCertificateStore{}

	err := runServiceOnListeners(
		context.Background(),
		testServiceRuntimeConfig(runner, http.NotFoundHandler(), certificateStore.tlsConfig()),
		healthListener,
		candidateListener,
	)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "serve candidate admission requests") {
		t.Fatalf("runServiceOnListeners() error = %v, want candidate listener error", err)
	}
	select {
	case <-runnerStopped:
	default:
		t.Fatal("supervisor did not stop after candidate listener failure")
	}
	if !healthListener.IsClosed() || !candidateListener.IsClosed() {
		t.Fatalf("listeners after failure = health:%t candidate:%t, want both closed", healthListener.IsClosed(), candidateListener.IsClosed())
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
			{component: candidateServiceComponent, err: http.ErrServerClosed},
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "supervisor stopped unexpectedly") {
		t.Fatalf("serviceRuntimeError() error = %v, want unexpected supervisor stop", err)
	}
}

func TestCandidateTLSDisablesSessionResumptionAcrossCertificateReplacement(t *testing.T) {
	t.Parallel()

	certificateStore := &candidateCertificateStore{}
	firstCertificatePEM, firstPrivateKeyPEM := testTLSKeyPair(t, "candidate.test")
	secondCertificatePEM, secondPrivateKeyPEM := testTLSKeyPair(t, "candidate.test")
	if err := certificateStore.StoreCandidateCertificate(firstCertificatePEM, firstPrivateKeyPEM); err != nil {
		t.Fatalf("store first candidate certificate: %v", err)
	}
	firstCertificate, err := certificateStore.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get first candidate certificate: %v", err)
	}

	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(firstCertificatePEM) || !rootPool.AppendCertsFromPEM(secondCertificatePEM) {
		t.Fatal("candidate certificates were not added to the test root pool")
	}
	candidateListener, serviceContext := startTestCandidateService(
		t,
		http.NotFoundHandler(),
		certificateStore.tlsConfig(),
	)
	clientConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		RootCAs:            rootPool,
		ServerName:         "candidate.test",
		NextProtos:         []string{"http/1.1"},
		ClientSessionCache: tls.NewLRUClientSessionCache(1),
	}

	firstConnection := dialTestCandidateTLS(t, serviceContext, candidateListener, clientConfig)
	firstState := firstConnection.ConnectionState()
	if firstState.DidResume {
		t.Fatal("first candidate handshake unexpectedly resumed a TLS session")
	}
	if len(firstState.PeerCertificates) != 1 ||
		!bytes.Equal(firstState.PeerCertificates[0].Raw, firstCertificate.Certificate[0]) {
		t.Fatal("first candidate handshake did not serve the first certificate")
	}
	if err := firstConnection.Close(); err != nil {
		t.Fatalf("close first candidate connection: %v", err)
	}

	if err := certificateStore.StoreCandidateCertificate(secondCertificatePEM, secondPrivateKeyPEM); err != nil {
		t.Fatalf("store second candidate certificate: %v", err)
	}
	secondCertificate, err := certificateStore.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get second candidate certificate: %v", err)
	}
	secondConnection := dialTestCandidateTLS(t, serviceContext, candidateListener, clientConfig)
	t.Cleanup(func() { _ = secondConnection.Close() })
	secondState := secondConnection.ConnectionState()
	if secondState.DidResume {
		t.Fatal("second candidate handshake resumed the session established before certificate replacement")
	}
	if len(secondState.PeerCertificates) != 1 ||
		!bytes.Equal(secondState.PeerCertificates[0].Raw, secondCertificate.Certificate[0]) {
		t.Fatal("second candidate handshake did not serve the replacement certificate")
	}
}

func TestCandidateHTTPRequestsNeverReuseConnections(t *testing.T) {
	t.Parallel()

	certificatePEM, privateKeyPEM := testTLSKeyPair(t, "candidate.test")
	certificateStore := &candidateCertificateStore{}
	if err := certificateStore.StoreCandidateCertificate(certificatePEM, privateKeyPEM); err != nil {
		t.Fatalf("store candidate certificate: %v", err)
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	candidateListener, serviceContext := startTestCandidateService(t, handler, certificateStore.tlsConfig())

	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("candidate certificate was not added to the test root pool")
	}
	var dialCount atomic.Int32
	connectionClosed := make(chan struct{}, 2)
	dialedConnections := make(chan net.Conn, 2)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := candidateListener.DialContext(ctx)
			if err == nil {
				dialCount.Add(1)
				connection = &closeNotifyingConnection{Conn: connection, closed: connectionClosed}
				dialedConnections <- connection
			}
			return connection, err
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootPool,
			ServerName: "candidate.test",
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	for requestNumber := int32(1); requestNumber <= 2; requestNumber++ {
		request, err := http.NewRequestWithContext(serviceContext, http.MethodGet, "https://candidate.test/probe", nil)
		if err != nil {
			t.Fatalf("create candidate request %d: %v", requestNumber, err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("candidate request %d: %v", requestNumber, err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		connection := <-dialedConnections
		// net.Pipe has no socket buffers, so simultaneous TLS close alerts can
		// block each other. A bounded deadline preserves the server behavior
		// under test while giving this in-memory transport TCP-like progress.
		_ = connection.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if readErr != nil || closeErr != nil {
			t.Fatalf("consume candidate response %d: read error = %v, close error = %v", requestNumber, readErr, closeErr)
		}
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("candidate response %d status = %d, want 204", requestNumber, response.StatusCode)
		}
		if !response.Close {
			t.Fatalf("candidate response %d allowed HTTP connection reuse", requestNumber)
		}
		if got := dialCount.Load(); got != requestNumber {
			t.Fatalf("candidate request %d used %d connections, want %d", requestNumber, got, requestNumber)
		}
		select {
		case <-connectionClosed:
		case <-time.After(serviceTestWait):
			t.Fatalf("candidate request %d connection was not closed", requestNumber)
		}
	}
}

func TestServiceRuntimeConfigValidation(t *testing.T) {
	t.Parallel()

	store := &candidateCertificateStore{}
	valid := testServiceRuntimeConfig(
		rotationRunnerFunc(func(context.Context) error { return nil }),
		http.NotFoundHandler(),
		store.tlsConfig(),
	)
	tests := []struct {
		name   string
		mutate func(*serviceRuntimeConfig)
		want   string
	}{
		{name: "health address", mutate: func(config *serviceRuntimeConfig) { config.HealthBindAddress = "" }, want: "health bind address"},
		{name: "health handler", mutate: func(config *serviceRuntimeConfig) { config.HealthHandler = nil }, want: "health handler"},
		{name: "candidate address", mutate: func(config *serviceRuntimeConfig) { config.CandidateBindAddress = "" }, want: "candidate bind address"},
		{name: "same addresses", mutate: func(config *serviceRuntimeConfig) { config.CandidateBindAddress = config.HealthBindAddress }, want: "must be distinct"},
		{name: "candidate handler", mutate: func(config *serviceRuntimeConfig) { config.CandidateHandler = nil }, want: "candidate admission handler"},
		{name: "TLS config", mutate: func(config *serviceRuntimeConfig) { config.CandidateTLSConfig = nil }, want: "TLS configuration"},
		{name: "dynamic certificate", mutate: func(config *serviceRuntimeConfig) {
			config.CandidateTLSConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		}, want: "dynamically"},
		{name: "minimum TLS version", mutate: func(config *serviceRuntimeConfig) { config.CandidateTLSConfig.MinVersion = tls.VersionTLS11 }, want: "TLS 1.2"},
		{name: "supervisor", mutate: func(config *serviceRuntimeConfig) { config.Supervisor = nil }, want: "supervisor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			if config.CandidateTLSConfig != nil {
				config.CandidateTLSConfig = config.CandidateTLSConfig.Clone()
			}
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

func testServiceRuntimeConfig(
	runner rotationRunner,
	candidateHandler http.Handler,
	tlsConfig *tls.Config,
) serviceRuntimeConfig {
	probes := &probeState{}
	return serviceRuntimeConfig{
		HealthBindAddress:    "health-listener",
		HealthHandler:        probes.handler(),
		CandidateBindAddress: "candidate-listener",
		CandidateHandler:     candidateHandler,
		CandidateTLSConfig:   tlsConfig,
		Supervisor: newSupervisor(runner, supervisorConfig{
			RunInterval:      time.Hour,
			OperationTimeout: time.Hour,
			RetryInitial:     time.Second,
			RetryMax:         time.Minute,
		}, probes, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

func startTestCandidateService(
	t *testing.T,
	candidateHandler http.Handler,
	tlsConfig *tls.Config,
) (*testConnectionListener, context.Context) {
	t.Helper()

	healthListener := newTestConnectionListener()
	candidateListener := newTestConnectionListener()
	runnerStarted := make(chan struct{})
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		close(runnerStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runServiceOnListeners(
			ctx,
			testServiceRuntimeConfig(runner, candidateHandler, tlsConfig),
			healthListener,
			candidateListener,
		)
	}()
	select {
	case <-runnerStarted:
	case <-time.After(serviceTestWait):
		cancel()
		t.Fatal("candidate test service did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Errorf("runServiceOnListeners() error = %v", err)
			}
		case <-time.After(serviceTestWait):
			t.Error("runServiceOnListeners() did not stop after cancellation")
		}
	})
	return candidateListener, ctx
}

func dialTestCandidateTLS(
	t *testing.T,
	ctx context.Context,
	listener *testConnectionListener,
	config *tls.Config,
) *tls.Conn {
	t.Helper()

	plainConnection := listener.Dial(t)
	tlsConnection := tls.Client(plainConnection, config)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = plainConnection.Close()
		t.Fatalf("candidate TLS handshake: %v", err)
	}
	return tlsConnection
}

type testConnectionListener struct {
	connections chan net.Conn
	accepted    chan struct{}
	closed      chan struct{}
	acceptOnce  sync.Once
	closeOnce   sync.Once
}

func newTestConnectionListener() *testConnectionListener {
	return &testConnectionListener{
		connections: make(chan net.Conn),
		accepted:    make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (l *testConnectionListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.accepted) })
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := l.DialContext(ctx)
	if err != nil {
		t.Fatalf("dial test listener: %v", err)
	}
	return client
}

func (l *testConnectionListener) DialContext(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-l.closed:
		_ = server.Close()
		_ = client.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = server.Close()
		_ = client.Close()
		return nil, ctx.Err()
	}
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
}

func (l *testErrorListener) Accept() (net.Conn, error) {
	if l.waitFor != nil {
		<-l.waitFor
	}
	return nil, l.err
}

func (l *testErrorListener) Close() error {
	l.closeOnce.Do(func() {
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

type closeNotifyingConnection struct {
	net.Conn
	closed    chan<- struct{}
	closeOnce sync.Once
}

func (c *closeNotifyingConnection) Close() error {
	c.closeOnce.Do(func() { c.closed <- struct{}{} })
	return c.Conn.Close()
}
