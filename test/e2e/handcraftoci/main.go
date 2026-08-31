// Command handcraftoci publishes one intentionally unvalidated schema layer.
// It exists only to prove that the operator rejects a correctly typed OCI
// artifact whose content bypassed the normal safe publication path.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	schemaArtifactType = "application/vnd.stokaro.ptah.schema.v1"
	schemaLayerType    = "application/vnd.stokaro.ptah.schema.hcl.v1"
	emptyConfigType    = "application/vnd.oci.empty.v1+json"
	manifestType       = "application/vnd.oci.image.manifest.v1+json"
	maxSchemaBytes     = 1 << 20
	maxResponseBytes   = 64 << 10
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
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: e2e-handcraft-oci <oci-reference> <schema.hcl>")
		os.Exit(2)
	}
	contents, err := readSchema(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e-handcraft-oci: read bounded schema input")
		os.Exit(1)
	}
	ref, err := parseReference(os.Args[1])
	if err != nil || os.Getenv("PTAH_OCI_REGISTRY") != ref.host {
		fmt.Fprintln(os.Stderr, "e2e-handcraft-oci: invalid registry reference")
		os.Exit(1)
	}
	credential := credentials{
		username: os.Getenv("PTAH_OCI_USERNAME"),
		password: os.Getenv("PTAH_OCI_PASSWORD"),
	}
	if credential.username == "" || credential.password == "" {
		fmt.Fprintln(os.Stderr, "e2e-handcraft-oci: registry credentials are required")
		os.Exit(1)
	}
	client := newRegistryClient(nil)
	digest, err := publish(context.Background(), client, ref, credential, contents)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e-handcraft-oci: publish handcrafted OCI artifact")
		os.Exit(1)
	}
	fmt.Printf("Digest: %s\n", digest)
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
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxSchemaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 || len(contents) > maxSchemaBytes {
		return nil, errors.New("schema input is empty or oversized")
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
