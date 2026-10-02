package resultdelivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultstore"
)

type RetryPolicy struct {
	Attempts       int
	Interval       time.Duration
	AttemptTimeout time.Duration
	TotalTimeout   time.Duration
}

// Sender holds only delivery configuration, not an executor or SQL callback.
// Send retries the one immutable payload it was handed.
type Sender struct {
	client    *http.Client
	transport *http.Transport
	endpoint  string
	identity  Identity
	retry     RetryPolicy
}

// NewSender requires authenticated TLS in both directions and refuses redirects.
// endpoint is the receiver origin, with no path, query, or user information.
func NewSender(endpoint string, identity Identity, tlsConfig *tls.Config, retry RetryPolicy) (*Sender, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") ||
		!identity.valid() || tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs == nil ||
		len(tlsConfig.Certificates) != 1 || tlsConfig.Certificates[0].PrivateKey == nil ||
		retry.Attempts < 1 || retry.Attempts > 8 || retry.Interval <= 0 || retry.Interval > 30*time.Second ||
		retry.AttemptTimeout <= 0 || retry.TotalTimeout < retry.AttemptTimeout || retry.TotalTimeout > 5*time.Minute {
		return nil, errors.New("invalid result sender configuration")
	}
	if tlsConfig.MaxVersion != 0 && tlsConfig.MaxVersion < tls.VersionTLS13 {
		return nil, errors.New("result delivery requires TLS 1.3")
	}
	if tlsConfig.ServerName != "" && tlsConfig.ServerName != parsed.Hostname() {
		return nil, ErrAuthority
	}
	bound, err := ClientIdentity(tlsConfig.Certificates[0])
	if err != nil || bound != identity {
		return nil, ErrAuthority
	}
	config := tlsConfig.Clone()
	config.MinVersion = tls.VersionTLS13
	config.ServerName = parsed.Hostname()
	transport := &http.Transport{TLSClientConfig: config, TLSHandshakeTimeout: retry.AttemptTimeout,
		ResponseHeaderTimeout: retry.AttemptTimeout, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1,
		IdleConnTimeout: 30 * time.Second, DisableCompression: true}
	name, _ := resultstore.Name(identity.Binding)
	parsed.Path = PathPrefix + name
	return &Sender{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		transport: transport, endpoint: parsed.String(), identity: identity, retry: retry}, nil
}

func (s *Sender) Close() { s.transport.CloseIdleConnections() }

// Send never changes bytes between attempts, never follows a redirect, and
// returns no payload or server response body in an error. A lost response after
// durable persistence may be retried; a definitive refusal is returned at once.
func (s *Sender) Send(parent context.Context, payload []byte) (resultstore.Receipt, error) {
	ctx, cancel := context.WithTimeout(parent, s.retry.TotalTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return resultstore.Receipt{}, err
	}
	if len(payload) == 0 || int64(len(payload)) > resultstore.MaxPayloadBytes {
		return resultstore.Receipt{}, ErrPayload
	}
	payload = bytes.Clone(payload)
	if _, err := Decode(s.identity, payload); err != nil {
		return resultstore.Receipt{}, err
	}
	digest := payloadDigest(payload)
	for attempt := 0; attempt < s.retry.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return resultstore.Receipt{}, err
		}
		attemptCtx, stop := context.WithTimeout(ctx, s.retry.AttemptTimeout)
		receipt, retry, err := s.send(attemptCtx, payload, digest)
		stop()
		if err == nil || !retry {
			return receipt, err
		}
		if attempt+1 == s.retry.Attempts {
			return resultstore.Receipt{}, errors.New("result delivery attempts exhausted")
		}
		timer := time.NewTimer(s.retry.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return resultstore.Receipt{}, ctx.Err()
		case <-timer.C:
		}
	}
	return resultstore.Receipt{}, errors.New("result delivery attempts exhausted")
}

func (s *Sender) send(ctx context.Context, payload []byte, digest string) (resultstore.Receipt, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return resultstore.Receipt{}, false, errors.New("cannot prepare result delivery")
	}
	request.Header.Set("Content-Type", ContentType)
	request.Header.Set(DigestHeader, digest)
	response, err := s.client.Do(request)
	if err != nil {
		return resultstore.Receipt{}, true, errors.New("result receiver unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode == http.StatusInternalServerError || response.StatusCode == http.StatusBadGateway ||
			response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout
		return resultstore.Receipt{}, retry, fmt.Errorf("result receiver returned HTTP %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		return resultstore.Receipt{}, false, errors.New("result receipt content type is invalid")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(encoded) > 4096 {
		return resultstore.Receipt{}, true, errors.New("result receipt is incomplete or too large")
	}
	var receipt resultstore.Receipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return resultstore.Receipt{}, true, errors.New("result receipt is invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return resultstore.Receipt{}, false, errors.New("result receipt has trailing data")
	}
	canonical, _ := json.Marshal(receipt)
	if !bytes.Equal(bytes.TrimSuffix(encoded, []byte("\n")), canonical) {
		return resultstore.Receipt{}, false, errors.New("result receipt is not canonical")
	}
	name, _ := resultstore.Name(s.identity.Binding)
	if receipt.Name != name+"-complete" || receipt.UID == "" || receipt.Digest != digest || receipt.Size != int64(len(payload)) {
		return resultstore.Receipt{}, false, errors.New("result receipt does not match the delivered bytes")
	}
	return receipt, false, nil
}
