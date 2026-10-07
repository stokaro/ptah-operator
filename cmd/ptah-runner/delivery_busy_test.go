package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A transient refusal may outlast four one-second attempts while remaining far
// inside the runner's existing two-minute delivery deadline.
func TestRunnerWaitsForRecoverableReceiverBusyWindow(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	var started atomic.Int64
	var attempts atomic.Int64
	server := f.server(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				now := time.Now()
				started.CompareAndSwap(0, now.UnixNano())
				attempts.Add(1)
				if now.Before(time.Unix(0, started.Load()).Add(5 * time.Second)) {
					w.Header().Set("Retry-After", "1")
					http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	executable, counter := applyExecutable(t)
	terminationLog := filepath.Join(t.TempDir(), "termination-log")
	if err := os.WriteFile(terminationLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(f.environment(t), "PTAH_TEST_INVOCATIONS="+counter), terminationLog)
	invocations, err := os.ReadFile(counter)
	if err != nil || string(invocations) != "run\n" {
		t.Fatalf("Apply invocations=%q, error=%v", invocations, err)
	}
	if code != 0 {
		t.Fatalf("delivery stopped after %s with %d attempts during a five-second busy window: exit=%d, %s", time.Since(time.Unix(0, started.Load())).Round(time.Millisecond), attempts.Load(), code, stderr.String())
	}
	if _, _, err := f.store.Load(t.Context(), f.identity.Binding); err != nil {
		t.Fatalf("delivery returned success without a durable receipt: %v", err)
	}
}
