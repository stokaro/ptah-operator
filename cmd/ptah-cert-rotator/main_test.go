package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeHealthServer struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s *fakeHealthServer) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

func (s *fakeHealthServer) Close() error {
	return s.close()
}

func TestStopHealthServerForcesCloseAfterShutdownError(t *testing.T) {
	t.Parallel()

	serverResult := make(chan error, 1)
	closeCalls := 0
	server := &fakeHealthServer{
		shutdown: func(context.Context) error {
			return context.DeadlineExceeded
		},
		close: func() error {
			closeCalls++
			serverResult <- http.ErrServerClosed
			return nil
		},
	}
	serverErr, shutdownErr := stopHealthServer(server, serverResult, time.Second)
	if !errors.Is(serverErr, http.ErrServerClosed) {
		t.Fatalf("server error = %v, want http.ErrServerClosed", serverErr)
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want context deadline exceeded", shutdownErr)
	}
	if closeCalls != 1 {
		t.Fatalf("Close() calls = %d, want 1", closeCalls)
	}
}

func TestStopHealthServerCannotWaitForeverForServeResult(t *testing.T) {
	t.Parallel()

	serverResult := make(chan error)
	closeCalls := 0
	server := &fakeHealthServer{
		shutdown: func(context.Context) error { return nil },
		close: func() error {
			closeCalls++
			return nil
		},
	}
	started := time.Now()
	serverErr, shutdownErr := stopHealthServer(server, serverResult, 10*time.Millisecond)
	if serverErr != nil {
		t.Fatalf("server error = %v, want nil", serverErr)
	}
	if shutdownErr == nil || !strings.Contains(shutdownErr.Error(), "timed out waiting") {
		t.Fatalf("shutdown error = %v, want bounded-wait timeout", shutdownErr)
	}
	if closeCalls != 1 {
		t.Fatalf("Close() calls = %d, want 1", closeCalls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopHealthServer() took %s, want a bounded return", elapsed)
	}
}
