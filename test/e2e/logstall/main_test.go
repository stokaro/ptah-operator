package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLogResponseStartsButNeverCompletes(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(stalledLogHandler("/containerLogs/ns/pod/ptah", &recorder{output: io.Discard}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/containerLogs/ns/pod/ptah", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	prefix := make([]byte, len("e2e: result response held open\n"))
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read ended without the caller's deadline: %v", err)
	}
}

func TestOtherPodIsNotStalled(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	stalledLogHandler("/containerLogs/ns/pod/ptah", &recorder{output: io.Discard}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/containerLogs/ns/other/ptah", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status %d", response.Code)
	}
}
