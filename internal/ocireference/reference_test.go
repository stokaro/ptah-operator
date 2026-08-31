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
