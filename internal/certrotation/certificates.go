package certrotation

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const certificateBackdate = 5 * time.Minute

type certificateMaterial struct {
	caPEM    []byte
	caKeyPEM []byte
	certPEM  []byte
	keyPEM   []byte
	ca       *x509.Certificate
	caKey    crypto.Signer
	leaf     *x509.Certificate
}

type secretState struct {
	current                      certificateMaterial
	currentServingChainAuthentic bool
	rotateCA                     bool
	rotateServing                bool
	normalizeSecret              bool
}

func inspectSecret(secret *corev1.Secret, config Config, now time.Time) (secretState, error) {
	ca, normalizedCA, caErr := parseSingleCertificate(secret.Data[CACertificateKey])
	current := certificateMaterial{
		caPEM:    normalizedCA,
		caKeyPEM: append([]byte(nil), secret.Data[CAPrivateKeyKey]...),
		certPEM:  append([]byte(nil), secret.Data[corev1.TLSCertKey]...),
		keyPEM:   append([]byte(nil), secret.Data[corev1.TLSPrivateKeyKey]...),
		ca:       ca,
	}

	caKey, caKeyErr := parsePrivateKey(current.caKeyPEM)
	if caKeyErr == nil {
		current.caKey = caKey
	}
	rotateCA := caErr != nil || caKeyErr != nil || ca == nil || !ca.IsCA || !publicKeysEqual(ca.PublicKey, signerPublicKey(caKey)) ||
		!certificateCurrentlyValid(ca, now) || !now.Add(config.RenewalThreshold).Before(ca.NotAfter)

	leaf, leafErr := parseLeafAndKey(current.certPEM, current.keyPEM)
	if leafErr == nil {
		current.leaf = leaf
	}
	currentServingChainAuthentic := leafErr == nil && servingCertificateAuthentic(leaf, ca, requiredDNSNames(config))
	if leafErr == nil && caErr == nil && !currentServingChainAuthentic {
		// A well-formed but unrelated ca.crt is corrupted state. Preserve an
		// authentic live signer from managed webhook bundles during a full CA
		// replacement instead of issuing under an unrelated key.
		rotateCA = true
	}
	rotateServing := leafErr != nil || !servingCertificateValid(leaf, ca, requiredDNSNames(config), now, config.RenewalThreshold)
	if rotateServing && !rotateCA && !now.Add(config.ServingCertificateValidity).Before(ca.NotAfter) {
		// Do not issue a replacement that outlives the current CA.
		rotateCA = true
	}

	return secretState{
		current:                      current,
		currentServingChainAuthentic: currentServingChainAuthentic,
		rotateCA:                     rotateCA,
		rotateServing:                rotateServing,
		normalizeSecret:              secret.Type != corev1.SecretTypeTLS,
	}, nil
}

func generateMaterial(reader io.Reader, now time.Time, config Config) (certificateMaterial, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), reader)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("generate CA private key: %w", err)
	}
	caSerial, err := randomSerial(reader)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("generate CA serial: %w", err)
	}
	ca := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: config.ServiceName + " webhook CA"},
		NotBefore:             now.Add(-certificateBackdate),
		NotAfter:              now.Add(config.CACertificateValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	caDER, err := x509.CreateCertificate(reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("create CA certificate: %w", err)
	}
	parsedCA, err := x509.ParseCertificate(caDER)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("parse generated CA certificate: %w", err)
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("marshal CA private key: %w", err)
	}
	base := certificateMaterial{
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		caKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}),
		ca:       parsedCA,
		caKey:    caKey,
	}
	return generateServingMaterial(reader, now, config, base)
}

func generateServingMaterial(
	reader io.Reader,
	now time.Time,
	config Config,
	caMaterial certificateMaterial,
) (certificateMaterial, error) {
	if caMaterial.ca == nil || caMaterial.caKey == nil {
		return certificateMaterial{}, errors.New("CA certificate and private key are required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), reader)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("generate serving private key: %w", err)
	}
	serial, err := randomSerial(reader)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("generate serving serial: %w", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: config.ServiceName},
		DNSNames:     requiredDNSNames(config),
		NotBefore:    now.Add(-certificateBackdate),
		NotAfter:     now.Add(config.ServingCertificateValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if !leaf.NotAfter.Before(caMaterial.ca.NotAfter) {
		return certificateMaterial{}, errors.New("serving certificate validity would reach or exceed CA expiry")
	}
	leafDER, err := x509.CreateCertificate(reader, leaf, caMaterial.ca, key.Public(), caMaterial.caKey)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("create serving certificate: %w", err)
	}
	parsedLeaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("parse generated serving certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("marshal serving private key: %w", err)
	}
	caMaterial.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	caMaterial.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	caMaterial.leaf = parsedLeaf
	return caMaterial, nil
}

func parseSingleCertificate(data []byte) (*x509.Certificate, []byte, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("a PEM CERTIFICATE block is required")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("exactly one PEM certificate is required")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), nil
}

func parseLeafAndKey(certificatePEM, keyPEM []byte) (*x509.Certificate, error) {
	certificate, _, err := parseSingleCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(certificate.PublicKey, signerPublicKey(key)) {
		return nil, errors.New("serving certificate and private key do not match")
	}
	return certificate, nil
}

func parsePrivateKey(data []byte) (crypto.Signer, error) {
	if len(data) == 0 {
		return nil, errors.New("private key is missing")
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("exactly one PEM private key is required")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, errors.New("PKCS#8 key cannot sign")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported or malformed private key")
}

func servingCertificateValid(
	leaf *x509.Certificate,
	ca *x509.Certificate,
	requiredNames []string,
	now time.Time,
	threshold time.Duration,
) bool {
	if !servingCertificateAuthentic(leaf, ca, requiredNames) ||
		!certificateCurrentlyValid(leaf, now) || !now.Add(threshold).Before(leaf.NotAfter) {
		return false
	}
	return true
}

// servingCertificateAuthentic validates the timeless cryptographic and
// identity relationship between a serving certificate and its CA. Rotation
// uses this to decide whether an expired current CA may safely remain in the
// overlap while the newly generated CA is deployed.
func servingCertificateAuthentic(leaf *x509.Certificate, ca *x509.Certificate, requiredNames []string) bool {
	if leaf == nil || ca == nil || leaf.CheckSignatureFrom(ca) != nil || leaf.IsCA {
		return false
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return false
	}
	for _, name := range requiredNames {
		if leaf.VerifyHostname(name) != nil {
			return false
		}
	}
	return true
}

func certificateCurrentlyValid(certificate *x509.Certificate, now time.Time) bool {
	return certificate != nil && !now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter)
}

func requiredDNSNames(config Config) []string {
	return []string{
		config.ServiceName,
		fmt.Sprintf("%s.%s", config.ServiceName, config.ServiceNamespace),
		fmt.Sprintf("%s.%s.svc", config.ServiceName, config.ServiceNamespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", config.ServiceName, config.ServiceNamespace),
	}
}

func publicKeysEqual(left, right crypto.PublicKey) bool {
	if left == nil || right == nil {
		return false
	}
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftDER, rightDER)
}

func signerPublicKey(signer crypto.Signer) crypto.PublicKey {
	if signer == nil {
		return nil
	}
	return signer.Public()
}

func randomSerial(reader io.Reader) (*big.Int, error) {
	// A positive 128-bit random serial provides ample collision resistance and
	// stays below the 20-octet RFC 5280 limit.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := randInt(reader, limit)
		if err != nil {
			return nil, err
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

func randInt(reader io.Reader, max *big.Int) (*big.Int, error) {
	return rand.Int(reader, max)
}
