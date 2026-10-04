package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func apiTLSServer(t *testing.T, name string) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.ServerName != "kubernetes.default.svc" || r.Header.Get("Authorization") != "Bearer test-metrics-reader" {
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/metrics", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestAPIMetricsClientVerifiesCANameAndAuthentication(t *testing.T) {
	server, authority := apiTLSServer(t, "kubernetes.default.svc")
	config := &rest.Config{Host: server.URL, BearerToken: "test-metrics-reader", TLSClientConfig: rest.TLSClientConfig{CAData: authority}}
	client, err := apiMetricsClient(config, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("verified authenticated request refused")
	}
	if response, err := client.Get(server.URL + "/redirect"); err == nil {
		response.Body.Close()
		t.Fatal("metrics redirect followed")
	} else if response != nil {
		response.Body.Close()
	}
	wrongName, wrongAuthority := apiTLSServer(t, "wrong.default.svc")
	for _, test := range []struct {
		name, host string
		authority  []byte
	}{
		{"wrong CA", server.URL, wrongAuthority}, {"wrong name", wrongName.URL, wrongAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := rest.CopyConfig(config)
			cfg.CAData = test.authority
			client, err := apiMetricsClient(cfg, test.host)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			if response, err := client.Get(test.host + "/metrics"); err == nil {
				response.Body.Close()
				t.Fatal("unverified API endpoint accepted")
			}
		})
	}
	for _, cfg := range []*rest.Config{nil, {TLSClientConfig: rest.TLSClientConfig{Insecure: true, CAData: authority}}, {}} {
		if _, err := apiMetricsClient(cfg, server.URL); err == nil {
			t.Fatal("unverified transport configuration accepted")
		}
	}
}
