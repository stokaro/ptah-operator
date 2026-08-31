package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

type rotationRunnerFunc func(context.Context) error

func (f rotationRunnerFunc) Run(ctx context.Context) error {
	return f(ctx)
}

func TestSupervisorRetriesImmediatelyAndResetsBackoff(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	probes := &probeState{}
	outcomes := []error{
		errors.New("first failure containing PRIVATE KEY material"),
		errors.New("second failure"),
		nil,
		errors.New("failure after success"),
	}
	var events []string
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("reconciliation context has no operation deadline")
		}
		index := len(events) / 2
		events = append(events, fmt.Sprintf("run-%d", index+1))
		return outcomes[index]
	})

	ctx, cancel := context.WithCancel(context.Background())
	supervisor := newSupervisor(runner, supervisorConfig{
		RunInterval:      10 * time.Second,
		OperationTimeout: time.Minute,
		RetryInitial:     time.Second,
		RetryMax:         4 * time.Second,
	}, probes, logger)
	supervisor.jitter = func(base, _ time.Duration) time.Duration { return base }
	var waits []time.Duration
	supervisor.wait = func(_ context.Context, duration time.Duration) bool {
		waits = append(waits, duration)
		events = append(events, fmt.Sprintf("wait-%s", duration))
		if !probes.live.Load() {
			t.Error("liveness became false while the supervisor was running")
		}
		wantReady := len(waits) == 3
		if got := probes.ready.Load(); got != wantReady {
			t.Errorf("readiness after wait %d = %t, want %t", len(waits), got, wantReady)
		}
		if len(waits) == len(outcomes) {
			cancel()
			return false
		}
		return true
	}

	if err := supervisor.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 10 * time.Second, time.Second}
	if !reflect.DeepEqual(waits, wantWaits) {
		t.Fatalf("waits = %v, want %v", waits, wantWaits)
	}
	wantEvents := []string{
		"run-1", "wait-1s",
		"run-2", "wait-2s",
		"run-3", "wait-10s",
		"run-4", "wait-1s",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if probes.live.Load() || probes.ready.Load() {
		t.Fatalf("probe state after shutdown = live:%t ready:%t, want both false", probes.live.Load(), probes.ready.Load())
	}
	if strings.Contains(logs.String(), "PRIVATE KEY") {
		t.Fatalf("logs contain unsanitized error: %s", logs.String())
	}
}

func TestSupervisorOperationTimeoutIsRetried(t *testing.T) {
	t.Parallel()

	const operationTimeout = 20 * time.Millisecond
	probes := &probeState{}
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("reconciliation context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > operationTimeout {
			t.Errorf("operation deadline remaining = %s, want within (0, %s]", remaining, operationTimeout)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := newSupervisor(runner, supervisorConfig{
		RunInterval:      time.Hour,
		OperationTimeout: operationTimeout,
		RetryInitial:     time.Second,
		RetryMax:         time.Minute,
	}, probes, slog.New(slog.NewTextHandler(&logs, nil)))
	supervisor.jitter = func(base, _ time.Duration) time.Duration { return base }
	supervisor.wait = func(_ context.Context, duration time.Duration) bool {
		if duration != time.Second {
			t.Errorf("retry delay = %s, want 1s", duration)
		}
		if probes.ready.Load() {
			t.Error("readiness is true after an operation timeout")
		}
		cancel()
		return false
	}

	if err := supervisor.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(logs.String(), "operation timed out") {
		t.Fatalf("logs do not contain sanitized timeout classification: %s", logs.String())
	}
}

func TestSupervisorCancellationStopsCurrentRunCleanly(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var once sync.Once
	runner := rotationRunnerFunc(func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return fmt.Errorf("rotation interrupted: %w", ctx.Err())
	})
	probes := &probeState{}
	var logs bytes.Buffer
	supervisor := newSupervisor(runner, supervisorConfig{
		RunInterval:      time.Hour,
		OperationTimeout: time.Hour,
		RetryInitial:     time.Second,
		RetryMax:         time.Minute,
	}, probes, slog.New(slog.NewTextHandler(&logs, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	<-started
	if !probes.live.Load() || probes.ready.Load() {
		t.Fatalf("probe state during first run = live:%t ready:%t, want true/false", probes.live.Load(), probes.ready.Load())
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error after cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after context cancellation")
	}
	if strings.Contains(logs.String(), "failed; retrying") {
		t.Fatalf("shutdown was logged as a reconciliation failure: %s", logs.String())
	}
}

func TestSupervisorPreservesReadinessDuringLaterReconciliation(t *testing.T) {
	t.Parallel()

	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls int
	runner := rotationRunnerFunc(func(context.Context) error {
		calls++
		if calls == 1 {
			return nil
		}
		close(secondStarted)
		<-releaseSecond
		return errors.New("second reconciliation failed")
	})
	probes := &probeState{}
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := newSupervisor(runner, supervisorConfig{
		RunInterval:      time.Hour,
		OperationTimeout: time.Minute,
		RetryInitial:     time.Second,
		RetryMax:         time.Minute,
	}, probes, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	supervisor.jitter = func(base, _ time.Duration) time.Duration { return base }
	firstWait := make(chan struct{})
	supervisor.wait = func(_ context.Context, duration time.Duration) bool {
		if duration == time.Hour {
			close(firstWait)
			return true
		}
		if probes.ready.Load() {
			t.Error("readiness remained true after a failed reconciliation")
		}
		cancel()
		return false
	}

	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	<-firstWait
	<-secondStarted
	if !probes.ready.Load() {
		t.Error("readiness became false while a later reconciliation was still in progress")
	}
	close(releaseSecond)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after the failed reconciliation")
	}
}

func TestSupervisorConfigValidation(t *testing.T) {
	t.Parallel()

	valid := supervisorConfig{
		RunInterval:      time.Hour,
		OperationTimeout: time.Minute,
		RetryInitial:     time.Second,
		RetryMax:         time.Minute,
	}
	tests := []struct {
		name   string
		mutate func(*supervisorConfig)
		want   string
	}{
		{name: "run interval", mutate: func(c *supervisorConfig) { c.RunInterval = 0 }, want: "run interval must be positive"},
		{name: "operation timeout", mutate: func(c *supervisorConfig) { c.OperationTimeout = 0 }, want: "operation timeout must be positive"},
		{name: "retry initial", mutate: func(c *supervisorConfig) { c.RetryInitial = 0 }, want: "initial retry delay must be positive"},
		{name: "retry max", mutate: func(c *supervisorConfig) { c.RetryMax = 0 }, want: "maximum retry delay must be positive"},
		{name: "retry order", mutate: func(c *supervisorConfig) { c.RetryMax = time.Millisecond }, want: "maximum retry delay must not be less"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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

func TestRuntimeRelationshipValidation(t *testing.T) {
	t.Parallel()

	validSupervisor := supervisorConfig{
		RunInterval:      6 * time.Hour,
		OperationTimeout: 15 * time.Minute,
		RetryInitial:     time.Second,
		RetryMax:         time.Minute,
	}
	validRotation := certrotation.Config{
		RenewalThreshold:           30 * 24 * time.Hour,
		ServingCertificateValidity: 90 * 24 * time.Hour,
		ProbeTimeout:               5 * time.Minute,
		AcquireTimeout:             30 * time.Second,
	}
	if err := validateRuntimeRelationships(validSupervisor, validRotation); err != nil {
		t.Fatalf("valid relationship error = %v", err)
	}

	tests := []struct {
		name               string
		mutateSupervisor   func(*supervisorConfig)
		mutateCertRotation func(*certrotation.Config)
		want               string
	}{
		{
			name: "invalid rotation window",
			mutateCertRotation: func(config *certrotation.Config) {
				config.ServingCertificateValidity = config.RenewalThreshold
			},
			want: "must exceed the positive renewal threshold",
		},
		{
			name: "scheduled cycle equals rotation window",
			mutateSupervisor: func(config *supervisorConfig) {
				config.RunInterval = 60*24*time.Hour - config.OperationTimeout
			},
			want: "run interval plus operation timeout must be shorter",
		},
		{
			name: "scheduled cycle overflow",
			mutateSupervisor: func(config *supervisorConfig) {
				config.RunInterval = time.Duration(1<<63-1) - 10*time.Minute
			},
			want: "run interval plus operation timeout exceeds",
		},
		{
			name: "nonpositive probe timeout",
			mutateCertRotation: func(config *certrotation.Config) {
				config.ProbeTimeout = 0
			},
			want: "must be positive",
		},
		{
			name: "combined timeout overflow",
			mutateCertRotation: func(config *certrotation.Config) {
				config.AcquireTimeout = time.Duration(1<<63 - 2)
				config.ProbeTimeout = time.Nanosecond * 2
			},
			want: "exceeds the supported duration",
		},
		{
			name: "operation timeout equals combined timeout",
			mutateSupervisor: func(config *supervisorConfig) {
				config.OperationTimeout = 5*time.Minute + 30*time.Second
			},
			want: "operation timeout must exceed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			supervisor := validSupervisor
			rotation := validRotation
			if test.mutateSupervisor != nil {
				test.mutateSupervisor(&supervisor)
			}
			if test.mutateCertRotation != nil {
				test.mutateCertRotation(&rotation)
			}
			if err := validateRuntimeRelationships(supervisor, rotation); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateRuntimeRelationships() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRetryDelayBounds(t *testing.T) {
	t.Parallel()

	if got := nextRetryDelay(time.Second, 5*time.Second); got != 2*time.Second {
		t.Errorf("nextRetryDelay(1s, 5s) = %s, want 2s", got)
	}
	if got := nextRetryDelay(4*time.Second, 5*time.Second); got != 5*time.Second {
		t.Errorf("nextRetryDelay(4s, 5s) = %s, want 5s", got)
	}
	if got := nextRetryDelay(time.Duration(1<<63-2), time.Duration(1<<63-1)); got != time.Duration(1<<63-1) {
		t.Errorf("overflow-safe nextRetryDelay() = %s, want maximum duration", got)
	}
	for range 100 {
		got := retryDelayWithJitter(10*time.Second, 11*time.Second)
		if got < 10*time.Second || got > 11*time.Second {
			t.Fatalf("retryDelayWithJitter() = %s, want [10s, 11s]", got)
		}
	}
	if got := clampRetryDelay(0, time.Second, time.Minute); got != time.Second {
		t.Errorf("clampRetryDelay(0) = %s, want 1s", got)
	}
	if got := clampRetryDelay(time.Hour, time.Second, time.Minute); got != time.Minute {
		t.Errorf("clampRetryDelay(1h) = %s, want 1m", got)
	}
}

func TestSanitizedReconcileErrorDoesNotExposeServerMessage(t *testing.T) {
	t.Parallel()

	err := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "secrets"},
		"generated-tls",
		errors.New("server echoed PRIVATE KEY bytes"),
	)
	got := sanitizedReconcileError(fmt.Errorf("wrapped: %w", err))
	if got != "Kubernetes API request was forbidden" {
		t.Fatalf("sanitizedReconcileError() = %q", got)
	}
	if strings.Contains(got, "PRIVATE KEY") || strings.Contains(got, "generated-tls") {
		t.Fatalf("sanitized error exposes API details: %q", got)
	}
}
