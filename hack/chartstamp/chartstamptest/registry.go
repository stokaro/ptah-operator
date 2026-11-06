// Package chartstamptest serves the part of the OCI distribution API that a push
// and a helm pull use, so a test can run both against the bytes they would write
// to a real registry, with no daemon and no network. Like a public GHCR package, it
// reads for anyone and writes only for the credential it was given.
package chartstamptest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// PackagedChart is a chart package of the shape the packager writes, small enough
// for a test that does not read what is in it.
func PackagedChart(t *testing.T, version string) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	compressed.Header = gzip.Header{ModTime: time.Unix(1_700_000_000, 0).UTC(), OS: 255}
	archive := tar.NewWriter(compressed)
	body := "apiVersion: v2\nname: ptah-operator\nversion: " + version + "\nannotations:\n  artifacthub.io/license: Apache-2.0\n"
	if err := archive.WriteHeader(&tar.Header{
		Name: "ptah-operator/Chart.yaml", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
		ModTime: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

var (
	blobPath     = regexp.MustCompile(`^/v2/(.+)/blobs/(sha256:[0-9a-f]{64})$`)
	uploadPath   = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/(.*)$`)
	manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)
	tagsPath     = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

type manifest struct {
	mediaType string
	body      []byte
}

// Registry is an in-memory OCI registry.
type Registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]map[string]manifest
	uploads   int
	server    *httptest.Server

	// Username and Password, when set, are what a write has to present.
	Username, Password string
	// Private makes every read need that credential too, as a package does on
	// GHCR until someone makes it public.
	Private bool
}

// NewRegistry starts a registry for the length of a test.
func NewRegistry(t *testing.T) *Registry {
	t.Helper()
	r := &Registry{blobs: map[string][]byte{}, manifests: map[string]map[string]manifest{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

// Host is the registry's host:port, plain HTTP.
func (r *Registry) Host() string {
	return strings.TrimPrefix(r.server.URL, "http://")
}

// Blobs is how many blobs the registry holds, which a test reads to show a
// refused push wrote nothing.
func (r *Registry) Blobs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.blobs)
}

// Tags is what the repository's tags are, without the digests it also stores
// manifests under.
func (r *Registry) Tags(repository string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tagNames(repository)
}

func (r *Registry) allowed(request *http.Request) bool {
	read := request.Method == http.MethodGet || request.Method == http.MethodHead
	if r.Username == "" || (read && !r.Private) {
		return true
	}
	username, password, _ := request.BasicAuth()
	return username == r.Username && password == r.Password
}

func (r *Registry) serve(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.allowed(request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="chartstamptest"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := request.URL.Path
	switch {
	case path == "/v2/" || path == "/v2":
		w.WriteHeader(http.StatusOK)
	case tagsPath.MatchString(path):
		r.tags(w, tagsPath.FindStringSubmatch(path)[1])
	case uploadPath.MatchString(path):
		r.upload(w, request, uploadPath.FindStringSubmatch(path))
	case blobPath.MatchString(path):
		r.blob(w, request, blobPath.FindStringSubmatch(path)[2])
	case manifestPath.MatchString(path):
		match := manifestPath.FindStringSubmatch(path)
		r.manifest(w, request, match[1], match[2])
	default:
		http.NotFound(w, request)
	}
}

func (r *Registry) tagNames(repository string) []string {
	names := []string{}
	for reference := range r.manifests[repository] {
		if !strings.HasPrefix(reference, "sha256:") {
			names = append(names, reference)
		}
	}
	sort.Strings(names)
	return names
}

func (r *Registry) tags(w http.ResponseWriter, repository string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repository, "tags": r.tagNames(repository)})
}

func (r *Registry) upload(w http.ResponseWriter, request *http.Request, match []string) {
	switch request.Method {
	case http.MethodPost:
		r.uploads++
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%d", match[1], r.uploads))
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
		if digest != request.URL.Query().Get("digest") {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		r.blobs[digest] = body
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) blob(w http.ResponseWriter, request *http.Request, digest string) {
	body, ok := r.blobs[digest]
	if !ok {
		http.NotFound(w, request)
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("Content-Type", "application/octet-stream")
	if request.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (r *Registry) manifest(w http.ResponseWriter, request *http.Request, repository, reference string) {
	if request.Method == http.MethodPut {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
		if r.manifests[repository] == nil {
			r.manifests[repository] = map[string]manifest{}
		}
		stored := manifest{mediaType: request.Header.Get("Content-Type"), body: body}
		r.manifests[repository][reference] = stored
		r.manifests[repository][digest] = stored
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}
	stored, ok := r.manifests[repository][reference]
	if !ok {
		http.NotFound(w, request)
		return
	}
	w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256(stored.body)))
	w.Header().Set("Content-Length", fmt.Sprint(len(stored.body)))
	w.Header().Set("Content-Type", stored.mediaType)
	if request.Method != http.MethodHead {
		_, _ = w.Write(stored.body)
	}
}
