package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestCandidateCertificateStoreFailsClosedWhenEmptyOrCleared(t *testing.T) {
	t.Parallel()

	store := &candidateCertificateStore{}
	if _, err := store.GetCertificate(nil); !errors.Is(err, errCandidateCertificateUnavailable) {
		t.Fatalf("empty GetCertificate() error = %v, want unavailable", err)
	}
	certificatePEM, privateKeyPEM := testTLSKeyPair(t, "candidate-a")
	if err := store.StoreCandidateCertificate(certificatePEM, privateKeyPEM); err != nil {
		t.Fatalf("StoreCandidateCertificate() error = %v", err)
	}
	store.ClearCandidateCertificate()
	if _, err := store.GetCertificate(nil); !errors.Is(err, errCandidateCertificateUnavailable) {
		t.Fatalf("cleared GetCertificate() error = %v, want unavailable", err)
	}
}

func TestCandidateCertificateStoreAtomicallyReplacesCompleteKeyPair(t *testing.T) {
	t.Parallel()

	store := &candidateCertificateStore{}
	firstCertificatePEM, firstPrivateKeyPEM := testTLSKeyPair(t, "candidate-a")
	if err := store.StoreCandidateCertificate(firstCertificatePEM, firstPrivateKeyPEM); err != nil {
		t.Fatalf("store first candidate: %v", err)
	}
	first, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get first candidate: %v", err)
	}
	if first.Leaf == nil || first.Leaf.Subject.CommonName != "candidate-a" {
		t.Fatalf("first candidate leaf = %#v", first.Leaf)
	}

	if err := store.StoreCandidateCertificate([]byte("not PEM"), firstPrivateKeyPEM); err == nil {
		t.Fatal("malformed replacement was accepted")
	}
	retained, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get retained candidate: %v", err)
	}
	if !bytes.Equal(retained.Certificate[0], first.Certificate[0]) {
		t.Fatal("failed replacement changed the active certificate")
	}

	secondCertificatePEM, secondPrivateKeyPEM := testTLSKeyPair(t, "candidate-b")
	if err := store.StoreCandidateCertificate(secondCertificatePEM, secondPrivateKeyPEM); err != nil {
		t.Fatalf("store second candidate: %v", err)
	}
	second, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get second candidate: %v", err)
	}
	if second.Leaf == nil || second.Leaf.Subject.CommonName != "candidate-b" {
		t.Fatalf("second candidate leaf = %#v", second.Leaf)
	}
	if bytes.Equal(second.Certificate[0], first.Certificate[0]) {
		t.Fatal("successful replacement retained the prior certificate")
	}
	if config := store.tlsConfig(); config.MinVersion != tls.VersionTLS12 || config.GetCertificate == nil {
		t.Fatalf("TLS config = %#v, want TLS 1.2 minimum with dynamic certificate callback", config)
	}
}

func testTLSKeyPair(t *testing.T, commonName string) (certificatePEM, privateKeyPEM []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     []string{commonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
}
