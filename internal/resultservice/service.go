// Package resultservice connects durable delivery to the manager lifecycle.
// Trust is provisioned separately and read from bounded mounted files at startup.
package resultservice

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
	Endpoint, Address, CertificateDirectory string
	Uploads                                 int
	UploadTimeout                           time.Duration
	Consumer                                resultconsumer.Options
}

type Service struct {
	Issuer   *resultcredentials.Issuer
	Consumer *resultconsumer.Reader
	server   *http.Server
	address  string
	ready    atomic.Bool
	started  atomic.Bool
	notAfter time.Time
}

// New never generates or fetches trust through the Kubernetes API. Both replicas
// must mount the same dedicated client signing authority and overlapping trust.
// Required files: tls.crt, tls.key, ca.crt (server trust), client-ca.crt,
// client-ca.key, and client-trust.crt (current and overlapping client signers).
func New(config Config, writer client.Client, reader client.Reader) (*Service, error) {
	if writer == nil || reader == nil || config.CertificateDirectory == "" || jobconfig.ValidateEndpoint(config.Endpoint) != nil {
		return nil, errors.New("invalid result service configuration")
	}
	if _, _, err := net.SplitHostPort(config.Address); err != nil {
		return nil, errors.New("invalid result service listen address")
	}
	material := map[string][]byte{}
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt", "client-ca.crt", "client-ca.key", "client-trust.crt"} {
		data, err := readFile(config.CertificateDirectory, name)
		if err != nil {
			return nil, fmt.Errorf("result service cannot load %s", name)
		}
		material[name] = data
	}
	serving, err := tls.X509KeyPair(material["tls.crt"], material["tls.key"])
	if err != nil {
		return nil, errors.New("invalid result server key pair")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(material["ca.crt"]) {
		return nil, errors.New("invalid result server trust")
	}
	endpoint, _ := url.Parse(config.Endpoint)
	leaf, err := x509.ParseCertificate(serving.Certificate[0])
	if err != nil || leaf.IsCA {
		return nil, errors.New("invalid result server certificate")
	}
	intermediates := x509.NewCertPool()
	for _, der := range serving.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("invalid result server chain")
		}
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: endpoint.Hostname(), Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("result server certificate does not authenticate its endpoint")
	}
	signer, err := tls.X509KeyPair(material["client-ca.crt"], material["client-ca.key"])
	if err != nil {
		return nil, errors.New("invalid result client signer")
	}
	clientTrust := x509.NewCertPool()
	if !clientTrust.AppendCertsFromPEM(material["client-trust.crt"]) {
		return nil, errors.New("invalid result client trust")
	}
	issuer, err := resultcredentials.New(writer, reader, signer, clientTrust, material["ca.crt"])
	if err != nil {
		return nil, errors.New("result client signer is not trusted or usable")
	}
	signerLeaf, _ := x509.ParseCertificate(signer.Certificate[0]) // New verified this exact signer.
	store := resultstore.Store{Client: writer, Reader: reader}
	receiver, err := resultdelivery.NewReceiver(resultdelivery.ReceiverConfig{Store: store, Authorize: (resultauthority.Authorizer{Reader: reader}).Check, MaxConcurrent: config.Uploads, Timeout: config.UploadTimeout})
	if err != nil {
		return nil, err
	}
	server, err := receiver.Server(serving, clientTrust)
	if err != nil {
		return nil, err
	}
	// TLS failures must not emit certificate identities or transport diagnostics.
	server.ErrorLog = log.New(io.Discard, "", 0)
	consumer, err := resultconsumer.New(resultconsumer.StoreLoader{Store: store}, config.Consumer)
	if err != nil {
		return nil, err
	}
	expires := leaf.NotAfter
	if signerLeaf.NotAfter.Before(expires) {
		expires = signerLeaf.NotAfter
	}
	return &Service{Issuer: issuer, Consumer: consumer, server: server, address: config.Address, notAfter: expires}, nil
}

func readFile(directory, name string) ([]byte, error) {
	file, err := os.Open(filepath.Join(directory, name))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 64<<10 {
		return nil, errors.New("invalid trust file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(data) == 0 || len(data) > 64<<10 {
		return nil, errors.New("invalid trust file")
	}
	return data, nil
}

func (*Service) NeedLeaderElection() bool { return false }
func (s *Service) Ready(*http.Request) error {
	if !s.ready.Load() || !time.Now().Before(s.notAfter) {
		return errors.New("result service is not ready")
	}
	return nil
}

// Start serves on every replica. Publication is idempotent through the API;
// neither the listener nor background reading depends on leader election.
func (s *Service) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("result service cannot be restarted")
	}
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return errors.New("result service listener could not start")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.server.BaseContext = func(net.Listener) context.Context { return runCtx }
	s.address = listener.Addr().String()
	consumerDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { consumerDone <- s.Consumer.Start(runCtx) }()
	go func() { serverDone <- s.server.ServeTLS(listener, "", "") }()
	s.ready.Store(true)
	var failure error
	serverFinished, consumerFinished := false, false
	select {
	case <-ctx.Done():
	case <-serverDone:
		serverFinished = true
		if ctx.Err() == nil {
			failure = errors.New("result service listener stopped")
		}
	case <-consumerDone:
		consumerFinished = true
		if ctx.Err() == nil {
			failure = errors.New("result consumer stopped")
		}
	}
	s.ready.Store(false)
	cancel()
	// Cancel active API operations and close idle and active TLS connections.
	// A canceled upload cannot turn partial publication into an acknowledgment.
	_ = s.server.Close()
	if !serverFinished {
		<-serverDone
	}
	if !consumerFinished {
		<-consumerDone
	}
	return failure
}
