// Command logstall serves one deliberately unfinished kubelet log response.
// It runs only in the isolated node's network namespace during acceptance.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	output io.Writer
}

func (r *recorder) record(state, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = json.NewEncoder(r.output).Encode(map[string]any{"state": state, "path": path, "at": time.Now().UTC()})
}

func stalledLogHandler(path string, records *recorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		records.record("started", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "e2e: result response held open\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			records.record("flush-failed", r.URL.Path)
			return
		}
		<-r.Context().Done()
		records.record("canceled", r.URL.Path)
	})
}

func run() error {
	listen := flag.String("listen", ":10443", "address in the isolated node's network namespace")
	cert := flag.String("cert", "/tls.crt", "isolated kubelet serving certificate")
	key := flag.String("key", "/tls.key", "isolated kubelet serving key")
	path := flag.String("path", "", "exact containerLogs path to stall")
	lifetime := flag.Duration("lifetime", 10*time.Minute, "maximum lifetime even if the harness disappears")
	flag.Parse()
	if *path == "" || *lifetime <= 0 {
		return fmt.Errorf("path and a positive lifetime are required")
	}
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	records := &recorder{output: os.Stdout}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, *lifetime)
	defer deadline()
	server := &http.Server{
		Handler: stalledLogHandler(*path, records), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}},
	}
	go func() { <-ctx.Done(); _ = server.Close() }()
	records.record("listening", *path)
	err = server.Serve(tls.NewListener(listener, server.TLSConfig))
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
