package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPublishCreatesTypedSchemaArtifact(t *testing.T) {
	t.Parallel()
	const (
		username = "registry-user"
		password = "registry-password"
	)
	blobs := make(map[string][]byte)
	var storedManifest manifest
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		actualUser, actualPassword, ok := request.BasicAuth()
		if !ok || actualUser != username || actualPassword != password {
			t.Errorf("registry request did not carry exact Basic authentication")
			return testResponse(http.StatusUnauthorized, ""), nil
		}
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/blobs/uploads/"):
			return testResponse(http.StatusAccepted, "http://registry.test:5000/upload/next"), nil
		case request.Method == http.MethodPut && request.URL.Path == "/upload/next":
			contents, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read uploaded blob: %v", err)
			}
			blobs[request.URL.Query().Get("digest")] = contents
			return testResponse(http.StatusCreated, ""), nil
		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/"):
			if request.Header.Get("Content-Type") != manifestType {
				t.Errorf("manifest content type = %q", request.Header.Get("Content-Type"))
			}
			if err := json.NewDecoder(request.Body).Decode(&storedManifest); err != nil {
				t.Errorf("decode manifest: %v", err)
			}
			return testResponse(http.StatusCreated, ""), nil
		default:
			t.Errorf("unexpected registry request: %s %s", request.Method, request.URL)
			return testResponse(http.StatusNotFound, ""), nil
		}
	})}

	ref := registryReference{host: "registry.test:5000", repository: "schemas/unsafe", tag: "test"}
	schema := []byte(`role "credential_principal" { password = "sensitive-value" }`)
	manifestDigest, err := publish(
		context.Background(), client, ref,
		credentials{username: username, password: password}, schema,
	)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if manifestDigest == "" || storedManifest.ArtifactType != schemaArtifactType ||
		storedManifest.Annotations["io.stokaro.ptah.schema-format"] != "hcl" ||
		len(storedManifest.Layers) != 1 || storedManifest.Layers[0].MediaType != schemaLayerType ||
		storedManifest.Layers[0].Annotations["org.opencontainers.image.title"] != "schema.hcl" {
		t.Fatalf("stored manifest = %#v, digest = %q", storedManifest, manifestDigest)
	}
	storedSchema := blobs[storedManifest.Layers[0].Digest]
	if string(storedSchema) != string(schema) {
		t.Fatalf("stored schema = %q, want exact input", storedSchema)
	}
}

func TestRegistryClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	requestCount := 0
	client := newRegistryClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		response := testResponse(http.StatusTemporaryRedirect, "http://attacker.test/upload")
		response.Request = request
		return response, nil
	}))
	request, err := http.NewRequest(http.MethodPost, "http://registry.test:5000/v2/schemas/app/blobs/uploads/", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.SetBasicAuth("registry-user", "registry-password")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || requestCount != 1 {
		t.Fatalf("redirect status = %d, request count = %d", response.StatusCode, requestCount)
	}
}

func TestTLSProxyRoutesReadOnlyRegistryRequestsAndCounts(t *testing.T) {
	t.Parallel()
	target, err := url.Parse("http://registry.test:5000")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "registry-user" || password != "registry-password" {
			t.Errorf("proxied request did not preserve exact Basic authentication")
		}
		if request.Method != http.MethodGet || request.URL.RequestURI() != "/v2/team/schema/manifests/stable?proof=1" {
			t.Errorf("proxied request = %s %s", request.Method, request.URL.RequestURI())
		}
		if request.Host != target.Host {
			t.Errorf("proxied Host = %q, want %q", request.Host, target.Host)
		}
		if request.Header.Get("Forwarded") != "" || request.Header.Get("X-Forwarded-For") != "" {
			t.Errorf("proxy forwarded client-controlled routing headers")
		}
		response := testResponse(http.StatusOK, "")
		response.Header.Set("Content-Type", manifestType)
		response.Body = io.NopCloser(strings.NewReader(`{"schemaVersion":2}`))
		return response, nil
	})
	var count atomic.Uint64
	proxy := newTLSProxyHandlerWithTransport(target, &count, transport)
	request := httptest.NewRequest(http.MethodGet, "https://proxy.test:5443/v2/team/schema/manifests/stable?proof=1", nil)
	request.SetBasicAuth("registry-user", "registry-password")
	request.Header.Set("Forwarded", "for=attacker.invalid")
	request.Header.Set("X-Forwarded-For", "192.0.2.1")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d", recorder.Code)
	}

	adminRecorder := httptest.NewRecorder()
	newRequestCountHandler(&count).ServeHTTP(
		adminRecorder,
		httptest.NewRequest(http.MethodGet, "http://admin.test:8081/", nil),
	)
	if adminRecorder.Body.String() != "1\n" || count.Load() != 1 {
		t.Fatalf("request count = %q, atomic count = %d", adminRecorder.Body.String(), count.Load())
	}
}

func TestTLSProxyRejectsMutationsUnknownPathsAndRegistryRedirects(t *testing.T) {
	t.Parallel()
	upstreamRequests := 0
	target, err := url.Parse("http://registry.test:5000")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamRequests++
		return testResponse(http.StatusTemporaryRedirect, "https://attacker.invalid/v2/"), nil
	})
	var count atomic.Uint64
	proxy := newTLSProxyHandlerWithTransport(target, &count, transport)

	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/v2/team/schema/blobs/uploads/", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/not-registry", status: http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, "https://proxy.test:5443"+test.path, nil)
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, recorder.Code, test.status)
		}
	}
	if upstreamRequests != 0 || count.Load() != 0 {
		t.Fatalf("rejected requests reached upstream=%d or count=%d", upstreamRequests, count.Load())
	}

	redirectRecorder := httptest.NewRecorder()
	proxy.ServeHTTP(
		redirectRecorder,
		httptest.NewRequest(http.MethodGet, "https://proxy.test:5443/v2/", nil),
	)
	if redirectRecorder.Code != http.StatusBadGateway || redirectRecorder.Header().Get("Location") != "" {
		t.Fatalf("redirect response status = %d, Location = %q", redirectRecorder.Code, redirectRecorder.Header().Get("Location"))
	}
	if upstreamRequests != 1 || count.Load() != 1 {
		t.Fatalf("redirect request reached upstream=%d, count=%d", upstreamRequests, count.Load())
	}
}

func TestRequestCountAdminDoesNotExposeRegistryRoutes(t *testing.T) {
	t.Parallel()
	var count atomic.Uint64
	count.Store(9)
	for _, path := range []string{"/v2/", "/?details=true", "/_e2e/request-count"} {
		recorder := httptest.NewRecorder()
		newRequestCountHandler(&count).ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodGet, "http://admin.test:8081"+path, nil),
		)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}
}

func TestVerifyCertificateFilesBindsChainDNSAndServerUsage(t *testing.T) {
	t.Parallel()
	const dnsName = "e2e-registry-tls.ptah-test.svc.cluster.local"
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "e2e-test-server"},
		DNSNames:     []string{dnsName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(
		rand.Reader,
		serverTemplate,
		caCertificate,
		&serverKey.PublicKey,
		caKey,
	)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	caFile := writeCertificatePEM(t, "ca.pem", caDER)
	serverFile := writeCertificatePEM(t, "tls.crt", serverDER)

	if err := verifyCertificateFiles(caFile, serverFile, dnsName); err != nil {
		t.Fatalf("verifyCertificateFiles: %v", err)
	}
	if err := verifyCertificateFiles(caFile, serverFile, "other.test.svc.cluster.local"); err == nil {
		t.Fatal("verifyCertificateFiles accepted a different DNS name")
	}
	wrongCAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate wrong CA key: %v", err)
	}
	wrongCATemplate := *caTemplate
	wrongCATemplate.SerialNumber = big.NewInt(3)
	wrongCATemplate.Subject = pkix.Name{CommonName: "wrong-e2e-test-ca"}
	wrongCADER, err := x509.CreateCertificate(
		rand.Reader,
		&wrongCATemplate,
		&wrongCATemplate,
		&wrongCAKey.PublicKey,
		wrongCAKey,
	)
	if err != nil {
		t.Fatalf("create wrong CA: %v", err)
	}
	wrongCAFile := writeCertificatePEM(t, "wrong-ca.pem", wrongCADER)
	if err := verifyCertificateFiles(wrongCAFile, serverFile, dnsName); err == nil {
		t.Fatal("verifyCertificateFiles accepted a leaf signed by a different root")
	}

	clientOnlyTemplate := *serverTemplate
	clientOnlyTemplate.SerialNumber = big.NewInt(4)
	clientOnlyTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	clientOnlyDER, err := x509.CreateCertificate(
		rand.Reader,
		&clientOnlyTemplate,
		caCertificate,
		&serverKey.PublicKey,
		caKey,
	)
	if err != nil {
		t.Fatalf("create client-only certificate: %v", err)
	}
	clientOnlyFile := writeCertificatePEM(t, "client-only.crt", clientOnlyDER)
	if err := verifyCertificateFiles(caFile, clientOnlyFile, dnsName); err == nil {
		t.Fatal("verifyCertificateFiles accepted a certificate without serverAuth usage")
	}

	serverContents, err := os.ReadFile(serverFile)
	if err != nil {
		t.Fatalf("read server PEM: %v", err)
	}
	if err := os.WriteFile(serverFile, append(serverContents, serverContents...), 0o600); err != nil {
		t.Fatalf("write multiple server certificates: %v", err)
	}
	if err := verifyCertificateFiles(caFile, serverFile, dnsName); err == nil {
		t.Fatal("verifyCertificateFiles accepted multiple leaf certificates")
	}
}

func writeCertificatePEM(t *testing.T, name string, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	contents := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write certificate PEM: %v", err)
	}
	return path
}

func testResponse(status int, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func TestParseReferenceRejectsUnpinnedRegistryShape(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"registry.example/schemas/app:tag",
		"oci://registry.example/schemas/app:tag",
		"oci://registry.example:5000/schemas/app",
		"oci://registry.example:5000/schemas/app@sha256:bad",
	} {
		if _, err := parseReference(raw); err == nil {
			t.Errorf("parseReference(%q) unexpectedly succeeded", raw)
		}
	}
}
