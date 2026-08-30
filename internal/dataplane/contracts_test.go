package dataplane_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDecodeResolve(t *testing.T) {
	t.Parallel()
	data := `{"reference":"oci://registry.example/app:v1","pinned_reference":"oci://registry.example/app@` + digest + `","digest":"` + digest + `","media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`
	report, err := dataplane.DecodeResolve([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if report.Digest != digest {
		t.Fatalf("Digest = %q, want %q", report.Digest, digest)
	}
}

func TestDecodeVerifyRequiresArrayShape(t *testing.T) {
	t.Parallel()
	data := `{"reference":"oci://registry.example/app@` + digest + `","digest":"` + digest + `","satisfied":null,"findings":[]}`
	if _, err := dataplane.DecodeVerify([]byte(data)); err == nil {
		t.Fatal("DecodeVerify() accepted a null satisfied list")
	}
}

func TestDecodeDriftExpectedNegativeExit(t *testing.T) {
	t.Parallel()
	data := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}]}`
	if _, err := dataplane.DecodeDrift([]byte(data), 1); err != nil {
		t.Fatalf("DecodeDrift() rejected expected drift: %v", err)
	}
	if _, err := dataplane.DecodeDrift([]byte(data), 0); err == nil {
		t.Fatal("DecodeDrift() accepted an inconsistent exit code")
	}
}

func TestDecodePlanFailsClosed(t *testing.T) {
	t.Parallel()
	base := `{"format_version":1,"name":"p","dialect":"postgres","from_fingerprint":"from","to_fingerprint":"to","destructive":false,"statements":[{"sql":"DROP TABLE users","severity":"destructive","reason":"drops data"}]}`
	if _, err := dataplane.DecodePlan([]byte(base), "PostgreSQL"); err == nil || !strings.Contains(err.Error(), "destructive") {
		t.Fatalf("DecodePlan() error = %v, want destructive mismatch", err)
	}

	unknown := strings.Replace(base, `"destructive":false`, `"destructive":true,"future":1`, 1)
	if _, err := dataplane.DecodePlan([]byte(unknown), "PostgreSQL"); err == nil {
		t.Fatal("DecodePlan() accepted an unknown plan field")
	}
}
