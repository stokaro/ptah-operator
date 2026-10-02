package resultservice

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var trustFiles = []string{"tls.crt", "tls.key", "ca.crt", "client-ca.crt", "client-ca.key", "client-trust.crt"}

type trustSnapshot struct {
	tls      *tls.Config
	issuer   *resultcredentials.Issuer
	notAfter time.Time
	digest   [32]byte
}

// A projected Secret switches the ..data symlink atomically. Resolve it once,
// then read that generation only. Removal during a read refuses the candidate;
// it cannot splice a key or trust bundle from the next generation into this one.
func trustDirectory(directory string) (string, error) {
	projection := filepath.Join(directory, "..data")
	if _, err := os.Lstat(projection); err == nil {
		return filepath.EvalSymlinks(projection)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Plain directories are useful for immutable externally provisioned mounts.
	// Writers must replace the directory as a unit, never edit its members in place.
	return filepath.EvalSymlinks(directory)
}

func trustDigest(material map[string][]byte) [32]byte {
	h := sha256.New()
	for _, name := range trustFiles {
		fmt.Fprintf(h, "%s:%d:", name, len(material[name]))
		h.Write(material[name])
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func loadTrust(config Config, writer client.Client, reader client.Reader, previous *trustSnapshot) (*trustSnapshot, error) {
	directory, err := trustDirectory(config.CertificateDirectory)
	if err != nil {
		return nil, errors.New("result service cannot resolve trust projection")
	}
	material := map[string][]byte{}
	for _, name := range trustFiles {
		data, err := readTrustFile(directory, name)
		if err != nil {
			return nil, fmt.Errorf("result service cannot load %s", name)
		}
		material[name] = data
	}
	digest := trustDigest(material)
	if previous != nil && digest == previous.digest {
		return previous, nil
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
	chains, err := leaf.Verify(x509.VerifyOptions{DNSName: endpoint.Hostname(), Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
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

	expires := leaf.NotAfter
	// Readiness cannot outlive any certificate in the selected server chain.
	for _, cert := range chains[0] {
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}
	if signerLeaf.NotAfter.Before(expires) {
		expires = signerLeaf.NotAfter
	}
	return &trustSnapshot{tls: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serving}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientTrust}, issuer: issuer, notAfter: expires, digest: digest}, nil
}

func readTrustFile(directory, name string) ([]byte, error) {
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

func (s *Service) reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	next, err := loadTrust(s.config, s.writer, s.reader, s.trust.Load())
	if err != nil {
		s.reloadFailed.Store(true)
		return err
	}
	s.trust.Store(next)
	s.reloadFailed.Store(false)
	return nil
}

func (s *Service) tlsConfig(*tls.ClientHelloInfo) (*tls.Config, error) {
	return s.trust.Load().tls, nil
}

// A connection may predate removal of a client CA. Revalidate its certificate
// against current roots at every receiver authority check, including after an
// upload. VerifiedChains alone records only the original handshake decision.
func (s *Service) verifyClient(state *tls.ConnectionState) error {
	if state == nil || len(state.PeerCertificates) == 0 {
		return resultdelivery.ErrAuthority
	}
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: s.trust.Load().tls.ClientCAs, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return resultdelivery.ErrAuthority
	}
	return nil
}

// These delegates load one immutable snapshot per call. The stable Service
// pointer is shared by reconciliation and admission; neither retains a retired
// signer after the mount changes. Existing canonical credentials keep their keys.
func (s *Service) Ensure(ctx context.Context, identity resultdelivery.Identity) (resultcredentials.Credential, error) {
	if s.reloadFailed.Load() {
		return resultcredentials.Credential{}, resultcredentials.ErrCredential
	}
	return s.trust.Load().issuer.Ensure(ctx, identity)
}
func (s *Service) ValidateCreate(ctx context.Context, secret *corev1.Secret) error {
	return s.trust.Load().issuer.ValidateCreate(ctx, secret)
}
func (s *Service) ValidateRecordCreate(ctx context.Context, record *api.PtahResultRecord) error {
	return s.trust.Load().issuer.ValidateRecordCreate(ctx, record)
}
func (s *Service) AuthorizePublication(ctx context.Context, binding resultstore.Binding) (resultdelivery.Identity, error) {
	return s.trust.Load().issuer.AuthorizePublication(ctx, binding)
}
