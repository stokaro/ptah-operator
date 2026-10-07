package resultdelivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultstore"
)

// Publisher returns only a durable, fully read-back receipt. check must run
// before the first write and again after chunk writes, before committing or
// returning a chunked receipt. One atomic write needs only the first check.
type Publisher interface {
	PublishAuthorized(context.Context, resultstore.Binding, []byte, string, func(context.Context) error) (resultstore.Receipt, error)
}

// ReceiverConfig has no permissive authorization default. Authorize must use
// direct API reads to validate the complete identity, claim, epoch, Job, and Pod.
// It returns ErrAuthority for a definitive refusal and another error for a
// transient failure. It must return promptly when ctx is canceled. Admission and the consuming
// controller retain their own authority checks across concurrent state changes.
type ReceiverConfig struct {
	Store     Publisher
	Authorize func(context.Context, Identity) error
	// VerifyClient optionally adds a current-trust check to the mandatory mTLS
	// identity check. Rotating receivers use it to revoke preexisting connections.
	VerifyClient func(*tls.ConnectionState) error
	// AuthenticateToken selects Pod-bound tokens instead of mTLS. This is an
	// installation choice, not a request-controlled authentication fallback.
	AuthenticateToken AuthenticateToken
	MaxConcurrent     int
	Timeout           time.Duration
}

type Receiver struct {
	config ReceiverConfig
	slots  chan struct{}
}

func NewReceiver(config ReceiverConfig) (*Receiver, error) {
	if config.Store == nil || config.Authorize == nil || config.MaxConcurrent < 1 || config.MaxConcurrent > 8 ||
		config.Timeout <= 0 || config.Timeout > 5*time.Minute || config.AuthenticateToken != nil && config.VerifyClient != nil {
		return nil, errors.New("invalid result receiver configuration")
	}
	return &Receiver{config: config, slots: make(chan struct{}, config.MaxConcurrent)}, nil
}

// Server owns a dedicated listener; it is independent of reconcile workers.
// A token-authenticated receiver accepts no client CA. The certificate transport
// requires a dedicated client CA and exact per-Pod certificate issuance.
func (r *Receiver) Server(certificate tls.Certificate, clientCAs *x509.CertPool) (*http.Server, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil || (r.config.AuthenticateToken == nil) != (clientCAs != nil) {
		return nil, errors.New("receiver TLS certificate and authentication configuration are inconsistent")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	if r.config.AuthenticateToken == nil {
		config.ClientAuth, config.ClientCAs = tls.RequireAndVerifyClientCert, clientCAs.Clone()
	}
	return &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: r.config.Timeout + 5*time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10, WriteTimeout: r.config.Timeout + 5*time.Second,
		TLSConfig: config}, nil
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	refuse := func(code int) {
		// Do not let net/http drain an unread rejected body indefinitely after
		// this handler releases its upload slot.
		if request.ProtoMajor == 1 {
			w.Header().Set("Connection", "close")
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		http.Error(w, http.StatusText(code), code)
	}
	preflight := request.Method == http.MethodHead
	if request.Method != http.MethodPut && !preflight {
		refuse(http.StatusMethodNotAllowed)
		return
	}
	var identity Identity
	var token string
	var err error
	if r.config.AuthenticateToken != nil {
		// Parsing is bounded and grants no authority. The potentially blocking
		// TokenReview runs inside the upload slot and its request deadline.
		identity, token, err = tokenIdentity(request)
	} else {
		identity, err = authenticatedIdentity(request.TLS, time.Now())
	}
	if err != nil {
		refuse(http.StatusUnauthorized)
		return
	}
	name, _ := resultstore.Name(identity.Binding)
	if request.URL.Path != PathPrefix+name || request.URL.RawQuery != "" {
		refuse(http.StatusForbidden)
		return
	}
	if preflight {
		if request.ContentLength != 0 || request.Header.Get("Content-Encoding") != "" {
			refuse(http.StatusBadRequest)
			return
		}
	} else {
		if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != ContentType || request.Header.Get("Content-Encoding") != "" {
			refuse(http.StatusUnsupportedMediaType)
			return
		}
		if request.ContentLength < 1 {
			refuse(http.StatusLengthRequired)
			return
		}
		if request.ContentLength > resultstore.MaxPayloadBytes {
			refuse(http.StatusRequestEntityTooLarge)
			return
		}
		if len(request.Header.Values(DigestHeader)) != 1 {
			refuse(http.StatusBadRequest)
			return
		}
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		refuse(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), r.config.Timeout)
	defer cancel()
	// A context deadline alone cannot interrupt Body.Read. The connection's
	// read deadline also covers a client that sends headers and stops there.
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil {
		refuse(http.StatusInternalServerError)
		return
	}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.config.AuthenticateToken != nil {
			if err := r.config.AuthenticateToken(ctx, token, identity); err != nil {
				return err
			}
		} else {
			current, err := authenticatedIdentity(request.TLS, time.Now())
			if err != nil || current != identity {
				return ErrAuthority
			}
			if r.config.VerifyClient != nil {
				if err := r.config.VerifyClient(request.TLS); err != nil {
					return ErrAuthority
				}
			}
		}
		if err := r.config.Authorize(ctx, identity); err != nil {
			return err
		}
		return ctx.Err()
	}
	authorityError := func(err error) {
		if errors.Is(err, ErrAuthority) {
			refuse(http.StatusForbidden)
		} else {
			refuse(http.StatusServiceUnavailable)
		}
	}
	if err := check(ctx); err != nil {
		authorityError(err)
		return
	}
	if preflight {
		// This proves current delivery authentication, not an SQL permission or
		// a durable receipt. The operation's execution guards still apply.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, request.ContentLength+1))
	if err != nil || int64(len(payload)) != request.ContentLength {
		var timeout interface{ Timeout() bool }
		if ctx.Err() != nil || errors.As(err, &timeout) && timeout.Timeout() {
			refuse(http.StatusRequestTimeout)
		} else {
			refuse(http.StatusBadRequest)
		}
		return
	}
	digest := payloadDigest(payload)
	if request.Header.Get(DigestHeader) != digest {
		refuse(http.StatusBadRequest)
		return
	}
	if _, err := Decode(identity, payload); err != nil {
		refuse(http.StatusUnprocessableEntity)
		return
	}
	receipt, err := r.config.Store.PublishAuthorized(ctx, identity.Binding, payload, digest, check)
	if err != nil {
		switch {
		case errors.Is(err, ErrAuthority):
			refuse(http.StatusForbidden)
		case errors.Is(err, resultstore.ErrConflict):
			refuse(http.StatusConflict)
		case errors.Is(err, resultstore.ErrInvalid):
			refuse(http.StatusUnprocessableEntity)
		default:
			refuse(http.StatusServiceUnavailable)
		}
		return
	}
	if (receipt.Name != name && receipt.Name != name+"-complete") || receipt.UID == "" || receipt.Digest != digest || receipt.Size != int64(len(payload)) {
		refuse(http.StatusInternalServerError)
		return
	}
	if ctx.Err() != nil {
		refuse(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(receipt)
}
