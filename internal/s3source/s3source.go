// Package s3source reads a schema file or a migration directory out of one
// object of an S3-compatible bucket, pinned by the SHA-256 of its bytes.
//
// One GetObject request, path-style (https://<endpoint>/<bucket>/<key>),
// downloads the object to a private temporary file, hashing it as it writes,
// and the digest is compared before any byte is read as an archive.
// [sourcearchive] then reads the selection, and the result carries the
// content digest [sourcecontent.Digest] computes, as an HTTPS pull's does.
//
// The request is signed with Signature Version 4 by the key pair the caller
// passes, or sent unsigned. Nothing here reads a credential from anywhere
// else: no AWS_* variable, no ~/.aws file, no web identity token and no
// instance metadata. Whoever holds the identity of a Pod's ServiceAccount
// holds it in every container of the Pod, the one that holds the database URL
// included, so a fetch that picked one up would hand a container a credential
// nobody gave it.
//
// The pinned Ptah has no S3 pull, so the runner reads the object itself. The
// limits and the archive rules are the HTTPS pull's: the same download bound,
// and Ptah's own archive reader, copied into [sourcearchive].
package s3source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/sourcearchive"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcehost"
)

// MaxDownloadBytes bounds what one fetch downloads, as it bounds an HTTPS
// pull.
const MaxDownloadBytes = 64 << 20

// Bounds on the coordinates, as the API holds them.
const (
	maxKeyBytes       = 1024
	maxVersionIDBytes = 1024
	maxRegionBytes    = 64
	// maxErrorBodyBytes bounds the error document a refusal is read from.
	maxErrorBodyBytes = 16 << 10
	// maxErrorFieldBytes bounds the code and the message a refusal names.
	maxErrorFieldBytes = 256
)

// nullVersion is what S3 says for the version of an object in a bucket that
// keeps none, or one written before versioning was turned on.
const nullVersion = "null"

// emptyPayloadDigest is the SHA-256 of an empty request body, which a signed
// GET declares in x-amz-content-sha256.
const emptyPayloadDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// The refusals, matched with errors.Is. Each message names the object and,
// for a digest, both digests. An archive's own refusals are sourcearchive's.
var (
	ErrObject         = errors.New("invalid S3 object")
	ErrCredential     = errors.New("invalid S3 credential")
	ErrExpectation    = errors.New("invalid expected digest")
	ErrStatus         = errors.New("the S3 endpoint refused the object")
	ErrRedirect       = errors.New("the S3 endpoint redirected the request")
	ErrVersion        = errors.New("the S3 endpoint served another version")
	ErrTooLarge       = errors.New("object too large")
	ErrTruncated      = errors.New("object ended early")
	ErrDownloadDigest = errors.New("object digest differs")
	ErrDigest         = errors.New("content digest differs")
)

var (
	sha256Digest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)
	// versionPattern is printable ASCII without a space, as the API holds a
	// version ID; a server's x-amz-version-id is held to it too.
	versionPattern = regexp.MustCompile(`^[!-~]+$`)
)

// Object names one object: the endpoint it is read from, the region a request
// is signed for, the bucket, the key and, optionally, one version.
type Object struct {
	// Endpoint is https:// and an authority, with nothing after it.
	Endpoint string
	Region   string
	Bucket   string
	Key      string
	// VersionID selects one version; empty, the current one.
	VersionID string
}

// String names the object in a message: s3://bucket/key, the version and
// the endpoint's authority. It holds no credential, since an Object holds
// none.
func (o Object) String() string {
	shown := "s3://" + o.Bucket + "/" + o.Key
	if o.VersionID != "" {
		shown += " version " + o.VersionID
	}
	if authority, err := o.Authority(); err == nil {
		shown += " at " + authority
	}
	return shown
}

// Authority is the endpoint's host[:port], as a credential's host grant and a
// source policy name it.
func (o Object) Authority() (string, error) {
	parsed, err := url.Parse(o.Endpoint)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: the endpoint is not a URL", ErrObject)
	case parsed.Scheme != "https" || parsed.Opaque != "":
		return "", fmt.Errorf("%w: the endpoint %q is not https://", ErrObject, o.Endpoint)
	case parsed.User != nil:
		return "", fmt.Errorf("%w: the endpoint carries user information", ErrObject)
	case parsed.Host == "" || parsed.Hostname() == "":
		return "", fmt.Errorf("%w: the endpoint names no host", ErrObject)
	case parsed.Path != "" || parsed.RawPath != "":
		return "", fmt.Errorf("%w: the endpoint %q has a path; give https:// and the authority alone", ErrObject, o.Endpoint)
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return "", fmt.Errorf("%w: the endpoint has a query or a fragment", ErrObject)
	}
	if err := sourcehost.CheckASCII(parsed.Host); err != nil {
		return "", fmt.Errorf("%w: the endpoint: %w", ErrObject, err)
	}
	return parsed.Host, nil
}

// Validate refuses coordinates the API would not admit, so nothing is sent
// for them.
func (o Object) Validate() error {
	if _, err := o.Authority(); err != nil {
		return err
	}
	switch {
	case len(o.Region) > maxRegionBytes || !regionPattern.MatchString(o.Region):
		return fmt.Errorf("%w: the region %q is not 1 to %d lowercase letters, digits and hyphens", ErrObject, o.Region, maxRegionBytes)
	case len(o.Bucket) < 3 || len(o.Bucket) > 63 || !bucketPattern.MatchString(o.Bucket):
		return fmt.Errorf("%w: the bucket %q is not a bucket name", ErrObject, o.Bucket)
	case o.VersionID != "" && (len(o.VersionID) > maxVersionIDBytes || !versionPattern.MatchString(o.VersionID)):
		return fmt.Errorf("%w: the version ID is not 1 to %d bytes of printable ASCII", ErrObject, maxVersionIDBytes)
	}
	return ValidateKey(o.Key)
}

// ValidateKey refuses a key the API would not admit: more than 1024 bytes,
// invalid UTF-8, a leading "/", an empty, "." or ".." element, or a control
// character. S3 accepts each of them, but some S3-compatible servers
// normalize the path before they look the key up, and a signed request would
// then name another object than the one the spec does.
func ValidateKey(key string) error {
	switch {
	case key == "" || len(key) > maxKeyBytes:
		return fmt.Errorf("%w: the key is not 1 to %d bytes", ErrObject, maxKeyBytes)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: the key is not UTF-8", ErrObject)
	case strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return fmt.Errorf("%w: the key holds a control character", ErrObject)
	}
	for element := range strings.SplitSeq(key, "/") {
		if element == "" || element == "." || element == ".." {
			return fmt.Errorf("%w: the key %q has an empty, \".\" or \"..\" element", ErrObject, key)
		}
	}
	return nil
}

// Credentials sign a request: a key pair, and the session token a temporary
// pair carries. The zero value sends the request unsigned.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Present reports whether any part of a credential is set.
func (c Credentials) Present() bool {
	return c.AccessKeyID != "" || c.SecretAccessKey != "" || c.SessionToken != ""
}

// Validate refuses half a key pair, and a session token without one.
func (c Credentials) Validate() error {
	switch {
	case c.AccessKeyID != "" && c.SecretAccessKey == "":
		return fmt.Errorf("%w: an access key ID without its secret access key", ErrCredential)
	case c.SecretAccessKey != "" && c.AccessKeyID == "":
		return fmt.Errorf("%w: a secret access key without its access key ID", ErrCredential)
	case c.SessionToken != "" && c.AccessKeyID == "":
		return fmt.Errorf("%w: a session token without a key pair", ErrCredential)
	case strings.ContainsAny(c.AccessKeyID, "\r\n"):
		// A line break is usually the end of the file the Secret was made
		// from, and a request signed with it fails in the endpoint's words.
		return fmt.Errorf("%w: the access key ID holds a line break", ErrCredential)
	case strings.ContainsAny(c.AccessKeyID, "/, \t"):
		// The ID is written into the Authorization header's credential
		// scope, which a slash, a comma or a space would end early.
		return fmt.Errorf("%w: the access key ID holds a slash, a comma or a space", ErrCredential)
	case strings.ContainsAny(c.SecretAccessKey, "\r\n"):
		return fmt.Errorf("%w: the secret access key holds a line break", ErrCredential)
	case strings.ContainsAny(c.SessionToken, "\r\n"):
		return fmt.Errorf("%w: the session token holds a line break", ErrCredential)
	}
	return nil
}

// Options say what to fetch and how.
type Options struct {
	Object      Object
	Credentials Credentials
	// CAFile holds the only roots the endpoint's certificate may chain to.
	// Empty, the system's roots are used.
	CAFile string
	// ExpectDownloadDigest is required: sha256: and the SHA-256 of the exact
	// bytes GetObject returns.
	ExpectDownloadDigest string
	// Archive says how the selected files are carried and which they are.
	Archive sourcearchive.Options
	// ExpectDigest, when set, is the content digest the files have to have.
	ExpectDigest string
	// TempDir holds the object while it is read; empty, the system's
	// temporary directory.
	TempDir string
	// Now dates the signature; nil, time.Now.
	Now func() time.Time
	// Transport replaces the TLS transport; for tests.
	Transport http.RoundTripper
}

// Result is what a fetch read.
type Result struct {
	// DownloadDigest and DownloadSize describe the bytes GetObject returned.
	DownloadDigest string
	DownloadSize   int64
	// VersionID is the version the endpoint said it served. It is empty when
	// the endpoint named none, or named "null", the version of an object in
	// a bucket that keeps none, unless the request asked for "null" itself.
	VersionID string
	// ContentDigest is the digest of Files, as sourcecontent defines it.
	ContentDigest string
	// Files are the selected files, with paths relative to the selected
	// directory, or the file's own name for a selected file.
	Files []sourcecontent.File
}

// Fetch reads the object, holds it to the download digest, and reads the
// selected files from it.
func Fetch(ctx context.Context, o Options) (Result, error) {
	if err := o.Object.Validate(); err != nil {
		return Result{}, err
	}
	if err := o.Credentials.Validate(); err != nil {
		return Result{}, err
	}
	if !sha256Digest.MatchString(o.ExpectDownloadDigest) {
		return Result{}, fmt.Errorf("%w: the object digest %q is not sha256: and 64 lowercase hex digits", ErrExpectation, o.ExpectDownloadDigest)
	}
	if o.ExpectDigest != "" && !sha256Digest.MatchString(o.ExpectDigest) {
		return Result{}, fmt.Errorf("%w: the content digest %q is not sha256: and 64 lowercase hex digits", ErrExpectation, o.ExpectDigest)
	}
	if err := sourcearchive.Check(o.Archive); err != nil {
		return Result{}, err
	}
	client, err := newClient(o)
	if err != nil {
		return Result{}, err
	}
	body, err := fetchToFile(ctx, client, o)
	if err != nil {
		return Result{}, err
	}
	// The object goes once the selection is in memory, so the temporary
	// directory never holds it beside the output.
	files, err := func() ([]sourcecontent.File, error) {
		defer remove(body.file)
		if body.digest != o.ExpectDownloadDigest {
			return nil, fmt.Errorf("%w: %s gave %s, not %s", ErrDownloadDigest, o.Object, body.digest, o.ExpectDownloadDigest)
		}
		return sourcearchive.Read(body.file, body.size, o.Archive)
	}()
	if err != nil {
		return Result{}, err
	}
	content, err := sourcecontent.Digest(files)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s in %s: %w", sourcearchive.ErrSelection, o.Archive.Path, o.Object, err)
	}
	if o.ExpectDigest != "" && content != o.ExpectDigest {
		return Result{}, fmt.Errorf("%w: %s in %s has %s, not %s", ErrDigest, o.Archive.Path, o.Object, content, o.ExpectDigest)
	}
	return Result{
		DownloadDigest: body.digest, DownloadSize: body.size, VersionID: body.version,
		ContentDigest: content, Files: files,
	}, nil
}

func remove(file *os.File) {
	_ = file.Close()
	_ = os.Remove(file.Name())
}

// newClient is a client that verifies the endpoint with TLS 1.2 or later,
// goes through no proxy, and follows no redirect: S3 does not redirect a
// request addressed to the region the bucket is in, and the 301 or 307 it
// answers otherwise carries an error document that says so.
func newClient(o Options) (*http.Client, error) {
	transport := o.Transport
	if transport == nil {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile) // #nosec G304 -- the runner's private copy of the selected CA
			if err != nil {
				return nil, fmt.Errorf("read the CA file: %w", err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("the CA file holds no certificate")
			}
			tlsConfig.RootCAs = roots
		}
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsConfig,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// download is a body written to a temporary file, with its size, its digest
// and the version the endpoint said it served.
type download struct {
	file    *os.File
	size    int64
	digest  string
	version string
}

// fetchToFile sends GetObject and writes the body into a new temporary file.
// The caller removes the file.
func fetchToFile(ctx context.Context, client *http.Client, o Options) (download, error) {
	request, err := newRequest(ctx, o.Object)
	if err != nil {
		return download{}, err
	}
	if o.Credentials.Present() {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		sign(request, o.Object.Region, o.Credentials, now().UTC())
	}
	response, err := client.Do(request)
	if err != nil {
		// The client wraps the cause in the URL it requested, which names
		// the object again, escaped.
		if requested, ok := errors.AsType[*url.Error](err); ok {
			err = requested.Err
		}
		return download{}, fmt.Errorf("read %s: %w", o.Object, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return download{}, refusal(response, o.Object)
	}
	served := response.Header.Get("X-Amz-Version-Id")
	if served == nullVersion && o.Object.VersionID != nullVersion {
		served = ""
	}
	switch {
	case served != "" && (len(served) > maxVersionIDBytes || !versionPattern.MatchString(served)):
		return download{}, fmt.Errorf("%w: %s answered with a version ID that is not 1 to %d bytes of printable ASCII", ErrVersion, o.Object, maxVersionIDBytes)
	case o.Object.VersionID != "" && served != o.Object.VersionID:
		return download{}, fmt.Errorf("%w: %s was asked for and the endpoint said it served %s", ErrVersion, o.Object, shownVersion(served))
	case response.ContentLength > MaxDownloadBytes:
		return download{}, fmt.Errorf("%w: %s declares %d bytes, more than %d", ErrTooLarge, o.Object, response.ContentLength, MaxDownloadBytes)
	}
	file, err := os.CreateTemp(o.TempDir, "ptah-s3-*")
	if err != nil {
		return download{}, fmt.Errorf("create the object's temporary file: %w", err)
	}
	hash := sha256.New()
	// The request asked for identity, so the transport decodes nothing: an
	// object stored with Content-Encoding gzip is hashed as stored.
	written, larger, err := copyBounded(io.MultiWriter(file, hash), response.Body, MaxDownloadBytes)
	switch {
	case larger:
		err = fmt.Errorf("%w: %s gave more than %d bytes", ErrTooLarge, o.Object, MaxDownloadBytes)
	case errors.Is(err, io.ErrUnexpectedEOF), err == nil && response.ContentLength >= 0 && written != response.ContentLength:
		err = fmt.Errorf("%w: %s declared %d bytes and gave %d", ErrTruncated, o.Object, response.ContentLength, written)
	case err != nil:
		err = fmt.Errorf("read %s: %w", o.Object, err)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		remove(file)
		return download{}, err
	}
	return download{file: file, size: written, digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), version: served}, nil
}

// copyBounded copies at most limit bytes of src to dst, and reports whether
// src held more. It never writes more than limit bytes: the byte that shows
// src is larger is read and dropped. A fetch step writes the object to a
// memory-backed volume sized from MaxDownloadBytes, so an object larger than
// the bound is refused as too large rather than failing for lack of space.
func copyBounded(dst io.Writer, src io.Reader, limit int64) (int64, bool, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil || written < limit {
		return written, false, err
	}
	var next [1]byte
	read, err := io.ReadFull(src, next[:])
	switch {
	case read > 0:
		return written, true, nil
	case errors.Is(err, io.EOF):
		return written, false, nil
	default:
		return written, false, err
	}
}

func shownVersion(version string) string {
	if version == "" {
		return "no version"
	}
	return "version " + version
}

// newRequest is GetObject for the object, path-style: the bucket and the key
// in the path, each element of the key escaped once, and the version, if any,
// in the query. The path the request sends is the one a signature covers.
func newRequest(ctx context.Context, object Object) (*http.Request, error) {
	authority, err := object.Authority()
	if err != nil {
		return nil, err
	}
	target := &url.URL{
		Scheme:  "https",
		Host:    authority,
		Path:    "/" + object.Bucket + "/" + object.Key,
		RawPath: canonicalPath(object.Bucket, object.Key),
	}
	if object.VersionID != "" {
		target.RawQuery = "versionId=" + escape(object.VersionID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrObject, object, err)
	}
	if request.URL.EscapedPath() != target.RawPath || request.URL.RawQuery != target.RawQuery {
		return nil, fmt.Errorf("%w: %s does not keep its escaped path", ErrObject, object)
	}
	request.Host = authority
	// Without it Go's transport asks for gzip and decodes a gzip-encoded
	// object on the fly, and the digest would be the decoded bytes'.
	request.Header.Set("Accept-Encoding", "identity")
	return request, nil
}

// canonicalPath is "/bucket/key" with every byte of each element outside
// the unreserved set percent-encoded, once: what S3 expects a Signature
// Version 4 canonical URI to be, and what the request sends.
func canonicalPath(bucket, key string) string {
	elements := strings.Split(key, "/")
	for index, element := range elements {
		elements[index] = escape(element)
	}
	return "/" + escape(bucket) + "/" + strings.Join(elements, "/")
}

// escape percent-encodes every byte of value but the unreserved characters
// A-Z, a-z, 0-9, "-", ".", "_" and "~", in upper-case hex.
func escape(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var escaped strings.Builder
	for index := range len(value) {
		b := value[index]
		if 'A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~' {
			escaped.WriteByte(b)
			continue
		}
		escaped.WriteByte('%')
		escaped.WriteByte(hexDigits[b>>4])
		escaped.WriteByte(hexDigits[b&0x0f])
	}
	return escaped.String()
}

// sign adds a Signature Version 4 Authorization header for the S3 service to
// a GET with an empty body, dated at, over the host, the payload digest, the
// date and, for a temporary key pair, the session token.
func sign(request *http.Request, region string, credentials Credentials, at time.Time) {
	stamp := at.Format("20060102T150405Z")
	request.Header.Set("X-Amz-Date", stamp)
	request.Header.Set("X-Amz-Content-Sha256", emptyPayloadDigest)
	headers := map[string]string{
		"host":                 request.Host,
		"x-amz-content-sha256": emptyPayloadDigest,
		"x-amz-date":           stamp,
	}
	if credentials.SessionToken != "" {
		request.Header.Set("X-Amz-Security-Token", credentials.SessionToken)
		headers["x-amz-security-token"] = credentials.SessionToken
	}
	request.Header.Set("Authorization", authorization(request.Method, request.URL.EscapedPath(), request.URL.RawQuery,
		headers, region, credentials, at))
}

// authorization is the Signature Version 4 Authorization header of a request
// to the S3 service with an empty body: the method, the escaped path and the
// canonical query it sends, and the headers it signs, by lower-case name.
func authorization(method, path, query string, headers map[string]string, region string, credentials Credentials, at time.Time) string {
	stamp := at.Format("20060102T150405Z")
	day := at.Format("20060102")
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.Join(strings.Fields(headers[name]), " ") + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{method, path, query, canonicalHeaders.String(), signedHeaders, emptyPayloadDigest}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	requestDigest := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(requestDigest[:])
	key := hmacSHA256([]byte("AWS4"+credentials.SecretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	return "AWS4-HMAC-SHA256 Credential=" + credentials.AccessKeyID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

// errorDocument is the part of an S3 error response a refusal names: the
// code, such as NoSuchKey, AccessDenied, SignatureDoesNotMatch or
// ExpiredToken, and the server's sentence. Nothing else of it is read: a
// SignatureDoesNotMatch document also carries the string the server signed
// and the access key ID.
type errorDocument struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// refusal says why the endpoint did not return the object: the status, and
// the code and sentence of its error document when it sent one. A redirect is
// refused like any other answer that is not the object, and says so.
func refusal(response *http.Response, object Object) error {
	reason := response.Status
	var document errorDocument
	code := ""
	if body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes)); err == nil && len(body) > 0 {
		if xml.Unmarshal(body, &document) == nil {
			code = plain(document.Code)
			message := plain(document.Message)
			switch {
			case code != "" && message != "":
				reason += ", " + code + ": " + message
			case code != "":
				reason += ", " + code
			}
		}
	}
	refused := &endpointRefusal{code: code, versioned: object.VersionID != ""}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		refused.err = fmt.Errorf("%w: GET %s answered %s; every redirect is refused: name the endpoint and the region the bucket is served from",
			ErrRedirect, object, reason)
		return refused
	}
	refused.err = fmt.Errorf("%w: GET %s answered %s", ErrStatus, object, reason)
	return refused
}

// endpointRefusal is an answer from the endpoint that was not the object. It
// reads as the refusal it wraps, and keeps the error document's code and
// whether the request named a version, which is what Hint reads.
type endpointRefusal struct {
	err       error
	code      string
	versioned bool
}

func (r *endpointRefusal) Error() string { return r.err.Error() }
func (r *endpointRefusal) Unwrap() error { return r.err }

// The hints Hint gives.
const (
	// versionPermissionHint answers AccessDenied to a request that named a
	// version. Every operation after a Resolve names the version the Resolve
	// recorded, and S3 authorizes that request as s3:GetObjectVersion, so a
	// key pair or a bucket policy that grants only s3:GetObject resolves the
	// source and then fails every operation after it.
	versionPermissionHint = "a request for a version is authorized as s3:GetObjectVersion, not s3:GetObject: " +
		"grant s3:GetObjectVersion on the object to the key pair, or in the bucket policy for an unsigned read"
	// regionHint answers a signature whose scope names a region the endpoint
	// does not serve the bucket from.
	regionHint = "the request was signed for a region the endpoint does not serve the bucket from: set region to the bucket's region"
)

// Hint is what a person can change for a refusal err carries, when the
// error document's code names it, and "" otherwise. It is not part of err's
// message, so a caller that bounds the message can keep the hint whole. A
// redirect needs none: its message says what to change.
func Hint(err error) string {
	refused, ok := errors.AsType[*endpointRefusal](err)
	if !ok {
		return ""
	}
	switch {
	case refused.code == "AccessDenied" && refused.versioned:
		return versionPermissionHint
	case refused.code == "AuthorizationHeaderMalformed":
		return regionHint
	}
	return ""
}

// plain is a field of an error document as one bounded line of printable
// text.
func plain(value string) string {
	value = strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == ' '
	}), " ")
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= maxErrorFieldBytes {
		return value
	}
	cut := value[:maxErrorFieldBytes]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
