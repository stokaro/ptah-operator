// Command handcraftoci publishes one intentionally unvalidated schema layer.
// It exists only to prove that the operator rejects a correctly typed OCI
// artifact whose content bypassed the normal safe publication path.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	schemaArtifactType  = "application/vnd.stokaro.ptah.schema.v1"
	schemaLayerType     = "application/vnd.stokaro.ptah.schema.hcl.v1"
	emptyConfigType     = "application/vnd.oci.empty.v1+json"
	manifestType        = "application/vnd.oci.image.manifest.v1+json"
	maxSchemaBytes      = 1 << 20
	maxResponseBytes    = 64 << 10
	maxCertificateBytes = 1 << 20
	adminListenAddress  = ":8081"
)

var referencePart = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

type registryReference struct {
	host       string
	repository string
	tag        string
}

type credentials struct {
	username string
	password string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "tls-proxy":
			return runTLSProxy(args[1:])
		case "verify-certificate":
			return runVerifyCertificate(args[1:])
		}
	}
	if len(args) != 2 {
		return errors.New("usage: e2e-handcraft-oci <oci-reference> <schema.hcl>")
	}
	contents, err := readSchema(args[1])
	if err != nil {
		return errors.New("e2e-handcraft-oci: read bounded schema input")
	}
	ref, err := parseReference(args[0])
	if err != nil || os.Getenv("PTAH_OCI_REGISTRY") != ref.host {
		return errors.New("e2e-handcraft-oci: invalid registry reference")
	}
	credential := credentials{
		username: os.Getenv("PTAH_OCI_USERNAME"),
		password: os.Getenv("PTAH_OCI_PASSWORD"),
	}
	if credential.username == "" || credential.password == "" {
		return errors.New("e2e-handcraft-oci: registry credentials are required")
	}
	digest, err := publish(context.Background(), newRegistryClient(nil), ref, credential, contents)
	if err != nil {
		return errors.New("e2e-handcraft-oci: publish handcrafted OCI artifact")
	}
	fmt.Printf("Digest: %s\n", digest)
	return nil
}

func runVerifyCertificate(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: e2e-handcraft-oci verify-certificate <ca.pem> <tls.crt> <dns-name>")
	}
	if err := verifyCertificateFiles(args[0], args[1], args[2]); err != nil {
		return errors.New("e2e-handcraft-oci: verify server certificate")
	}
	return nil
}

func verifyCertificateFiles(caPath, certificatePath, dnsName string) error {
	if dnsName == "" || strings.TrimSpace(dnsName) != dnsName || net.ParseIP(dnsName) != nil ||
		strings.ContainsAny(dnsName, "/:@") {
		return errors.New("server certificate DNS name is invalid")
	}
	caPEM, err := readBoundedFile(caPath, maxCertificateBytes)
	if err != nil {
		return errors.New("read certificate authority")
	}
	certificatePEM, err := readBoundedFile(certificatePath, maxCertificateBytes)
	if err != nil {
		return errors.New("read server certificate")
	}
	caCertificate, err := parseSingleCertificate(caPEM)
	if err != nil || !caCertificate.IsCA || !caCertificate.BasicConstraintsValid ||
		caCertificate.KeyUsage&x509.KeyUsageCertSign == 0 || caCertificate.CheckSignatureFrom(caCertificate) != nil {
		return errors.New("parse self-signed certificate authority")
	}
	certificate, err := parseSingleCertificate(certificatePEM)
	if err != nil {
		return errors.New("server certificate file must contain exactly one PEM certificate")
	}
	if certificate.IsCA {
		return errors.New("parse non-CA server certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	if _, err := certificate.Verify(x509.VerifyOptions{
		DNSName:   dnsName,
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return errors.New("server certificate chain, DNS name, or usage is invalid")
	}
	return nil
}

func parseSingleCertificate(contents []byte) (*x509.Certificate, error) {
	block, trailing := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, errors.New("certificate input must contain exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("parse certificate")
	}
	return certificate, nil
}

type tlsProxyConfig struct {
	listenAddress string
	upstream      *url.URL
	certificate   string
	privateKey    string
}

func runTLSProxy(args []string) error {
	flags := flag.NewFlagSet("tls-proxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listenAddress := flags.String("listen", "", "")
	upstream := flags.String("upstream", "", "")
	certificate := flags.String("cert-file", "", "")
	privateKey := flags.String("key-file", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("e2e-handcraft-oci: invalid TLS proxy arguments")
	}
	config, err := validateTLSProxyConfig(*listenAddress, *upstream, *certificate, *privateKey)
	if err != nil {
		return errors.New("e2e-handcraft-oci: invalid TLS proxy configuration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serveTLSProxy(ctx, config); err != nil {
		return errors.New("e2e-handcraft-oci: TLS proxy failed")
	}
	return nil
}

func validateTLSProxyConfig(listenAddress, upstream, certificate, privateKey string) (tlsProxyConfig, error) {
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil || host != "" {
		return tlsProxyConfig{}, errors.New("TLS proxy must listen on all interfaces at an explicit port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1024 || portNumber > 65535 {
		return tlsProxyConfig{}, errors.New("TLS proxy listen port is invalid")
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme != "http" || target.Host == "" || target.User != nil ||
		(target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" {
		return tlsProxyConfig{}, errors.New("TLS proxy upstream must be an authority-only HTTP URL")
	}
	if _, _, err := net.SplitHostPort(target.Host); err != nil {
		return tlsProxyConfig{}, errors.New("TLS proxy upstream must have an explicit port")
	}
	if !filepath.IsAbs(certificate) || !filepath.IsAbs(privateKey) || certificate == privateKey {
		return tlsProxyConfig{}, errors.New("TLS proxy certificate and private-key paths must be distinct and absolute")
	}
	return tlsProxyConfig{
		listenAddress: listenAddress,
		upstream:      target,
		certificate:   certificate,
		privateKey:    privateKey,
	}, nil
}

func serveTLSProxy(ctx context.Context, config tlsProxyConfig) error {
	var requestCount atomic.Uint64
	tlsServer := &http.Server{
		Addr:              config.listenAddress,
		Handler:           newTLSProxyHandler(config.upstream, &requestCount),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS13,
		},
	}
	adminServer := &http.Server{
		Addr:              adminListenAddress,
		Handler:           newRequestCountHandler(&requestCount),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    4 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	type serverResult struct {
		name string
		err  error
	}
	serverErrors := make(chan serverResult, 2)
	go func() {
		serverErrors <- serverResult{name: "TLS proxy", err: tlsServer.ListenAndServeTLS(config.certificate, config.privateKey)}
	}()
	go func() {
		serverErrors <- serverResult{name: "admin", err: adminServer.ListenAndServe()}
	}()
	shutdown := func() error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tlsErr := tlsServer.Shutdown(shutdownContext)
		adminErr := adminServer.Shutdown(shutdownContext)
		return errors.Join(tlsErr, adminErr)
	}
	select {
	case result := <-serverErrors:
		if err := shutdown(); err != nil {
			return err
		}
		<-serverErrors
		if errors.Is(result.err, http.ErrServerClosed) {
			return fmt.Errorf("%s server closed unexpectedly", result.name)
		}
		return result.err
	case <-ctx.Done():
		if err := shutdown(); err != nil {
			return err
		}
		for range 2 {
			result := <-serverErrors
			if !errors.Is(result.err, http.ErrServerClosed) {
				return result.err
			}
		}
		return nil
	}
}

func newTLSProxyHandler(upstream *url.URL, requestCount *atomic.Uint64) http.Handler {
	return newTLSProxyHandlerWithTransport(upstream, requestCount, nil)
}

func newTLSProxyHandlerWithTransport(
	upstream *url.URL,
	requestCount *atomic.Uint64,
	transport http.RoundTripper,
) http.Handler {
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
			DisableCompression:    true,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       30 * time.Second,
		}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Host = upstream.Host
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
		},
		Transport: transport,
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
				return errors.New("registry redirects are not permitted")
			}
			return nil
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			response.Header().Del("Location")
			http.Error(response, "upstream registry response rejected", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2" && !strings.HasPrefix(request.URL.Path, "/v2/") {
			http.NotFound(response, request)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestCount.Add(1)
		proxy.ServeHTTP(response, request)
	})
}

func newRequestCountHandler(requestCount *atomic.Uint64) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/" || request.URL.RawQuery != "" {
			http.NotFound(response, request)
			return
		}
		_, _ = fmt.Fprintf(response, "%d\n", requestCount.Load())
	})
}

func newRegistryClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func readSchema(path string) ([]byte, error) {
	return readBoundedFile(path, maxSchemaBytes)
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 || int64(len(contents)) > maximum {
		return nil, errors.New("input is empty or oversized")
	}
	return contents, nil
}

func parseReference(raw string) (registryReference, error) {
	trimmed := strings.TrimPrefix(raw, "oci://")
	if trimmed == raw || strings.ContainsAny(trimmed, "?#@") {
		return registryReference{}, errors.New("invalid OCI reference")
	}
	slash := strings.IndexByte(trimmed, '/')
	if slash <= 0 || slash == len(trimmed)-1 {
		return registryReference{}, errors.New("invalid OCI repository")
	}
	host, repositoryTag := trimmed[:slash], trimmed[slash+1:]
	colon := strings.LastIndexByte(repositoryTag, ':')
	if colon <= 0 || colon == len(repositoryTag)-1 {
		return registryReference{}, errors.New("OCI tag is required")
	}
	repository, tag := repositoryTag[:colon], repositoryTag[colon+1:]
	if !referencePart.MatchString(repository) || !referencePart.MatchString(tag) ||
		strings.Contains(tag, "/") {
		return registryReference{}, errors.New("invalid OCI repository or tag")
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		return registryReference{}, errors.New("OCI registry must include an explicit port")
	}
	return registryReference{host: host, repository: repository, tag: tag}, nil
}

func publish(
	ctx context.Context,
	client *http.Client,
	ref registryReference,
	credential credentials,
	schema []byte,
) (string, error) {
	config := []byte("{}")
	configDescriptor := descriptor{MediaType: emptyConfigType, Digest: digest(config), Size: len(config)}
	layerDescriptor := descriptor{
		MediaType: schemaLayerType,
		Digest:    digest(schema),
		Size:      len(schema),
		Annotations: map[string]string{
			"org.opencontainers.image.title": "schema.hcl",
		},
	}
	if err := uploadBlob(ctx, client, ref, credential, configDescriptor, config); err != nil {
		return "", err
	}
	if err := uploadBlob(ctx, client, ref, credential, layerDescriptor, schema); err != nil {
		return "", err
	}
	manifestBytes, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     manifestType,
		ArtifactType:  schemaArtifactType,
		Config:        configDescriptor,
		Layers:        []descriptor{layerDescriptor},
		Annotations: map[string]string{
			"io.stokaro.ptah.kind":          schemaArtifactType,
			"io.stokaro.ptah.schema-format": "hcl",
		},
	})
	if err != nil {
		return "", errors.New("encode OCI manifest")
	}
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", ref.host, ref.repository, ref.tag)
	status, _, err := request(ctx, client, http.MethodPut, manifestURL, manifestType, credential, manifestBytes)
	if err != nil || status != http.StatusCreated {
		return "", errors.New("store OCI manifest")
	}
	return digest(manifestBytes), nil
}

func uploadBlob(
	ctx context.Context,
	client *http.Client,
	ref registryReference,
	credential credentials,
	desc descriptor,
	contents []byte,
) error {
	startURL := fmt.Sprintf("http://%s/v2/%s/blobs/uploads/", ref.host, ref.repository)
	status, location, err := request(ctx, client, http.MethodPost, startURL, "", credential, nil)
	if err != nil || status != http.StatusAccepted || location == "" {
		return errors.New("start OCI blob upload")
	}
	base, err := url.Parse(startURL)
	if err != nil {
		return errors.New("parse OCI upload base")
	}
	target, err := base.Parse(location)
	if err != nil || target.Host != ref.host || target.Scheme != "http" {
		return errors.New("registry returned an unsafe upload location")
	}
	query := target.Query()
	query.Set("digest", desc.Digest)
	target.RawQuery = query.Encode()
	status, _, err = request(ctx, client, http.MethodPut, target.String(), "application/octet-stream", credential, contents)
	if err != nil || status != http.StatusCreated {
		return errors.New("complete OCI blob upload")
	}
	return nil
}

func request(
	ctx context.Context,
	client *http.Client,
	method string,
	target string,
	contentType string,
	credential credentials,
	body []byte,
) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", errors.New("create registry request")
	}
	request.SetBasicAuth(credential.username, credential.password)
	request.Header.Set("User-Agent", "ptah-operator-e2e")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", errors.New("execute registry request")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	return response.StatusCode, response.Header.Get("Location"), nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}
