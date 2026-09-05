package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
)

var errCandidateCertificateUnavailable = errors.New("candidate serving certificate is unavailable")

// candidateCertificateStore is the concurrency boundary between the rotation
// state machine and the future candidate admission listener. A complete key
// pair is parsed before the atomic swap, so handshakes observe either the old
// complete certificate or the new complete certificate.
type candidateCertificateStore struct {
	certificate atomic.Pointer[tls.Certificate]
}

func (s *candidateCertificateStore) StoreCandidateCertificate(certificatePEM, privateKeyPEM []byte) error {
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return fmt.Errorf("parse candidate serving key pair: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return errors.New("candidate serving key pair contains no certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse candidate serving leaf: %w", err)
	}
	certificate.Leaf = leaf
	s.certificate.Store(&certificate)
	return nil
}

func (s *candidateCertificateStore) ClearCandidateCertificate() {
	s.certificate.Store(nil)
}

func (s *candidateCertificateStore) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate := s.certificate.Load()
	if certificate == nil {
		return nil, errCandidateCertificateUnavailable
	}
	return certificate, nil
}

func (s *candidateCertificateStore) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.GetCertificate,
	}
}
