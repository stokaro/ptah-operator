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

func TestCandidateRuntimeRelationshipValidation(t *testing.T) {
	t.Parallel()

	valid := func() error {
		return validateCandidateRuntimeRelationships(
			15*time.Minute,
			30*time.Second,
			5*time.Minute,
			10*time.Second,
			time.Second,
			5*time.Second,
		)
	}
	if err := valid(); err != nil {
		t.Fatalf("valid candidate timing error = %v", err)
	}
	for _, test := range []struct {
		name string
		call func() error
		want string
	}{
		{name: "stability", call: func() error {
			return validateCandidateRuntimeRelationships(time.Minute, time.Second, time.Second, 0, time.Second, time.Second)
		}, want: "timing values must be positive"},
		{name: "poll", call: func() error {
			return validateCandidateRuntimeRelationships(time.Minute, time.Second, time.Second, time.Second, 0, time.Second)
		}, want: "timing values must be positive"},
		{name: "request", call: func() error {
			return validateCandidateRuntimeRelationships(time.Minute, time.Second, time.Second, time.Second, time.Second, 0)
		}, want: "timing values must be positive"},
		{name: "request operation bound", call: func() error {
			return validateCandidateRuntimeRelationships(time.Second, time.Second, time.Second, time.Second, time.Second, time.Second)
		}, want: "shorter than the operation timeout"},
		{name: "stability operation bound", call: func() error {
			return validateCandidateRuntimeRelationships(10*time.Second, time.Second, time.Second, 10*time.Second, time.Second, time.Second)
		}, want: "three candidate admission stability barriers"},
		{name: "poll operation bound", call: func() error {
			return validateCandidateRuntimeRelationships(10*time.Second, time.Second, time.Second, time.Second, 10*time.Second, time.Second)
		}, want: "three candidate admission stability barriers"},
		{name: "complete operation budget", call: func() error {
			return validateCandidateRuntimeRelationships(36*time.Second, time.Second, 10*time.Second, 5*time.Second, 5*time.Second, time.Second)
		}, want: "three candidate admission stability barriers"},
		{name: "barrier overflow", call: func() error {
			return validateCandidateRuntimeRelationships(
				time.Duration(1<<63-1),
				time.Second,
				time.Second,
				time.Duration(1<<63-2),
				time.Duration(1<<62),
				time.Second,
			)
		}, want: "stability barrier exceeds"},
		{name: "budget overflow", call: func() error {
			return validateCandidateRuntimeRelationships(
				time.Duration(1<<63-1),
				time.Duration(1<<62),
				time.Duration(1<<62),
				time.Second,
				time.Second,
				time.Second,
			)
		}, want: "operation budget exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.call(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestCandidateAdmissionBarrierFloorRoundsToPollBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		stability time.Duration
		poll      time.Duration
		want      time.Duration
	}{
		{name: "exact", stability: 10 * time.Second, poll: 5 * time.Second, want: 10 * time.Second},
		{name: "rounded", stability: 11 * time.Second, poll: 5 * time.Second, want: 15 * time.Second},
		{name: "first retry", stability: time.Second, poll: 10 * time.Second, want: 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := candidateAdmissionBarrierFloor(test.stability, test.poll)
			if err != nil || got != test.want {
				t.Fatalf("candidateAdmissionBarrierFloor(%s, %s) = %s, %v; want %s", test.stability, test.poll, got, err, test.want)
			}
		})
	}
}
