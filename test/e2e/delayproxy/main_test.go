package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer collects the connection log written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) events(t *testing.T) []event {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []event
	scanner := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("connection log line %q: %v", scanner.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// echoUpstream records when each connection reached it and echoes its bytes.
func echoUpstream(t *testing.T) (string, <-chan time.Time) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	arrivals := make(chan time.Time, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			arrivals <- time.Now()
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String(), arrivals
}

func startProxy(t *testing.T, upstream string, delay time.Duration, output io.Writer) (*proxy, string) {
	t.Helper()
	p := newProxy(upstream, delay, time.Second, output)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.serve(listener) }()
	t.Cleanup(func() { listener.Close() })
	return p, listener.Addr().String()
}

func TestEveryConnectionReachesTheDatabaseOnlyAfterTheDelay(t *testing.T) {
	upstream, arrivals := echoUpstream(t)
	var output lockedBuffer
	const delay = 300 * time.Millisecond
	p, address := startProxy(t, upstream, delay, &output)

	dialed := time.Now()
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case arrived := <-arrivals:
		if waited := arrived.Sub(dialed); waited < delay {
			t.Fatalf("the database saw the connection after %s, before the %s delay", waited, delay)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection never reached the database")
	}
	if _, err := client.Write([]byte("SELECT 1")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len("SELECT 1"))
	if _, err := io.ReadFull(client, reply); err != nil || string(reply) != "SELECT 1" {
		t.Fatalf("the proxied session returned %q, %v", reply, err)
	}
	client.Close()
	if !p.drain(5 * time.Second) {
		t.Fatal("a closed session kept the proxy from draining")
	}

	events := output.events(t)
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.Event)
	}
	if strings.Join(kinds, ",") != "accepted,connected,closed" {
		t.Fatalf("connection log = %v", kinds)
	}
	if events[1].DelayedSeconds < delay.Seconds() || events[2].ClientBytes != int64(len("SELECT 1")) || events[2].ServerBytes != int64(len("SELECT 1")) {
		t.Fatalf("the log does not account for the delay and the bytes: %+v", events)
	}
}

func TestAnOpenSessionHoldsTheDrain(t *testing.T) {
	upstream, arrivals := echoUpstream(t)
	p, address := startProxy(t, upstream, 10*time.Millisecond, io.Discard)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	<-arrivals
	if p.drain(200 * time.Millisecond) {
		t.Fatal("the proxy drained while a session was still open")
	}
	client.Close()
	if !p.drain(5 * time.Second) {
		t.Fatal("the proxy did not drain after the session closed")
	}
}

func TestAnUnreachableDatabaseIsLoggedAndTheClientClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := listener.Addr().String()
	listener.Close()
	var output lockedBuffer
	p, address := startProxy(t, unreachable, 10*time.Millisecond, &output)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the client read %v, want the proxy to close it", err)
	}
	if !p.drain(5 * time.Second) {
		t.Fatal("a failed dial left the connection open")
	}
	events := output.events(t)
	if len(events) != 2 || events[1].Event != "dial-failed" || events[1].Error == "" {
		t.Fatalf("connection log = %+v", events)
	}
}

func TestTheReadinessProbeReadsTheHealthEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	if code := checkReady(address); code != 0 {
		t.Fatalf("a serving proxy probed as %d", code)
	}
	server.Close()
	if code := checkReady(address); code != 1 {
		t.Fatalf("a stopped proxy probed as %d", code)
	}
}
