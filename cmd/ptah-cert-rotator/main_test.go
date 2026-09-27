package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeHTTPServer struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s *fakeHTTPServer) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

func (s *fakeHTTPServer) Close() error {
	return s.close()
}

func TestStopHTTPServersForcesCloseAfterShutdownError(t *testing.T) {
	t.Parallel()

	closeCalls := 0
	server := &fakeHTTPServer{
		shutdown: func(context.Context) error {
			return context.DeadlineExceeded
		},
		close: func() error {
			closeCalls++
			return nil
		},
	}
	err := stopHTTPServers([]namedHTTPServer{{name: "test", server: server}}, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopHTTPServers() error = %v, want context deadline exceeded", err)
	}
	if closeCalls != 1 {
		t.Fatalf("Close() calls = %d, want 1", closeCalls)
	}
}

func TestStopHTTPServersHonorsShutdownDeadline(t *testing.T) {
	t.Parallel()

	closeCalls := 0
	server := &fakeHTTPServer{
		shutdown: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		close: func() error {
			closeCalls++
			return nil
		},
	}
	started := time.Now()
	err := stopHTTPServers([]namedHTTPServer{{name: "test", server: server}}, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("stopHTTPServers() error = %v, want shutdown deadline", err)
	}
	if closeCalls != 1 {
		t.Fatalf("Close() calls = %d, want 1", closeCalls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopHTTPServers() took %s, want a bounded return", elapsed)
	}
}

func TestParseFlagsTiesTheCASwitchDelayToTheRunInterval(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "unset follows the run interval", args: []string{"--run-interval=3h"}, want: 3 * time.Hour},
		{name: "default run interval", want: 6 * time.Hour},
		{name: "explicit delay", args: []string{"--run-interval=168h", "--ca-switch-delay=30s"}, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, supervisor, _, err := parseFlags(append(testRotatorArgs(), test.args...))
			if err != nil {
				t.Fatalf("parseFlags() error = %v", err)
			}
			if config.CASwitchDelay != test.want {
				t.Fatalf("CA switch delay = %s, want %s (run interval %s)", config.CASwitchDelay, test.want, supervisor.RunInterval)
			}
		})
	}
}

// The rotator stopped proving a CA transition through an admission canary, and
// the chart stopped passing the canary's arguments with it. A manifest that
// still passes one is refused rather than silently ignored.
func TestParseFlagsRefusesTheRetiredCanaryArguments(t *testing.T) {
	t.Parallel()

	for _, argument := range []string{
		"--candidate-service-name=ptah-cert-transition",
		"--candidate-bind-address=:9444",
		"--candidate-probe-config-map-name=ptah-cert-canary",
		"--candidate-probe-username=system:serviceaccount:ptah-system:ptah-cert-rotator",
		"--candidate-mutating-field-manager=ptah-certificate-rotation-canary-mutate-v1",
		"--candidate-validating-field-manager=ptah-certificate-rotation-canary-validate-v1",
		"--candidate-stability-duration=10s",
		"--candidate-poll-interval=1s",
		"--candidate-request-timeout=5s",
	} {
		if _, _, _, err := parseFlags(append(testRotatorArgs(), argument)); err == nil {
			t.Errorf("parseFlags() accepted the retired argument %s", argument)
		}
	}
	if _, _, _, err := parseFlags(testRotatorArgs()); err != nil {
		t.Fatalf("parseFlags() rejected the chart's arguments: %v", err)
	}
}

func TestParseFlagsRejectsASwitchThatCannotFitTheRenewalThreshold(t *testing.T) {
	t.Parallel()

	_, _, _, err := parseFlags(append(testRotatorArgs(), "--renewal-threshold=720h", "--ca-switch-delay=714h"))
	if err == nil || !strings.Contains(err.Error(), "CA switch delay") {
		t.Fatalf("parseFlags() error = %v, want the CA switch delay refused", err)
	}
}

// testRotatorArgs are the non-default arguments every rotator needs; parseFlags
// validates timing relationships but leaves names to certrotation.New.
func testRotatorArgs() []string {
	return []string{"--namespace=ptah-system"}
}
