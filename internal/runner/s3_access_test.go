package runner

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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// The key pair the S3 rows sign with. Both are what a refusal, a frame and a
// log line must never carry.
const (
	s3TestAccessKeyID     = "AKIA-ACCESS-KEY-ID-0b1e"
	s3TestSecretAccessKey = "SECRET-ACCESS-KEY-c4fe"
	s3TestSessionToken    = "SESSION-TOKEN-77aa"
	s3TestBucket          = "orders-releases"
	s3TestVersion         = "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"
)

// fakeS3 is a path-style bucket behind TLS: GET /<bucket>/<key> returns the
// object stored there and the version it holds, and counts every request,
// so a row can prove a refusal came before any.
type fakeS3 struct {
	server *httptest.Server
	caFile string

	mu       sync.Mutex
	objects  map[string]fakeS3Object
	requests []*http.Request
}

type fakeS3Object struct {
	body    []byte
	version string
	status  int
	refusal string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	bucket := &fakeS3{objects: map[string]fakeS3Object{}}
	bucket.server = httptest.NewTLSServer(http.HandlerFunc(bucket.serve))
	t.Cleanup(bucket.server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bucket.server.Certificate().Raw})
	bucket.caFile = writeGitTrustSource(t, t.TempDir(), "ca.pem", ca)
	return bucket
}

func (b *fakeS3) put(key string, object fakeS3Object) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects["/"+s3TestBucket+"/"+key] = object
}

func (b *fakeS3) serve(writer http.ResponseWriter, request *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, request.Clone(request.Context()))
	object, found := b.objects[request.URL.Path]
	b.mu.Unlock()
	switch {
	case !found:
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>"))
		return
	case object.status != 0:
		writer.WriteHeader(object.status)
		_, _ = writer.Write([]byte(object.refusal))
		return
	}
	if version := request.URL.Query().Get("versionId"); version != "" && version != object.version {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("<Error><Code>NoSuchVersion</Code><Message>The specified version does not exist.</Message></Error>"))
		return
	}
	if object.version != "" {
		writer.Header().Set("x-amz-version-id", object.version)
	}
	_, _ = writer.Write(object.body)
}

func (b *fakeS3) received() []*http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*http.Request(nil), b.requests...)
}

func (b *fakeS3) endpoint() string  { return b.server.URL }
func (b *fakeS3) authority() string { return strings.TrimPrefix(b.server.URL, "https://") }

func s3Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func s3TarGzip(t *testing.T, root string, files []sourcecontent.File) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: root + "/" + file.Path, Mode: 0o644, Size: int64(len(file.Data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(file.Data); err != nil {
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

func contentDigest(t *testing.T, files []sourcecontent.File) string {
	t.Helper()
	digest, err := sourcecontent.Digest(files)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

var s3TestMigrations = []sourcecontent.File{
	{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")},
	{Path: "nested/0002_index.up.sql", Data: []byte("CREATE INDEX orders_id ON orders (id);\r\n")},
}

// s3Environment is what the manager sets for an S3 source in endpoint's
// bucket: the coordinates, then extra.
func s3Environment(endpoint, key, digest, format, selects, path string, extra ...string) []string {
	return append([]string{
		EnvS3Endpoint + "=" + endpoint,
		EnvS3Region + "=us-east-1",
		EnvS3Bucket + "=" + s3TestBucket,
		EnvS3Key + "=" + key,
		EnvS3Digest + "=" + digest,
		EnvS3Format + "=" + format,
		EnvS3Path + "=" + path,
		EnvS3Selects + "=" + selects,
	}, extra...)
}

func s3ResolveEnvironment(endpoint, key, digest string, extra ...string) []string {
	return withRunnerProtocol(append([]string{envOperationID + "=resolve-s3"},
		s3Environment(endpoint, key, digest, "TarGzip", "directory", "app/db/migrations", extra...)...))
}

// s3Signed is a key pair granted for authority, under the CA in caFile.
func s3Signed(authority, caFile string) []string {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		panic(err)
	}
	return []string{
		EnvS3AccessKeyID + "=" + s3TestAccessKeyID, EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey,
		EnvS3HostGrant + "=" + authority, EnvS3BucketGrant + "=" + s3TestBucket,
		EnvS3CASourceFile + "=" + caFile, EnvS3CASHA256Grant + "=" + gitTestDigest(ca),
	}
}

func TestPrepareS3SourceAccessHoldsTheKeyPairToItsGrants(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ca := []byte("-----BEGIN CERTIFICATE-----\ns3\n-----END CERTIFICATE-----\n")
	caFile := writeGitTrustSource(t, directory, "ca.pem", ca)
	digest := "sha256:" + strings.Repeat("d", 64)
	archive := S3Source{
		Endpoint: "https://objects.example.com", Region: "us-east-1", Bucket: s3TestBucket, Key: "app/1.4.0/app.tar.gz",
		Digest: digest, Format: "TarGzip", Path: "app/db/migrations", selects: s3SelectsDirectory,
	}
	file := S3Source{
		Endpoint: "https://objects.example.com:9000", Region: "us-east-1", Bucket: s3TestBucket, Key: "app/1.4.0/schema.hcl",
		Digest: digest, Format: "File", Path: "schema.hcl", Schema: true, selects: s3SelectsFile,
	}
	with := func(source S3Source, change func(*S3Source)) S3Source {
		change(&source)
		return source
	}
	pair := []string{EnvS3AccessKeyID + "=" + s3TestAccessKeyID, EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey}
	granted := append([]string{EnvS3HostGrant + "=objects.example.com"}, pair...)
	plus := func(base []string, extra ...string) []string { return append(append([]string(nil), base...), extra...) }
	for _, row := range []struct {
		name        string
		source      S3Source
		environment []string
		refusal     string
	}{
		{name: "unsigned", source: archive},
		{name: "unsigned under a CA", source: archive, environment: []string{EnvS3CASourceFile + "=" + caFile}},
		{name: "a granted key pair", source: archive, environment: granted},
		{name: "a granted key pair and session token", source: archive, environment: plus(granted, EnvS3SessionToken+"="+s3TestSessionToken)},
		{name: "a grant in capitals", source: archive, environment: plus(pair, EnvS3HostGrant+"=OBJECTS.EXAMPLE.COM")},
		{name: "a grant for the endpoint's port", source: file, environment: plus(pair, EnvS3HostGrant+"=objects.example.com:9000")},
		{name: "a bucket grant for the source's bucket", source: archive, environment: plus(granted, EnvS3BucketGrant+"="+s3TestBucket)},
		{name: "a granted key pair and CA", source: archive, environment: plus(granted,
			EnvS3CASourceFile+"="+caFile, EnvS3CASHA256Grant+"="+gitTestDigest(ca))},

		{name: "an access key ID without a secret", source: archive,
			environment: []string{EnvS3HostGrant + "=objects.example.com", EnvS3AccessKeyID + "=" + s3TestAccessKeyID},
			refusal:     "an access key ID without a secret access key"},
		{name: "a secret without an access key ID", source: archive,
			environment: []string{EnvS3HostGrant + "=objects.example.com", EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey},
			refusal:     "a secret access key without an access key ID"},
		{name: "a session token without a key pair", source: archive,
			environment: []string{EnvS3HostGrant + "=objects.example.com", EnvS3SessionToken + "=" + s3TestSessionToken},
			refusal:     "a session token without a key pair"},
		{name: "an access key ID with a line break", source: archive,
			environment: plus(granted, EnvS3AccessKeyID+"="+s3TestAccessKeyID+"\n"), refusal: "access key ID"},
		{name: "a key pair without a grant", source: archive, environment: pair, refusal: "not granted for the source's endpoint"},
		{name: "a key pair granted for another endpoint", source: archive, environment: plus(pair, EnvS3HostGrant+"=attacker.example"),
			refusal: "not granted for the source's endpoint"},
		{name: "a grant without the endpoint's port", source: file, environment: plus(pair, EnvS3HostGrant+"=objects.example.com"),
			refusal: "not granted for the source's endpoint"},
		{name: "a grant for the endpoint's host on another port", source: archive, environment: plus(pair, EnvS3HostGrant+"=objects.example.com:9000"),
			refusal: "not granted for the source's endpoint"},
		// kubectl create secret --from-file keeps the file's last newline.
		{name: "a host grant with a trailing newline", source: archive, environment: plus(pair, EnvS3HostGrant+"=objects.example.com\n"),
			refusal: `host grant is refused: the host "objects.example.com\n" is not printable ASCII`},
		{name: "a bucket grant for another bucket", source: archive, environment: plus(granted, EnvS3BucketGrant+"=orders-private"),
			refusal: `granted for the bucket "orders-private", not the source's`},
		{name: "an empty bucket grant", source: archive, environment: plus(granted, EnvS3BucketGrant+"="),
			refusal: `granted for the bucket "", not the source's`},
		{name: "a bucket grant with a trailing newline", source: archive, environment: plus(granted, EnvS3BucketGrant+"="+s3TestBucket+"\n"),
			refusal: `granted for the bucket "orders-releases\n", not the source's`},
		{name: "a host grant without a key pair", source: archive, environment: []string{EnvS3HostGrant + "=objects.example.com"},
			refusal: "without a key pair"},
		{name: "a bucket grant without a key pair", source: archive, environment: []string{EnvS3BucketGrant + "=" + s3TestBucket},
			refusal: "without a key pair"},
		{name: "a CA grant without a key pair", source: archive, environment: []string{
			EnvS3CASourceFile + "=" + caFile, EnvS3CASHA256Grant + "=" + gitTestDigest(ca),
		}, refusal: "without a key pair"},
		{name: "a key pair with an ungranted CA", source: archive, environment: plus(granted, EnvS3CASourceFile+"="+caFile),
			refusal: "not the one the credential's owner granted"},
		{name: "a key pair with another CA", source: archive, environment: plus(granted,
			EnvS3CASourceFile+"="+caFile, EnvS3CASHA256Grant+"="+gitTestDigest([]byte("other"))),
			refusal: "not the one the credential's owner granted"},
		{name: "a CA grant without the CA", source: archive, environment: plus(granted, EnvS3CASHA256Grant+"="+gitTestDigest(ca)),
			refusal: "without the certificate authority: the credential's Secret pins caSHA256, so the source has to select that CA in transport.caFrom"},
		{name: "an unreadable CA", source: archive, environment: []string{EnvS3CASourceFile + "=" + filepath.Join(directory, "missing")},
			refusal: "read the S3 certificate authority"},

		{name: "http://", source: with(archive, func(s *S3Source) { s.Endpoint = "http://objects.example.com" }), refusal: "the S3 source"},
		{name: "a path in the endpoint", source: with(archive, func(s *S3Source) { s.Endpoint = "https://objects.example.com/" + s3TestBucket }),
			refusal: "the S3 source"},
		{name: "a user in the endpoint", source: with(archive, func(s *S3Source) { s.Endpoint = "https://bot@objects.example.com" }),
			refusal: "the S3 source"},
		{name: "an endpoint host that is not ASCII", source: with(archive, func(s *S3Source) { s.Endpoint = "https://objİcts.example.com" }),
			refusal: "the S3 source"},
		{name: "a region in capitals", source: with(archive, func(s *S3Source) { s.Region = "US-EAST-1" }), refusal: "the region"},
		{name: "a bucket in capitals", source: with(archive, func(s *S3Source) { s.Bucket = "Orders" }), refusal: "is not a bucket name"},
		{name: "a key that climbs", source: with(archive, func(s *S3Source) { s.Key = "app/../app.tar.gz" }), refusal: `".." element`},
		{name: "a key from the root", source: with(archive, func(s *S3Source) { s.Key = "/app.tar.gz" }), refusal: `".." element`},
		{name: "a version with a space", source: with(archive, func(s *S3Source) { s.VersionID = "a version" }), refusal: "printable ASCII"},
		{name: "an uppercase digest", source: with(archive, func(s *S3Source) { s.Digest = strings.ToUpper(digest) }),
			refusal: "not a lowercase SHA-256 digest"},
		{name: "a format the API does not name", source: with(archive, func(s *S3Source) { s.Format = "tar.gz" }),
			refusal: "not File, TarGzip or Zip"},
		{name: "File with a nested path", source: with(file, func(s *S3Source) { s.Path = "db/schema.hcl" }),
			refusal: "the one name the object is given"},
		{name: "a migration directory as a File", source: with(archive, func(s *S3Source) { s.Format, s.Path = "File", "migrations" }),
			refusal: "comes in an archive"},
		{name: "a schema selecting the root", source: with(archive, func(s *S3Source) { s.Path, s.Schema, s.selects = ".", true, s3SelectsFile }),
			refusal: "selects one file"},
		{name: "an unclean path", source: with(archive, func(s *S3Source) { s.Path = "app/../db" }), refusal: "not a clean relative path"},
		{name: "an unknown selection", source: with(archive, func(s *S3Source) { s.selects = "tree" }), refusal: "neither a file nor a directory"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			access, cleanup, err := PrepareS3SourceAccess(row.source, row.environment, t.TempDir())
			if row.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), row.refusal) {
					t.Fatalf("PrepareS3SourceAccess() = %v, want a refusal naming %q", err, row.refusal)
				}
				for _, credential := range []string{s3TestSecretAccessKey, s3TestSessionToken} {
					if strings.Contains(err.Error(), credential) {
						t.Fatalf("the refusal %q carries a credential", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if access.caFile == caFile {
				t.Fatalf("the read would use the mutable projection %s, not a private copy", caFile)
			}
			if access.caFile != "" {
				if copied, err := os.ReadFile(access.caFile); err != nil || !bytes.Equal(copied, ca) {
					t.Fatalf("the CA copy holds %q, %v", copied, err)
				}
			}
		})
	}
}

// A Resolve reads the object itself and starts no child: Ptah has no S3
// pull, and the key pair reaches no process the runner starts.
func TestRunnerResolvesAnS3SourceItself(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	body := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	bucket.put("app/1.4.0/app.tar.gz", fakeS3Object{body: body, version: s3TestVersion})
	for _, row := range []struct {
		name, version, want string
	}{
		{name: "the current version", want: s3TestVersion},
		{name: "the named version", version: s3TestVersion, want: s3TestVersion},
	} {
		t.Run(row.name, func(t *testing.T) {
			environment := s3ResolveEnvironment(bucket.endpoint(), "app/1.4.0/app.tar.gz", s3Digest(body),
				append(s3Signed(bucket.authority(), bucket.caFile), EnvS3SessionToken+"="+s3TestSessionToken)...)
			if row.version != "" {
				environment = append(environment, EnvS3VersionID+"="+row.version)
			}
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{Operation: OperationResolve, Environment: environment, Executor: executor, TempDir: t.TempDir()})
			size := int64(len(s3TestMigrations[0].Data) + len(s3TestMigrations[1].Data))
			if result.Error != nil || result.ChildExitCode != 0 || result.ResolvedDownloadDigest != s3Digest(body) ||
				result.ResolvedContentDigest != contentDigest(t, s3TestMigrations) || result.ResolvedVersionID != row.want ||
				result.ResolvedContentBytes != size || result.ResolvedCommit != "" || result.ResolvedDigest != "" {
				t.Fatalf("Run() = %#v", result)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("the Resolve started %d children", len(executor.calls))
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatal(err)
			}
			if parsed, err := ParseResultFor(frame, OperationResolve, "resolve-s3"); err != nil || !reflect.DeepEqual(parsed, result) {
				t.Fatalf("ParseResultFor() = %#v, %v", parsed, err)
			}
			for _, credential := range []string{s3TestSecretAccessKey, s3TestSessionToken} {
				if strings.Contains(string(frame), credential) {
					t.Fatalf("the frame carries %q", credential)
				}
			}
		})
	}
	// Every request was signed with the key pair the Secret holds, for the
	// region the spec names, and carried the session token.
	requests := bucket.received()
	if len(requests) != 2 {
		t.Fatalf("the bucket received %d requests, want one per Resolve", len(requests))
	}
	for _, request := range requests {
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential="+s3TestAccessKeyID+"/") ||
			!strings.Contains(authorization, "/us-east-1/s3/aws4_request") ||
			request.Header.Get("X-Amz-Security-Token") != s3TestSessionToken ||
			request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("the request carried Authorization %q, token %q, Accept-Encoding %q", authorization,
				request.Header.Get("X-Amz-Security-Token"), request.Header.Get("Accept-Encoding"))
		}
	}
}

// A bucket that keeps no versions serves none, or "null", and the
// resolution records none: every later read takes whatever the key holds,
// held to the same digests.
func TestRunnerResolvesAnUnversionedS3Object(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	body := []byte("table \"orders\" {}\n")
	bucket.put("schema.hcl", fakeS3Object{body: body})
	bucket.put("null/schema.hcl", fakeS3Object{body: body, version: "null"})
	for _, key := range []string{"schema.hcl", "null/schema.hcl"} {
		t.Run(key, func(t *testing.T) {
			environment := withRunnerProtocol(append([]string{envOperationID + "=resolve-s3"},
				s3Environment(bucket.endpoint(), key, s3Digest(body), "File", "file", "schema.hcl", EnvS3CASourceFile+"="+bucket.caFile)...))
			result := Run(t.Context(), Config{Operation: OperationResolve, Environment: environment, Executor: &scriptedExecutor{t: t}, TempDir: t.TempDir()})
			if result.Error != nil || result.ResolvedVersionID != "" ||
				result.ResolvedContentDigest != contentDigest(t, []sourcecontent.File{{Path: "schema.hcl", Data: body}}) {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
	requests := bucket.received()
	if len(requests) != 2 {
		t.Fatalf("the bucket received %d requests, want one per Resolve", len(requests))
	}
	for _, request := range requests {
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-Amz-Date") != "" {
			t.Fatalf("an unsigned source sent Authorization %q", request.Header.Get("Authorization"))
		}
	}
}

func TestRunnerRefusesAnUngrantedS3KeyPairBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	body := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	bucket.put("app/1.4.0/app.tar.gz", fakeS3Object{body: body})
	pair := []string{EnvS3AccessKeyID + "=" + s3TestAccessKeyID, EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey, EnvS3CASourceFile + "=" + bucket.caFile}
	for _, row := range []struct {
		name        string
		environment []string
	}{
		{name: "another endpoint", environment: append([]string{EnvS3HostGrant + "=attacker.example"}, pair...)},
		{name: "another bucket", environment: append([]string{EnvS3HostGrant + "=" + bucket.authority(), EnvS3BucketGrant + "=orders-private"}, pair...)},
		{name: "half a key pair", environment: []string{EnvS3HostGrant + "=" + bucket.authority(), EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey}},
	} {
		t.Run(row.name, func(t *testing.T) {
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: s3ResolveEnvironment(bucket.endpoint(), "app/1.4.0/app.tar.gz", s3Digest(body), row.environment...),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != CodeInvalidS3Access || result.ChildExitCode != -1 || len(executor.calls) != 0 {
				t.Fatalf("Run() = %#v after %d calls", result, len(executor.calls))
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResultFor(frame, OperationResolve, "resolve-s3"); err != nil {
				t.Fatalf("the refusal frame does not parse: %v", err)
			}
			if strings.Contains(string(frame), s3TestSecretAccessKey) {
				t.Fatal("the frame carries the secret access key")
			}
		})
	}
	if requests := bucket.received(); len(requests) != 0 {
		t.Fatalf("the bucket received %d requests before the refusal", len(requests))
	}
	// The same bucket serves the object to a key pair that is granted, so
	// the count above measured the refusal and not a bucket nobody reached.
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: s3ResolveEnvironment(bucket.endpoint(), "app/1.4.0/app.tar.gz", s3Digest(body), s3Signed(bucket.authority(), bucket.caFile)...),
		Executor:    &scriptedExecutor{t: t},
		TempDir:     t.TempDir(),
	})
	if result.Error != nil || len(bucket.received()) != 1 {
		t.Fatalf("a granted Resolve = %+v after %d requests", result.Error, len(bucket.received()))
	}
}

func TestRunnerRefusesAnS3SourceOutsideAResolveOrBesideAnother(t *testing.T) {
	t.Parallel()
	environment := func(extra ...string) []string {
		return s3ResolveEnvironment("https://objects.example.com", "app.tar.gz", "sha256:"+strings.Repeat("d", 64), extra...)
	}
	for _, row := range []struct {
		name        string
		operation   Operation
		environment []string
	}{
		{name: "beside an OCI reference", operation: OperationResolve,
			environment: environment(envRequestedReference + "=oci://registry.example/schema:1")},
		{name: "beside a Git source", operation: OperationResolve,
			environment: environment(EnvGitURL+"="+gitTestURL, EnvGitBranch+"=main", EnvGitPath+"=db", EnvGitSelects+"=directory")},
		{name: "beside an HTTPS source", operation: OperationResolve,
			environment: environment(EnvHTTPSURL+"="+httpsTestURL, EnvHTTPSDigest+"="+httpsTestDownload)},
		{name: "in a verify", operation: OperationVerify, environment: environment()},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{Operation: row.operation, Environment: row.environment, Executor: executor, TempDir: t.TempDir()})
			if result.Error == nil || result.Error.Code != "invalid_configuration" || len(executor.calls) != 0 {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
}

// The endpoint's refusal reaches the status as its code and message, and a
// secret the server echoes is redacted on the way; a body that is not the
// pinned bytes is refused before any entry is read.
func TestRunnerNamesWhyAnS3ReadFailed(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	body := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	bucket.put("denied.tar.gz", fakeS3Object{status: http.StatusForbidden,
		refusal: "<Error><Code>AccessDenied</Code><Message>no grant for " + s3TestSecretAccessKey + "</Message></Error>"})
	bucket.put("overwritten.tar.gz", fakeS3Object{body: body})
	for _, row := range []struct {
		name, key, digest, says string
	}{
		{name: "a refusal", key: "denied.tar.gz", digest: s3Digest(body), says: "AccessDenied"},
		{name: "a missing key", key: "missing.tar.gz", digest: s3Digest(body), says: "NoSuchKey"},
		{name: "other bytes", key: "overwritten.tar.gz", digest: "sha256:" + strings.Repeat("e", 64), says: s3Digest(body)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: s3ResolveEnvironment(bucket.endpoint(), row.key, row.digest, s3Signed(bucket.authority(), bucket.caFile)...),
				Executor:    &scriptedExecutor{t: t},
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != CodeS3SourceRefused || !strings.Contains(result.Error.Message, row.says) {
				t.Fatalf("Run() error = %+v, want %s naming %q", result.Error, CodeS3SourceRefused, row.says)
			}
			if strings.Contains(result.Error.Message, s3TestSecretAccessKey) || len(result.Error.Message) > maxPullRefusalBytes {
				t.Fatalf("the refusal %q carries the secret or runs past %d bytes", result.Error.Message, maxPullRefusalBytes)
			}
		})
	}
}

func s3FetchEnvironment(endpoint, key, digest, format, selects, path, content string, extra ...string) []string {
	return s3Environment(endpoint, key, digest, format, selects, path,
		append([]string{EnvExpectedContentDigest + "=" + content}, extra...)...)
}

func TestFetchS3SourceDeliversTheBoundContent(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	archive := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	schema := []byte("table \"orders\" {}\n")
	bucket.put("app/1.4.0/app.tar.gz", fakeS3Object{body: archive, version: s3TestVersion})
	bucket.put("app/1.4.0/schema.hcl", fakeS3Object{body: schema})
	for _, row := range []struct {
		name, key, format, selects, path string
		body                             []byte
		files                            []sourcecontent.File
		extra                            []string
	}{
		{name: "a migration directory in one version of a tar.gz", key: "app/1.4.0/app.tar.gz", format: "TarGzip", selects: "directory",
			path: "app/db/migrations", body: archive, files: s3TestMigrations, extra: []string{EnvS3VersionID + "=" + s3TestVersion}},
		{name: "a schema file", key: "app/1.4.0/schema.hcl", format: "File", selects: "file", path: "schema.hcl", body: schema,
			files: []sourcecontent.File{{Path: "schema.hcl", Data: schema}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			digest := contentDigest(t, row.files)
			destination := filepath.Join(t.TempDir(), "source")
			environment := s3FetchEnvironment(bucket.endpoint(), row.key, s3Digest(row.body), row.format, row.selects, row.path, digest,
				append(s3Signed(bucket.authority(), bucket.caFile), row.extra...)...)
			if err := FetchS3Source(t.Context(), environment, destination, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			delivered, err := sourcecontent.ReadTree(destination, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if got := contentDigest(t, delivered); got != digest {
				t.Fatalf("the destination gives %s, want %s", got, digest)
			}
		})
	}
}

func TestFetchS3SourceRefusesOtherContent(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	archive := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	bucket.put("app.tar.gz", fakeS3Object{body: archive, version: s3TestVersion})
	digest := contentDigest(t, s3TestMigrations)
	other := contentDigest(t, s3TestMigrations[:1])
	signed := s3Signed(bucket.authority(), bucket.caFile)
	fetch := func(content string, extra ...string) []string {
		return s3FetchEnvironment(bucket.endpoint(), "app.tar.gz", s3Digest(archive), "TarGzip", "directory", "app/db/migrations", content,
			append(append([]string(nil), signed...), extra...)...)
	}
	for _, row := range []struct {
		name        string
		environment []string
		destination string
		refusal     string
		access      bool
	}{
		{name: "another content digest", environment: fetch(other), refusal: "content digest"},
		{name: "a version the bucket does not hold", environment: fetch(digest, EnvS3VersionID+"=another-version"), refusal: "NoSuchVersion"},
		{name: "another object digest", refusal: s3Digest(archive),
			environment: s3FetchEnvironment(bucket.endpoint(), "app.tar.gz", "sha256:"+strings.Repeat("e", 64), "TarGzip", "directory",
				"app/db/migrations", digest, signed...)},
		{name: "no bound content digest", environment: fetch(""), refusal: "needs the object digest and the resolved content digest"},
		{name: "a relative destination", environment: fetch(digest), destination: "source", refusal: "not an absolute path"},
		{name: "a Git source as well", environment: fetch(digest, EnvGitURL+"="+gitTestURL, EnvGitBranch+"=main", EnvGitPath+"=db"),
			refusal: "names a Git source as well"},
		{name: "an HTTPS source as well", environment: fetch(digest, EnvHTTPSURL+"="+httpsTestURL), refusal: "names an HTTPS source as well"},
		{name: "a key pair granted for another bucket", environment: fetch(digest, EnvS3BucketGrant+"=orders-private"),
			refusal: "granted for the bucket", access: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			destination := row.destination
			if destination == "" {
				destination = filepath.Join(t.TempDir(), "source")
			}
			err := FetchS3Source(t.Context(), row.environment, destination, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), row.refusal) || !strings.HasPrefix(err.Error(), CodeS3SourceRefused) {
				t.Fatalf("FetchS3Source() = %v, want a refusal naming %q", err, row.refusal)
			}
			var refused *AccessRefusalError
			if isAccess := errors.As(err, &refused); isAccess != row.access ||
				(row.access && refused.Refusal.Code != CodeInvalidS3Access) {
				t.Fatalf("FetchS3Source() = %v, an access refusal %v, want %v", err, isAccess, row.access)
			}
			if _, statErr := os.Stat(destination); statErr == nil {
				t.Fatal("a refused fetch left files for the operation to read")
			}
			if strings.Contains(err.Error(), s3TestSecretAccessKey) {
				t.Fatal("the refusal carries the secret access key")
			}
		})
	}
}

// An endpoint's error document is the endpoint's to write, and one that
// echoes the key pair and the session token back reaches the status only
// redacted: the fetch's refusal names the code and the sentence with each
// value replaced, in any case.
func TestAnS3FetchRefusalCarriesNoKey(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	echoed := "signed by " + s3TestAccessKeyID + " with " + strings.ToLower(s3TestSecretAccessKey) + " under " + s3TestSessionToken
	bucket.put("app.tar.gz", fakeS3Object{status: http.StatusForbidden,
		refusal: "<Error><Code>InvalidAccessKeyId</Code><Message>" + echoed + "</Message></Error>"})
	environment := s3FetchEnvironment(bucket.endpoint(), "app.tar.gz", "sha256:"+strings.Repeat("e", 64), "TarGzip", "directory",
		"app/db/migrations", contentDigest(t, s3TestMigrations),
		append(s3Signed(bucket.authority(), bucket.caFile), EnvS3SessionToken+"="+s3TestSessionToken)...)
	err := FetchS3Source(t.Context(), environment, filepath.Join(t.TempDir(), "source"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden, InvalidAccessKeyId: signed by") {
		t.Fatalf("FetchS3Source() = %v, want the endpoint's refusal", err)
	}
	for _, credential := range []string{s3TestAccessKeyID, s3TestSecretAccessKey, s3TestSessionToken} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(credential)) {
			t.Fatalf("the refusal %q carries %q", err, credential)
		}
	}
}

// A refusal to a request for the version a Resolve recorded ends with the
// permission that version needs, whole, even when the object's name fills
// the bound the refusal is held to.
func TestAnS3FetchRefusalKeepsItsHint(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	long := strings.Repeat("releases/", 100) + "app.tar.gz"
	for _, key := range []string{"app.tar.gz", long} {
		bucket.put(key, fakeS3Object{status: http.StatusForbidden,
			refusal: "<Error><Code>AccessDenied</Code><Message>Access Denied.</Message></Error>"})
	}
	for _, row := range []struct {
		name, key, version string
		hint               bool
	}{
		{name: "a version", key: "app.tar.gz", version: s3TestVersion, hint: true},
		{name: "a version of a long key", key: long, version: s3TestVersion, hint: true},
		{name: "the current object", key: "app.tar.gz"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			extra := s3Signed(bucket.authority(), bucket.caFile)
			if row.version != "" {
				extra = append(extra, EnvS3VersionID+"="+row.version)
			}
			environment := s3FetchEnvironment(bucket.endpoint(), row.key, "sha256:"+strings.Repeat("e", 64), "TarGzip", "directory",
				"app/db/migrations", contentDigest(t, s3TestMigrations), extra...)
			err := FetchS3Source(t.Context(), environment, filepath.Join(t.TempDir(), "source"), t.TempDir())
			// The long key's name fills the bound before the endpoint's code,
			// which is what the hint has to survive.
			if err == nil || (row.key != long && !strings.Contains(err.Error(), "AccessDenied")) {
				t.Fatalf("FetchS3Source() = %v, want AccessDenied", err)
			}
			refusal := strings.TrimPrefix(err.Error(), CodeS3SourceRefused+": ")
			if len(refusal) > maxPullRefusalBytes {
				t.Fatalf("the refusal holds %d bytes, more than %d", len(refusal), maxPullRefusalBytes)
			}
			_, hint, hinted := strings.Cut(refusal, "; a request for a version is authorized as s3:GetObjectVersion")
			hinted = hinted && strings.HasSuffix(hint, "in the bucket policy for an unsigned read")
			if hinted != row.hint {
				t.Fatalf("the refusal %q ends with the version permission hint: %v, want %v", refusal, hinted, row.hint)
			}
		})
	}
}

// The key pair reaches the fetch through files the kubelet projected, and
// the request is signed with what they hold.
func TestFetchS3SourceReadsTheKeyPairFromItsFiles(t *testing.T) {
	t.Parallel()
	bucket := newFakeS3(t)
	archive := s3TarGzip(t, "app/db/migrations", s3TestMigrations)
	bucket.put("app.tar.gz", fakeS3Object{body: archive})
	directory := t.TempDir()
	files := map[string]string{
		EnvS3AccessKeyIDFile: "AKIA-FROM-FILE", EnvS3SecretAccessKeyFile: "secret-from-file", EnvS3SessionTokenFile: "token-from-file",
	}
	var extra []string
	for variable, value := range files {
		path := filepath.Join(directory, strings.ToLower(variable))
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		extra = append(extra, variable+"="+path)
	}
	ca, err := os.ReadFile(bucket.caFile)
	if err != nil {
		t.Fatal(err)
	}
	extra = append(extra, EnvS3HostGrant+"="+bucket.authority(), EnvS3CASourceFile+"="+bucket.caFile, EnvS3CASHA256Grant+"="+gitTestDigest(ca))
	environment := s3FetchEnvironment(bucket.endpoint(), "app.tar.gz", s3Digest(archive), "TarGzip", "directory", "app/db/migrations",
		contentDigest(t, s3TestMigrations), extra...)
	if err := FetchS3Source(t.Context(), environment, filepath.Join(t.TempDir(), "source"), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	request := bucket.received()[0]
	if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIA-FROM-FILE/") ||
		request.Header.Get("X-Amz-Security-Token") != "token-from-file" {
		t.Fatalf("the request carried Authorization %q and token %q", request.Header.Get("Authorization"), request.Header.Get("X-Amz-Security-Token"))
	}
}

// No child the runner starts receives an S3 coordinate, grant or key.
func TestChildEnvironmentStripsTheS3Source(t *testing.T) {
	t.Parallel()
	environment := append(s3Signed("objects.example.com", writeGitTrustSource(t, t.TempDir(), "ca.pem", []byte("ca"))),
		s3Environment("https://objects.example.com", "app.tar.gz", "sha256:"+strings.Repeat("d", 64), "TarGzip", "directory", "db",
			EnvS3VersionID+"="+s3TestVersion, EnvS3SessionToken+"="+s3TestSessionToken)...)
	for _, variable := range childEnvironment(environment) {
		if strings.HasPrefix(variable, "PTAH_OPERATOR_S3_") || strings.HasPrefix(variable, "PTAH_S3_") {
			t.Fatalf("a child receives %s", variable)
		}
	}
}

func TestTheS3KeyPairIsRedacted(t *testing.T) {
	t.Parallel()
	redactor := NewRedactor([]string{
		EnvS3AccessKeyID + "=" + s3TestAccessKeyID, EnvS3SecretAccessKey + "=" + s3TestSecretAccessKey, EnvS3SessionToken + "=" + s3TestSessionToken,
	})
	text := redactor.Redact("signed " + s3TestAccessKeyID + " with " + s3TestSecretAccessKey + " and " + s3TestSessionToken)
	for _, credential := range []string{s3TestAccessKeyID, s3TestSecretAccessKey, s3TestSessionToken} {
		if strings.Contains(text, credential) {
			t.Fatalf("%q carries %q", text, credential)
		}
	}
}
