package main

import (
	"context"
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
	HealthBindAddress string
	HealthHandler     http.Handler
	Supervisor        *rotationSupervisor
}

func (c serviceRuntimeConfig) validate() error {
	switch {
	case strings.TrimSpace(c.HealthBindAddress) == "":
		return errors.New("health bind address is required")
	case c.HealthHandler == nil:
		return errors.New("health handler is required")
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
	return runServiceOnListener(ctx, config, healthListener)
}

func runServiceOnListener(
	ctx context.Context,
	config serviceRuntimeConfig,
	healthListener net.Listener,
) error {
	if ctx == nil {
		return errors.New("service context is required")
	}
	if err := config.validate(); err != nil {
		return err
	}
	if healthListener == nil {
		return errors.New("health listener is required")
	}

	healthServer := &http.Server{
		Handler:           config.HealthHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}

	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan serviceRuntimeResult, 2)
	go func() {
		results <- serviceRuntimeResult{component: healthServiceComponent, err: healthServer.Serve(healthListener)}
	}()
	go func() {
		results <- serviceRuntimeResult{component: supervisorServiceComponent, err: config.Supervisor.Run(serviceCtx)}
	}()

	first := <-results
	contextWasDone := ctx.Err() != nil
	cancel()
	shutdownErr := stopHTTPServers([]namedHTTPServer{
		{name: "health", server: healthServer},
	}, serviceShutdownTimeout)

	allResults := []serviceRuntimeResult{first}
	waitTimer := time.NewTimer(serviceShutdownTimeout)
	defer waitTimer.Stop()
	for len(allResults) < 2 {
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
		case healthServiceComponent:
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
