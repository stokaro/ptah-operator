package certrotation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"
)

type probeRequest struct {
	Address          string
	ServerName       string
	CACertificatePEM []byte
	LeafCertificate  *x509.Certificate
}

type certificateProber interface {
	Probe(context.Context, probeRequest) error
}

type tlsCertificateProber struct{}

func (tlsCertificateProber) Probe(ctx context.Context, request probeRequest) error {
	if request.LeafCertificate == nil {
		return errors.New("expected serving certificate is missing")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(request.CACertificatePEM) {
		return errors.New("replacement CA certificate could not be loaded")
	}

	if err := probeOnce(ctx, request, roots); err != nil {
		return fmt.Errorf("TLS handshake with expected certificate: %w", err)
	}
	return nil
}

func probeOnce(ctx context.Context, request probeRequest, roots *x509.CertPool) error {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: request.ServerName,
		},
	}
	connection, err := dialer.DialContext(ctx, "tcp", request.Address)
	if err != nil {
		return err
	}
	defer connection.Close()
	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		return errors.New("probe connection is not TLS")
	}
	state := tlsConnection.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return errors.New("webhook returned no serving certificate")
	}
	if !certificateRawEqual(state.PeerCertificates[0], request.LeafCertificate) {
		return errors.New("webhook still serves a different certificate")
	}
	return nil
}
