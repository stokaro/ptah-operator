package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// A projected file and its metric do not prove that the webhook reloaded it.
// Verify a fresh connection to the exact Pod's forwarded port using the
// installed authority, DNS identity, and complete expected leaf.
func alProbeServingCertificate(ctx context.Context, address string, authority []byte, expected *x509.Certificate) error {
	if expected == nil || len(expected.DNSNames) == 0 || len(expected.Raw) == 0 {
		return errors.New("the serving certificate probe needs the exact leaf and DNS identity")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority) {
		return errors.New("the serving certificate probe has no readable authority")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("the serving certificate probe could not reach the forwarded Pod")
	}
	defer connection.Close()
	secure := tls.Client(connection, &tls.Config{
		RootCAs: roots, ServerName: expected.DNSNames[0], MinVersion: tls.VersionTLS12,
	})
	if secure.HandshakeContext(ctx) != nil {
		return errors.New("the serving certificate probe did not complete a trusted TLS handshake")
	}
	state := secure.ConnectionState()
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, expected.Raw) {
		return errors.New("the webhook still serves another certificate")
	}
	return nil
}
