package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const serviceShutdownTimeout = 5 * time.Second

type serviceRuntimeConfig struct {
	HealthBindAddress    string
	HealthHandler        http.Handler
	CandidateBindAddress string
	CandidateHandler     http.Handler
	CandidateTLSConfig   *tls.Config
	Supervisor           *rotationSupervisor
}

func (c serviceRuntimeConfig) validate() error {
	switch {
	case strings.TrimSpace(c.HealthBindAddress) == "":
		return errors.New("health bind address is required")
	case c.HealthHandler == nil:
		return errors.New("health handler is required")
	case strings.TrimSpace(c.CandidateBindAddress) == "":
		return errors.New("candidate bind address is required")
	case c.CandidateBindAddress == c.HealthBindAddress:
		return errors.New("candidate and health bind addresses must be distinct")
	case c.CandidateHandler == nil:
		return errors.New("candidate admission handler is required")
	case c.CandidateTLSConfig == nil:
		return errors.New("candidate TLS configuration is required")
	case c.CandidateTLSConfig.GetCertificate == nil:
		return errors.New("candidate TLS configuration must load certificates dynamically")
	case c.CandidateTLSConfig.MinVersion < tls.VersionTLS12:
		return errors.New("candidate TLS configuration must require TLS 1.2 or newer")
	case c.Supervisor == nil:
		return errors.New("certificate rotation supervisor is required")
	default:
		return nil
	}
}

func runService(ctx context.Context, config serviceRuntimeConfig) error {
	if ctx == nil {
		return errors.New("service context is required")
	}
	if err := config.validate(); err != nil {
		return err
	}

	listenConfig := &net.ListenConfig{}
	healthListener, err := listenConfig.Listen(ctx, "tcp", config.HealthBindAddress)
	if err != nil {
		return fmt.Errorf("listen for health probes: %w", err)
	}
	candidateListener, err := listenConfig.Listen(ctx, "tcp", config.CandidateBindAddress)
	if err != nil {
		return errors.Join(
			fmt.Errorf("listen for candidate admission requests: %w", err),
			closeListenerError("health probe", healthListener),
		)
	}

	return runServiceOnListeners(ctx, config, healthListener, candidateListener)
}

func runServiceOnListeners(
	ctx context.Context,
	config serviceRuntimeConfig,
	healthListener net.Listener,
	candidateListener net.Listener,
) error {
	if ctx == nil {
		return errors.New("service context is required")
	}
	if err := config.validate(); err != nil {
		return err
	}
	if healthListener == nil || candidateListener == nil {
		return errors.New("health and candidate listeners are required")
	}

	healthServer := &http.Server{
		Handler:           config.HealthHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	candidateTLSConfig := config.CandidateTLSConfig.Clone()
	// A trust-transition proof must perform a fresh handshake with the
	// certificate currently in the atomic store. Reusing an HTTP/2 connection
	// or a TLS session established for an earlier phase would be a false proof.
	candidateTLSConfig.NextProtos = []string{"http/1.1"}
	candidateTLSConfig.SessionTicketsDisabled = true
	candidateServer := &http.Server{
		Handler:           config.CandidateHandler,
		TLSConfig:         candidateTLSConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	candidateServer.SetKeepAlivesEnabled(false)

	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan serviceRuntimeResult, 3)
	go func() {
		results <- serviceRuntimeResult{component: healthServiceComponent, err: healthServer.Serve(healthListener)}
	}()
	go func() {
		results <- serviceRuntimeResult{
			component: candidateServiceComponent,
			err:       candidateServer.ServeTLS(candidateListener, "", ""),
		}
	}()
	go func() {
		results <- serviceRuntimeResult{component: supervisorServiceComponent, err: config.Supervisor.Run(serviceCtx)}
	}()

	first := <-results
	contextWasDone := ctx.Err() != nil
	cancel()
	shutdownErr := stopHTTPServers([]namedHTTPServer{
		{name: "health", server: healthServer},
		{name: "candidate admission", server: candidateServer},
	}, serviceShutdownTimeout)

	allResults := []serviceRuntimeResult{first}
	waitTimer := time.NewTimer(serviceShutdownTimeout)
	defer waitTimer.Stop()
	for len(allResults) < 3 {
		select {
		case result := <-results:
			allResults = append(allResults, result)
		case <-waitTimer.C:
			return errors.Join(shutdownErr, errors.New("timed out waiting for certificate rotation services to stop"))
		}
	}
	return serviceRuntimeError(first.component, contextWasDone, allResults, shutdownErr)
}

type serviceComponent string

const (
	healthServiceComponent     serviceComponent = "health"
	candidateServiceComponent  serviceComponent = "candidate admission"
	supervisorServiceComponent serviceComponent = "certificate rotation supervisor"
)

type serviceRuntimeResult struct {
	component serviceComponent
	err       error
}

func serviceRuntimeError(
	first serviceComponent,
	contextWasDone bool,
	results []serviceRuntimeResult,
	shutdownErr error,
) error {
	errs := []error{shutdownErr}
	for _, result := range results {
		switch result.component {
		case supervisorServiceComponent:
			if result.err != nil {
				errs = append(errs, result.err)
			} else if result.component == first && !contextWasDone {
				errs = append(errs, errors.New("certificate rotation supervisor stopped unexpectedly"))
			}
		case healthServiceComponent, candidateServiceComponent:
			if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
				errs = append(errs, fmt.Errorf("serve %s requests: %w", result.component, result.err))
			} else if result.component == first && !contextWasDone {
				errs = append(errs, fmt.Errorf("%s server stopped unexpectedly", result.component))
			}
		default:
			errs = append(errs, errors.New("unknown certificate rotation service stopped"))
		}
	}
	return errors.Join(errs...)
}

type httpServerShutdown interface {
	Shutdown(context.Context) error
	Close() error
}

type namedHTTPServer struct {
	name   string
	server httpServerShutdown
}

func stopHTTPServers(servers []namedHTTPServer, timeout time.Duration) error {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), timeout)
	defer shutdownCancel()

	results := make(chan error, len(servers))
	for _, entry := range servers {
		go func() {
			if entry.server == nil {
				results <- fmt.Errorf("shut down %s server: server is required", entry.name)
				return
			}
			err := entry.server.Shutdown(shutdownCtx)
			if err != nil {
				err = errors.Join(err, entry.server.Close())
			}
			if err != nil {
				err = fmt.Errorf("shut down %s server: %w", entry.name, err)
			}
			results <- err
		}()
	}

	var errs []error
	for range servers {
		errs = append(errs, <-results)
	}
	return errors.Join(errs...)
}

func closeListenerError(name string, listener net.Listener) error {
	if err := listener.Close(); err != nil {
		return fmt.Errorf("close %s listener: %w", name, err)
	}
	return nil
}
