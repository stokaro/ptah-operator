package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestExpiredResultTrustPreservesBindingsAndSignsExpiredMaterial(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ca := func() (cert, key []byte) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, k.Public(), k)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
	}
	serverCA, serverKey := ca()
	clientCA, clientKey := ca()
	root, err := parseSingleCertificate(serverCA)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := resultFixtureSigner(serverKey, false)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"receiver.operator.svc", "receiver.operator.svc.cluster.local"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, root, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	original := expiredTrustJournal{Version: 1, ProjectionUID: "projection-uid", PolicyUID: "policy-uid", Phase: "stable", Current: expiredTrustKeys{ServerCA: serverCA, ServerKey: serverKey, ClientCA: clientCA, ClientKey: clientKey, ServerCertificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ServerCertificateKey: serverKey}}
	input, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := expiredResultTrust(input, now)
	if err != nil {
		t.Fatal(err)
	}
	var result expiredTrustJournal
	if err := json.Unmarshal(fixture.Journal, &result); err != nil {
		t.Fatal(err)
	}
	if result.Phase != "prepare" || result.Next == nil || result.ProjectionUID != original.ProjectionUID || result.PolicyUID != original.PolicyUID {
		t.Fatal("lost installation identity or pending candidate")
	}
	for _, keys := range []expiredTrustKeys{result.Current, *result.Next} {
		server, err := parseSingleCertificate(keys.ServerCA)
		if err != nil {
			t.Fatal(err)
		}
		client, err := parseSingleCertificate(keys.ClientCA)
		if err != nil {
			t.Fatal(err)
		}
		serving, err := parseSingleCertificate(keys.ServerCertificate)
		if err != nil {
			t.Fatal(err)
		}
		for _, cert := range []*x509.Certificate{server, client, serving} {
			if cert.NotAfter.After(now.Add(-5*time.Minute)) || !cert.NotBefore.Before(cert.NotAfter) {
				t.Fatal("fixture did not cross expiry plus skew")
			}
		}
		if server.CheckSignatureFrom(server) != nil || client.CheckSignatureFrom(client) != nil || serving.CheckSignatureFrom(server) != nil {
			t.Fatal("fixture is corrupt rather than expired")
		}
		if serving.VerifyHostname("receiver.operator.svc") != nil {
			t.Fatal("fixture changed serving identity")
		}
		if _, err := resultFixtureSigner(keys.ServerKey, false); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(result.Current.ServerKey, original.Current.ServerKey) || bytes.Equal(result.Next.ServerKey, original.Current.ServerKey) {
		t.Fatal("candidate keys were not distinct from current keys")
	}
	if len(fixture.Projection) != 6 || len(fixture.Policy) != 3 || len(fixture.Public) != 6 {
		t.Fatal("incomplete projection, policy or public evidence")
	}
	for _, bad := range [][]byte{[]byte("{}"), append(bytes.Clone(input), '\n'), bytes.Replace(input, []byte(`"stable"`), []byte(`"prepare"`), 1)} {
		if _, err := expiredResultTrust(bad, now); err == nil {
			t.Fatal("accepted an unknown or pending input journal")
		}
	}
}
