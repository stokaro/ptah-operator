package main

import (
	"net/http"
	"sync/atomic"
)

type probeState struct {
	live  atomic.Bool
	ready atomic.Bool
}

func (s *probeState) setLive(value bool) {
	s.live.Store(value)
}

func (s *probeState) setReady(value bool) {
	s.ready.Store(value)
}

func (s *probeState) handler(others ...*probeState) http.Handler {
	states := append([]*probeState{s}, others...)
	healthy := func(ready bool) bool {
		for _, state := range states {
			if !state.live.Load() || (ready && !state.ready.Load()) {
				return false
			}
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		serveProbe(writer, request, healthy(false))
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, request *http.Request) {
		serveProbe(writer, request, healthy(true))
	})
	return mux
}

func serveProbe(writer http.ResponseWriter, request *http.Request, healthy bool) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !healthy {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("unavailable\n"))
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("ok\n"))
}
