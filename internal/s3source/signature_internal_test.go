package s3source

import (
	"strings"
	"testing"
	"time"
)

// TestTheSignatureIsTheOneAWSPublishes holds the signer to the Signature
// Version 4 examples AWS publishes for S3 ("Signature Calculations for the
// Authorization Header: Transferring Payload in a Single Chunk"): the same
// key pair, date, region, path, query and headers give the same signature.
// The GET Object example signs a Range header the fetch never sends; the
// signer signs whatever headers it is given, so the example still holds it.
func TestTheSignatureIsTheOneAWSPublishes(t *testing.T) {
	t.Parallel()
	credentials := Credentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	at := time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC)
	base := func(extra map[string]string) map[string]string {
		headers := map[string]string{
			"host":                 "examplebucket.s3.amazonaws.com",
			"x-amz-content-sha256": emptyPayloadDigest,
			"x-amz-date":           "20130524T000000Z",
		}
		for name, value := range extra {
			headers[name] = value
		}
		return headers
	}
	for _, example := range []struct {
		name, path, query string
		headers           map[string]string
		signedHeaders     string
		signature         string
	}{
		{
			name: "GET Object", path: "/test.txt",
			headers:       base(map[string]string{"range": "bytes=0-9"}),
			signedHeaders: "host;range;x-amz-content-sha256;x-amz-date",
			signature:     "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "GET Bucket Lifecycle", path: "/", query: "lifecycle=",
			headers:       base(nil),
			signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature:     "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "GET Bucket (List Objects)", path: "/", query: "max-keys=2&prefix=J",
			headers:       base(nil),
			signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature:     "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			t.Parallel()
			got := authorization("GET", example.path, example.query, example.headers, "us-east-1", credentials, at)
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" +
				example.signedHeaders + ", Signature=" + example.signature
			if got != want {
				t.Fatalf("authorization:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestASignatureCoversWhatTheRequestSends holds sign to the request it signs:
// the header it adds covers the escaped path and the query the request
// sends, the Host it sends, the date and payload headers it adds, and the
// session token only when there is one. A signature over another path than
// the one sent would name another object, or fail at every server.
func TestASignatureCoversWhatTheRequestSends(t *testing.T) {
	t.Parallel()
	object := Object{
		Endpoint: "https://minio.example:9000", Region: "us-east-1", Bucket: "orders",
		Key: "releases/1.4 +ü%/orders.tar.gz", VersionID: "3/L4kq+tJ=",
	}
	at := time.Date(2026, time.November, 23, 10, 0, 0, 0, time.UTC)
	for _, credentials := range []Credentials{
		{AccessKeyID: "AKID", SecretAccessKey: "secret"},
		{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"},
	} {
		request, err := newRequest(t.Context(), object)
		if err != nil {
			t.Fatal(err)
		}
		sign(request, object.Region, credentials, at)
		const path = "/orders/releases/1.4%20%2B%C3%BC%25/orders.tar.gz"
		if request.URL.EscapedPath() != path || request.URL.RawQuery != "versionId=3%2FL4kq%2BtJ%3D" || request.Host != "minio.example:9000" {
			t.Fatalf("the request sends %s?%s to %s", request.URL.EscapedPath(), request.URL.RawQuery, request.Host)
		}
		headers := map[string]string{
			"host":                 "minio.example:9000",
			"x-amz-content-sha256": emptyPayloadDigest,
			"x-amz-date":           "20261123T100000Z",
		}
		if credentials.SessionToken != "" {
			headers["x-amz-security-token"] = "token"
		}
		want := authorization("GET", path, "versionId=3%2FL4kq%2BtJ%3D", headers, "us-east-1", credentials, at)
		if got := request.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization:\n got %s\nwant %s", got, want)
		}
		if got := request.Header.Get("X-Amz-Security-Token"); got != credentials.SessionToken {
			t.Errorf("X-Amz-Security-Token is %q, the credential's token is %q", got, credentials.SessionToken)
		}
		if strings.Contains(request.Header.Get("Authorization"), "secret") {
			t.Error("the Authorization header carries the secret access key")
		}
	}
}
