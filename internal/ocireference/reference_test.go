package ocireference_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/ocireference"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMatchRequestedUsesEffectiveSelector(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		requested string
		reported  string
	}{
		{name: "implicit latest", requested: "oci://registry.example/team/schema", reported: "oci://registry.example/team/schema:latest"},
		{name: "tag", requested: "oci://registry.example/team/schema:v1", reported: "oci://registry.example/team/schema:v1"},
		{name: "digest selects bytes", requested: "oci://registry.example/team/schema:display@" + digest, reported: "oci://registry.example/team/schema:other@" + digest},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := ocireference.MatchRequested(test.requested, test.reported); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMatchRequestedRejectsDifferentIdentity(t *testing.T) {
	t.Parallel()

	for _, reported := range []string{
		"oci://other.example/team/schema:v1",
		"oci://registry.example/other/schema:v1",
		"oci://registry.example/team/schema:v2",
	} {
		if err := ocireference.MatchRequested("oci://registry.example/team/schema:v1", reported); err == nil {
			t.Fatalf("MatchRequested() accepted %q", reported)
		}
	}
}

func TestValidateResolutionBindsRepositoryAndDigest(t *testing.T) {
	t.Parallel()

	if err := ocireference.ValidateResolution(
		"oci://registry.example/team/schema",
		"oci://registry.example/team/schema@"+digest,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []string{
		"oci://other.example/team/schema@" + digest,
		"oci://registry.example/other/schema@" + digest,
		"oci://registry.example/team/schema:v1",
		"oci://registry.example/team/schema@sha256:" + strings.Repeat("f", 64),
	} {
		if err := ocireference.ValidateResolution("oci://registry.example/team/schema", pinned, digest); err == nil {
			t.Fatalf("ValidateResolution() accepted %q", pinned)
		}
	}
}

func TestValidateResolutionCannotRedirectDigestSelectedRequest(t *testing.T) {
	t.Parallel()

	otherDigest := "sha256:" + strings.Repeat("f", 64)
	if err := ocireference.ValidateResolution(
		"oci://registry.example/team/schema@"+digest,
		"oci://registry.example/team/schema@"+otherDigest,
		otherDigest,
	); err == nil {
		t.Fatal("ValidateResolution() redirected a digest-selected request to different content")
	}
}

func TestParseRejectsCredentialAndTransportDecorations(t *testing.T) {
	t.Parallel()

	for _, reference := range []string{
		"oci://user:password@registry.example/team/schema:v1",
		"oci://registry.example/team/schema:v1?token=secret",
		"oci://registry.example/team/schema:v1#fragment",
		"oci://registry.example/team%2fschema:v1",
	} {
		if _, err := ocireference.Parse(reference); err == nil {
			t.Fatalf("Parse() accepted %q", reference)
		}
	}
}

func TestMatchAuthorityUsesExactCanonicalRegistry(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		reference string
		grant     string
	}{
		{reference: "oci://REGISTRY.example/team/schema:v1", grant: "registry.EXAMPLE"},
		{reference: "oci://registry.example:5443/team/schema:v1", grant: "REGISTRY.example:5443"},
		{reference: "oci://[2001:db8::1]:5443/team/schema:v1", grant: "[2001:DB8::1]:5443"},
		{reference: "oci://[2001:db8::1]/team/schema:v1", grant: "[2001:DB8::1]"},
		{reference: "oci://localhost:5000/team/schema:v1", grant: "LOCALHOST:5000"},
		{reference: "oci://docker.io/team/schema:v1", grant: "registry-1.docker.io"},
	} {
		if err := ocireference.MatchAuthority(test.reference, test.grant); err != nil {
			t.Errorf("MatchAuthority(%q, %q) error = %v", test.reference, test.grant, err)
		}
	}
}

func TestMatchAuthorityDoesNotWidenGrant(t *testing.T) {
	t.Parallel()

	for _, grant := range []string{
		"other.example",
		"registry.example:443",
		"registry.example.",
		"registry-1.docker.io",
	} {
		if err := ocireference.MatchAuthority("oci://registry.example/team/schema:v1", grant); err == nil {
			t.Errorf("MatchAuthority() accepted distinct authority %q", grant)
		}
	}
	if err := ocireference.MatchAuthority("oci://docker.io/team/schema:v1", "docker.io"); err == nil {
		t.Error("MatchAuthority() accepted the logical Docker registry instead of its effective HTTP authority")
	}
}

func TestCanonicalAuthorityRejectsDecorations(t *testing.T) {
	t.Parallel()

	for _, authority := range []string{
		"",
		" registry.example",
		"registry.example ",
		"https://registry.example",
		"user@registry.example",
		"registry.example/repository",
		"registry.example?query",
		"registry.example#fragment",
		"registry\\example",
		"registry.example:",
		"registry.example:http",
		"registry.example:0",
		"registry.example:65536",
		":443",
		"2001:db8::1",
		"[2001:db8::1",
		"[2001:db8::1]:",
		"[2001:db8::1]:http",
		"[127.0.0.1]:443",
		"registry%2eexample",
		"registry%2fexample",
		"registry.example\n",
		"r\u00e9gistry.example",
	} {
		if _, err := ocireference.CanonicalAuthority(authority); err == nil {
			t.Errorf("CanonicalAuthority() accepted %q", authority)
		}
	}
}
