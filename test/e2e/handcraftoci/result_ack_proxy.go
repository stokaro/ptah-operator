package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
)

const resultDigestHeader = "X-Ptah-Result-Digest"

type ackReceipt struct {
	Name, UID, Digest string
	Size              int64
}

type ackAttempt struct {
	ReceivedAt time.Time  `json:"receivedAt"`
	Receipt    ackReceipt `json:"receipt"`
}

type ackEvidence struct {
	FirstGateEnabled        bool             `json:"firstGateEnabled,omitempty"`
	FirstWaits              int              `json:"firstWaits,omitempty"`
	FirstResumedAt          *time.Time       `json:"firstResumedAt,omitempty"`
	FirstAdmissions         []firstAdmission `json:"firstAdmissions,omitempty"`
	ClientCertificateDigest string           `json:"clientCertificateDigest,omitempty"`
	Authentication          string           `json:"authentication,omitempty"`
	IdentityDigest          string           `json:"identityDigest,omitempty"`
	JobUID                  string           `json:"jobUID,omitempty"`
	PodUID                  string           `json:"podUID,omitempty"`
	Preflights              int              `json:"preflights"`
	Attempts                []ackAttempt     `json:"attempts"`
	Dropped                 bool             `json:"dropped"`
	Released                bool             `json:"released"`
	RetryGateEnabled        bool             `json:"retryGateEnabled,omitempty"`
	RetryWaits              int              `json:"retryWaits,omitempty"`
	RetryResumedAt          *time.Time       `json:"retryResumedAt,omitempty"`
}

// Only the disposable fixture holds these keys. It forwards one original Pod's
// identity to the real receiver, drops its first successful PUT response, and
// holds the identical retry until the harness restores the ordinary Service.
// Neither payload bytes nor credentials appear in its evidence or errors.
type resultACKProxy struct {
	firstNamespace  string
	pending         *firstPending
	resumeFirst     chan struct{}
	firstAdmissions chan struct{}
	client          *http.Client
	origin          string
	clientLeaf      []byte
	jobUID          string
	mu              sync.Mutex
	evidence        ackEvidence
	slot            chan struct{}
	release         chan struct{}
	resumeRetry     chan struct{}
}

func newResultACKProxy(client *http.Client, origin string, clientLeaf []byte) *resultACKProxy {
	return &resultACKProxy{resumeFirst: make(chan struct{}), firstAdmissions: make(chan struct{}), client: client, origin: origin, clientLeaf: bytes.Clone(clientLeaf), slot: make(chan struct{}, 1), release: make(chan struct{}), resumeRetry: make(chan struct{}), evidence: ackEvidence{ClientCertificateDigest: digest(clientLeaf), Attempts: []ackAttempt{}}}
}

func newResultACKTokenProxy(client *http.Client, origin, jobUID string) *resultACKProxy {
	p := newResultACKProxy(client, origin, nil)
	p.jobUID = jobUID
	p.evidence.ClientCertificateDigest = ""
	p.evidence.Authentication = "pod-token"
	return p
}

// This only restricts the fixture to the intended Job. The real receiver must
// authenticate the forwarded token before any preflight or receipt is counted.
func (p *resultACKProxy) tokenClaim(r *http.Request) (string, string, bool) {
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 {
		return "", "", false
	}
	authorization := r.Header.Values("Authorization")
	identity := r.Header.Values("X-Ptah-Result-Identity")
	if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") ||
		!resultdelivery.ValidToken(strings.TrimPrefix(authorization[0], "Bearer ")) ||
		len(identity) != 1 || len(identity[0]) > 4<<10 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(identity[0])
	var claim struct {
		Binding struct{ JobUID, PodUID string } `json:"binding"`
	}
	if err != nil || json.Unmarshal(raw, &claim) != nil || claim.Binding.JobUID != p.jobUID || claim.Binding.PodUID == "" {
		return "", "", false
	}
	identityDigest := digest(raw)
	p.mu.Lock()
	pinned := p.evidence.IdentityDigest
	p.mu.Unlock()
	return claim.Binding.PodUID, identityDigest, (pinned == "" && r.Method == http.MethodHead) || pinned == identityDigest
}

func (p *resultACKProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var podUID, identityDigest string
	valid := false
	if p.jobUID != "" {
		podUID, identityDigest, valid = p.tokenClaim(r)
	} else {
		valid = r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) == 1 && bytes.Equal(r.TLS.PeerCertificates[0].Raw, p.clientLeaf)
	}
	if !valid {
		http.Error(w, "wrong fixture identity", http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" || !strings.HasPrefix(r.URL.Path, "/v1/results/") || (r.Method != http.MethodHead && r.Method != http.MethodPut) {
		http.Error(w, "wrong fixture request", http.StatusBadRequest)
		return
	}
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	default:
		http.Error(w, "fixture busy", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 50_397_184))
	if err != nil || r.ContentLength != int64(len(body)) || (r.Method == http.MethodHead && len(body) != 0) || (r.Method == http.MethodPut && (len(body) == 0 || r.Header.Get(resultDigestHeader) != digest(body))) {
		http.Error(w, "invalid fixture payload", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	count := len(p.evidence.Attempts)
	conflict := count > 0 && (count >= 2 || p.evidence.Attempts[0].Receipt.Digest != digest(body))
	p.mu.Unlock()
	if r.Method == http.MethodPut && conflict {
		http.Error(w, "unexpected fixture retry", http.StatusConflict)
		return
	}
	if r.Method == http.MethodPut && count == 0 {
		p.mu.Lock()
		paused := p.evidence.FirstGateEnabled && p.evidence.FirstResumedAt == nil
		if paused {
			if p.pending != nil && (p.pending.Path != r.URL.Path || p.pending.Digest != digest(body)) {
				p.mu.Unlock()
				http.Error(w, "first fixture payload changed", http.StatusConflict)
				return
			}
			p.pending = &firstPending{Path: r.URL.Path, Payload: body, Digest: digest(body)}
			p.evidence.FirstWaits++
		}
		p.mu.Unlock()
		if paused {
			select {
			case <-p.resumeFirst:
			case <-r.Context().Done():
				return
			}
		}
	}
	if r.Method == http.MethodPut && count == 1 {
		p.mu.Lock()
		paused := p.evidence.RetryGateEnabled && p.evidence.RetryResumedAt == nil
		if paused {
			p.evidence.RetryWaits++
		}
		p.mu.Unlock()
		if paused {
			select {
			case <-p.resumeRetry:
			case <-r.Context().Done():
				return
			}
		}
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "fixture request failed", http.StatusBadGateway)
		return
	}
	request.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	request.Header.Set(resultDigestHeader, r.Header.Get(resultDigestHeader))
	if p.jobUID != "" {
		request.Header.Set("Authorization", r.Header.Get("Authorization"))
		request.Header.Set("X-Ptah-Result-Identity", r.Header.Get("X-Ptah-Result-Identity"))
	}
	response, err := p.client.Do(request)
	if err != nil {
		http.Error(w, "fixture upstream failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if r.Method == http.MethodHead {
		if response.StatusCode == http.StatusNoContent {
			p.mu.Lock()
			if p.jobUID != "" {
				p.evidence.JobUID, p.evidence.PodUID, p.evidence.IdentityDigest = p.jobUID, podUID, identityDigest
			}
			p.evidence.Preflights++
			p.mu.Unlock()
		}
		w.WriteHeader(response.StatusCode)
		return
	}
	if response.StatusCode != http.StatusOK {
		// A refusal or storage failure must never count as a lost acknowledgment.
		w.WriteHeader(response.StatusCode)
		return
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	var receipt ackReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err != nil || len(encoded) > 4096 || response.Header.Get("Content-Type") != "application/json" || decoder.Decode(&receipt) != nil || receipt.UID == "" || (receipt.Name != strings.TrimPrefix(r.URL.Path, "/v1/results/")+"-complete" && receipt.Name != strings.TrimPrefix(r.URL.Path, "/v1/results/")) || receipt.Digest != digest(body) || receipt.Size != int64(len(body)) {
		http.Error(w, "invalid fixture receipt", http.StatusBadGateway)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "invalid fixture receipt", http.StatusBadGateway)
		return
	}
	p.mu.Lock()
	if count > 0 && receipt != p.evidence.Attempts[0].Receipt {
		p.mu.Unlock()
		http.Error(w, "fixture receipt changed", http.StatusBadGateway)
		return
	}
	p.evidence.Attempts = append(p.evidence.Attempts, ackAttempt{ReceivedAt: time.Now().UTC(), Receipt: receipt})
	p.mu.Unlock()
	if count == 0 {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, "fixture cannot drop response", http.StatusInternalServerError)
			return
		}
		_ = conn.Close() // No status or response bytes reached the original runner.
		p.mu.Lock()
		p.evidence.Dropped = true
		p.mu.Unlock()
		return
	}
	select {
	case <-p.release:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	case <-r.Context().Done():
	}
}

func (p *resultACKProxy) admin(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/pending-first":
		if !p.evidence.FirstGateEnabled || p.pending == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.pending)
	case r.Method == http.MethodPost && r.URL.Path == "/resume-first":
		if !p.evidence.FirstGateEnabled || p.evidence.FirstWaits == 0 || len(p.evidence.FirstAdmissions) != 2 || p.evidence.FirstAdmissions[0].ReleasedAt == nil || p.evidence.FirstAdmissions[1].ReleasedAt == nil {
			http.Error(w, "first publication is not ready", http.StatusConflict)
			return
		}
		if p.evidence.FirstResumedAt == nil {
			now := time.Now().UTC()
			p.evidence.FirstResumedAt = &now
			p.pending = nil
			close(p.resumeFirst)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/evidence":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.evidence)
	case r.Method == http.MethodPost && r.URL.Path == "/resume-retry":
		if !p.evidence.RetryGateEnabled || !p.evidence.Dropped || p.evidence.RetryWaits == 0 {
			http.Error(w, "retry is not paused", http.StatusConflict)
			return
		}
		if p.evidence.RetryResumedAt == nil {
			now := time.Now().UTC()
			p.evidence.RetryResumedAt = &now
			close(p.resumeRetry)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/release":
		if !p.evidence.Dropped || len(p.evidence.Attempts) != 2 {
			http.Error(w, "retry is not ready", http.StatusConflict)
			return
		}
		if !p.evidence.Released {
			close(p.release)
			p.evidence.Released = true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func runResultACKProxy(args []string) error {
	flags := flag.NewFlagSet("result-ack-proxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	backend := flags.String("backend-address", "", "")
	serverName := flags.String("server-name", "", "")
	trustDir := flags.String("trust-directory", "", "")
	credentialDir := flags.String("credential-directory", "", "")
	jobUID := flags.String("job-uid", "", "")
	pauseRetry := flags.Bool("pause-retry", false, "")
	pauseFirst := flags.String("pause-first-namespace", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *serverName == "" || strings.ContainsAny(*serverName, "/:@?#") || !filepath.IsAbs(*trustDir) ||
		(*jobUID == "" && !filepath.IsAbs(*credentialDir)) || (*jobUID != "" && (*credentialDir != "" || *pauseFirst != "" || len(*jobUID) > 128 || strings.ContainsAny(*jobUID, " \t\r\n"))) {
		return errors.New("invalid result ACK proxy configuration")
	}
	if _, _, err := net.SplitHostPort(*backend); err != nil {
		return errors.New("invalid result ACK proxy backend")
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(*trustDir, "tls.crt"), filepath.Join(*trustDir, "tls.key"))
	if err != nil {
		return errors.New("cannot load result ACK proxy server certificate")
	}
	readPool := func(path string) (*x509.CertPool, error) {
		data, err := os.ReadFile(path)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("cannot load result ACK proxy trust")
		}
		return pool, nil
	}
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: *serverName}
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}, NextProtos: []string{"http/1.1"}}
	var clientLeaf []byte
	caDirectory := *trustDir
	if *jobUID == "" {
		credential, err := tls.LoadX509KeyPair(filepath.Join(*credentialDir, "tls.crt"), filepath.Join(*credentialDir, "tls.key"))
		if err != nil {
			return errors.New("cannot load result ACK proxy client certificate")
		}
		clientRoots, err := readPool(filepath.Join(*trustDir, "client-trust.crt"))
		if err != nil {
			return err
		}
		clientLeaf = credential.Certificate[0]
		clientTLS.Certificates = []tls.Certificate{credential}
		serverTLS.ClientAuth, serverTLS.ClientCAs = tls.RequireAndVerifyClientCert, clientRoots
		caDirectory = *credentialDir
	}
	serverRoots, err := readPool(filepath.Join(caDirectory, "ca.crt"))
	if err != nil {
		return err
	}
	clientTLS.RootCAs = serverRoots
	transport := &http.Transport{TLSClientConfig: clientTLS, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", *backend)
	}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 20 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture refuses redirects") }}
	proxy := newResultACKProxy(client, "https://"+*serverName, clientLeaf)
	if *jobUID != "" {
		proxy = newResultACKTokenProxy(client, "https://"+*serverName, *jobUID)
	}
	proxy.evidence.RetryGateEnabled = *pauseRetry
	proxy.evidence.FirstGateEnabled = *pauseFirst != ""
	proxy.firstNamespace = *pauseFirst
	dataServer := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0), TLSConfig: serverTLS}
	adminServer := &http.Server{Handler: http.HandlerFunc(proxy.admin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 4 << 10}
	dataListener, err := net.Listen("tcp", ":9444")
	if err != nil {
		return errors.New("cannot listen for result ACK proxy traffic")
	}
	defer dataListener.Close()
	adminListener, err := net.Listen("tcp", "127.0.0.1:8081")
	if err != nil {
		return errors.New("cannot listen for result ACK proxy control")
	}
	defer adminListener.Close()
	defer dataServer.Close()
	defer adminServer.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	failures := make(chan error, 3)
	if *pauseFirst != "" {
		listener, err := net.Listen("tcp", ":9445")
		if err != nil {
			return errors.New("cannot listen for publication barrier")
		}
		defer listener.Close()
		barrierServer := &http.Server{Handler: http.HandlerFunc(proxy.publicationBarrier), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 12 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}}}
		defer barrierServer.Close()
		go func() { failures <- barrierServer.ServeTLS(listener, "", "") }()
	}
	go func() { failures <- dataServer.ServeTLS(dataListener, "", "") }()
	go func() { failures <- adminServer.Serve(adminListener) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-failures:
		return errors.New("result ACK proxy listener stopped")
	}
}
