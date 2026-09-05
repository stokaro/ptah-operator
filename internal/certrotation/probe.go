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
	IdentityOnly     bool
}

type certificateProber interface {
	Probe(context.Context, probeRequest) error
}

type tlsCertificateProber struct{}

func (tlsCertificateProber) Probe(ctx context.Context, request probeRequest) error {
	if request.LeafCertificate == nil {
		return errors.New("expected serving certificate is missing")
	}
	if err := probeOnce(ctx, request); err != nil {
		return fmt.Errorf("TLS handshake with expected certificate: %w", err)
	}
	return nil
}

func probeOnce(ctx context.Context, request probeRequest) error {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: request.ServerName,
	}
	if request.IdentityOnly {
		// The caller has already verified the expected leaf's DNS identity and
		// signature against a candidate CA. Disable the standard chain check so
		// an expired but still served leaf can prove which candidate is live;
		// VerifyConnection below still requires the byte-exact expected leaf.
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // exact leaf verification replaces the default verifier
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			return verifyExpectedLeaf(state, request.LeafCertificate)
		}
	} else {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(request.CACertificatePEM) {
			return errors.New("replacement CA certificate could not be loaded")
		}
		tlsConfig.RootCAs = roots
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    tlsConfig,
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
	return verifyExpectedLeaf(state, request.LeafCertificate)
}

func verifyExpectedLeaf(state tls.ConnectionState, expected *x509.Certificate) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("webhook returned no serving certificate")
	}
	if !certificateRawEqual(state.PeerCertificates[0], expected) {
		return errors.New("webhook still serves a different certificate")
	}
	return nil
}
