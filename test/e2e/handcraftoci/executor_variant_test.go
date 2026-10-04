package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func executorSourceFixture(media string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"` + media + `","annotations":{"kept":"source"},"config":{"digest":"sha256:config","size":42},"layers":[{"digest":"sha256:layer","size":100}],"manifests":[{"digest":"sha256:platform","platform":{"os":"linux","architecture":"amd64"}}],"extension":{"kept":true}}`)
}

func TestExecutorVariantPreservesImageContents(t *testing.T) {
	t.Parallel()
	for _, media := range []string{manifestType, "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json"} {
		t.Run(media, func(t *testing.T) {
			raw := executorSourceFixture(media)
			updated, gotMedia, err := executorVariant(raw)
			if err != nil || gotMedia != media || digest(raw) == digest(updated) {
				t.Fatalf("variant identity: %s, %v", gotMedia, err)
			}
			var before, after map[string]any
			if json.Unmarshal(raw, &before) != nil || json.Unmarshal(updated, &after) != nil {
				t.Fatal("invalid manifest")
			}
			if after["annotations"].(map[string]any)[executorVariantAnnotation] != "execution-binding-proof" {
				t.Fatal("variant identity is not explicitly marked as a test fixture")
			}
			delete(after["annotations"].(map[string]any), executorVariantAnnotation)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("the variant changed content, platform, or existing metadata")
			}
			if _, _, err := executorVariant(updated); err == nil {
				t.Fatal("a variant was accepted as the original executor")
			}
		})
	}
	for _, bad := range []string{"", "{}", "null", `{"schemaVersion":2,"mediaType":"unsupported"}`,
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`,
		strings.Replace(string(executorSourceFixture(manifestType)), `"schemaVersion":2`, `"schemaVersion":1`, 1),
		strings.Replace(string(executorSourceFixture(manifestType)), `"annotations":{"kept":"source"}`, `"annotations":null`, 1),
		strings.Replace(string(executorSourceFixture(manifestType)), `"schemaVersion":2`, `"schemaVersion":2,"artifactType":"arbitrary"`, 1),
	} {
		if _, _, err := executorVariant([]byte(bad)); err == nil {
			t.Fatal("an invalid or non-image manifest became an executor")
		}
	}
}

func TestExecutorVariantVerifiesRegistryReadBack(t *testing.T) {
	t.Parallel()
	for _, broken := range []string{"", "source bytes", "source header", "oversized source", "redirect", "write refused", "read-back bytes", "read-back header"} {
		t.Run(broken, func(t *testing.T) {
			source := executorSourceFixture("application/vnd.oci.image.index.v1+json")
			sourceDigest := digest(source)
			var stored []byte
			reads, writes := 0, 0
			credential := credentials{username: "fixture-user", password: "private-value"}
			client := newRegistryClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				user, password, ok := req.BasicAuth()
				if !ok || user != credential.username || password != credential.password || req.URL.Host != "registry.test:5000" {
					t.Fatal("registry credentials escaped their declared destination")
				}
				status, body, hash := http.StatusOK, source, sourceDigest
				if req.Method == http.MethodPut {
					writes++
					var err error
					stored, err = io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					if req.URL.Path != "/v2/executor/manifests/proof" || req.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
						t.Fatal("unexpected write target")
					}
					status, body, hash = http.StatusCreated, nil, ""
					if broken == "write refused" {
						status = http.StatusForbidden
					}
				} else {
					reads++
					if req.Method != http.MethodGet || req.Header.Get("Accept") == "" {
						t.Fatal("unexpected read")
					}
					if reads == 1 {
						if req.URL.Path != "/v2/executor/manifests/"+sourceDigest {
							t.Fatal("source was not read by digest")
						}
						switch broken {
						case "source bytes":
							body = []byte("changed")
						case "source header":
							hash = "wrong"
						case "oversized source":
							body = []byte(strings.Repeat("x", maxSchemaBytes+1))
						case "redirect":
							status = http.StatusTemporaryRedirect
						}
					} else {
						if req.URL.Path != "/v2/executor/manifests/"+digest(stored) {
							t.Fatal("variant was not read by digest")
						}
						body, hash = stored, digest(stored)
						if broken == "read-back bytes" {
							body = source
						}
						if broken == "read-back header" {
							hash = sourceDigest
						}
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Docker-Content-Digest": []string{hash}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}))
			got, err := publishExecutorVariant(context.Background(), client, registryReference{host: "registry.test:5000", repository: "executor", tag: "proof"}, credential, sourceDigest)
			if broken == "" {
				if err != nil || reads != 2 || writes != 1 || got != digest(stored) || got == sourceDigest {
					t.Fatalf("read/write/read-back: reads=%d writes=%d digest=%s error=%v", reads, writes, got, err)
				}
			} else if err == nil || got != "" {
				t.Fatal("a failed registry transaction supplied an executor identity")
			} else if strings.Contains(err.Error(), credential.password) {
				t.Fatal("registry failure disclosed its credential")
			}
		})
	}
}
