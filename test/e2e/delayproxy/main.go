// Command delayproxy stands in front of a database and holds every new
// connection for a declared delay before reaching it. The capacity harness
// points the database Service at it to measure a slow dependency without
// creating any object in the databases the operator manages: a function or
// trigger there would be schema the operator then plans to remove.
//
// Each connection writes JSON lines when it is accepted, connected and closed,
// so the evidence of the delay is the proxy's own account of each session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// event is one line of the log.
type event struct {
	Event          string    `json:"event"`
	Connection     int64     `json:"connection"`
	Client         string    `json:"client,omitempty"`
	At             time.Time `json:"at"`
	DelayedSeconds float64   `json:"delayedSeconds,omitempty"`
	ClientBytes    int64     `json:"clientBytes,omitempty"`
	ServerBytes    int64     `json:"serverBytes,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type proxy struct {
	upstream    string
	delay       time.Duration
	dialTimeout time.Duration
	now         func() time.Time

	mu     sync.Mutex
	output *json.Encoder

	open atomic.Int64
	ids  atomic.Int64
}

func newProxy(upstream string, delay, dialTimeout time.Duration, output io.Writer) *proxy {
	return &proxy{upstream: upstream, delay: delay, dialTimeout: dialTimeout, now: time.Now, output: json.NewEncoder(output)}
}

func (p *proxy) record(e event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.output.Encode(e); err != nil {
		log.Printf("write connection log: %v", err)
	}
}

// serve accepts until the listener closes. A connection already accepted is
// carried to its end even after the listener closes: dropping it would add a
// fault of its own to the one being measured.
func (p *proxy) serve(listener net.Listener) error {
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		p.open.Add(1)
		go func() {
			defer p.open.Add(-1)
			p.handle(client)
		}()
	}
}

func (p *proxy) handle(client net.Conn) {
	defer client.Close()
	id := p.ids.Add(1)
	accepted := p.now().UTC()
	p.record(event{Event: "accepted", Connection: id, Client: client.RemoteAddr().String(), At: accepted})
	time.Sleep(p.delay)
	server, err := net.DialTimeout("tcp", p.upstream, p.dialTimeout)
	if err != nil {
		p.record(event{Event: "dial-failed", Connection: id, At: p.now().UTC(), Error: err.Error()})
		return
	}
	defer server.Close()
	connected := p.now().UTC()
	p.record(event{Event: "connected", Connection: id, At: connected, DelayedSeconds: connected.Sub(accepted).Seconds()})

	var clientBytes, serverBytes int64
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		clientBytes, _ = io.Copy(server, client)
		closeWrite(server)
	}()
	go func() {
		defer copies.Done()
		serverBytes, _ = io.Copy(client, server)
		closeWrite(client)
	}()
	copies.Wait()
	p.record(event{Event: "closed", Connection: id, At: p.now().UTC(), ClientBytes: clientBytes, ServerBytes: serverBytes})
}

// closeWrite passes one side's end of stream to the other and leaves the
// reverse direction open, the way a database protocol finishes a session.
func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	_ = conn.Close()
}

// drain waits for every accepted connection to end, and reports whether all of
// them did before the timeout. The count is read rather than waited on, so a
// connection accepted while a drain is already waiting is still counted.
func (p *proxy) drain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for p.open.Load() > 0 {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func main() {
	listen := flag.String("listen", ":5432", "address the database clients connect to")
	upstream := flag.String("upstream", "", "the database, as host:port")
	delay := flag.Duration("delay", 30*time.Second, "how long each new connection waits before reaching the database")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "bound on reaching the database once the delay has passed")
	health := flag.String("health", ":8081", "address of the readiness endpoint, kept apart so a probe opens no delayed session")
	drainTimeout := flag.Duration("drain", 14*time.Minute, "how long a stop waits for open sessions to end")
	probe := flag.String("probe", "", "check a running proxy's readiness endpoint at host:port and exit; the image has no shell for an exec probe")
	flag.Parse()
	log.SetOutput(os.Stderr)
	if *probe != "" {
		os.Exit(checkReady(*probe))
	}
	if *upstream == "" || *delay <= 0 || *dialTimeout <= 0 || *drainTimeout <= 0 {
		log.Fatal("delayproxy needs -upstream and positive -delay, -dial-timeout and -drain")
	}

	p := newProxy(*upstream, *delay, *dialTimeout, os.Stdout)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	healthServer := &http.Server{Addr: *health, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "open=%d\n", p.open.Load())
	})}
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- p.serve(listener) }()
	select {
	case err := <-served:
		log.Fatal(err)
	case <-stop.Done():
	}
	_ = listener.Close()
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = healthServer.Shutdown(shutdown)
	if !p.drain(*drainTimeout) {
		log.Printf("stopped with %d sessions still open", p.open.Load())
		os.Exit(1)
	}
}

// checkReady is the readiness probe. A network probe from the kubelet would
// need its own way through the database's ingress policy, which the proxy
// shares; a probe run inside the container needs none.
func checkReady(address string) int {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
