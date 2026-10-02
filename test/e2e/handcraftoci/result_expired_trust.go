package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultcredentials"
)

// This fixture changes signed validity dates, not the cluster clock or the
// production retirement bounds. Private output must stay in the harness pipe.
type expiredTrustKeys struct {
	ServerCA, ServerKey, ServerCertificate, ServerCertificateKey []byte
	ClientCA, ClientKey                                          []byte
}
type expiredTrustJournal struct {
	Version       int               `json:"version"`
	ProjectionUID string            `json:"projectionUID"`
	PolicyUID     string            `json:"policyUID"`
	Phase         string            `json:"phase"`
	Current       expiredTrustKeys  `json:"current"`
	Next          *expiredTrustKeys `json:"next,omitempty"`
}
type expiredTrustFixture struct {
	Journal    []byte            `json:"journal"`
	Projection map[string][]byte `json:"projection"`
	Policy     map[string]string `json:"policy"`
	Public     map[string][]byte `json:"public"`
}

func runExpiredResultTrust(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: e2e-handcraft-oci result-expired-trust < stable-journal.json")
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 128<<10+1))
	if err != nil || len(input) == 0 || len(input) > 128<<10 {
		return errors.New("invalid bounded rotation journal input")
	}
	fixture, err := expiredResultTrust(input, time.Now().UTC())
	if err != nil {
		return errors.New("cannot prepare expired result trust fixture")
	}
	return json.NewEncoder(os.Stdout).Encode(fixture)
}

func expiredResultTrust(input []byte, now time.Time) (expiredTrustFixture, error) {
	var st expiredTrustJournal
	if err := json.Unmarshal(input, &st); err != nil {
		return expiredTrustFixture{}, err
	}
	if st.Version != 1 || st.ProjectionUID == "" || st.PolicyUID == "" || st.Phase != "stable" || st.Next != nil {
		return expiredTrustFixture{}, errors.New("expected a stable installation journal")
	}
	original, _ := json.Marshal(st)
	if string(original) != string(input) {
		return expiredTrustFixture{}, errors.New("unexpected journal fields or encoding")
	}
	current, err := ageResultKeys(st.Current, now, false)
	if err != nil {
		return expiredTrustFixture{}, err
	}
	next, err := ageResultKeys(st.Current, now, true)
	if err != nil {
		return expiredTrustFixture{}, err
	}
	st.Current, st.Next, st.Phase = current, &next, "prepare"
	journal, err := json.Marshal(st)
	if err != nil {
		return expiredTrustFixture{}, err
	}
	ca, err := parseSingleCertificate(current.ClientCA)
	if err != nil {
		return expiredTrustFixture{}, err
	}
	return expiredTrustFixture{Journal: journal, Projection: map[string][]byte{
		"ca.crt": current.ServerCA, "tls.crt": current.ServerCertificate, "tls.key": current.ServerCertificateKey,
		"client-ca.crt": current.ClientCA, "client-ca.key": current.ClientKey, "client-trust.crt": current.ClientCA,
	}, Policy: resultcredentials.EnrollmentData(ca.Raw, current.ServerCA), Public: map[string][]byte{
		"currentServerCA": current.ServerCA, "currentClientCA": current.ClientCA, "currentLeaf": current.ServerCertificate,
		"expiredCandidateServerCA": next.ServerCA, "expiredCandidateClientCA": next.ClientCA, "expiredCandidateLeaf": next.ServerCertificate,
	}}, nil
}

func ageResultKeys(source expiredTrustKeys, now time.Time, replaceKeys bool) (expiredTrustKeys, error) {
	var output expiredTrustKeys
	server, serverKey, serverPEM, keyPEM, err := ageResultCA(source.ServerCA, source.ServerKey, now, replaceKeys)
	if err != nil {
		return output, err
	}
	_, _, clientPEM, clientKeyPEM, err := ageResultCA(source.ClientCA, source.ClientKey, now, replaceKeys)
	if err != nil {
		return output, err
	}
	leaf, err := parseSingleCertificate(source.ServerCertificate)
	if err != nil {
		return output, err
	}
	key, err := resultFixtureSigner(source.ServerCertificateKey, replaceKeys)
	if err != nil {
		return output, err
	}
	leaf.Raw, leaf.RawTBSCertificate, leaf.RawSubjectPublicKeyInfo = nil, nil, nil
	leaf.NotBefore, leaf.NotAfter = now.Add(-47*time.Hour), now.Add(-11*time.Minute)
	leaf.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return output, err
	}
	leaf.AuthorityKeyId = server.SubjectKeyId
	der, err := x509.CreateCertificate(rand.Reader, leaf, server, key.Public(), serverKey)
	if err != nil {
		return output, err
	}
	leafKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return output, err
	}
	output = expiredTrustKeys{ServerCA: serverPEM, ServerKey: keyPEM, ServerCertificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ServerCertificateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKey}), ClientCA: clientPEM, ClientKey: clientKeyPEM}
	return output, nil
}

func resultFixtureSigner(data []byte, replace bool) (crypto.Signer, error) {
	if replace {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	block, rest := pem.Decode(data)
	if block == nil || len(rest) != 0 {
		return nil, errors.New("invalid fixture private key")
	}
	value, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := value.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported fixture private key")
	}
	return signer, nil
}

func ageResultCA(cert, keyData []byte, now time.Time, replace bool) (*x509.Certificate, crypto.Signer, []byte, []byte, error) {
	template, err := parseSingleCertificate(cert)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !template.IsCA || template.CheckSignatureFrom(template) != nil {
		return nil, nil, nil, nil, errors.New("invalid fixture CA")
	}
	key, err := resultFixtureSigner(keyData, replace)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	public, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hash := sha256.Sum256(public)
	template.Raw, template.RawTBSCertificate, template.RawSubjectPublicKeyInfo = nil, nil, nil
	template.SubjectKeyId, template.AuthorityKeyId = hash[:20], nil
	template.PublicKey = key.Public()
	template.NotBefore, template.NotAfter = now.Add(-48*time.Hour), now.Add(-10*time.Minute)
	template.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// Distinct subjects make an expired candidate distinguishable in public evidence.
	if replace {
		template.Subject.CommonName = "expired-qualification-candidate-" + hex.EncodeToString(hash[:4])
		template.RawSubject = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return parsed, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}), nil
}
