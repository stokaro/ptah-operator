package s3source_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/s3source"
	"github.com/stokaro/ptah-operator/internal/sourcearchive"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// bucket is a fake S3 endpoint: one object per key, each with its versions,
// the requests it was sent, and the answer to give instead of the object.
type bucket struct {
	mu       sync.Mutex
	objects  map[string][]version
	requests []*http.Request
	answer   func(http.ResponseWriter, *http.Request) bool
}

type version struct {
	id, encoding string
	data         []byte
}

func (b *bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, r.Clone(r.Context()))
	answer := b.answer
	versions := b.objects[r.URL.Path]
	b.mu.Unlock()
	if answer != nil && answer(w, r) {
		return
	}
	if len(versions) == 0 {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>x</Key></Error>`))
		return
	}
	served := versions[len(versions)-1]
	// A key whose versions carry no ID is on a server that keeps none and
	// ignores the versionId a request asks for.
	if want := r.URL.Query().Get("versionId"); want != "" && served.id != "" {
		found := false
		for _, candidate := range versions {
			if candidate.id == want {
				served, found = candidate, true
			}
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchVersion</Code><Message>The specified version does not exist.</Message></Error>`))
			return
		}
	}
	if served.id != "" {
		w.Header().Set("X-Amz-Version-Id", served.id)
	}
	if served.encoding != "" {
		w.Header().Set("Content-Encoding", served.encoding)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(served.data)))
	_, _ = w.Write(served.data)
}

func (b *bucket) last(t *testing.T) *http.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("the endpoint received no request")
	}
	return b.requests[len(b.requests)-1]
}

// serve starts the endpoint behind TLS and returns it with the PEM file of
// its certificate, the only root a fetch trusts.
func serve(t *testing.T, b *bucket) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(b)
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(ca, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	return server, ca
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func tarGzip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func options(server *httptest.Server, ca, key string, data []byte) s3source.Options {
	return s3source.Options{
		Object: s3source.Object{
			Endpoint: server.URL, Region: "us-east-1", Bucket: "orders", Key: key,
		},
		CAFile:               ca,
		ExpectDownloadDigest: digestOf(data),
		Archive:              sourcearchive.Options{Format: sourcearchive.FormatFile, Path: "schema.hcl", Kind: sourcearchive.File},
		TempDir:              os.TempDir(),
	}
}

func TestAFetchReadsTheObjectItIsPinnedTo(t *testing.T) {
	t.Parallel()
	schema := []byte("table \"orders\" {}\n")
	archive := tarGzip(t, map[string]string{"db/migrations/0001_init.up.sql": "CREATE TABLE a (id int);\n", "README": "x"})
	b := &bucket{objects: map[string][]version{
		"/orders/schema.hcl":          {{id: "v1", data: schema}},
		"/orders/releases/1.4.tar.gz": {{id: "v1", data: archive}},
		"/orders/unversioned.hcl":     {{id: "null", data: schema}},
	}}
	server, ca := serve(t, b)

	t.Run("a schema file, unsigned", func(t *testing.T) {
		result, err := s3source.Fetch(t.Context(), options(server, ca, "schema.hcl", schema))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := sourcecontent.Digest([]sourcecontent.File{{Path: "schema.hcl", Data: schema}})
		if result.ContentDigest != want || result.DownloadDigest != digestOf(schema) || result.VersionID != "v1" {
			t.Fatalf("the fetch read %+v", result)
		}
		request := b.last(t)
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-Amz-Date") != "" {
			t.Errorf("an unsigned fetch sent Authorization %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("the fetch asked for %q, not identity", request.Header.Get("Accept-Encoding"))
		}
	})

	t.Run("a migration directory, signed with a session token", func(t *testing.T) {
		o := options(server, ca, "releases/1.4.tar.gz", archive)
		o.Archive = sourcearchive.Options{Format: sourcearchive.FormatTarGzip, Path: "db/migrations", Kind: sourcearchive.Directory}
		o.Credentials = s3source.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"}
		o.Now = func() time.Time { return time.Date(2026, time.November, 23, 10, 0, 0, 0, time.UTC) }
		result, err := s3source.Fetch(t.Context(), o)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Files) != 1 || result.Files[0].Path != "0001_init.up.sql" {
			t.Fatalf("the fetch selected %+v", result.Files)
		}
		request := b.last(t)
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=AKID/20261123/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=") {
			t.Errorf("the fetch sent Authorization %q", authorization)
		}
		if request.Header.Get("X-Amz-Security-Token") != "token" || request.URL.Path != "/orders/releases/1.4.tar.gz" {
			t.Errorf("the fetch sent %s with token %q", request.URL.Path, request.Header.Get("X-Amz-Security-Token"))
		}
	})

	t.Run("the null version of a bucket that keeps none is no version", func(t *testing.T) {
		result, err := s3source.Fetch(t.Context(), options(server, ca, "unversioned.hcl", schema))
		if err != nil {
			t.Fatal(err)
		}
		if result.VersionID != "" {
			t.Fatalf("the fetch recorded the version %q", result.VersionID)
		}
	})

	t.Run("the version the spec names", func(t *testing.T) {
		o := options(server, ca, "schema.hcl", schema)
		o.Object.VersionID = "v1"
		if _, err := s3source.Fetch(t.Context(), o); err != nil {
			t.Fatal(err)
		}
		if got := b.last(t).URL.RawQuery; got != "versionId=v1" {
			t.Errorf("the fetch asked for %q", got)
		}
	})
}

// TestAnObjectStoredEncodedIsHashedAsStored holds the fetch to the bytes the
// bucket returns: an object uploaded with Content-Encoding gzip is not
// decoded on the way, so its digest is what sha256sum prints for the upload.
func TestAnObjectStoredEncodedIsHashedAsStored(t *testing.T) {
	t.Parallel()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("table \"orders\" {}\n"))
	_ = writer.Close()
	b := &bucket{objects: map[string][]version{"/orders/schema.hcl": {{encoding: "gzip", data: compressed.Bytes()}}}}
	server, ca := serve(t, b)
	result, err := s3source.Fetch(t.Context(), options(server, ca, "schema.hcl", compressed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if result.DownloadDigest != digestOf(compressed.Bytes()) || !bytes.Equal(result.Files[0].Data, compressed.Bytes()) {
		t.Fatalf("the fetch decoded the object: %s", result.DownloadDigest)
	}
}

func TestAFetchRefusesWhatItIsNotPinnedTo(t *testing.T) {
	t.Parallel()
	schema := []byte("table \"orders\" {}\n")
	other := []byte("table \"other\" {}\n")
	b := &bucket{objects: map[string][]version{
		"/orders/schema.hcl":      {{id: "v1", data: schema}, {id: "v2", data: other}},
		"/orders/unversioned.hcl": {{id: "null", data: schema}},
		"/orders/ignored.hcl":     {{data: schema}},
	}}
	server, ca := serve(t, b)
	for _, refused := range []struct {
		name    string
		change  func(*s3source.Options)
		want    error
		message string
	}{
		{
			name:    "an overwritten key",
			change:  func(*s3source.Options) {},
			want:    s3source.ErrDownloadDigest,
			message: "gave " + digestOf(other) + ", not " + digestOf(schema),
		},
		{
			name:    "a missing key",
			change:  func(o *s3source.Options) { o.Object.Key = "absent.hcl" },
			want:    s3source.ErrStatus,
			message: "404 Not Found, NoSuchKey: The specified key does not exist.",
		},
		{
			name:    "a missing version",
			change:  func(o *s3source.Options) { o.Object.VersionID = "v9" },
			want:    s3source.ErrStatus,
			message: "NoSuchVersion",
		},
		{
			name: "another content digest",
			change: func(o *s3source.Options) {
				o.Object.VersionID = "v1"
				o.ExpectDigest = "sha256:" + strings.Repeat("0", 64)
			},
			want:    s3source.ErrDigest,
			message: "not sha256:" + strings.Repeat("0", 64),
		},
		{
			name: "a version the endpoint does not say it served",
			change: func(o *s3source.Options) {
				o.Object.Key = "ignored.hcl"
				o.Object.VersionID = "v1"
			},
			want:    s3source.ErrVersion,
			message: "said it served no version",
		},
		{
			name: "the null version of a bucket that keeps none, asked for by another ID",
			change: func(o *s3source.Options) {
				o.Object.Key = "unversioned.hcl"
				o.Object.VersionID = "v1"
			},
			want:    s3source.ErrStatus,
			message: "NoSuchVersion",
		},
		{
			name:    "a certificate that does not chain to the selected CA",
			change:  func(o *s3source.Options) { o.CAFile = "" },
			message: "certificate",
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			o := options(server, ca, "schema.hcl", schema)
			refused.change(&o)
			_, err := s3source.Fetch(t.Context(), o)
			if err == nil || (refused.want != nil && !errors.Is(err, refused.want)) || !strings.Contains(err.Error(), refused.message) {
				t.Fatalf("Fetch: %v, want %v naming %q", err, refused.want, refused.message)
			}
		})
	}
}

// TestARedirectIsRefusedAndNotFollowed holds the fetch to the endpoint the
// spec names: S3 answers a request sent to another region's endpoint with a
// 301 and an error document, and the fetch names that and sends nothing to
// the address the redirect gives.
func TestARedirectIsRefusedAndNotFollowed(t *testing.T) {
	t.Parallel()
	elsewhere := &bucket{}
	other, _ := serve(t, elsewhere)
	b := &bucket{answer: func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Location", other.URL+"/orders/schema.hcl")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`<Error><Code>PermanentRedirect</Code><Message>The bucket you are attempting to access must be addressed using the specified endpoint.</Message></Error>`))
		return true
	}}
	server, ca := serve(t, b)
	o := options(server, ca, "schema.hcl", []byte("x"))
	o.Credentials = s3source.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	_, err := s3source.Fetch(t.Context(), o)
	if !errors.Is(err, s3source.ErrRedirect) || !strings.Contains(err.Error(), "301 Moved Permanently, PermanentRedirect: The bucket you are attempting") {
		t.Fatalf("Fetch: %v", err)
	}
	elsewhere.mu.Lock()
	defer elsewhere.mu.Unlock()
	if len(elsewhere.requests) != 0 {
		t.Fatalf("the fetch followed the redirect with %d requests", len(elsewhere.requests))
	}
}

// TestASignatureMismatchNamesTheCodeAndNotTheDocument holds a refusal to the
// code and the sentence of the error document: a SignatureDoesNotMatch
// document also carries the string the server signed and the access key ID,
// and neither is read.
func TestASignatureMismatchNamesTheCodeAndNotTheDocument(t *testing.T) {
	t.Parallel()
	b := &bucket{answer: func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message><AWSAccessKeyId>AKIDLEAKED</AWSAccessKeyId><StringToSign>AWS4-HMAC-SHA256 LEAKED</StringToSign></Error>`))
		return true
	}}
	server, ca := serve(t, b)
	o := options(server, ca, "schema.hcl", []byte("x"))
	o.Credentials = s3source.Credentials{AccessKeyID: "AKID", SecretAccessKey: "wrong"}
	_, err := s3source.Fetch(t.Context(), o)
	if !errors.Is(err, s3source.ErrStatus) || !strings.Contains(err.Error(), "403 Forbidden, SignatureDoesNotMatch: The request signature we calculated") {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(err.Error(), "LEAKED") {
		t.Fatalf("the refusal quotes the rest of the document: %v", err)
	}
}

func TestADownloadIsBounded(t *testing.T) {
	t.Parallel()
	for _, answer := range []struct {
		name    string
		write   func(http.ResponseWriter)
		want    error
		message string
	}{
		{
			name: "a declared length past the bound",
			write: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", strconv.Itoa(s3source.MaxDownloadBytes+1))
				w.WriteHeader(http.StatusOK)
			},
			want: s3source.ErrTooLarge, message: "declares 67108865 bytes",
		},
		{
			name: "a body past the bound",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusOK)
				chunk := bytes.Repeat([]byte("x"), 1<<20)
				for range 65 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			},
			want: s3source.ErrTooLarge, message: "gave more than 67108864 bytes",
		},
		{
			name: "a version ID that is not printable ASCII",
			write: func(w http.ResponseWriter) {
				w.Header().Set("X-Amz-Version-Id", "v 1")
				w.WriteHeader(http.StatusOK)
			},
			want: s3source.ErrVersion, message: "not 1 to 1024 bytes of printable ASCII",
		},
	} {
		t.Run(answer.name, func(t *testing.T) {
			t.Parallel()
			b := &bucket{answer: func(w http.ResponseWriter, _ *http.Request) bool {
				answer.write(w)
				return true
			}}
			server, ca := serve(t, b)
			_, err := s3source.Fetch(t.Context(), options(server, ca, "schema.hcl", []byte("x")))
			if !errors.Is(err, answer.want) || !strings.Contains(err.Error(), answer.message) {
				t.Fatalf("Fetch: %v, want %v naming %q", err, answer.want, answer.message)
			}
		})
	}
}

// TestNothingButTheCallerSignsARequest holds the fetch to the credential it
// is given. The AWS SDKs read a key pair from AWS_* variables and ~/.aws,
// and every container of a Pod sees those, so an unsigned fetch that picked
// one up would sign with a key nobody gave it.
func TestNothingButTheCallerSignsARequest(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]\naws_access_key_id = AKIAHOME\naws_secret_access_key = home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "environment")
	t.Setenv("AWS_SESSION_TOKEN", "environment-token")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", filepath.Join(home, "token"))
	schema := []byte("table \"orders\" {}\n")
	b := &bucket{objects: map[string][]version{"/orders/schema.hcl": {{data: schema}}}}
	server, ca := serve(t, b)
	if _, err := s3source.Fetch(t.Context(), options(server, ca, "schema.hcl", schema)); err != nil {
		t.Fatal(err)
	}
	request := b.last(t)
	for _, header := range []string{"Authorization", "X-Amz-Security-Token", "X-Amz-Date"} {
		if value := request.Header.Get(header); value != "" {
			t.Errorf("an unsigned fetch sent %s %q", header, value)
		}
	}
}

func TestCoordinatesTheAPIRefusesAreRefusedBeforeARequest(t *testing.T) {
	t.Parallel()
	valid := s3source.Object{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "orders", Key: "a/b.hcl"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the valid object is refused: %v", err)
	}
	for _, refused := range []struct {
		name   string
		change func(*s3source.Object)
	}{
		{"http://", func(o *s3source.Object) { o.Endpoint = "http://s3.example.com" }},
		{"a path", func(o *s3source.Object) { o.Endpoint = "https://s3.example.com/orders" }},
		{"a trailing slash", func(o *s3source.Object) { o.Endpoint = "https://s3.example.com/" }},
		{"user information", func(o *s3source.Object) { o.Endpoint = "https://key:secret@s3.example.com" }},
		{"a query", func(o *s3source.Object) { o.Endpoint = "https://s3.example.com?x=1" }},
		{"a non-ASCII host", func(o *s3source.Object) { o.Endpoint = "https://gİthub.com" }},
		{"an upper-case region", func(o *s3source.Object) { o.Region = "US-EAST-1" }},
		{"a long region", func(o *s3source.Object) { o.Region = strings.Repeat("a", 65) }},
		{"a short bucket", func(o *s3source.Object) { o.Bucket = "ab" }},
		{"an upper-case bucket", func(o *s3source.Object) { o.Bucket = "Orders" }},
		{"a key with ..", func(o *s3source.Object) { o.Key = "a/../b.hcl" }},
		{"a key with .", func(o *s3source.Object) { o.Key = "./b.hcl" }},
		{"a leading /", func(o *s3source.Object) { o.Key = "/b.hcl" }},
		{"an empty element", func(o *s3source.Object) { o.Key = "a//b.hcl" }},
		{"a control character", func(o *s3source.Object) { o.Key = "a\nb.hcl" }},
		{"a long key", func(o *s3source.Object) { o.Key = strings.Repeat("ü", 513) }},
		{"a version with a space", func(o *s3source.Object) { o.VersionID = "v 1" }},
	} {
		t.Run(refused.name, func(t *testing.T) {
			t.Parallel()
			object := valid
			refused.change(&object)
			if err := object.Validate(); !errors.Is(err, s3source.ErrObject) {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestHalfACredentialIsRefused(t *testing.T) {
	t.Parallel()
	for _, refused := range []s3source.Credentials{
		{AccessKeyID: "AKID"},
		{SecretAccessKey: "secret"},
		{SessionToken: "token"},
		{SecretAccessKey: "secret", SessionToken: "token"},
		{AccessKeyID: "AK/ID", SecretAccessKey: "secret"},
	} {
		if err := refused.Validate(); !errors.Is(err, s3source.ErrCredential) {
			t.Errorf("Validate(%+v): %v", refused, err)
		}
	}
	for _, admitted := range []s3source.Credentials{
		{},
		{AccessKeyID: "AKID", SecretAccessKey: "secret"},
		{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"},
	} {
		if err := admitted.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", admitted, err)
		}
	}
}
