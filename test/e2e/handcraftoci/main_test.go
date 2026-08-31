package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
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
