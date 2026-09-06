package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeHandlerTracksLivenessAndLatestSuccess(t *testing.T) {
	t.Parallel()

	state := &probeState{}
	handler := state.handler()
	assertProbeStatus(t, handler, "/healthz", http.StatusServiceUnavailable)
	assertProbeStatus(t, handler, "/readyz", http.StatusServiceUnavailable)

	state.setLive(true)
	assertProbeStatus(t, handler, "/healthz", http.StatusOK)
	assertProbeStatus(t, handler, "/readyz", http.StatusServiceUnavailable)

	state.setReady(true)
	assertProbeStatus(t, handler, "/healthz", http.StatusOK)
	assertProbeStatus(t, handler, "/readyz", http.StatusOK)

	state.setLive(false)
	assertProbeStatus(t, handler, "/healthz", http.StatusServiceUnavailable)
	assertProbeStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
}

func TestProbeHandlerRestrictsMethodsAndPaths(t *testing.T) {
	t.Parallel()

	state := &probeState{}
	state.setLive(true)
	handler := state.handler()

	request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if got := response.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("POST /healthz Allow = %q, want %q", got, "GET, HEAD")
	}

	assertProbeStatus(t, handler, "/unknown", http.StatusNotFound)
}

func assertProbeStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s status = %d, want %d; body = %q", path, response.Code, want, response.Body.String())
	}
	if path == "/healthz" || path == "/readyz" {
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}
}
