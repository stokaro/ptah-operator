package certrotation

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := probe.Probe(ctx, request); err != nil {
		t.Fatalf("Probe() exact leaf error = %v", err)
	}
	request.LeafCertificate = alternate.leaf
	if err := probe.Probe(ctx, request); err == nil {
		t.Fatal("Probe() accepted a different leaf signed by the same CA")
	}

	_ = listener.Close()
	select {
	case <-serveFinished:
	case <-time.After(time.Second):
		t.Fatal("TLS test server did not stop")
	}
}
