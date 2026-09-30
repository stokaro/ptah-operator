package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func runnerELFFixture(architecture string) []byte {
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})
	binary.LittleEndian.PutUint16(header[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(header[18:], uint16(map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[architecture]))
	binary.LittleEndian.PutUint32(header[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(header[52:], 64)
	return header
}

func runnerSourceFixture(t *testing.T) (source, config, record []byte) {
	t.Helper()
	config = []byte(`{"os":"linux","architecture":"amd64","config":{"Env":["kept=value"],"Entrypoint":["/manager"]},"rootfs":{"type":"layers","diff_ids":["sha256:` + strings.Repeat("b", 64) + `"],"extension":"kept"},"history":[{"created_by":"shipping source"}],"extension":{"kept":true}}`)
	source, _ = json.Marshal(manifest{SchemaVersion: 2, MediaType: manifestType,
		Config:      descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digest(config), Size: len(config)},
		Layers:      []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:" + strings.Repeat("a", 64), Size: 123}},
		Annotations: map[string]string{"shipping": "kept"}})
	record, _ = json.Marshal(map[string]any{"supportedProtocol": runner.ProtocolVersion, "refusedProtocol": runner.ProtocolVersion + 1,
		"sourceSHA256": strings.Repeat("c", 64), "overlaySHA256": strings.Repeat("d", 64), "scope": runnerFixtureScope})
	return source, config, record
}

func TestRunnerVariantAddsOnlyTheExplicitExecutableLayer(t *testing.T) {
	t.Parallel()
	source, config, record := runnerSourceFixture(t)
	executable := runnerELFFixture("amd64")
	variant, err := runnerVariant(source, config, executable, record, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(variant.layer))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil || reader.Close() != nil {
		t.Fatal("the runner layer is not a complete gzip stream")
	}
	archive := tar.NewReader(bytes.NewReader(raw))
	header, err := archive.Next()
	if err != nil || header.Name != "ptah-runner" || header.Typeflag != tar.TypeReg || header.Mode != 0o755 || header.Uid != 65532 || header.Gid != 65532 {
		t.Fatal("the fixture replaced something outside the exact unprivileged runner path")
	}
	body, err := io.ReadAll(archive)
	if err != nil || !bytes.Equal(body, executable) {
		t.Fatal("the executable bytes changed in publication")
	}
	if _, err := archive.Next(); err != io.EOF {
		t.Fatal("another file was added to the runner layer")
	}
	var beforeConfig, afterConfig map[string]any
	json.Unmarshal(config, &beforeConfig)
	json.Unmarshal(variant.config, &afterConfig)
	rootFS := afterConfig["rootfs"].(map[string]any)
	ids := rootFS["diff_ids"].([]any)
	if len(ids) != 2 || ids[1] != digest(raw) {
		t.Fatal("the new root filesystem is not bound to the exact uncompressed runner layer")
	}
	rootFS["diff_ids"] = ids[:1]
	if !reflect.DeepEqual(beforeConfig, afterConfig) {
		t.Fatal("the fixture changed the manager entrypoint, environment or other source configuration")
	}
	var beforeManifest, afterManifest manifest
	json.Unmarshal(source, &beforeManifest)
	json.Unmarshal(variant.manifest, &afterManifest)
	if len(afterManifest.Layers) != 2 || !reflect.DeepEqual(afterManifest.Layers[1], variant.layerDescriptor) || !reflect.DeepEqual(afterManifest.Layers[:1], beforeManifest.Layers) ||
		afterManifest.Config.Digest != digest(variant.config) || afterManifest.Config.Size != len(variant.config) ||
		afterManifest.Annotations[runnerVariantAnnotation+"-provenance"] != digest(record) || afterManifest.Annotations["shipping"] != "kept" {
		t.Fatal("the source layers or explicit fixture provenance were lost")
	}
	if _, err := runnerVariant(variant.manifest, variant.config, executable, record, "amd64"); err == nil {
		t.Fatal("a synthetic runner was accepted as the shipping source")
	}
}

func TestRunnerVariantRefusesWrongPlatformsAndSourceContracts(t *testing.T) {
	t.Parallel()
	source, config, record := runnerSourceFixture(t)
	for name, test := range map[string]struct {
		source, config, executable, record []byte
		architecture                       string
	}{
		"wrong binary platform": {source, config, runnerELFFixture("arm64"), record, "amd64"},
		"wrong image platform":  {source, config, runnerELFFixture("amd64"), record, "arm64"},
		"unsupported platform":  {source, config, runnerELFFixture("amd64"), record, "riscv64"},
		"not an executable":     {source, config, []byte("ordinary source file"), record, "amd64"},
		"wrong source config":   {source, []byte(`{}`), runnerELFFixture("amd64"), record, "amd64"},
		"no source platform":    {[]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`), config, runnerELFFixture("amd64"), record, "amd64"},
		"no provenance":         {source, config, runnerELFFixture("amd64"), nil, "amd64"},
		"changed counter":       {source, config, runnerELFFixture("amd64"), []byte(strings.Replace(string(record), `"supportedProtocol":1`, `"supportedProtocol":2`, 1)), "amd64"},
		"compatibility claim":   {source, config, runnerELFFixture("amd64"), []byte(strings.Replace(string(record), runnerFixtureScope, "future compatibility", 1)), "amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runnerVariant(test.source, test.config, test.executable, test.record, test.architecture); err == nil {
				t.Fatal("an invalid or unrelated runner supplied a qualified image identity")
			}
		})
	}
}

func TestRunnerVariantReadsEveryPublishedByteBackByDigest(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"", "index", "index child digest", "index child size", "source manifest", "source config", "unsafe upload", "upload refused", "blob read-back", "manifest refused", "manifest read-back"} {
		t.Run(fault, func(t *testing.T) {
			source, config, record := runnerSourceFixture(t)
			sourceDigest := digest(source)
			root, rootDigest := source, sourceDigest
			if strings.HasPrefix(fault, "index") {
				size := len(source)
				if fault == "index child size" {
					size++
				}
				root, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []any{
					map[string]any{"mediaType": manifestType, "size": 1, "digest": "sha256:" + strings.Repeat("a", 64), "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
					map[string]any{"mediaType": manifestType, "size": size, "digest": sourceDigest, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
				}})
				rootDigest = digest(root)
			}
			var stored []byte
			blobs := map[string][]byte{digest(config): config}
			credential := credentials{username: "fixture-user", password: "private-value"}
			uploads, blobChecks, manifests := 0, 0, 0
			registry := newRegistryClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				user, password, ok := req.BasicAuth()
				if !ok || user != credential.username || password != credential.password || req.URL.Host != "registry.test:5000" {
					t.Fatal("a publication credential escaped its declared registry")
				}
				status, body := http.StatusOK, []byte(nil)
				header := http.Header{}
				switch {
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/manifests/"):
					body = source
					if strings.HasPrefix(fault, "index") && strings.HasSuffix(req.URL.Path, rootDigest) {
						body = root
						header.Set("Docker-Content-Digest", rootDigest)
					} else if strings.HasSuffix(req.URL.Path, sourceDigest) {
						if fault == "source manifest" || fault == "index child digest" {
							body = []byte("corrupt")
						}
						header.Set("Docker-Content-Digest", sourceDigest)
					} else {
						body = stored
						header.Set("Docker-Content-Digest", digest(stored))
						if !strings.HasSuffix(req.URL.Path, digest(stored)) {
							t.Fatal("the synthetic manifest was not read by its exact digest")
						}
						if fault == "manifest read-back" {
							body = source
						}
					}
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/"):
					body = blobs[req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]]
					if body == nil {
						t.Fatal("an unrecorded blob was read")
					}
					if bytes.Equal(body, config) && fault == "source config" {
						body = []byte("corrupt")
					} else if !bytes.Equal(body, config) {
						blobChecks++
						if fault == "blob read-back" {
							body = append(bytes.Clone(body), 0)
						}
					}
				case req.Method == http.MethodPost:
					status = http.StatusAccepted
					header.Set("Location", "/v2/runner/blobs/uploads/upload")
					if fault == "unsafe upload" {
						header.Set("Location", "http://other.test/private")
					}
				case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/blobs/uploads/"):
					uploads++
					body, _ = io.ReadAll(req.Body)
					if req.URL.Query().Get("digest") != digest(body) {
						t.Fatal("a blob was uploaded without its exact content digest")
					}
					blobs[digest(body)] = bytes.Clone(body)
					status, body = http.StatusCreated, nil
					if fault == "upload refused" {
						status = http.StatusForbidden
					}
				case req.Method == http.MethodPut && req.URL.Path == "/v2/runner/manifests/proof":
					manifests++
					stored, _ = io.ReadAll(req.Body)
					status = http.StatusCreated
					if fault == "manifest refused" {
						status = http.StatusForbidden
					}
				default:
					t.Fatalf("unexpected registry transaction: %s %s", req.Method, req.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}))
			got, err := publishRunnerVariant(context.Background(), registry, registryReference{host: "registry.test:5000", repository: "runner", tag: "proof"},
				credential, rootDigest, runnerELFFixture("amd64"), record, "amd64")
			if fault == "" || fault == "index" {
				if err != nil || uploads != 2 || blobChecks != 2 || manifests != 1 || got != digest(stored) || got == sourceDigest {
					t.Fatalf("the complete source, upload and read-back controls failed: uploads=%d checks=%d manifests=%d result=%s error=%v", uploads, blobChecks, manifests, got, err)
				}
			} else if err == nil || got != "" || strings.Contains(err.Error(), credential.password) {
				t.Fatal("a corrupt or failed publication supplied an image identity or leaked its credential")
			}
		})
	}
}

func TestRunnerIndexRequiresOneExactNativePlatform(t *testing.T) {
	t.Parallel()
	for _, architecture := range []string{"amd64", "arm64"} {
		entry := map[string]any{"mediaType": manifestType, "size": 42, "digest": "sha256:" + strings.Repeat("a", 64), "platform": map[string]string{"os": "linux", "architecture": architecture}}
		encode := func(entries []any) []byte {
			raw, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": entries})
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		if result, err := runnerPlatformManifest(encode([]any{entry}), architecture); err != nil || result.Digest != entry["digest"] {
			t.Fatal("native image was not selected", result, err)
		}
		for name, entries := range map[string][]any{"empty": {}, "ambiguous": {entry, entry}, "wrong platform": {map[string]any{"platform": map[string]string{"os": "windows", "architecture": architecture}}}} {
			t.Run(architecture+"/"+name, func(t *testing.T) {
				if _, err := runnerPlatformManifest(encode(entries), architecture); err == nil {
					t.Fatal("an ambiguous or missing native image passed")
				}
			})
		}
	}
}
