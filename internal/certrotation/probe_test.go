package certrotation

import (
	"crypto/rand"
	"crypto/tls"
	"strings"
	"testing"
	"time"
)

func TestTLSCertificateProberRequiresExactLeaf(t *testing.T) {
	t.Parallel()
	config := testConfig()
	now := time.Now().UTC()
	served := mustGenerateMaterial(t, now, config)
	alternate, err := generateServingMaterial(rand.Reader, now.Add(time.Minute), config, served)
	if err != nil {
		t.Fatalf("generate alternate serving certificate: %v", err)
	}
	keyPair, err := tls.X509KeyPair(served.certPEM, served.keyPEM)
	if err != nil {
		t.Fatalf("load serving key pair: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{keyPair},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serveFinished := make(chan struct{})
	go func() {
		defer close(serveFinished)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			if tlsConnection, ok := connection.(*tls.Conn); ok {
				_ = tlsConnection.Handshake()
			}
			_ = connection.Close()
		}
	}()

	request := probeRequest{
		Address:          listener.Addr().String(),
		ServerName:       config.ServiceName + "." + config.ServiceNamespace + ".svc",
		CACertificatePEM: served.caPEM,
		LeafCertificate:  served.leaf,
	}
	probe := tlsCertificateProber{}
	// The test's own context: a wall-clock deadline here fails a handshake that
	// is merely slow under -race on a loaded host, and the test binary's
	// -timeout already bounds one that hangs.
	ctx := t.Context()
	if err := probe.Probe(ctx, request); err != nil {
		t.Fatalf("Probe() exact leaf error = %v", err)
	}
	// The refusal has to be the leaf comparison: any other error, such as an
	// expired deadline, would pass a check that only asks for an error.
	request.LeafCertificate = alternate.leaf
	const differentLeaf = "webhook still serves a different certificate"
	if err := probe.Probe(ctx, request); err == nil || !strings.Contains(err.Error(), differentLeaf) {
		t.Fatalf("Probe() different leaf signed by the same CA error = %v, want %q", err, differentLeaf)
	}

	_ = listener.Close()
	<-serveFinished
}
