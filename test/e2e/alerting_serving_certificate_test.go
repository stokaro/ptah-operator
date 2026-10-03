package e2e

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlServingProbeRequiresTheExactTrustedLeaf(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	data, other := alCertificateFixture(t, now), alCertificateFixture(t, now)
	warning, _, err := alServingCertificate(data, now, alCertificateWarning+alCertificateLifetime)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := firstCertificate(warning)
	if err != nil {
		t.Fatal(err)
	}
	reissued, _, err := alServingCertificate(data, now, alCertificateWarning+alCertificateLifetime)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := tls.X509KeyPair(data["ca.crt"], data["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	expiredTemplate := *expected
	expiredTemplate.SerialNumber = big.NewInt(1000)
	expiredTemplate.NotBefore, expiredTemplate.NotAfter = now.Add(-time.Minute), now.Add(-time.Second)
	expiredDER, err := x509.CreateCertificate(rand.Reader, &expiredTemplate, ca.Leaf, expected.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	expired := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: expiredDER})
	for _, row := range []struct {
		name        string
		leaf, key   []byte
		expected    *x509.Certificate
		authority   []byte
		wantSuccess bool
	}{
		{"exact trusted warning", warning, data["tls.key"], expected, data["ca.crt"], true},
		{"old projected leaf", data["tls.crt"], data["tls.key"], expected, data["ca.crt"], false},
		{"same expiry and key but another serial", reissued, data["tls.key"], expected, data["ca.crt"], false},
		{"untrusted authority", other["tls.crt"], other["tls.key"], expected, data["ca.crt"], false},
		{"expired endpoint", expired, data["tls.key"], expected, data["ca.crt"], false},
		{"unreadable authority", warning, data["tls.key"], expected, []byte("private-fixture-material"), false},
		{"absent expected identity", warning, data["tls.key"], nil, data["ca.crt"], false},
	} {
		t.Run(row.name, func(t *testing.T) {
			pair, err := tls.X509KeyPair(row.leaf, row.key)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.StartTLS()
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err = alProbeServingCertificate(ctx, server.Listener.Addr().String(), row.authority, row.expected)
			if (err == nil) != row.wantSuccess {
				t.Fatalf("serving certificate probe = %v; want success %t", err, row.wantSuccess)
			}
			if err != nil && (strings.Contains(err.Error(), "private-fixture-material") || strings.Contains(err.Error(), "-----BEGIN")) {
				t.Fatal("the serving probe exposed input material")
			}
		})
	}
}

func TestAlServingProbeCancelsAnUnfinishedTLSHandshake(t *testing.T) {
	t.Parallel()
	data := alCertificateFixture(t, time.Now().UTC())
	expected, err := firstCertificate(data["tls.crt"])
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if connection, err := listener.Accept(); err == nil {
			accepted <- connection
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- alProbeServingCertificate(ctx, listener.Addr().String(), data["ca.crt"], expected) }()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not reach the server that withholds its TLS reply")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unfinished TLS handshake was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left the serving probe's TLS handshake running")
	}
}
